package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestPublicWorkflowReadTokenScope(t *testing.T) {
	for _, permission := range []string{"read", "", "write"} {
		t.Run("actions:"+permission, func(t *testing.T) {
			original := HTTPClient
			t.Cleanup(func() { HTTPClient = original })
			HTTPClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				var body struct {
					Repositories []string          `json:"repositories"`
					Permissions  map[string]string `json:"permissions"`
				}
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if strings.Join(body.Repositories, ",") != "repo" || body.Permissions["actions"] != "read" || body.Permissions["administration"] != "write" || len(body.Permissions) != 2 {
					t.Fatalf("unexpected permission scope: %#v", body)
				}
				response := fmt.Sprintf(`{"token":"unit-test-token","expires_at":"2099-01-01T00:00:00Z","permissions":{"administration":"write","actions":%q},"repositories":[{"full_name":"trusted/repo"}]}`, permission)
				return &http.Response{StatusCode: 201, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response))}, nil
			})}
			app := &App{AppID: 1, InstallationID: 2, PrivateKey: testPrivateKey(t), AllowedRepositories: []string{"trusted/repo"}, ReadWorkflowRuns: true}
			_, err := app.InstallationTokenContext(context.Background())
			if (err == nil) != (permission == "read") {
				t.Fatalf("permission=%q err=%v", permission, err)
			}
		})
	}
}
