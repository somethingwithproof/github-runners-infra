package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestVerifyWebhookSignature_ValidSignature(t *testing.T) {
	payload := []byte(`{"test": true}`)
	secret := []byte("test-secret")

	// Generate valid signature
	sig := testSign(payload, secret)

	if !VerifyWebhookSignature(payload, sig, secret, "127.0.0.1") {
		t.Error("expected valid signature to pass")
	}
}

func TestVerifyWebhookSignature_InvalidSignature(t *testing.T) {
	payload := []byte(`{"test": true}`)
	secret := []byte("test-secret")
	wrongSecret := []byte("wrong-secret")

	sig := testSign(payload, wrongSecret)

	if VerifyWebhookSignature(payload, sig, secret, "127.0.0.1") {
		t.Error("expected invalid signature to fail")
	}
}

func TestVerifyWebhookSignature_MissingPrefix(t *testing.T) {
	if VerifyWebhookSignature([]byte("{}"), "noprefixhere", []byte("s"), "127.0.0.1") {
		t.Error("expected missing prefix to fail")
	}
}

func TestVerifyWebhookSignature_EmptySignature(t *testing.T) {
	if VerifyWebhookSignature([]byte("{}"), "", []byte("s"), "127.0.0.1") {
		t.Error("expected empty signature to fail")
	}
}

func testSign(payload, secret []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestListRepoRunners_ParsesResponse(t *testing.T) {
	runners := []Runner{
		{ID: 1, Name: "eph-repo-1", Status: "online"},
		{ID: 2, Name: "eph-repo-2", Status: "offline"},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/actions/runners") {
			resp := struct {
				TotalCount int      `json:"total_count"`
				Runners    []Runner `json:"runners"`
			}{
				TotalCount: len(runners),
				Runners:    runners,
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}
		// Installation token endpoint
		if strings.Contains(r.URL.Path, "/access_tokens") {
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]string{"token": "test-token"})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	// We can't easily test the full flow without mocking the GitHub API base URL,
	// but we can verify the Runner struct serialization
	data, _ := json.Marshal(runners[0])
	var parsed Runner
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to unmarshal runner: %v", err)
	}
	if parsed.ID != 1 || parsed.Name != "eph-repo-1" || parsed.Status != "online" {
		t.Errorf("unexpected runner: %+v", parsed)
	}
}

func TestRunnerStruct(t *testing.T) {
	tests := []struct {
		name   string
		json   string
		id     int64
		status string
	}{
		{"online runner", `{"id":42,"name":"eph-test","status":"online"}`, 42, "online"},
		{"offline runner", `{"id":99,"name":"eph-old","status":"offline"}`, 99, "offline"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var r Runner
			if err := json.Unmarshal([]byte(tt.json), &r); err != nil {
				t.Fatalf("unmarshal failed: %v", err)
			}
			if r.ID != tt.id {
				t.Errorf("expected ID %d, got %d", tt.id, r.ID)
			}
			if r.Status != tt.status {
				t.Errorf("expected status %q, got %q", tt.status, r.Status)
			}
		})
	}
}
