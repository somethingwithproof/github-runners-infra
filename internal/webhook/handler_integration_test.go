package webhook

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDeduplication(t *testing.T) {
	h := newTestHandler()
	event := WorkflowJobEvent{
		Action: "queued",
		WorkflowJob: WorkflowJob{
			ID:     42,
			Labels: []string{"self-hosted"},
		},
		Repo: RepoInfo{FullName: "org/repo"},
	}
	body, _ := json.Marshal(event)
	sig := signPayload(body, testSecret)

	// First request should be accepted
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(body)))
	req.Header.Set("X-Hub-Signature-256", sig)
	req.Header.Set("X-GitHub-Event", "workflow_job")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	// The handler will try to provision (and fail since no GitHub app is configured),
	// but the response should still be 202 (accepted into worker pool)
	if w.Code != http.StatusAccepted {
		t.Fatalf("first request: expected 202, got %d", w.Code)
	}

	// Second request with same job ID should be deduped
	req2 := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(body)))
	req2.Header.Set("X-Hub-Signature-256", sig)
	req2.Header.Set("X-GitHub-Event", "workflow_job")
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)

	if w2.Code != http.StatusOK {
		t.Errorf("duplicate request: expected 200, got %d", w2.Code)
	}
	if !strings.Contains(w2.Body.String(), "duplicate") {
		t.Errorf("expected 'duplicate' in body, got %q", w2.Body.String())
	}
}

func TestDeduplicatorExpiry(t *testing.T) {
	d := &jobDeduplicator{
		seen: make(map[int64]time.Time),
		ttl:  50 * time.Millisecond,
	}

	if d.isDuplicate(1) {
		t.Error("first call should not be duplicate")
	}
	if !d.isDuplicate(1) {
		t.Error("second call should be duplicate")
	}

	time.Sleep(60 * time.Millisecond)

	// Manually trigger cleanup
	d.mu.Lock()
	cutoff := time.Now().Add(-d.ttl)
	for id, ts := range d.seen {
		if ts.Before(cutoff) {
			delete(d.seen, id)
		}
	}
	d.mu.Unlock()

	if d.isDuplicate(1) {
		t.Error("should not be duplicate after expiry")
	}
}

func TestDestroyEndpoint_InvalidMethod(t *testing.T) {
	h := newTestHandler()
	req := httptest.NewRequest(http.MethodGet, "/destroy", nil)
	w := httptest.NewRecorder()
	h.HandleDestroy(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

func TestDestroyEndpoint_UnknownToken(t *testing.T) {
	h := newTestHandler()
	body := `{"destroy_token":"aabbccdd00112233aabbccdd00112233aabbccdd00112233aabbccdd00112233"}`
	req := httptest.NewRequest(http.MethodPost, "/destroy", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.HandleDestroy(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for unknown token, got %d", w.Code)
	}
}

func TestDestroyEndpoint_InvalidToken(t *testing.T) {
	h := newTestHandler()
	body := `{"destroy_token":"not-hex!"}`
	req := httptest.NewRequest(http.MethodPost, "/destroy", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.HandleDestroy(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid token, got %d", w.Code)
	}
}

func TestDestroyEndpoint_EmptyBody(t *testing.T) {
	h := newTestHandler()
	body := `{}`
	req := httptest.NewRequest(http.MethodPost, "/destroy", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.HandleDestroy(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for empty body, got %d", w.Code)
	}
}

func TestHMACDestroy_NotConfigured(t *testing.T) {
	h := newTestHandler() // no destroy secret configured
	body := `{"droplet_id":123,"signature":"abc"}`
	req := httptest.NewRequest(http.MethodPost, "/destroy/hmac", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.HandleDestroyHMAC(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 when not configured, got %d", w.Code)
	}
}

func TestHMACDestroy_BadSignature(t *testing.T) {
	h := NewHandler(Config{
		WebhookSecret: []byte(testSecret),
		DestroySecret: "test-destroy-secret",
	})

	body := `{"droplet_id":123,"signature":"badsig"}`
	req := httptest.NewRequest(http.MethodPost, "/destroy/hmac", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.HandleDestroyHMAC(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for bad signature, got %d", w.Code)
	}
}

func TestHMACCompute(t *testing.T) {
	secret := []byte("test-secret")
	sig1 := computeHMAC("123", secret)
	sig2 := computeHMAC("123", secret)
	sig3 := computeHMAC("456", secret)

	if sig1 != sig2 {
		t.Error("same input should produce same HMAC")
	}
	if sig1 == sig3 {
		t.Error("different input should produce different HMAC")
	}
	if len(sig1) != 64 {
		t.Errorf("expected 64-char hex HMAC, got %d chars", len(sig1))
	}
}
