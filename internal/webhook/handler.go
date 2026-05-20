package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/thomasvincent/github-runners-infra/internal/digitalocean"
	gh "github.com/thomasvincent/github-runners-infra/internal/github"
	"github.com/thomasvincent/github-runners-infra/internal/metrics"
)

const maxBodySize = 1 * 1024 * 1024 // 1 MB

var (
	safeNameRegex = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)
	repoRegex     = regexp.MustCompile(`^[a-zA-Z0-9._-]+/[a-zA-Z0-9._-]+$`)
)

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

type Handler struct {
	webhookSecret     []byte
	githubApp         *gh.App
	doClient          *digitalocean.Client
	doToken           string
	requiredLabel     string
	runnerVersion     string
	callbackURL       string
	destroySecret     []byte
	maxActiveDroplets int64
	activeCount       atomic.Int64 // tracks in-flight + created droplets for cost cap
	workerPool        chan struct{}
	rateLimiter       *repoRateLimiter
	wg                sync.WaitGroup
	dedup             *jobDeduplicator
	destroyTokens     sync.Map // token → dropletID
	stop              func()   // cancels background goroutines
}

func (h *Handler) Wait() {
	h.wg.Wait()
}

type Config struct {
	WebhookSecret     []byte
	GitHubApp         *gh.App
	DOClient          *digitalocean.Client
	DOToken           string
	RequiredLabel     string
	RunnerVersion     string
	CallbackURL       string
	DestroySecret     string
	MaxConcurrent     int
	MaxPerRepoPerMin  int
	MaxActiveDroplets int
}

// repoRateLimiter implements a simple per-repo token bucket.
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

	if len(valid) == 0 {
		delete(rl.buckets, repo)
	}

	rl.buckets[repo] = append(valid, now)
	return true
}

// sweep removes all expired entries from all repos.
func (rl *repoRateLimiter) sweep() {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	cutoff := time.Now().Add(-rl.window)
	for repo, times := range rl.buckets {
		valid := times[:0]
		for _, t := range times {
			if t.After(cutoff) {
				valid = append(valid, t)
			}
		}
		if len(valid) == 0 {
			delete(rl.buckets, repo)
		} else {
			rl.buckets[repo] = valid
		}
	}
}

// jobDeduplicator prevents duplicate provisioning for the same workflow job.
type jobDeduplicator struct {
	mu   sync.Mutex
	seen map[int64]time.Time
	ttl  time.Duration
}

func newJobDeduplicator(ctx context.Context, ttl time.Duration) *jobDeduplicator {
	d := &jobDeduplicator{
		seen: make(map[int64]time.Time),
		ttl:  ttl,
	}
	go d.cleanupLoop(ctx)
	return d
}

func (d *jobDeduplicator) isDuplicate(jobID int64) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	if _, ok := d.seen[jobID]; ok {
		return true
	}
	d.seen[jobID] = time.Now()
	return false
}

func (d *jobDeduplicator) cleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.mu.Lock()
			cutoff := time.Now().Add(-d.ttl)
			for id, t := range d.seen {
				if t.Before(cutoff) {
					delete(d.seen, id)
				}
			}
			d.mu.Unlock()
		}
	}
}

func NewHandler(cfg Config) *Handler {
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
	var maxActive int64
	if cfg.MaxActiveDroplets > 0 {
		maxActive = int64(cfg.MaxActiveDroplets)
	}

	var destroySecret []byte
	if cfg.DestroySecret != "" {
		destroySecret = []byte(cfg.DestroySecret)
	}

	ctx, cancel := context.WithCancel(context.Background())

	h := &Handler{
		webhookSecret:     cfg.WebhookSecret,
		githubApp:         cfg.GitHubApp,
		doClient:          cfg.DOClient,
		doToken:           cfg.DOToken,
		requiredLabel:     label,
		runnerVersion:     version,
		callbackURL:       cfg.CallbackURL,
		destroySecret:     destroySecret,
		maxActiveDroplets: maxActive,
		workerPool:        make(chan struct{}, maxConcurrent),
		rateLimiter:       newRepoRateLimiter(maxPerRepo),
		dedup:             newJobDeduplicator(ctx, 10*time.Minute),
		stop:              cancel,
	}

	// Periodic sweep of stale rate limiter keys (repos that stopped sending webhooks)
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				h.rateLimiter.sweep()
			}
		}
	}()

	return h
}

// Stop cancels background goroutines. Call during shutdown or in tests.
func (h *Handler) Stop() {
	if h.stop != nil {
		h.stop()
	}
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

	clientIP := r.Header.Get("X-Forwarded-For")
	if clientIP == "" {
		clientIP = r.RemoteAddr
	}

	sig := r.Header.Get("X-Hub-Signature-256")
	if !gh.VerifyWebhookSignature(body, sig, h.webhookSecret, clientIP) {
		metrics.WebhookRequests.WithLabelValues("unauthorized").Inc()
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	eventType := r.Header.Get("X-GitHub-Event")
	if eventType != "workflow_job" {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "ok")
		return
	}

	var event WorkflowJobEvent
	if err := json.Unmarshal(body, &event); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	if event.Action != "queued" {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "ok")
		return
	}

	if !h.hasRequiredLabel(event.WorkflowJob.Labels) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "ok")
		return
	}

	// Duplicate webhook detection
	if h.dedup.isDuplicate(event.WorkflowJob.ID) {
		metrics.DuplicateWebhooks.Inc()
		slog.Warn("duplicate webhook suppressed", "job_id", event.WorkflowJob.ID, "repo", event.Repo.FullName)
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "duplicate")
		return
	}

	repoKey := event.Repo.FullName
	if !h.rateLimiter.allow(repoKey) {
		metrics.RateLimitHits.Inc()
		metrics.WebhookRequests.WithLabelValues("rate_limited").Inc()
		slog.Warn("rate limit exceeded", "repo", repoKey, "client_ip", clientIP)
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}

	select {
	case h.workerPool <- struct{}{}:
		metrics.WorkerPoolSize.Inc()
		h.wg.Add(1)
		go func() {
			defer h.wg.Done()
			defer func() {
				<-h.workerPool
				metrics.WorkerPoolSize.Dec()
			}()
			h.provisionRunner(event)
		}()
		metrics.WebhookRequests.WithLabelValues("accepted").Inc()
		w.WriteHeader(http.StatusAccepted)
		_, _ = fmt.Fprint(w, "provisioning")
	default:
		metrics.WebhookRequests.WithLabelValues("pool_full").Inc()
		slog.Warn("worker pool full", "job_id", event.WorkflowJob.ID)
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
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	owner := event.Repo.Owner.Login
	repo := event.Repo.Name
	repoFull := fmt.Sprintf("%s/%s", owner, repo)
	jobID := event.WorkflowJob.ID

	if !safeNameRegex.MatchString(owner) || !safeNameRegex.MatchString(repo) {
		slog.Error("invalid owner/repo", "owner", owner, "repo", repo)
		metrics.ProvisioningErrors.Inc()
		return
	}

	if !repoRegex.MatchString(repoFull) {
		slog.Error("invalid repo format", "repo", repoFull)
		metrics.ProvisioningErrors.Inc()
		return
	}

	// Cost guardrail: atomic counter prevents TOCTOU race
	if h.maxActiveDroplets > 0 {
		if h.activeCount.Load() >= h.maxActiveDroplets {
			slog.Warn("active droplet cap reached", "active", h.activeCount.Load(), "cap", h.maxActiveDroplets, "job_id", jobID)
			metrics.ProvisioningErrors.Inc()
			return
		}
		h.activeCount.Add(1)
		defer h.activeCount.Add(-1)
	}

	runnerToken, err := h.githubApp.GenerateRepoRunnerToken(owner, repo)
	if err != nil {
		slog.Error("runner token generation failed", "repo", repoFull, "error", err)
		metrics.ProvisioningErrors.Inc()
		return
	}

	runnerName := fmt.Sprintf("eph-%s-%d-%d", repo, jobID, time.Now().Unix())
	if len(runnerName) > 63 {
		runnerName = strings.TrimRight(runnerName[:63], "-.")
	}

	var safeLabels []string
	for _, l := range event.WorkflowJob.Labels {
		cleaned := strings.TrimSpace(l)
		if safeNameRegex.MatchString(cleaned) {
			safeLabels = append(safeLabels, cleaned)
		}
	}
	labels := strings.Join(safeLabels, ",")

	params := digitalocean.RunnerParams{
		RunnerName:    runnerName,
		RunnerToken:   runnerToken,
		RunnerLabels:  labels,
		RunnerOrg:     owner,
		RunnerRepo:    repoFull,
		RunnerVersion: h.runnerVersion,
	}

	// Use scoped self-destruct if configured, otherwise fall back to DO token
	if h.callbackURL != "" && len(h.destroySecret) > 0 {
		destroyToken := generateDestroyToken()
		params.DestroyToken = destroyToken
		params.CallbackURL = h.callbackURL

		droplet, err := h.doClient.CreateRunner(ctx, params)
		if err != nil {
			slog.Error("create droplet failed", "job_id", jobID, "error", err)
			metrics.ProvisioningErrors.Inc()
			return
		}

		// Register the destroy token → droplet ID mapping
		h.destroyTokens.Store(destroyToken, droplet.ID)

		metrics.DropletsCreated.Inc()
		metrics.ProvisioningDuration.Observe(time.Since(start).Seconds())
		slog.Info("provisioned runner", "runner", runnerName, "droplet_id", droplet.ID, "repo", repoFull, "job_id", jobID)
	} else {
		params.DOToken = h.doToken

		droplet, err := h.doClient.CreateRunner(ctx, params)
		if err != nil {
			slog.Error("create droplet failed", "job_id", jobID, "error", err)
			metrics.ProvisioningErrors.Inc()
			return
		}

		metrics.DropletsCreated.Inc()
		metrics.ProvisioningDuration.Observe(time.Since(start).Seconds())
		slog.Info("provisioned runner", "runner", runnerName, "droplet_id", droplet.ID, "repo", repoFull, "job_id", jobID)
	}
}

func generateDestroyToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// HandleDestroy processes scoped self-destruct callbacks from runner droplets.
func (h *Handler) HandleDestroy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	var req struct {
		DestroyToken string `json:"destroy_token"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.DestroyToken == "" {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	// Verify the token is a hex string to prevent injection
	if _, err := hex.DecodeString(req.DestroyToken); err != nil {
		http.Error(w, "invalid token", http.StatusBadRequest)
		return
	}

	val, ok := h.destroyTokens.LoadAndDelete(req.DestroyToken)
	if !ok {
		http.Error(w, "unknown token", http.StatusNotFound)
		return
	}

	dropletID, ok2 := val.(int)
	if !ok2 {
		slog.Error("destroy token mapped to unexpected type", "token", req.DestroyToken)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := h.doClient.DeleteDroplet(ctx, dropletID); err != nil {
		slog.Error("callback delete failed", "droplet_id", dropletID, "error", err)
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}

	metrics.DropletsDestroyed.WithLabelValues("callback").Inc()
	slog.Info("callback: destroyed droplet", "droplet_id", dropletID)
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, "deleted")
}

// HandleDestroyHMAC processes self-destruct callbacks authenticated with HMAC.
// Used when the droplet discovers its own ID at runtime and signs it.
func (h *Handler) HandleDestroyHMAC(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if len(h.destroySecret) == 0 {
		http.Error(w, "not configured", http.StatusNotFound)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	var req struct {
		DropletID int    `json:"droplet_id"`
		Signature string `json:"signature"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.DropletID == 0 || req.Signature == "" {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	expected := computeHMAC(fmt.Sprintf("%d", req.DropletID), h.destroySecret)
	if !hmac.Equal([]byte(req.Signature), []byte(expected)) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := h.doClient.DeleteDroplet(ctx, req.DropletID); err != nil {
		slog.Error("HMAC destroy failed", "droplet_id", req.DropletID, "error", err)
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}

	metrics.DropletsDestroyed.WithLabelValues("callback").Inc()
	slog.Info("HMAC destroy: deleted droplet", "droplet_id", req.DropletID)
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, "deleted")
}

func computeHMAC(message string, secret []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(message))
	return hex.EncodeToString(mac.Sum(nil))
}
