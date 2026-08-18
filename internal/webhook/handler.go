package webhook

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/thomasvincent/github-runners-infra/internal/compute"
	gh "github.com/thomasvincent/github-runners-infra/internal/github"
	"github.com/thomasvincent/github-runners-infra/internal/state"
)

const maxBodySize = 1 * 1024 * 1024

var (
	safeNameRegex = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,99}$`)
	deliveryRegex = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)
	checksumRegex = regexp.MustCompile(`^[a-fA-F0-9]{64}$`)
	versionRegex  = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
)

// WorkflowJobEvent contains only the fields used by the controller.
type WorkflowJobEvent struct {
	Action       string      `json:"action"`
	WorkflowJob  WorkflowJob `json:"workflow_job"`
	Repo         RepoInfo    `json:"repository"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
}

type WorkflowJob struct {
	ID         int64    `json:"id"`
	Name       string   `json:"name"`
	Labels     []string `json:"labels"`
	RunnerID   int64    `json:"runner_id"`
	RunnerName string   `json:"runner_name"`
}

type RepoInfo struct {
	FullName string `json:"full_name"`
	Name     string `json:"name"`
	Private  bool   `json:"private"`
	Owner    struct {
		Login string `json:"login"`
	} `json:"owner"`
}

// GitHubClient is the narrow control-plane contract required by the worker.
type GitHubClient interface {
	GenerateRepoJITConfig(context.Context, string, string, string, int64, []string) (gh.JITConfig, error)
	RemoveRepoRunner(context.Context, string, string, int64) error
}

// ComputeClient owns cloud resources. Provider credentials stay behind this
// interface and are never rendered into runner user data.
type ComputeClient interface {
	Provider() string
	FindRunner(context.Context, string) (*compute.RunnerInstance, bool, error)
	CreateRunner(context.Context, compute.RunnerParams) (*compute.RunnerInstance, error)
	DeleteRunner(context.Context, string, string) error
	CleanupRunner(context.Context, string) error
}

// Handler authenticates webhooks and drives durable runner lifecycle workers.
type Handler struct {
	webhookSecret         []byte
	githubClient          GitHubClient
	computeClient         ComputeClient
	store                 state.Store
	requiredLabel         string
	allowedLabels         map[string]string
	allowedRepositories   map[string]struct{}
	runnerVersion         string
	runnerSHA256          string
	chefInstallerSHA256   string
	runnerGroupID         int64
	maxAttempts           int
	workerCount           int
	pollInterval          time.Duration
	maxRunnerAge          time.Duration
	cancelledRunnerTTL    time.Duration
	livenessSettleWindow  time.Duration
	livenessConfirmations int
	reaperTimeout         time.Duration
	installationID        int64
	provider              string
	notify                chan struct{}
	wg                    sync.WaitGroup
}

// Config holds controller configuration. Repository and label allowlists are
// mandatory so installing the GitHub App does not implicitly grant runner use.
type Config struct {
	WebhookSecret       []byte
	GitHubClient        GitHubClient
	ComputeClient       ComputeClient
	Store               state.Store
	RequiredLabel       string
	AllowedLabels       []string
	AllowedRepositories []string
	RunnerVersion       string
	RunnerSHA256        string
	ChefInstallerSHA256 string
	RunnerGroupID       int64
	WorkerCount         int
	MaxLiveRunners      int
	MaxAttempts         int
	PollInterval        time.Duration
	MaxRunnerAge        time.Duration
	CancelledRunnerTTL  time.Duration
	InstallationID      int64
}

func NewHandler(cfg Config) (*Handler, error) {
	if len(cfg.WebhookSecret) < 32 {
		return nil, fmt.Errorf("webhook secret must be at least 32 bytes")
	}
	if cfg.GitHubClient == nil || cfg.ComputeClient == nil || cfg.Store == nil {
		return nil, fmt.Errorf("GitHub client, compute client, and state store are required")
	}
	if cfg.InstallationID <= 0 {
		return nil, fmt.Errorf("GitHub App installation ID is required")
	}
	if !versionRegex.MatchString(cfg.RunnerVersion) {
		return nil, fmt.Errorf("runner version must use numeric x.y.z format")
	}
	if !checksumRegex.MatchString(cfg.RunnerSHA256) {
		return nil, fmt.Errorf("runner SHA-256 must be exactly 64 hexadecimal characters")
	}
	if !checksumRegex.MatchString(cfg.ChefInstallerSHA256) {
		return nil, fmt.Errorf("chef installer SHA-256 must be exactly 64 hexadecimal characters")
	}

	requiredLabel := strings.ToLower(strings.TrimSpace(cfg.RequiredLabel))
	if requiredLabel == "" {
		requiredLabel = "self-hosted"
	}
	allowedLabels := make(map[string]string, len(cfg.AllowedLabels))
	for _, raw := range cfg.AllowedLabels {
		label := strings.TrimSpace(raw)
		if !safeNameRegex.MatchString(label) {
			return nil, fmt.Errorf("invalid allowed runner label %q", raw)
		}
		allowedLabels[strings.ToLower(label)] = label
	}
	if _, ok := allowedLabels[requiredLabel]; !ok {
		return nil, fmt.Errorf("required label %q must be present in allowed labels", requiredLabel)
	}

	allowedRepositories := make(map[string]struct{}, len(cfg.AllowedRepositories))
	for _, raw := range cfg.AllowedRepositories {
		repo := strings.ToLower(strings.TrimSpace(raw))
		if !validRepository(repo) {
			return nil, fmt.Errorf("invalid allowed repository %q", raw)
		}
		allowedRepositories[repo] = struct{}{}
	}
	if len(allowedRepositories) == 0 {
		return nil, fmt.Errorf("at least one allowed repository is required")
	}

	workerCount := cfg.WorkerCount
	if workerCount <= 0 {
		workerCount = 4
	}
	maxLiveRunners := cfg.MaxLiveRunners
	if maxLiveRunners <= 0 {
		maxLiveRunners = 20
	}
	if err := cfg.Store.SetMaxLiveRunners(maxLiveRunners); err != nil {
		return nil, err
	}
	maxAttempts := cfg.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 5
	}
	pollInterval := cfg.PollInterval
	if pollInterval <= 0 {
		pollInterval = time.Second
	}
	maxRunnerAge := cfg.MaxRunnerAge
	if maxRunnerAge <= 0 {
		maxRunnerAge = 6 * time.Hour
	}
	if maxRunnerAge < time.Hour {
		return nil, fmt.Errorf("maximum runner age must be at least one hour")
	}
	cancelledRunnerTTL := cfg.CancelledRunnerTTL
	if cancelledRunnerTTL <= 0 {
		cancelledRunnerTTL = 5 * time.Minute
	}
	if cancelledRunnerTTL < time.Minute {
		return nil, fmt.Errorf("cancelled runner TTL must be at least one minute")
	}
	runnerGroupID := cfg.RunnerGroupID
	if runnerGroupID <= 0 {
		runnerGroupID = 1
	}

	return &Handler{
		webhookSecret:         append([]byte(nil), cfg.WebhookSecret...),
		githubClient:          cfg.GitHubClient,
		computeClient:         cfg.ComputeClient,
		store:                 cfg.Store,
		requiredLabel:         requiredLabel,
		allowedLabels:         allowedLabels,
		allowedRepositories:   allowedRepositories,
		runnerVersion:         cfg.RunnerVersion,
		runnerSHA256:          strings.ToLower(cfg.RunnerSHA256),
		chefInstallerSHA256:   strings.ToLower(cfg.ChefInstallerSHA256),
		runnerGroupID:         runnerGroupID,
		maxAttempts:           maxAttempts,
		workerCount:           workerCount,
		pollInterval:          pollInterval,
		maxRunnerAge:          maxRunnerAge,
		cancelledRunnerTTL:    cancelledRunnerTTL,
		livenessSettleWindow:  2 * time.Minute,
		livenessConfirmations: 3,
		reaperTimeout:         45 * time.Second,
		installationID:        cfg.InstallationID,
		provider:              cfg.ComputeClient.Provider(),
		notify:                make(chan struct{}, 1),
	}, nil
}

// Start launches durable workers. Pending work is loaded from the store, so a
// process restart does not lose an accepted webhook.
func (h *Handler) Start(ctx context.Context) {
	for i := 0; i < h.workerCount; i++ {
		h.wg.Add(1)
		go h.worker(ctx)
	}
	h.wg.Add(1)
	go h.reaper(ctx)
	h.signal()
}

// Wait blocks until all workers have stopped after their context is canceled.
func (h *Handler) Wait() {
	h.wg.Wait()
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	clientIP := r.RemoteAddr
	if !gh.VerifyWebhookSignature(body, r.Header.Get("X-Hub-Signature-256"), h.webhookSecret, clientIP) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Header.Get("X-GitHub-Event") != "workflow_job" {
		writeResponse(w, http.StatusOK, "ignored")
		return
	}

	var event WorkflowJobEvent
	if err := json.Unmarshal(body, &event); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if event.Installation.ID != h.installationID {
		log.Printf("SECURITY: rejected GitHub App installation %d", event.Installation.ID)
		http.Error(w, "installation not authorized", http.StatusForbidden)
		return
	}
	repository, err := h.validateRepository(event.Repo)
	if err != nil {
		log.Printf("SECURITY: rejected workflow job %d: %v", event.WorkflowJob.ID, err)
		http.Error(w, "repository not authorized", http.StatusForbidden)
		return
	}
	key := jobKey(repository, event.WorkflowJob.ID)

	switch event.Action {
	case "queued":
		h.handleQueued(w, r, event, repository, key)
	case "completed":
		if _, err := h.validateLabels(event.WorkflowJob.Labels); err != nil {
			writeResponse(w, http.StatusOK, "ignored")
			return
		}
		if err := h.store.RecordCompletion(r.Context(), state.Record{
			Key: key, JobID: event.WorkflowJob.ID, Owner: event.Repo.Owner.Login,
			Repository: event.Repo.Name, Provider: h.provider,
			GitHubRunnerID: event.WorkflowJob.RunnerID, RunnerName: event.WorkflowJob.RunnerName,
			NextAttemptAt: time.Now().UTC().Add(h.cancelledRunnerTTL),
		}); err != nil {
			http.Error(w, "state unavailable", http.StatusServiceUnavailable)
			return
		}
		h.signal()
		writeResponse(w, http.StatusAccepted, "completion recorded")
	default:
		writeResponse(w, http.StatusOK, "ignored")
	}
}

func (h *Handler) handleQueued(w http.ResponseWriter, r *http.Request, event WorkflowJobEvent, repository, key string) {
	deliveryID := r.Header.Get("X-GitHub-Delivery")
	if !deliveryRegex.MatchString(deliveryID) {
		http.Error(w, "missing or invalid delivery ID", http.StatusBadRequest)
		return
	}
	labels, err := h.validateLabels(event.WorkflowJob.Labels)
	if err != nil {
		log.Printf("SECURITY: rejected labels for %s job %d: %v", repository, event.WorkflowJob.ID, err)
		http.Error(w, "runner labels not authorized", http.StatusForbidden)
		return
	}
	created, err := h.store.Create(r.Context(), state.Record{
		Key:        key,
		DeliveryID: deliveryID,
		JobID:      event.WorkflowJob.ID,
		Owner:      event.Repo.Owner.Login,
		Repository: event.Repo.Name,
		Labels:     labels,
		Provider:   h.provider,
	})
	if err != nil {
		log.Printf("ERROR: persist job %s: %v", key, err)
		http.Error(w, "state unavailable", http.StatusServiceUnavailable)
		return
	}
	if !created {
		writeResponse(w, http.StatusOK, "duplicate")
		return
	}
	h.signal()
	writeResponse(w, http.StatusAccepted, "queued")
}

func (h *Handler) worker(ctx context.Context) {
	defer h.wg.Done()
	ticker := time.NewTicker(h.pollInterval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		record, kind, err := h.store.ClaimNext(ctx, time.Now(), h.maxAttempts)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("ERROR: claim runner work: %v", err)
		} else if record != nil {
			if ctx.Err() != nil {
				if releaseErr := h.store.ReleaseClaim(context.Background(), record.Key, kind); releaseErr != nil {
					log.Printf("ERROR: release unstarted %s claim for %s: %v", kind, record.Key, releaseErr)
				}
				return
			}
			h.process(ctx, *record, kind)
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-h.notify:
		case <-ticker.C:
		}
	}
}

func (h *Handler) reaper(ctx context.Context) {
	defer h.wg.Done()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		if err := h.reapOnce(ctx); err != nil && ctx.Err() == nil {
			log.Printf("ERROR: reconcile expired runners: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (h *Handler) reapOnce(ctx context.Context) error {
	reapCtx, cancel := context.WithTimeout(ctx, h.reaperTimeout)
	defer cancel()
	return h.expireRunners(reapCtx)
}

func (h *Handler) expireRunners(ctx context.Context) error {
	var reconciliationErrors []error
	if _, err := h.store.PruneDeleted(ctx); err != nil {
		reconciliationErrors = append(reconciliationErrors, fmt.Errorf("prune deleted lifecycle records: %w", err))
	}
	provisioned, err := h.store.ListProvisioned(ctx)
	if err != nil {
		reconciliationErrors = append(reconciliationErrors, fmt.Errorf("list provisioned runners: %w", err))
	} else {
		for _, record := range provisioned {
			if record.Provider != h.provider {
				continue
			}
			_, found, findErr := h.computeClient.FindRunner(ctx, record.Key)
			if findErr != nil {
				reconciliationErrors = append(reconciliationErrors, fmt.Errorf("check runner liveness for %s: %w", record.Key, findErr))
				continue
			}
			if found {
				if record.MissingChecks != 0 {
					if seenErr := h.store.RecordRunnerSeen(ctx, record.Key); seenErr != nil {
						reconciliationErrors = append(reconciliationErrors, fmt.Errorf("record runner seen for %s: %w", record.Key, seenErr))
					}
				}
				continue
			}
			confirmed, observeErr := h.store.ObserveRunnerMissing(
				ctx, record.Key, time.Now(), h.livenessSettleWindow, h.livenessConfirmations,
			)
			if observeErr != nil {
				reconciliationErrors = append(reconciliationErrors, fmt.Errorf("record missing runner %s: %w", record.Key, observeErr))
				continue
			}
			if !confirmed {
				continue
			}
			if record.GitHubRunnerID != 0 && record.GitHubRunnerOwned {
				if removeErr := h.githubClient.RemoveRepoRunner(ctx, record.Owner, record.Repository, record.GitHubRunnerID); removeErr != nil {
					reconciliationErrors = append(reconciliationErrors, fmt.Errorf("remove missing instance JIT runner %d: %w", record.GitHubRunnerID, removeErr))
					continue
				}
			}
			if requeueErr := h.store.RequeueMissingRunner(ctx, record.Key, h.maxAttempts); requeueErr != nil {
				reconciliationErrors = append(reconciliationErrors, fmt.Errorf("requeue missing runner %s: %w", record.Key, requeueErr))
				continue
			}
			log.Printf("WARN: provider instance for %s disappeared; queued a replacement", record.Key)
			h.signal()
		}
	}
	records, err := h.store.ListExpired(ctx, time.Now().Add(-h.maxRunnerAge))
	if err != nil {
		return err
	}
	for _, record := range records {
		if record.Provider != h.provider {
			err := fmt.Errorf("cannot expire runner %s owned by provider %q with configured provider %q", record.Key, record.Provider, h.provider)
			log.Printf("ERROR: %v", err)
			reconciliationErrors = append(reconciliationErrors, err)
			continue
		}
		if record.InstanceID == "" {
			if record.GitHubRunnerID != 0 && record.GitHubRunnerOwned {
				if err := h.githubClient.RemoveRepoRunner(ctx, record.Owner, record.Repository, record.GitHubRunnerID); err != nil {
					reconciliationErrors = append(reconciliationErrors, fmt.Errorf("remove expired JIT runner %d: %w", record.GitHubRunnerID, err))
					continue
				}
			}
			if err := h.computeClient.CleanupRunner(ctx, record.Key); err != nil {
				reconciliationErrors = append(reconciliationErrors, fmt.Errorf("clean up untracked provider resources for %s: %w", record.Key, err))
				continue
			}
			if err := h.store.MarkDeleted(ctx, record.Key); err != nil {
				reconciliationErrors = append(reconciliationErrors, err)
				continue
			}
			continue
		}
		if record.DeferDeletion && record.GitHubRunnerID != 0 && record.GitHubRunnerOwned {
			// Deregistration fails while GitHub considers a runner busy. Doing it
			// before cloud deletion both prevents new assignment and fails closed
			// if a cancelled job's runner picked up different work.
			if err := h.githubClient.RemoveRepoRunner(ctx, record.Owner, record.Repository, record.GitHubRunnerID); err != nil {
				reconciliationErrors = append(reconciliationErrors, fmt.Errorf("deregister deferred-cancellation runner %d: %w", record.GitHubRunnerID, err))
				continue
			}
			if err := h.store.ClearJIT(ctx, record.Key); err != nil {
				reconciliationErrors = append(reconciliationErrors, fmt.Errorf("clear deferred-cancellation runner %d: %w", record.GitHubRunnerID, err))
				continue
			}
		}
		log.Printf("WARN: runner %s exceeded maximum lifetime; scheduling owned instance %s for deletion", record.Key, record.InstanceID)
		if err := h.store.ScheduleDeletion(ctx, record.Key); err != nil {
			reconciliationErrors = append(reconciliationErrors, err)
			continue
		}
	}
	if len(records) != 0 {
		h.signal()
	}
	return errors.Join(reconciliationErrors...)
}

func (h *Handler) process(parent context.Context, record state.Record, kind state.WorkKind) {
	if parent.Err() != nil {
		if err := h.store.ReleaseClaim(context.Background(), record.Key, kind); err != nil {
			log.Printf("ERROR: release canceled %s claim for %s: %v", kind, record.Key, err)
		}
		return
	}
	if record.Provider != h.provider {
		err := fmt.Errorf("state record belongs to provider %q, controller is configured for %q", record.Provider, h.provider)
		switch kind {
		case state.WorkProvision:
			h.provisionFailed(record, err)
		case state.WorkDelete:
			if stateErr := h.store.MarkOrphaned(context.Background(), record.Key, err.Error()); stateErr != nil {
				log.Printf("ERROR: persist provider mismatch for %s: %v", record.Key, stateErr)
			}
		}
		return
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Minute)
	defer cancel()
	switch kind {
	case state.WorkProvision:
		h.provision(ctx, record)
	case state.WorkDelete:
		h.delete(ctx, record)
	}
}

func (h *Handler) provision(ctx context.Context, record state.Record) {
	runnerName := runnerName(record.Repository, record.JobID)
	existing, found, err := h.computeClient.FindRunner(ctx, record.Key)
	if err != nil {
		h.provisionFailed(record, fmt.Errorf("reconcile runner instance: %w", err))
		return
	}
	if found {
		if record.GitHubRunnerID == 0 {
			if deleteErr := h.computeClient.DeleteRunner(ctx, existing.ID, record.Key); deleteErr != nil {
				h.provisionFailed(record, fmt.Errorf("delete runner with missing JIT identity: %w", deleteErr))
				return
			}
			h.provisionFailed(record, fmt.Errorf("removed runner with missing JIT identity"))
			return
		}
		if err := h.store.MarkProvisioned(context.Background(), record.Key, existing.ID, record.GitHubRunnerID, existing.Name); err != nil {
			log.Printf("ERROR: persist reconciled runner %s: %v", record.Key, err)
			h.provisionFailed(record, fmt.Errorf("persist reconciled runner: %w", err))
		}
		h.signal()
		return
	}
	if record.GitHubRunnerID != 0 && record.GitHubRunnerOwned {
		if err := h.githubClient.RemoveRepoRunner(ctx, record.Owner, record.Repository, record.GitHubRunnerID); err != nil {
			h.provisionFailed(record, fmt.Errorf("remove stale JIT runner %d: %w", record.GitHubRunnerID, err))
			return
		}
		if err := h.store.ClearJIT(context.Background(), record.Key); err != nil {
			h.provisionFailed(record, fmt.Errorf("clear stale JIT runner state: %w", err))
			return
		}
	}
	jit, err := h.githubClient.GenerateRepoJITConfig(
		ctx, record.Owner, record.Repository, runnerName, h.runnerGroupID, record.Labels,
	)
	if err != nil {
		h.provisionFailed(record, fmt.Errorf("generate JIT config: %w", err))
		return
	}
	if err := h.store.MarkJITCreated(context.Background(), record.Key, jit.RunnerID, runnerName); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		cleanupErr := h.githubClient.RemoveRepoRunner(cleanupCtx, record.Owner, record.Repository, jit.RunnerID)
		cancel()
		if cleanupErr != nil {
			log.Printf("WARN: remove unpersisted JIT runner %d: %v", jit.RunnerID, cleanupErr)
		}
		h.provisionFailed(record, fmt.Errorf("persist JIT runner identity: %w", err))
		return
	}
	record.GitHubRunnerID = jit.RunnerID
	record.RunnerName = runnerName

	instance, err := h.computeClient.CreateRunner(ctx, compute.RunnerParams{
		JobKey:              record.Key,
		ProvisionEpoch:      record.ProvisionEpoch,
		RunnerName:          runnerName,
		RunnerJITConfig:     jit.EncodedConfig,
		RunnerVersion:       h.runnerVersion,
		RunnerSHA256:        h.runnerSHA256,
		ChefInstallerSHA256: h.chefInstallerSHA256,
	})
	if err != nil {
		h.provisionFailed(record, fmt.Errorf("create runner instance: %w", err))
		return
	}
	if err := h.store.MarkProvisioned(context.Background(), record.Key, instance.ID, jit.RunnerID, runnerName); err != nil {
		log.Printf("ERROR: persist provisioned runner %s: %v", record.Key, err)
		h.provisionFailed(record, fmt.Errorf("persist provisioned runner: %w", err))
		return
	}
	log.Printf("Provisioned runner %s (instance %s) for %s/%s job %d", runnerName, instance.ID, record.Owner, record.Repository, record.JobID)
	h.signal()
}

func (h *Handler) provisionFailed(record state.Record, err error) {
	if record.Attempts >= h.maxAttempts {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		cleanupErr := h.computeClient.CleanupRunner(cleanupCtx, record.Key)
		cancel()
		if errors.Is(cleanupErr, compute.ErrOwnershipMismatch) {
			if record.GitHubRunnerID != 0 {
				removeCtx, removeCancel := context.WithTimeout(context.Background(), 30*time.Second)
				removeErr := h.githubClient.RemoveRepoRunner(removeCtx, record.Owner, record.Repository, record.GitHubRunnerID)
				removeCancel()
				if removeErr == nil {
					_ = h.store.ClearJIT(context.Background(), record.Key)
				}
			}
			if stateErr := h.store.MarkOrphaned(context.Background(), record.Key, cleanupErr.Error()); stateErr != nil {
				log.Printf("ERROR: persist orphaned failed provision for %s: %v", record.Key, stateErr)
			}
			log.Printf("ERROR: failed provision for %s requires operator cleanup: %v", record.Key, cleanupErr)
			return
		}
		if cleanupErr != nil {
			log.Printf("WARN: clean up exhausted provision for %s: %v", record.Key, cleanupErr)
		}
		if record.GitHubRunnerID != 0 {
			removeCtx, removeCancel := context.WithTimeout(context.Background(), 30*time.Second)
			removeErr := h.githubClient.RemoveRepoRunner(removeCtx, record.Owner, record.Repository, record.GitHubRunnerID)
			removeCancel()
			if removeErr != nil {
				log.Printf("WARN: deregister exhausted JIT runner %d: %v", record.GitHubRunnerID, removeErr)
			} else if clearErr := h.store.ClearJIT(context.Background(), record.Key); clearErr != nil {
				log.Printf("WARN: clear exhausted JIT runner %d: %v", record.GitHubRunnerID, clearErr)
			}
		}
	}
	retryAt := time.Now().Add(retryDelay(record.Attempts))
	if stateErr := h.store.MarkProvisionFailed(context.Background(), record.Key, err.Error(), retryAt, h.maxAttempts); stateErr != nil {
		log.Printf("ERROR: persist provisioning failure for %s: %v", record.Key, stateErr)
	}
	log.Printf("ERROR: provision attempt %d for %s: %v", record.Attempts, record.Key, err)
	h.signal()
}

func (h *Handler) delete(ctx context.Context, record state.Record) {
	if err := h.computeClient.DeleteRunner(ctx, record.InstanceID, record.Key); err != nil {
		if errors.Is(err, compute.ErrOwnershipMismatch) {
			if stateErr := h.store.MarkOrphaned(context.Background(), record.Key, err.Error()); stateErr != nil {
				log.Printf("ERROR: persist orphaned runner %s: %v", record.Key, stateErr)
			}
			log.Printf("ERROR: runner %s requires operator cleanup: %v", record.Key, err)
			return
		}
		retryAt := time.Now().Add(retryDelay(record.DeleteAttempts))
		if stateErr := h.store.MarkDeleteFailed(context.Background(), record.Key, err.Error(), retryAt, h.maxAttempts); stateErr != nil {
			log.Printf("ERROR: persist deletion failure for %s: %v", record.Key, stateErr)
		}
		log.Printf("ERROR: delete attempt %d for %s: %v", record.DeleteAttempts, record.Key, err)
		h.signal()
		return
	}
	if record.GitHubRunnerID != 0 {
		if err := h.githubClient.RemoveRepoRunner(ctx, record.Owner, record.Repository, record.GitHubRunnerID); err != nil {
			retryAt := time.Now().Add(retryDelay(record.DeleteAttempts))
			if stateErr := h.store.MarkDeleteFailed(context.Background(), record.Key, err.Error(), retryAt, h.maxAttempts); stateErr != nil {
				log.Printf("ERROR: persist GitHub runner deletion failure for %s: %v", record.Key, stateErr)
			}
			h.signal()
			return
		}
	}
	if err := h.store.MarkDeleted(context.Background(), record.Key); err != nil {
		log.Printf("ERROR: persist deleted runner %s: %v", record.Key, err)
		return
	}
	log.Printf("Deleted owned runner instance %s for %s", record.InstanceID, record.Key)
}

func (h *Handler) validateRepository(repo RepoInfo) (string, error) {
	if !safeNameRegex.MatchString(repo.Owner.Login) || !safeNameRegex.MatchString(repo.Name) {
		return "", fmt.Errorf("invalid owner or repository name")
	}
	fullName := strings.ToLower(repo.Owner.Login + "/" + repo.Name)
	if !strings.EqualFold(repo.FullName, fullName) {
		return "", fmt.Errorf("repository identity fields disagree")
	}
	if !repo.Private {
		return "", fmt.Errorf("public repositories are not permitted")
	}
	if _, ok := h.allowedRepositories[fullName]; !ok {
		return "", fmt.Errorf("repository %s is not allowlisted", fullName)
	}
	return fullName, nil
}

func (h *Handler) validateLabels(requested []string) ([]string, error) {
	seenRequired := false
	result := make([]string, 0, len(requested))
	seen := make(map[string]struct{}, len(requested))
	for _, raw := range requested {
		key := strings.ToLower(strings.TrimSpace(raw))
		canonical, ok := h.allowedLabels[key]
		if !ok {
			return nil, fmt.Errorf("label %q is not allowlisted", raw)
		}
		if key == h.requiredLabel {
			seenRequired = true
		}
		if _, duplicate := seen[key]; !duplicate {
			result = append(result, canonical)
			seen[key] = struct{}{}
		}
	}
	if !seenRequired {
		return nil, fmt.Errorf("required label %q is missing", h.requiredLabel)
	}
	return result, nil
}

func (h *Handler) signal() {
	select {
	case h.notify <- struct{}{}:
	default:
	}
}

func validRepository(repo string) bool {
	parts := strings.Split(repo, "/")
	return len(parts) == 2 && safeNameRegex.MatchString(parts[0]) && safeNameRegex.MatchString(parts[1])
}

func jobKey(repository string, jobID int64) string {
	return fmt.Sprintf("%s:%d", strings.ToLower(repository), jobID)
}

func runnerName(repository string, jobID int64) string {
	base := strings.Map(func(character rune) rune {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' {
			return character
		}
		return '-'
	}, strings.ToLower(repository))
	hash := sha256.Sum256([]byte(jobKey(repository, jobID)))
	suffix := "-" + hex.EncodeToString(hash[:6])
	maxBase := 63 - len("eph-") - len(suffix)
	if len(base) > maxBase {
		base = base[:maxBase]
	}
	base = strings.TrimRight(base, "-")
	return "eph-" + base + suffix
}

func retryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 6 {
		attempt = 6
	}
	return time.Duration(1<<(attempt-1)) * 5 * time.Second
}

func writeResponse(w http.ResponseWriter, status int, message string) {
	w.WriteHeader(status)
	_, _ = fmt.Fprint(w, message)
}
