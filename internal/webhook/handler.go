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
	RepoRunnerStatus(context.Context, string, string, int64) (gh.RunnerStatus, error)
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
	SweepOrphanedRunners(context.Context, map[string]struct{}, time.Time) (int, error)
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
	registrationTimeout   time.Duration
	livenessSettleWindow  time.Duration
	livenessConfirmations int
	reaperTimeout         time.Duration
	livenessCheckInterval time.Duration
	installationID        int64
	provider              string
	notify                chan struct{}
	ingestSlots           chan struct{}
	ingestWait            time.Duration
	wg                    sync.WaitGroup
}

// Config holds controller configuration. Repository and label allowlists are
// mandatory so installing the GitHub App does not implicitly grant runner use.
type Config struct {
	WebhookSecret         []byte
	GitHubClient          GitHubClient
	ComputeClient         ComputeClient
	Store                 state.Store
	RequiredLabel         string
	AllowedLabels         []string
	AllowedRepositories   []string
	RunnerVersion         string
	RunnerSHA256          string
	ChefInstallerSHA256   string
	RunnerGroupID         int64
	WorkerCount           int
	MaxLiveRunners        int
	MaxAttempts           int
	PollInterval          time.Duration
	MaxRunnerAge          time.Duration
	CancelledRunnerTTL    time.Duration
	RegistrationTimeout   time.Duration
	LivenessSettleWindow  time.Duration
	LivenessConfirmations int
	InstallationID        int64
}

func NewHandler(cfg Config) (*Handler, error) {
	if err := validateHandlerConfig(cfg); err != nil {
		return nil, err
	}
	requiredLabel, allowedLabels, err := normalizeAllowedLabels(cfg.RequiredLabel, cfg.AllowedLabels)
	if err != nil {
		return nil, err
	}
	allowedRepositories, err := normalizeAllowedRepositories(cfg.AllowedRepositories)
	if err != nil {
		return nil, err
	}
	limits, err := resolveHandlerLimits(cfg)
	if err != nil {
		return nil, err
	}
	if err := cfg.Store.SetMaxLiveRunners(limits.maxLiveRunners); err != nil {
		return nil, err
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
		runnerGroupID:         limits.runnerGroupID,
		maxAttempts:           limits.maxAttempts,
		workerCount:           limits.workerCount,
		pollInterval:          limits.pollInterval,
		maxRunnerAge:          limits.maxRunnerAge,
		cancelledRunnerTTL:    limits.cancelledRunnerTTL,
		registrationTimeout:   limits.registrationTimeout,
		livenessSettleWindow:  limits.livenessSettleWindow,
		livenessConfirmations: limits.livenessConfirmations,
		reaperTimeout:         max(5*time.Minute, time.Duration(limits.maxLiveRunners+10)*30*time.Second),
		livenessCheckInterval: 5 * time.Minute,
		installationID:        cfg.InstallationID,
		provider:              cfg.ComputeClient.Provider(),
		notify:                make(chan struct{}, 1),
		ingestSlots:           make(chan struct{}, 64),
		ingestWait:            3 * time.Second,
	}, nil
}

func validateHandlerConfig(cfg Config) error {
	if len(cfg.WebhookSecret) < 32 {
		return fmt.Errorf("webhook secret must be at least 32 bytes")
	}
	if cfg.GitHubClient == nil || cfg.ComputeClient == nil || cfg.Store == nil {
		return fmt.Errorf("GitHub client, compute client, and state store are required")
	}
	if cfg.InstallationID <= 0 {
		return fmt.Errorf("GitHub App installation ID is required")
	}
	if !versionRegex.MatchString(cfg.RunnerVersion) {
		return fmt.Errorf("runner version must use numeric x.y.z format")
	}
	if !checksumRegex.MatchString(cfg.RunnerSHA256) {
		return fmt.Errorf("runner SHA-256 must be exactly 64 hexadecimal characters")
	}
	if !checksumRegex.MatchString(cfg.ChefInstallerSHA256) {
		return fmt.Errorf("chef installer SHA-256 must be exactly 64 hexadecimal characters")
	}
	return nil
}

func normalizeAllowedLabels(required string, rawLabels []string) (string, map[string]string, error) {
	requiredLabel := strings.ToLower(strings.TrimSpace(required))
	if requiredLabel == "" {
		requiredLabel = "self-hosted"
	}
	allowedLabels := make(map[string]string, len(rawLabels))
	for _, raw := range rawLabels {
		label := strings.TrimSpace(raw)
		if !safeNameRegex.MatchString(label) {
			return "", nil, fmt.Errorf("invalid allowed runner label %q", raw)
		}
		allowedLabels[strings.ToLower(label)] = label
	}
	if _, ok := allowedLabels[requiredLabel]; !ok {
		return "", nil, fmt.Errorf("required label %q must be present in allowed labels", requiredLabel)
	}
	return requiredLabel, allowedLabels, nil
}

func normalizeAllowedRepositories(rawRepositories []string) (map[string]struct{}, error) {
	allowedRepositories := make(map[string]struct{}, len(rawRepositories))
	for _, raw := range rawRepositories {
		repo := strings.ToLower(strings.TrimSpace(raw))
		if !validRepository(repo) {
			return nil, fmt.Errorf("invalid allowed repository %q", raw)
		}
		allowedRepositories[repo] = struct{}{}
	}
	if len(allowedRepositories) == 0 {
		return nil, fmt.Errorf("at least one allowed repository is required")
	}
	return allowedRepositories, nil
}

type handlerLimits struct {
	workerCount, maxLiveRunners, maxAttempts int
	pollInterval, maxRunnerAge               time.Duration
	cancelledRunnerTTL                       time.Duration
	registrationTimeout                      time.Duration
	livenessSettleWindow                     time.Duration
	livenessConfirmations                    int
	runnerGroupID                            int64
}

func resolveHandlerLimits(cfg Config) (handlerLimits, error) {
	limits := handlerLimits{
		workerCount: cfg.WorkerCount, maxLiveRunners: cfg.MaxLiveRunners, maxAttempts: cfg.MaxAttempts,
		pollInterval: cfg.PollInterval, maxRunnerAge: cfg.MaxRunnerAge, cancelledRunnerTTL: cfg.CancelledRunnerTTL,
		registrationTimeout:   cfg.RegistrationTimeout,
		livenessSettleWindow:  cfg.LivenessSettleWindow,
		livenessConfirmations: cfg.LivenessConfirmations,
		runnerGroupID:         cfg.RunnerGroupID,
	}
	if limits.workerCount <= 0 {
		limits.workerCount = 4
	}
	if limits.maxLiveRunners <= 0 {
		limits.maxLiveRunners = 20
	}
	if limits.maxAttempts <= 0 {
		limits.maxAttempts = 5
	}
	if limits.pollInterval <= 0 {
		limits.pollInterval = time.Second
	}
	if limits.maxRunnerAge <= 0 {
		limits.maxRunnerAge = 6 * time.Hour
	}
	if limits.maxRunnerAge < time.Hour {
		return limits, fmt.Errorf("maximum runner age must be at least one hour")
	}
	if limits.cancelledRunnerTTL <= 0 {
		limits.cancelledRunnerTTL = 5 * time.Minute
	}
	if limits.cancelledRunnerTTL < time.Minute {
		return limits, fmt.Errorf("cancelled runner TTL must be at least one minute")
	}
	if limits.registrationTimeout <= 0 {
		limits.registrationTimeout = 10 * time.Minute
	}
	if limits.registrationTimeout < time.Minute || limits.registrationTimeout > time.Hour {
		return limits, fmt.Errorf("runner registration timeout must be between one minute and one hour")
	}
	if limits.livenessSettleWindow <= 0 {
		limits.livenessSettleWindow = 2 * time.Minute
	}
	if limits.livenessConfirmations <= 0 {
		limits.livenessConfirmations = 3
	}
	if limits.runnerGroupID <= 0 {
		limits.runnerGroupID = 1
	}
	return limits, nil
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
	ingestTimer := time.NewTimer(h.ingestWait)
	defer ingestTimer.Stop()
	select {
	case h.ingestSlots <- struct{}{}:
		defer func() { <-h.ingestSlots }()
	case <-ingestTimer.C:
		http.Error(w, "webhook ingest busy; retry later", http.StatusServiceUnavailable)
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
	repository, targeted, err := h.validateRepository(event.Repo)
	if err != nil {
		log.Printf("SECURITY: rejected workflow job %d: %v", event.WorkflowJob.ID, err)
		http.Error(w, "repository not authorized", http.StatusForbidden)
		return
	}
	if !targeted {
		writeResponse(w, http.StatusOK, "ignored")
		return
	}
	key := jobKey(repository, event.WorkflowJob.ID)

	switch event.Action {
	case "queued":
		labels, targeted, err := h.authorizeLabels(event.WorkflowJob.Labels)
		if !targeted {
			writeResponse(w, http.StatusOK, "ignored")
			return
		}
		if err != nil {
			log.Printf("SECURITY: rejected labels for %s job %d: %v", repository, event.WorkflowJob.ID, err)
			http.Error(w, "runner labels not authorized", http.StatusForbidden)
			return
		}
		h.handleQueued(w, r, event, repository, key, labels)
	case "completed":
		_, targeted, err := h.authorizeLabels(event.WorkflowJob.Labels)
		if !targeted {
			writeResponse(w, http.StatusOK, "ignored")
			return
		}
		if err != nil {
			log.Printf("SECURITY: rejected labels for %s job %d: %v", repository, event.WorkflowJob.ID, err)
			http.Error(w, "runner labels not authorized", http.StatusForbidden)
			return
		}
		if _, found, getErr := h.store.Get(r.Context(), key); getErr != nil {
			http.Error(w, "state unavailable", http.StatusServiceUnavailable)
			return
		} else if !found {
			owned, ownershipErr := h.store.OwnsRunner(r.Context(), event.Repo.Owner.Login, event.Repo.Name, event.WorkflowJob.RunnerID)
			if ownershipErr != nil {
				http.Error(w, "state unavailable", http.StatusServiceUnavailable)
				return
			}
			if !owned && !looksLikeEphemeralRunnerName(event.WorkflowJob.RunnerName) {
				writeResponse(w, http.StatusOK, "ignored")
				return
			}
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

func (h *Handler) handleQueued(w http.ResponseWriter, r *http.Request, event WorkflowJobEvent, repository, key string, labels []string) {
	deliveryID := r.Header.Get("X-GitHub-Delivery")
	if !deliveryRegex.MatchString(deliveryID) {
		http.Error(w, "missing or invalid delivery ID", http.StatusBadRequest)
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
				if releaseErr := h.store.ReleaseClaim(context.WithoutCancel(ctx), record.Key, kind); releaseErr != nil {
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
	cutoff := time.Now().Add(-h.maxRunnerAge)
	if _, err := h.store.PruneDeleted(ctx); err != nil {
		reconciliationErrors = append(reconciliationErrors, fmt.Errorf("prune deleted lifecycle records: %w", err))
	}
	reconciliationErrors = append(reconciliationErrors, h.reconcileLiveness(ctx, cutoff)...)
	reconciliationErrors = append(reconciliationErrors, h.expireAged(ctx, cutoff)...)
	return errors.Join(reconciliationErrors...)
}

func (h *Handler) reconcileLiveness(ctx context.Context, cutoff time.Time) []error {
	var reconciliationErrors []error
	reconciliationErrors = append(reconciliationErrors, h.reconcileOrphanedGitHubRunners(ctx)...)
	knownInstances, err := h.store.KnownInstanceIDs(ctx)
	if err != nil {
		reconciliationErrors = append(reconciliationErrors, fmt.Errorf("list known provider instances: %w", err))
	} else if deleted, sweepErr := h.computeClient.SweepOrphanedRunners(ctx, knownInstances, cutoff); sweepErr != nil {
		reconciliationErrors = append(reconciliationErrors, fmt.Errorf("sweep orphaned provider instances: %w", sweepErr))
	} else {
		if deleted != 0 {
			log.Printf("WARN: reclaimed %d controller-owned provider resources absent from durable state", deleted)
		}
		if released, releaseErr := h.store.ReleaseSweptOrphans(ctx, h.provider, cutoff); releaseErr != nil {
			reconciliationErrors = append(reconciliationErrors, fmt.Errorf("release swept orphan admission: %w", releaseErr))
		} else if released != 0 {
			log.Printf("WARN: released %d swept orphan records from fleet admission", released)
		}
	}
	reconciliationErrors = append(reconciliationErrors, h.reconcileProvisionedRunners(ctx)...)
	return reconciliationErrors
}

func (h *Handler) expireAged(ctx context.Context, cutoff time.Time) []error {
	var expirationErrors []error
	records, err := h.store.ListExpired(ctx, cutoff)
	if err != nil {
		return []error{err}
	}
	for _, record := range records {
		if err := h.expireRunner(ctx, record); err != nil {
			expirationErrors = append(expirationErrors, err)
		}
	}
	if len(records) != 0 {
		h.signal()
	}
	return expirationErrors
}

func (h *Handler) reconcileOrphanedGitHubRunners(ctx context.Context) []error {
	records, err := h.store.ListOrphaned(ctx)
	if err != nil {
		return []error{fmt.Errorf("list orphaned GitHub runners: %w", err)}
	}
	var reconciliationErrors []error
	for _, record := range records {
		if record.GitHubRunnerID == 0 || !record.GitHubRunnerOwned ||
			(!record.NextAttemptAt.IsZero() && record.NextAttemptAt.After(time.Now())) {
			continue
		}
		if err := h.githubClient.RemoveRepoRunner(ctx, record.Owner, record.Repository, record.GitHubRunnerID); err != nil {
			retryAt, rateLimited := gh.RateLimitReset(err)
			if rateLimited {
				retryAt = clampThrottleRetryAt(retryAt)
			} else {
				retryAt = time.Now().Add(retryDelay(record.ReconcileFailures + 1))
			}
			if stateErr := h.store.DeferOrphanCleanup(context.WithoutCancel(ctx), record.Key, err.Error(), retryAt); stateErr != nil {
				err = errors.Join(err, fmt.Errorf("persist orphan GitHub cleanup backoff: %w", stateErr))
			}
			reconciliationErrors = append(reconciliationErrors, err)
			continue
		}
		if err := h.store.ClearJIT(context.WithoutCancel(ctx), record.Key); err != nil {
			reconciliationErrors = append(reconciliationErrors, fmt.Errorf("clear orphaned GitHub runner %d: %w", record.GitHubRunnerID, err))
		}
	}
	return reconciliationErrors
}

func (h *Handler) reconcileProvisionedRunners(ctx context.Context) []error {
	var reconciliationErrors []error
	provisioned, err := h.store.ListProvisioned(ctx)
	if err != nil {
		return []error{fmt.Errorf("list provisioned runners: %w", err)}
	}
	for _, record := range provisioned {
		if record.Provider != h.provider {
			continue
		}
		requeued, err := h.reconcileProvisionedRunner(ctx, record)
		if err != nil {
			reconciliationErrors = append(reconciliationErrors, err)
			continue
		}
		if requeued {
			log.Printf("WARN: runner %s failed reconciliation; queued a replacement", record.Key)
			h.signal()
		}
	}
	return reconciliationErrors
}

func (h *Handler) reconcileProvisionedRunner(ctx context.Context, record state.Record) (bool, error) {
	if !record.NextAttemptAt.IsZero() && record.NextAttemptAt.After(time.Now()) {
		return false, nil
	}
	if !record.RegisteredAt.IsZero() && time.Since(record.ReconciledAt) < h.livenessCheckInterval {
		return false, nil
	}
	_, found, err := h.computeClient.FindRunner(ctx, record.Key)
	if err != nil {
		if errors.Is(err, compute.ErrDuplicateInstances) {
			return h.reconcileDuplicateRunners(ctx, record)
		}
		wrapped := fmt.Errorf("check runner liveness for %s: %w", record.Key, err)
		return false, h.deferReconciliation(ctx, record, wrapped)
	}
	if found {
		return h.reconcileProviderFoundRunner(ctx, record)
	}
	return h.reconcileProviderMissingRunner(ctx, record)
}

func (h *Handler) reconcileProviderFoundRunner(ctx context.Context, record state.Record) (bool, error) {
	if record.MissingChecks != 0 {
		if err := h.store.ClearProviderMissing(ctx, record.Key); err != nil {
			return false, fmt.Errorf("clear provider-missing observations for %s: %w", record.Key, err)
		}
	}
	runnerStatus, err := h.githubClient.RepoRunnerStatus(ctx, record.Owner, record.Repository, record.GitHubRunnerID)
	if err != nil {
		wrapped := fmt.Errorf("check GitHub registration for %s: %w", record.Key, err)
		return false, h.deferReconciliation(ctx, record, wrapped)
	}
	if runnerStatus == gh.RunnerMissing {
		return h.reconcileGitHubMissingRunner(ctx, record)
	}
	if err := h.store.ClearRunnerMissing(ctx, record.Key); err != nil {
		return false, fmt.Errorf("clear successful reconciliation for %s: %w", record.Key, err)
	}
	if runnerStatus == gh.RunnerOffline && record.RegisteredAt.IsZero() && time.Since(record.ProvisionedAt) >= h.registrationTimeout {
		return h.requeueUnregisteredRunner(ctx, record)
	}
	if runnerStatus != gh.RunnerOnline || !record.RegisteredAt.IsZero() {
		return false, nil
	}
	return false, wrapRecordSeenError(record.Key, h.store.RecordRunnerSeen(ctx, record.Key))
}

func (h *Handler) reconcileGitHubMissingRunner(ctx context.Context, record state.Record) (bool, error) {
	confirmed, err := h.store.ObserveGitHubRunnerMissing(ctx, record.Key, time.Now(), h.livenessSettleWindow, h.livenessConfirmations)
	if err != nil || !confirmed {
		return false, wrapMissingObservationError(record.Key, err)
	}
	if record.RegisteredAt.IsZero() && time.Since(record.ProvisionedAt) < h.registrationTimeout {
		// GitHub's runner lookup can be eventually consistent while a JIT runner
		// boots. Preserve both identities until the full startup grace expires.
		return false, nil
	}
	if err := h.store.ScheduleDeletion(ctx, record.Key); err != nil {
		return false, fmt.Errorf("schedule confirmed-missing GitHub runner %s for deletion: %w", record.Key, err)
	}
	return true, nil
}

func (h *Handler) reconcileProviderMissingRunner(ctx context.Context, record state.Record) (bool, error) {
	if record.GitHubMissingChecks != 0 {
		if err := h.store.ClearGitHubMissing(ctx, record.Key); err != nil {
			return false, fmt.Errorf("clear GitHub-missing observations for %s: %w", record.Key, err)
		}
	}
	confirmed, err := h.store.ObserveRunnerMissing(ctx, record.Key, time.Now(), h.livenessSettleWindow, h.livenessConfirmations)
	if err != nil || !confirmed {
		return false, wrapMissingObservationError(record.Key, err)
	}
	if record.GitHubRunnerID != 0 && record.GitHubRunnerOwned {
		if err := h.githubClient.RemoveRepoRunner(ctx, record.Owner, record.Repository, record.GitHubRunnerID); err != nil {
			wrapped := fmt.Errorf("remove missing instance JIT runner %d: %w", record.GitHubRunnerID, err)
			return false, h.deferReconciliation(ctx, record, wrapped)
		}
	}
	if err := h.store.RequeueMissingRunner(ctx, record.Key, h.maxAttempts); err != nil {
		return false, fmt.Errorf("requeue missing runner %s: %w", record.Key, err)
	}
	return true, nil
}

func (h *Handler) reconcileDuplicateRunners(ctx context.Context, record state.Record) (bool, error) {
	if err := h.computeClient.CleanupRunner(ctx, record.Key); err != nil {
		return false, fmt.Errorf("clean up duplicate runners for %s: %w", record.Key, err)
	}
	if record.GitHubRunnerID != 0 && record.GitHubRunnerOwned {
		if err := h.githubClient.RemoveRepoRunner(ctx, record.Owner, record.Repository, record.GitHubRunnerID); err != nil {
			wrapped := fmt.Errorf("remove duplicate-instance JIT runner %d: %w", record.GitHubRunnerID, err)
			return false, h.deferReconciliation(ctx, record, wrapped)
		}
	}
	if err := h.store.RequeueMissingRunner(ctx, record.Key, h.maxAttempts); err != nil {
		return false, fmt.Errorf("requeue duplicate runners for %s: %w", record.Key, err)
	}
	return true, nil
}

func (h *Handler) requeueUnregisteredRunner(ctx context.Context, record state.Record) (bool, error) {
	if record.GitHubRunnerID != 0 && record.GitHubRunnerOwned {
		if err := h.githubClient.RemoveRepoRunner(ctx, record.Owner, record.Repository, record.GitHubRunnerID); err != nil {
			wrapped := fmt.Errorf("remove unregistered JIT runner %d: %w", record.GitHubRunnerID, err)
			return false, h.deferReconciliation(ctx, record, wrapped)
		}
	}
	if err := h.computeClient.DeleteRunner(ctx, record.InstanceID, record.Key); err != nil {
		return false, fmt.Errorf("delete unregistered runner %s: %w", record.InstanceID, err)
	}
	if err := h.store.RequeueMissingRunner(ctx, record.Key, h.maxAttempts); err != nil {
		return false, fmt.Errorf("requeue unregistered runner %s: %w", record.Key, err)
	}
	return true, nil
}

func wrapRecordSeenError(key string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("record runner seen for %s: %w", key, err)
}

func wrapMissingObservationError(key string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("record missing runner %s: %w", key, err)
}

func (h *Handler) expireRunner(ctx context.Context, record state.Record) error {
	if record.Provider != h.provider {
		err := fmt.Errorf("cannot expire runner %s owned by provider %q with configured provider %q", record.Key, record.Provider, h.provider)
		log.Printf("ERROR: %v", err)
		if stateErr := h.markOrphaned(ctx, record, err); stateErr != nil {
			return errors.Join(err, fmt.Errorf("persist provider-mismatch orphan: %w", stateErr))
		}
		return err
	}
	if record.InstanceID == "" {
		return h.deleteUntrackedExpiredRunner(ctx, record)
	}
	if err := h.prepareDeferredDeletion(ctx, record); err != nil {
		return h.deferReconciliation(ctx, record, err)
	}
	log.Printf("WARN: runner %s exceeded maximum lifetime; scheduling owned instance %s for deletion", record.Key, record.InstanceID)
	return h.store.ScheduleDeletion(ctx, record.Key)
}

func (h *Handler) deferReconciliation(ctx context.Context, record state.Record, cause error) error {
	retryAt, rateLimited := gh.RateLimitReset(cause)
	if rateLimited {
		retryAt = clampThrottleRetryAt(retryAt)
	} else {
		retryAt = time.Now().Add(retryDelay(record.ReconcileFailures + 1))
	}
	countFailure := !rateLimited && gh.PersistentClientError(cause)
	if err := h.store.DeferReconciliation(
		context.WithoutCancel(ctx), record.Key, cause.Error(), retryAt, h.maxAttempts, countFailure, rateLimited,
	); err != nil {
		return errors.Join(cause, fmt.Errorf("persist reconciliation backoff for %s: %w", record.Key, err))
	}
	return cause
}

func (h *Handler) markOrphaned(ctx context.Context, record state.Record, cause error) error {
	persistCtx := context.WithoutCancel(ctx)
	var cleanupErr error
	if record.GitHubRunnerID != 0 && record.GitHubRunnerOwned {
		removeCtx, cancel := context.WithTimeout(persistCtx, 30*time.Second)
		removeErr := h.githubClient.RemoveRepoRunner(removeCtx, record.Owner, record.Repository, record.GitHubRunnerID)
		cancel()
		if removeErr != nil {
			cleanupErr = fmt.Errorf("deregister orphaned GitHub runner %d: %w", record.GitHubRunnerID, removeErr)
		} else if clearErr := h.store.ClearJIT(persistCtx, record.Key); clearErr != nil {
			cleanupErr = fmt.Errorf("clear orphaned GitHub runner identity: %w", clearErr)
		}
	}
	orphanCause := errors.Join(cause, cleanupErr)
	if err := h.store.MarkOrphaned(persistCtx, record.Key, orphanCause.Error()); err != nil {
		return errors.Join(orphanCause, fmt.Errorf("persist orphaned runner %s: %w", record.Key, err))
	}
	return cleanupErr
}

func (h *Handler) deleteUntrackedExpiredRunner(ctx context.Context, record state.Record) error {
	attempt, ready, err := h.store.BeginCleanupAttempt(ctx, record.Key, h.maxAttempts)
	if err != nil || !ready {
		return err
	}
	if record.GitHubRunnerID != 0 && record.GitHubRunnerOwned {
		if err := h.githubClient.RemoveRepoRunner(ctx, record.Owner, record.Repository, record.GitHubRunnerID); err != nil {
			return h.untrackedCleanupFailed(ctx, record, attempt, fmt.Errorf("remove expired JIT runner %d: %w", record.GitHubRunnerID, err))
		}
	}
	if err := h.computeClient.CleanupRunner(ctx, record.Key); err != nil {
		return h.untrackedCleanupFailed(ctx, record, attempt, fmt.Errorf("clean up untracked provider resources for %s: %w", record.Key, err))
	}
	return h.store.MarkDeleted(ctx, record.Key)
}

func (h *Handler) untrackedCleanupFailed(ctx context.Context, record state.Record, attempt int, cleanupErr error) error {
	if errors.Is(cleanupErr, compute.ErrOwnershipMismatch) {
		if err := h.markOrphaned(ctx, record, cleanupErr); err != nil {
			return errors.Join(cleanupErr, fmt.Errorf("persist orphaned cleanup state: %w", err))
		}
		return cleanupErr
	}
	if resetAt, rateLimited := gh.RateLimitReset(cleanupErr); rateLimited {
		if err := h.store.DeferRateLimitedCleanup(context.WithoutCancel(ctx), record.Key, cleanupErr.Error(), clampThrottleRetryAt(resetAt), h.maxAttempts, true); err != nil {
			return errors.Join(cleanupErr, fmt.Errorf("persist rate-limited cleanup backoff: %w", err))
		}
		return cleanupErr
	}
	if errors.Is(cleanupErr, context.Canceled) || errors.Is(cleanupErr, context.DeadlineExceeded) || ctx.Err() != nil {
		retryAt := time.Now().Add(time.Minute)
		if err := h.store.DeferRateLimitedCleanup(context.WithoutCancel(ctx), record.Key, cleanupErr.Error(), retryAt, h.maxAttempts, false); err != nil {
			return errors.Join(cleanupErr, fmt.Errorf("persist controller-timeout cleanup backoff: %w", err))
		}
		return cleanupErr
	}
	retryAt := time.Now().Add(retryDelay(attempt))
	if err := h.store.MarkDeleteFailed(ctx, record.Key, cleanupErr.Error(), retryAt, h.maxAttempts); err != nil {
		return errors.Join(cleanupErr, fmt.Errorf("persist cleanup failure: %w", err))
	}
	return cleanupErr
}

func (h *Handler) prepareDeferredDeletion(ctx context.Context, record state.Record) error {
	if !record.DeferDeletion || record.GitHubRunnerID == 0 || !record.GitHubRunnerOwned {
		return nil
	}
	if err := h.githubClient.RemoveRepoRunner(ctx, record.Owner, record.Repository, record.GitHubRunnerID); err != nil {
		return fmt.Errorf("deregister deferred-cancellation runner %d: %w", record.GitHubRunnerID, err)
	}
	if err := h.store.ClearJIT(ctx, record.Key); err != nil {
		return fmt.Errorf("clear deferred-cancellation runner %d: %w", record.GitHubRunnerID, err)
	}
	return nil
}

func (h *Handler) process(parent context.Context, record state.Record, kind state.WorkKind) {
	persistCtx := context.WithoutCancel(parent)
	if parent.Err() != nil {
		if err := h.store.ReleaseClaim(persistCtx, record.Key, kind); err != nil {
			log.Printf("ERROR: release canceled %s claim for %s: %v", kind, record.Key, err)
		}
		return
	}
	if record.Provider != h.provider {
		err := fmt.Errorf("state record belongs to provider %q, controller is configured for %q", record.Provider, h.provider)
		if stateErr := h.markOrphaned(persistCtx, record, err); stateErr != nil {
			log.Printf("ERROR: persist provider mismatch for %s: %v", record.Key, stateErr)
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
		h.provisionFailed(ctx, record, fmt.Errorf("reconcile runner instance: %w", err))
		return
	}
	if found {
		if record.GitHubRunnerID == 0 {
			if deleteErr := h.computeClient.DeleteRunner(ctx, existing.ID, record.Key); deleteErr != nil {
				h.provisionFailed(ctx, record, fmt.Errorf("delete runner with missing JIT identity: %w", deleteErr))
				return
			}
			h.provisionFailed(ctx, record, fmt.Errorf("removed runner with missing JIT identity"))
			return
		}
		if err := h.store.MarkProvisioned(context.WithoutCancel(ctx), record.Key, existing.ID, record.GitHubRunnerID, existing.Name); err != nil {
			log.Printf("ERROR: persist reconciled runner %s: %v", record.Key, err)
			h.provisionFailed(ctx, record, fmt.Errorf("persist reconciled runner: %w", err))
		}
		h.signal()
		return
	}
	if record.GitHubRunnerID != 0 && record.GitHubRunnerOwned {
		if err := h.githubClient.RemoveRepoRunner(ctx, record.Owner, record.Repository, record.GitHubRunnerID); err != nil {
			h.provisionFailed(ctx, record, fmt.Errorf("remove stale JIT runner %d: %w", record.GitHubRunnerID, err))
			return
		}
		if err := h.store.ClearJIT(context.WithoutCancel(ctx), record.Key); err != nil {
			h.provisionFailed(ctx, record, fmt.Errorf("clear stale JIT runner state: %w", err))
			return
		}
	}
	jit, err := h.githubClient.GenerateRepoJITConfig(
		ctx, record.Owner, record.Repository, runnerName, h.runnerGroupID, record.Labels,
	)
	if err != nil {
		h.provisionFailed(ctx, record, fmt.Errorf("generate JIT config: %w", err))
		return
	}
	persistCtx := context.WithoutCancel(ctx)
	if err := h.store.MarkJITCreated(persistCtx, record.Key, jit.RunnerID, runnerName); err != nil {
		cleanupCtx, cancel := context.WithTimeout(persistCtx, 30*time.Second)
		cleanupErr := h.githubClient.RemoveRepoRunner(cleanupCtx, record.Owner, record.Repository, jit.RunnerID)
		cancel()
		if cleanupErr != nil {
			log.Printf("WARN: remove unpersisted JIT runner %d: %v", jit.RunnerID, cleanupErr)
		}
		h.provisionFailed(ctx, record, fmt.Errorf("persist JIT runner identity: %w", err))
		return
	}
	record.GitHubRunnerID = jit.RunnerID
	record.GitHubRunnerOwned = true
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
		h.provisionFailed(ctx, record, fmt.Errorf("create runner instance: %w", err))
		return
	}
	if err := h.store.MarkProvisioned(persistCtx, record.Key, instance.ID, jit.RunnerID, runnerName); err != nil {
		log.Printf("ERROR: persist provisioned runner %s: %v", record.Key, err)
		h.provisionFailed(ctx, record, fmt.Errorf("persist provisioned runner: %w", err))
		return
	}
	log.Printf("Provisioned runner %s (instance %s) for %s/%s job %d", runnerName, instance.ID, record.Owner, record.Repository, record.JobID)
	h.signal()
}

func (h *Handler) provisionFailed(ctx context.Context, record state.Record, err error) {
	persistCtx := context.WithoutCancel(ctx)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		if releaseErr := h.store.ReleaseClaim(persistCtx, record.Key, state.WorkProvision); releaseErr != nil {
			log.Printf("ERROR: release canceled provision claim for %s: %v", record.Key, releaseErr)
		}
		return
	}
	if retryAt, rateLimited := gh.RateLimitReset(err); rateLimited {
		if stateErr := h.store.DeferRateLimitedWork(
			persistCtx, record.Key, state.WorkProvision, err.Error(), clampThrottleRetryAt(retryAt), h.maxAttempts,
		); stateErr != nil {
			log.Printf("ERROR: defer rate-limited provision for %s: %v", record.Key, stateErr)
		}
		return
	}
	if errors.Is(err, compute.ErrCreateOutcomeUnknown) {
		if stateErr := h.markOrphaned(persistCtx, record, err); stateErr != nil {
			log.Printf("ERROR: persist ambiguous provider create for %s: %v", record.Key, stateErr)
		}
		log.Printf("ERROR: provider create outcome for %s is unknown; automatic retry disabled", record.Key)
		return
	}
	if record.Attempts >= h.maxAttempts {
		cleanupCtx, cancel := context.WithTimeout(persistCtx, 30*time.Second)
		cleanupErr := h.computeClient.CleanupRunner(cleanupCtx, record.Key)
		cancel()
		if errors.Is(cleanupErr, compute.ErrOwnershipMismatch) {
			if stateErr := h.markOrphaned(persistCtx, record, cleanupErr); stateErr != nil {
				log.Printf("ERROR: persist orphaned failed provision for %s: %v", record.Key, stateErr)
			}
			log.Printf("ERROR: failed provision for %s requires operator cleanup: %v", record.Key, cleanupErr)
			return
		}
		if cleanupErr != nil {
			log.Printf("WARN: clean up exhausted provision for %s: %v", record.Key, cleanupErr)
		}
		if record.GitHubRunnerID != 0 && record.GitHubRunnerOwned {
			removeCtx, removeCancel := context.WithTimeout(persistCtx, 30*time.Second)
			removeErr := h.githubClient.RemoveRepoRunner(removeCtx, record.Owner, record.Repository, record.GitHubRunnerID)
			removeCancel()
			if removeErr != nil {
				log.Printf("WARN: deregister exhausted JIT runner %d: %v", record.GitHubRunnerID, removeErr)
			} else if clearErr := h.store.ClearJIT(persistCtx, record.Key); clearErr != nil {
				log.Printf("WARN: clear exhausted JIT runner %d: %v", record.GitHubRunnerID, clearErr)
			}
		}
	}
	retryAt := time.Now().Add(retryDelay(record.Attempts))
	if stateErr := h.store.MarkProvisionFailed(persistCtx, record.Key, err.Error(), retryAt, h.maxAttempts); stateErr != nil {
		log.Printf("ERROR: persist provisioning failure for %s: %v", record.Key, stateErr)
	}
	log.Printf("ERROR: provision attempt %d for %s: %v", record.Attempts, record.Key, err)
	h.signal()
}

func (h *Handler) delete(ctx context.Context, record state.Record) {
	persistCtx := context.WithoutCancel(ctx)
	if err := h.computeClient.DeleteRunner(ctx, record.InstanceID, record.Key); err != nil {
		if h.releaseCanceledDelete(ctx, persistCtx, record, err) {
			return
		}
		if errors.Is(err, compute.ErrOwnershipMismatch) {
			if stateErr := h.markOrphaned(persistCtx, record, err); stateErr != nil {
				log.Printf("ERROR: persist orphaned runner %s: %v", record.Key, stateErr)
			}
			log.Printf("ERROR: runner %s requires operator cleanup: %v", record.Key, err)
			return
		}
		retryAt := time.Now().Add(retryDelay(record.DeleteAttempts))
		if stateErr := h.store.MarkDeleteFailed(persistCtx, record.Key, err.Error(), retryAt, h.maxAttempts); stateErr != nil {
			log.Printf("ERROR: persist deletion failure for %s: %v", record.Key, stateErr)
		}
		log.Printf("ERROR: delete attempt %d for %s: %v", record.DeleteAttempts, record.Key, err)
		h.signal()
		return
	}
	if err := h.computeClient.CleanupRunner(ctx, record.Key); err != nil {
		if h.releaseCanceledDelete(ctx, persistCtx, record, err) {
			return
		}
		retryAt := time.Now().Add(retryDelay(record.DeleteAttempts))
		if stateErr := h.store.MarkDeleteFailed(persistCtx, record.Key, err.Error(), retryAt, h.maxAttempts); stateErr != nil {
			log.Printf("ERROR: persist duplicate cleanup failure for %s: %v", record.Key, stateErr)
		}
		h.signal()
		return
	}
	if record.GitHubRunnerID != 0 && record.GitHubRunnerOwned {
		if err := h.githubClient.RemoveRepoRunner(ctx, record.Owner, record.Repository, record.GitHubRunnerID); err != nil {
			if h.releaseCanceledDelete(ctx, persistCtx, record, err) {
				return
			}
			if resetAt, rateLimited := gh.RateLimitReset(err); rateLimited {
				if stateErr := h.store.DeferRateLimitedWork(
					persistCtx, record.Key, state.WorkDelete, err.Error(), clampThrottleRetryAt(resetAt), h.maxAttempts,
				); stateErr != nil {
					log.Printf("ERROR: defer rate-limited deletion for %s: %v", record.Key, stateErr)
				}
				h.signal()
				return
			}
			retryAt := time.Now().Add(retryDelay(record.DeleteAttempts))
			if stateErr := h.store.MarkDeleteFailed(persistCtx, record.Key, err.Error(), retryAt, h.maxAttempts); stateErr != nil {
				log.Printf("ERROR: persist GitHub runner deletion failure for %s: %v", record.Key, stateErr)
			}
			h.signal()
			return
		}
	}
	if err := h.store.MarkDeleted(persistCtx, record.Key); err != nil {
		log.Printf("ERROR: persist deleted runner %s: %v", record.Key, err)
		retryAt := time.Now().Add(retryDelay(record.DeleteAttempts))
		if stateErr := h.store.MarkDeleteFailed(persistCtx, record.Key, err.Error(), retryAt, h.maxAttempts); stateErr != nil {
			log.Printf("ERROR: release failed deletion state for %s: %v", record.Key, stateErr)
			if releaseErr := h.store.ReleaseClaim(persistCtx, record.Key, state.WorkDelete); releaseErr != nil {
				log.Printf("ERROR: release stuck deletion claim for %s: %v", record.Key, releaseErr)
			}
		}
		h.signal()
		return
	}
	log.Printf("Deleted owned runner instance %s for %s", record.InstanceID, record.Key)
}

func (h *Handler) releaseCanceledDelete(ctx, persistCtx context.Context, record state.Record, err error) bool {
	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		return false
	}
	if releaseErr := h.store.ReleaseClaim(persistCtx, record.Key, state.WorkDelete); releaseErr != nil {
		log.Printf("ERROR: release canceled deletion claim for %s: %v", record.Key, releaseErr)
	}
	return true
}

func (h *Handler) validateRepository(repo RepoInfo) (string, bool, error) {
	if !safeNameRegex.MatchString(repo.Owner.Login) || !safeNameRegex.MatchString(repo.Name) {
		return "", false, fmt.Errorf("invalid owner or repository name")
	}
	fullName := strings.ToLower(repo.Owner.Login + "/" + repo.Name)
	if !strings.EqualFold(repo.FullName, fullName) {
		return "", false, fmt.Errorf("repository identity fields disagree")
	}
	if !repo.Private {
		return fullName, false, nil
	}
	if _, ok := h.allowedRepositories[fullName]; !ok {
		return fullName, false, nil
	}
	return fullName, true, nil
}

func (h *Handler) authorizeLabels(requested []string) ([]string, bool, error) {
	for _, raw := range requested {
		if strings.EqualFold(strings.TrimSpace(raw), h.requiredLabel) {
			labels, err := h.validateLabels(requested)
			return labels, true, err
		}
	}
	return nil, false, nil
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

func looksLikeEphemeralRunnerName(name string) bool {
	if !strings.HasPrefix(name, "eph-") || len(name) < len("eph-x-")+12 {
		return false
	}
	suffix := name[len(name)-12:]
	_, err := hex.DecodeString(suffix)
	return err == nil
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

func clampThrottleRetryAt(retryAt time.Time) time.Time {
	now := time.Now()
	floor := now.Add(5 * time.Second)
	if retryAt.After(floor) {
		return retryAt
	}
	jitter := time.Duration(uint64(now.UnixNano()) % uint64(time.Second))
	return floor.Add(jitter)
}

func writeResponse(w http.ResponseWriter, status int, message string) {
	w.WriteHeader(status)
	_, _ = fmt.Fprint(w, message)
}
