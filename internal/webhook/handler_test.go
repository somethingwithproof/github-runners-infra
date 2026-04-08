package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gh "github.com/thomasvincent/github-runners-infra/internal/github"
)

const testSecret = "test-webhook-secret"

func signPayload(payload []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func newTestHandler() *Handler {
	return NewHandler(Config{
		WebhookSecret:    []byte(testSecret),
		MaxConcurrent:    2,
		MaxPerRepoPerMin: 5,
	})
}

func TestMethodNotAllowed(t *testing.T) {
	h := newTestHandler()
	req := httptest.NewRequest(http.MethodGet, "/webhook", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

func TestInvalidSignature(t *testing.T) {
	h := newTestHandler()
	body := `{}`
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", "sha256=bad")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestNonWorkflowJobEvent(t *testing.T) {
	h := newTestHandler()
	body := `{}`
	sig := signPayload([]byte(body), testSecret)

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", sig)
	req.Header.Set("X-GitHub-Event", "push")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for non-workflow_job event, got %d", w.Code)
	}
}

func TestNonQueuedAction(t *testing.T) {
	h := newTestHandler()
	event := WorkflowJobEvent{
		Action: "completed",
		WorkflowJob: WorkflowJob{
			ID:     1,
			Labels: []string{"self-hosted"},
		},
	}
	body, _ := json.Marshal(event)
	sig := signPayload(body, testSecret)

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(body)))
	req.Header.Set("X-Hub-Signature-256", sig)
	req.Header.Set("X-GitHub-Event", "workflow_job")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for non-queued action, got %d", w.Code)
	}
}

func TestMissingRequiredLabel(t *testing.T) {
	h := newTestHandler()
	event := WorkflowJobEvent{
		Action: "queued",
		WorkflowJob: WorkflowJob{
			ID:     1,
			Labels: []string{"ubuntu-latest"},
		},
	}
	body, _ := json.Marshal(event)
	sig := signPayload(body, testSecret)

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(body)))
	req.Header.Set("X-Hub-Signature-256", sig)
	req.Header.Set("X-GitHub-Event", "workflow_job")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for missing label, got %d", w.Code)
	}
}

func TestHasRequiredLabel(t *testing.T) {
	h := newTestHandler()
	tests := []struct {
		labels []string
		want   bool
	}{
		{[]string{"self-hosted"}, true},
		{[]string{"Self-Hosted"}, true},
		{[]string{"ubuntu-latest", "self-hosted"}, true},
		{[]string{"ubuntu-latest"}, false},
		{[]string{}, false},
		{nil, false},
	}

	for _, tt := range tests {
		got := h.hasRequiredLabel(tt.labels)
		if got != tt.want {
			t.Errorf("hasRequiredLabel(%v) = %v, want %v", tt.labels, got, tt.want)
		}
	}
}

func TestRateLimiter(t *testing.T) {
	rl := newRepoRateLimiter(3)
	repo := "org/repo"

	// First 3 should be allowed
	for i := 0; i < 3; i++ {
		if !rl.allow(repo) {
			t.Errorf("request %d should be allowed", i+1)
		}
	}

	// 4th should be denied
	if rl.allow(repo) {
		t.Error("4th request should be denied")
	}

	// Different repo should still be allowed
	if !rl.allow("org/other-repo") {
		t.Error("different repo should be allowed")
	}
}

func TestRateLimiterExpiry(t *testing.T) {
	rl := &repoRateLimiter{
		buckets: make(map[string][]time.Time),
		limit:   1,
		window:  50 * time.Millisecond,
	}

	repo := "org/repo"
	if !rl.allow(repo) {
		t.Error("first request should be allowed")
	}
	if rl.allow(repo) {
		t.Error("second request should be denied")
	}

	time.Sleep(60 * time.Millisecond)

	if !rl.allow(repo) {
		t.Error("request after window should be allowed")
	}
}

func TestRateLimiterMapCleanup(t *testing.T) {
	rl := &repoRateLimiter{
		buckets: make(map[string][]time.Time),
		limit:   1,
		window:  50 * time.Millisecond,
	}

	// Add entries for multiple repos
	rl.allow("org/repo-a")
	rl.allow("org/repo-b")
	rl.allow("org/repo-c")

	if len(rl.buckets) != 3 {
		t.Fatalf("expected 3 bucket entries, got %d", len(rl.buckets))
	}

	// Wait for all entries to expire
	time.Sleep(60 * time.Millisecond)

	// Accessing one repo should clean up its stale key and re-create it
	rl.allow("org/repo-a")

	// repo-a should still exist (re-created with fresh entry)
	if _, ok := rl.buckets["org/repo-a"]; !ok {
		t.Error("repo-a should still have bucket after access")
	}

	// repo-b and repo-c haven't been accessed, so they still have stale keys
	// but on next access they'll be cleaned up
	rl.allow("org/repo-b")
	time.Sleep(60 * time.Millisecond)
	rl.allow("org/repo-b")

	// After expiry+re-access, the map entry is fresh
	if entries := rl.buckets["org/repo-b"]; len(entries) != 1 {
		t.Errorf("expected 1 entry for repo-b, got %d", len(entries))
	}
}

func TestSignatureVerification(t *testing.T) {
	payload := []byte(`{"action":"queued"}`)
	secret := []byte("my-secret")

	validSig := signPayload(payload, "my-secret")
	invalidSig := signPayload(payload, "wrong-secret")

	tests := []struct {
		name      string
		signature string
		want      bool
	}{
		{"valid signature", validSig, true},
		{"invalid signature", invalidSig, false},
		{"missing prefix", "bad", false},
		{"empty signature", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := gh.VerifyWebhookSignature(payload, tt.signature, secret, "127.0.0.1")
			if got != tt.want {
				t.Errorf("VerifyWebhookSignature() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestInputValidationRegexes(t *testing.T) {
	tests := []struct {
		name    string
		regex   func(string) bool
		input   string
		allowed bool
	}{
		{"safe name: alphanumeric", safeNameRegex.MatchString, "my-repo_1", true},
		{"safe name: with dots", safeNameRegex.MatchString, "my.repo", false},
		{"safe name: injection attempt", safeNameRegex.MatchString, "repo;rm -rf /", false},
		{"safe name: empty", safeNameRegex.MatchString, "", false},
		{"repo: valid", repoRegex.MatchString, "owner/repo", true},
		{"repo: with dots", repoRegex.MatchString, "my.org/my.repo", true},
		{"repo: no slash", repoRegex.MatchString, "justrepo", false},
		{"repo: injection", repoRegex.MatchString, "owner/repo;echo pwned", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.regex(tt.input)
			if got != tt.allowed {
				t.Errorf("regex(%q) = %v, want %v", tt.input, got, tt.allowed)
			}
		})
	}
}

func TestWorkerPoolFull(t *testing.T) {
	h := NewHandler(Config{
		WebhookSecret: []byte(testSecret),
		MaxConcurrent: 1,
	})

	// Fill the worker pool
	h.workerPool <- struct{}{}

	event := WorkflowJobEvent{
		Action: "queued",
		WorkflowJob: WorkflowJob{
			ID:     1,
			Labels: []string{"self-hosted"},
		},
		Repo: RepoInfo{FullName: "org/repo"},
	}
	body, _ := json.Marshal(event)
	sig := signPayload(body, testSecret)

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(body)))
	req.Header.Set("X-Hub-Signature-256", sig)
	req.Header.Set("X-GitHub-Event", "workflow_job")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when worker pool full, got %d", w.Code)
	}

	// Drain the pool
	<-h.workerPool
}

func TestNewHandlerDefaults(t *testing.T) {
	h := NewHandler(Config{})

	if h.requiredLabel != "self-hosted" {
		t.Errorf("expected default label 'self-hosted', got %q", h.requiredLabel)
	}
	if h.runnerVersion != "2.331.0" {
		t.Errorf("expected default version '2.331.0', got %q", h.runnerVersion)
	}
	if cap(h.workerPool) != 10 {
		t.Errorf("expected default pool size 10, got %d", cap(h.workerPool))
	}
	if h.rateLimiter.limit != 20 {
		t.Errorf("expected default rate limit 20, got %d", h.rateLimiter.limit)
	}
}
