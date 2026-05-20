package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

func TestRunnerJSON_Roundtrip(t *testing.T) {
	runners := []Runner{
		{ID: 1, Name: "eph-repo-1", Status: "online"},
		{ID: 2, Name: "eph-repo-2", Status: "offline"},
	}

	for _, r := range runners {
		data, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("failed to marshal runner: %v", err)
		}
		var parsed Runner
		if err := json.Unmarshal(data, &parsed); err != nil {
			t.Fatalf("failed to unmarshal runner: %v", err)
		}
		if parsed.ID != r.ID || parsed.Name != r.Name || parsed.Status != r.Status {
			t.Errorf("roundtrip mismatch: got %+v, want %+v", parsed, r)
		}
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
