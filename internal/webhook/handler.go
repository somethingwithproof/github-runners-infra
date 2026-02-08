package webhook

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/thomasvincent/github-runners-infra/internal/digitalocean"
	gh "github.com/thomasvincent/github-runners-infra/internal/github"
)

const maxBodySize = 1 * 1024 * 1024 // 1 MB (#3)

// Input validation regexes (#9)
var (
	safeNameRegex = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
	repoRegex     = regexp.MustCompile(`^[a-zA-Z0-9._-]+/[a-zA-Z0-9._-]+$`)
)

// WorkflowJobEvent represents the GitHub workflow_job webhook payload.
type WorkflowJobEvent struct {
	Action      string      `json:"action"`
	WorkflowJob WorkflowJob `json:"workflow_job"`
	Org         *OrgInfo    `json:"organization,omitempty"`
	Repo        RepoInfo    `json:"repository"`
}

type WorkflowJob struct {
	ID     int64    `json:"id"`
	Name   string   `json:"name"`
	Labels []string `json:"labels"`
}

type OrgInfo struct {
	Login string `json:"login"`
}

type RepoInfo struct {
	FullName string `json:"full_name"`
	Name     string `json:"name"`
	Owner    struct {
		Login string `json:"login"`
	} `json:"owner"`
}

// Handler processes incoming GitHub webhooks.
type Handler struct {
	webhookSecret         []byte
	githubApp             *gh.App
	doClient              *digitalocean.Client
	doToken               string
	requiredLabel         string
	runnerVersion         string
	callbackSecret        string
	callbackSecretSSMPath string
	callbackURL           string
	chefInstallerSHA256   string
	workerPool            chan struct{}    // concurrency limiter (#8)
	rateLimiter           *repoRateLimiter // per-repo rate limiter (#7)
	ssmClient             *ssm.Client
}

// Config holds handler configuration.
type Config struct {
	WebhookSecret         []byte
	GitHubApp             *gh.App
	DOClient              *digitalocean.Client
	DOToken               string
	RequiredLabel         string
	RunnerVersion         string
	CallbackSecret        string
	CallbackSecretSSMPath string
	CallbackURL           string
	MaxConcurrent         int
	MaxPerRepoPerMin      int
	ChefInstallerSHA256   string
}

// repoRateLimiter implements a simple per-repo token bucket. (#7)
type repoRateLimiter struct {
	mu      sync.Mutex
	buckets map[string][]time.Time
	limit   int
	window  time.Duration
}

func newRepoRateLimiter(limit int) *repoRateLimiter {
	return &repoRateLimiter{
		buckets: make(map[string][]time.Time),
		limit:   limit,
		window:  time.Minute,
	}
}

func (rl *repoRateLimiter) allow(repo string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-rl.window)

	// Remove expired entries
	valid := rl.buckets[repo][:0]
	for _, t := range rl.buckets[repo] {
		if t.After(cutoff) {
			valid = append(valid, t)
		}
	}

	if len(valid) >= rl.limit {
		rl.buckets[repo] = valid
		return false
	}

	rl.buckets[repo] = append(valid, now)
	return true
}

// NewHandler creates a new webhook handler, returning an error instead of
// calling log.Fatalf so callers can handle failures gracefully.
func NewHandler(cfg Config) (*Handler, error) {
	label := cfg.RequiredLabel
	if label == "" {
		label = "self-hosted"
	}
	version := cfg.RunnerVersion
	if version == "" {
		version = "2.331.0"
	}
	maxConcurrent := cfg.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = 10
	}
	maxPerRepo := cfg.MaxPerRepoPerMin
	if maxPerRepo <= 0 {
		maxPerRepo = 20
	}

	// Initialize AWS SSM client
	awsCfg, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}
	ssmClient := ssm.NewFromConfig(awsCfg)

	// Set SSM path for callback secret — this is the single source of truth.
	// The handler fetches the secret from SSM at startup so that runners and
	// the destroy-callback endpoint use the same value.
	ssmPath := cfg.CallbackSecretSSMPath
	if ssmPath == "" {
		ssmPath = "/github-runners/callback-secret"
	}

	// Resolve callback secret from SSM (single source of truth)
	callbackSecret, err := resolveCallbackSecret(context.Background(), ssmClient, ssmPath, cfg.CallbackSecret)
	if err != nil {
		return nil, fmt.Errorf("resolving callback secret: %w", err)
	}

	return &Handler{
		webhookSecret:         cfg.WebhookSecret,
		githubApp:             cfg.GitHubApp,
		doClient:              cfg.DOClient,
		doToken:               cfg.DOToken,
		requiredLabel:         label,
		runnerVersion:         version,
		callbackSecret:        callbackSecret,
		callbackSecretSSMPath: ssmPath,
		callbackURL:           cfg.CallbackURL,
		chefInstallerSHA256:   cfg.ChefInstallerSHA256,
		workerPool:            make(chan struct{}, maxConcurrent),
		rateLimiter:           newRepoRateLimiter(maxPerRepo),
		ssmClient:             ssmClient,
	}, nil
}

// resolveCallbackSecret loads the callback secret from SSM. If SSM is
// unreachable or the parameter doesn't exist yet, it falls back to the
// supplied envSecret. This ensures both the destroy-callback handler and
// the runner cloud-init derive the secret from a single authoritative
// location (SSM), eliminating the previous two-source divergence.
func resolveCallbackSecret(ctx context.Context, client *ssm.Client, ssmPath, envSecret string) (string, error) {
	out, err := client.GetParameter(ctx, &ssm.GetParameterInput{
		Name:           &ssmPath,
		WithDecryption: boolPtr(true),
	})
	if err == nil && out.Parameter != nil && out.Parameter.Value != nil {
		return *out.Parameter.Value, nil
	}
	// Fallback: use the env-supplied secret (e.g. first deploy before SSM is populated)
	if envSecret != "" {
		log.Printf("WARN: callback secret not found in SSM (%s), using env fallback", ssmPath)
		return envSecret, nil
	}
	return "", fmt.Errorf("callback secret not available from SSM (%s) and no env fallback provided", ssmPath)
}

// ServeHTTP handles webhook requests.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Limit request body size (#3)
	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	clientIP := r.Header.Get("X-Forwarded-For")
	if clientIP == "" {
		clientIP = r.RemoteAddr
	}

	sig := r.Header.Get("X-Hub-Signature-256")
	if !gh.VerifyWebhookSignature(body, sig, h.webhookSecret, clientIP) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	eventType := r.Header.Get("X-GitHub-Event")
	if eventType != "workflow_job" {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
		return
	}

	var event WorkflowJobEvent
	if err := json.Unmarshal(body, &event); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	if event.Action != "queued" {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
		return
	}

	if !h.hasRequiredLabel(event.WorkflowJob.Labels) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
		return
	}

	// Rate limit per repo (#7)
	repoKey := event.Repo.FullName
	if !h.rateLimiter.allow(repoKey) {
		log.Printf("SECURITY: rate limit exceeded for %s from %s", repoKey, clientIP)
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}

	// Worker pool for bounded concurrency (#8)
	select {
	case h.workerPool <- struct{}{}:
		go func() {
			defer func() { <-h.workerPool }()
			h.provisionRunner(event)
		}()
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, "provisioning")
	default:
		log.Printf("WARN: worker pool full, rejecting job %d", event.WorkflowJob.ID)
		http.Error(w, "system busy", http.StatusServiceUnavailable)
	}
}

func (h *Handler) hasRequiredLabel(labels []string) bool {
	for _, l := range labels {
		if strings.EqualFold(l, h.requiredLabel) {
			return true
		}
	}
	return false
}

func (h *Handler) provisionRunner(event WorkflowJobEvent) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	owner := event.Repo.Owner.Login
	repo := event.Repo.Name

	// Validate inputs (#9)
	if !safeNameRegex.MatchString(owner) || !safeNameRegex.MatchString(repo) {
		log.Printf("ERROR: invalid owner/repo: %s/%s", owner, repo)
		return
	}

	runnerToken, err := h.githubApp.GenerateRepoRunnerToken(owner, repo)
	if err != nil {
		log.Printf("ERROR: runner token for %s/%s: %v", owner, repo, err)
		return
	}

	runnerName := fmt.Sprintf("eph-%s-%d-%d", repo, event.WorkflowJob.ID, time.Now().Unix())
	if len(runnerName) > 63 {
		runnerName = runnerName[:63]
	}

	// Store runner token in SSM Parameter Store with an expiration policy.
	// Tokens are ephemeral — schedule cleanup even if the runner never
	// calls back (e.g. provisioning failure). We tag with an expiry
	// timestamp and also defer a goroutine-based cleanup as a safety net.
	tokenParamName := fmt.Sprintf("/github-runners/tokens/%s", runnerName)
	expiresAt := time.Now().Add(30 * time.Minute).UTC().Format(time.RFC3339)
	_, err = h.ssmClient.PutParameter(ctx, &ssm.PutParameterInput{
		Name:      &tokenParamName,
		Value:     &runnerToken,
		Type:      types.ParameterTypeSecureString,
		Overwrite: boolPtr(true),
		Tags: []types.Tag{
			{Key: strPtr("expires-at"), Value: &expiresAt},
			{Key: strPtr("runner-name"), Value: &runnerName},
		},
	})
	if err != nil {
		log.Printf("ERROR: failed to store runner token in SSM: %v", err)
		return
	}

	// Schedule automatic cleanup of the token parameter after 30 minutes.
	// This prevents token accumulation if the runner never self-destructs.
	go func(paramName string) {
		time.Sleep(30 * time.Minute)
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		_, err := h.ssmClient.DeleteParameter(cleanupCtx, &ssm.DeleteParameterInput{
			Name: &paramName,
		})
		if err != nil {
			log.Printf("WARN: failed to cleanup expired token param %s: %v", paramName, err)
		} else {
			log.Printf("Cleaned up expired token param %s", paramName)
		}
	}(tokenParamName)

	// Validate and sanitize labels (#9)
	var safeLabels []string
	for _, l := range event.WorkflowJob.Labels {
		cleaned := strings.TrimSpace(l)
		if safeNameRegex.MatchString(cleaned) {
			safeLabels = append(safeLabels, cleaned)
		}
	}
	labels := strings.Join(safeLabels, ",")

	repoFull := fmt.Sprintf("%s/%s", owner, repo)
	if !repoRegex.MatchString(repoFull) {
		log.Printf("ERROR: invalid repo format: %s", repoFull)
		return
	}

	params := digitalocean.RunnerParams{
		RunnerName:             runnerName,
		RunnerTokenSSMParam:    tokenParamName,
		RunnerLabels:           labels,
		RunnerOrg:              owner,
		RunnerRepo:             repoFull,
		DOToken:                h.doToken,
		RunnerVersion:          h.runnerVersion,
		CallbackSecretSSMParam: h.callbackSecretSSMPath,
		CallbackURL:            h.callbackURL,
		ChefInstallerSHA256:    h.chefInstallerSHA256,
	}

	droplet, err := h.doClient.CreateRunner(ctx, params)
	if err != nil {
		log.Printf("ERROR: create droplet for job %d: %v", event.WorkflowJob.ID, err)
		return
	}

	log.Printf("Provisioned runner %s (droplet %d) for %s job %d",
		runnerName, droplet.ID, repoFull, event.WorkflowJob.ID)
}

// HandleDestroy processes self-destruct callbacks from runner droplets. (#1)
func (h *Handler) HandleDestroy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	secret := r.Header.Get("X-Callback-Secret")
	if secret == "" || subtle.ConstantTimeCompare([]byte(secret), []byte(h.callbackSecret)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	var req struct {
		DropletID int `json:"droplet_id"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.DropletID == 0 {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := h.doClient.DeleteDroplet(ctx, req.DropletID); err != nil {
		log.Printf("ERROR: callback delete droplet %d: %v", req.DropletID, err)
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}

	log.Printf("Callback: deleted droplet %d", req.DropletID)
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "deleted")
}

// boolPtr returns a pointer to a bool value
func boolPtr(b bool) *bool {
	return &b
}

// strPtr returns a pointer to a string value
func strPtr(s string) *string {
	return &s
}
