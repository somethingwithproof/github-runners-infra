package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gh "github.com/thomasvincent/github-runners-infra/internal/github"
)

func TestInstallationAndRepositoryBoundary(t *testing.T) {
	secret := []byte("unit-test-secret")
	for _, tc := range []struct {
		name                  string
		installation          int64
		owner, repo, fullName string
		allowlist             []string
		badSignature          bool
		want                  int
	}{
		{"trusted", 77, "trusted", "repo", "trusted/repo", []string{"trusted/repo"}, false, 200},
		{"other installation", 88, "trusted", "repo", "trusted/repo", []string{"trusted/repo"}, false, 403},
		{"missing installation", 0, "trusted", "repo", "trusted/repo", []string{"trusted/repo"}, false, 403},
		{"foreign owner", 77, "other", "repo", "other/repo", []string{"trusted/repo"}, false, 403},
		{"unlisted repository", 77, "trusted", "other", "trusted/other", []string{"trusted/repo"}, false, 403},
		{"identity mismatch", 77, "trusted", "repo", "other/repo", []string{"trusted/repo"}, false, 403},
		{"empty allowlist", 77, "trusted", "repo", "trusted/repo", nil, false, 403},
		{"bad signature", 77, "trusted", "repo", "trusted/repo", []string{"trusted/repo"}, true, 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := NewHandler(Config{WebhookSecret: secret, GitHubApp: &gh.App{InstallationID: 77}, AllowedRepositories: tc.allowlist})
			// Completed events exercise the authorization boundary without provisioning.
			body := fmt.Sprintf(`{"action":"completed","installation":{"id":%d},"repository":{"owner":{"login":%q},"name":%q,"full_name":%q}}`, tc.installation, tc.owner, tc.repo, tc.fullName)
			request := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
			mac := hmac.New(sha256.New, secret)
			mac.Write([]byte(body))
			signature := "sha256=" + hex.EncodeToString(mac.Sum(nil))
			if tc.badSignature {
				signature = "sha256=invalid"
			}
			request.Header.Set("X-Hub-Signature-256", signature)
			request.Header.Set("X-GitHub-Event", "workflow_job")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tc.want {
				t.Fatalf("status = %d; want %d", response.Code, tc.want)
			}
		})
	}
}
