package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	gh "github.com/thomasvincent/github-runners-infra/internal/github"
	"github.com/thomasvincent/github-runners-infra/internal/provider"
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
	RunID  int64    `json:"run_id"`
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
	webhookSecret []byte
	githubApp     *gh.App
	provisioner   provider.Provisioner
	doToken       string
	requiredLabel string
	runnerVersion string
	workerPool    chan struct{}    // concurrency limiter (#8)
	rateLimiter   *repoRateLimiter // per-repo rate limiter (#7)
}

// Config holds handler configuration.
type Config struct {
	WebhookSecret    []byte
	GitHubApp        *gh.App
	Provisioner      provider.Provisioner
	DOToken          string
	RequiredLabel    string
	RunnerVersion    string
	MaxConcurrent    int
	MaxPerRepoPerMin int
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

	// Prune empty buckets to prevent unbounded map growth
	if len(valid) == 0 {
		delete(rl.buckets, repo)
	}

	rl.buckets[repo] = append(valid, now)
	return true
}

// NewHandler creates a new webhook handler.
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

	return &Handler{
		webhookSecret: cfg.WebhookSecret,
		githubApp:     cfg.GitHubApp,
		provisioner:   cfg.Provisioner,
		doToken:       cfg.DOToken,
		requiredLabel: label,
		runnerVersion: version,
		workerPool:    make(chan struct{}, maxConcurrent),
		rateLimiter:   newRepoRateLimiter(maxPerRepo),
	}
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
		_, _ = fmt.Fprint(w, "ok")
		return
	}

	var event WorkflowJobEvent
	if err := json.Unmarshal(body, &event); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	// Only act on jobs targeting our runner label; ignore everything else so
	// completed events for other runners don't trigger spurious deletes.
	if !h.hasRequiredLabel(event.WorkflowJob.Labels) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "ok")
		return
	}

	switch event.Action {
	case "queued":
		h.handleQueued(w, event, clientIP)
	case "completed":
		h.handleCompleted(w, event)
	default:
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "ok")
	}
}

func (h *Handler) handleQueued(w http.ResponseWriter, event WorkflowJobEvent, clientIP string) {
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
		_, _ = fmt.Fprint(w, "provisioning")
	default:
		log.Printf("WARN: worker pool full, rejecting job %d", event.WorkflowJob.ID)
		http.Error(w, "system busy", http.StatusServiceUnavailable)
	}
}

// handleCompleted tears down the runner VM as the control-plane identity. The
// runner-node SA is log-only and cannot delete itself, so teardown MUST happen
// here. The cleanup reaper is the backstop for completion events that never
// arrive (missed webhook, control-plane crash).
func (h *Handler) handleCompleted(w http.ResponseWriter, event WorkflowJobEvent) {
	select {
	case h.workerPool <- struct{}{}:
		go func() {
			defer func() { <-h.workerPool }()
			h.teardownRunner(event)
		}()
		w.WriteHeader(http.StatusAccepted)
		_, _ = fmt.Fprint(w, "tearing down")
	default:
		// Backstopped by the reaper; acknowledge the event so GitHub does not
		// retry it and create duplicate teardown attempts while the pool is full.
		log.Printf("WARN: worker pool full, deferring teardown of job %d to reaper", event.WorkflowJob.ID)
		w.WriteHeader(http.StatusAccepted)
		_, _ = fmt.Fprint(w, "teardown deferred to reaper")
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

	runnerName := instanceName(repo, event.WorkflowJob.RunID, event.WorkflowJob.ID)

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

	params := provider.RunnerParams{
		RunnerName:    runnerName,
		RunnerToken:   runnerToken,
		RunnerLabels:  labels,
		RunnerOrg:     owner,
		RunnerRepo:    repoFull,
		DOToken:       h.doToken,
		RunnerVersion: h.runnerVersion,
	}

	inst, err := h.provisioner.Create(ctx, params)
	if err != nil {
		log.Printf("ERROR: create runner for job %d: %v", event.WorkflowJob.ID, err)
		return
	}

	log.Printf("Provisioned runner %s (instance %s) for %s job %d",
		runnerName, inst.ID, repoFull, event.WorkflowJob.ID)
}

// teardownRunner deletes the instance provisioned for a now-completed job. The
// instance name is re-derived from the same job/run ids the queued event used,
// so the two events resolve to the same host without any shared state.
func (h *Handler) teardownRunner(event WorkflowJobEvent) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	repo := event.Repo.Name
	if !safeNameRegex.MatchString(repo) {
		log.Printf("ERROR: invalid repo on completed event: %s", repo)
		return
	}

	name := instanceName(repo, event.WorkflowJob.RunID, event.WorkflowJob.ID)
	if err := h.provisioner.Delete(ctx, name); err != nil {
		// A missing instance is benign: the runner may have already gone via
		// Spot termination or the reaper. Log and move on.
		log.Printf("Teardown of %s (job %d) failed: %v", name, event.WorkflowJob.ID, err)
		return
	}

	log.Printf("Tore down runner %s for %s job %d", name, event.Repo.FullName, event.WorkflowJob.ID)
}

// instanceName derives the runner host name from the job/run ids. It must be
// deterministic so the queued and completed webhooks resolve to the same
// instance; the runner registers under this name (--ephemeral), so it doubles
// as the GitHub runner name.
//
// The result is RFC1035-valid for GCE: lowercase, starts with a letter (the
// "eph-" prefix guarantees this), only [a-z0-9-] otherwise, no trailing hyphen,
// at most 63 chars. The repo segment is sanitized in-place so that both webhook
// paths derive the same name from the same inputs. An unsanitized name is
// rejected by GCE at insert time and the job silently hangs to timeout.
func instanceName(repo string, runID, jobID int64) string {
	const prefix = "eph-"
	suffix := fmt.Sprintf("-%d-%d", runID, jobID)
	repoSegment := sanitizeNameSegment(repo)
	if repoSegment == "" {
		repoSegment = "repo"
	}
	maxRepoLength := 63 - len(prefix) - len(suffix)
	if len(repoSegment) > maxRepoLength {
		repoSegment = strings.TrimRight(repoSegment[:maxRepoLength], "-")
	}
	return prefix + repoSegment + suffix
}

// sanitizeNameSegment lowercases s and replaces every run of characters outside
// [a-z0-9] with a single hyphen, leaving an RFC1035-safe label fragment.
func sanitizeNameSegment(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevHyphen := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevHyphen = false
			continue
		}
		if !prevHyphen {
			b.WriteByte('-')
			prevHyphen = true
		}
	}
	return strings.Trim(b.String(), "-")
}
