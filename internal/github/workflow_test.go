package github

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestTrustedWorkflowRun(t *testing.T) {
	for _, tc := range []struct {
		name, event, head string
		id                int64
		status            int
		networkError      bool
		want              bool
		wantError         bool
	}{
		{"push", "push", "trusted/repo", 42, 200, false, true, false},
		{"schedule", "schedule", "trusted/repo", 42, 200, false, true, false},
		{"manual", "workflow_dispatch", "trusted/repo", 42, 200, false, true, false},
		{"same repository pull request", "pull_request", "trusted/repo", 42, 200, false, true, false},
		{"fork pull request", "pull_request", "external/repo", 42, 200, false, false, false},
		{"privileged PR", "pull_request_target", "trusted/repo", 42, 200, false, false, false},
		{"comment", "issue_comment", "trusted/repo", 42, 200, false, false, false},
		{"fork", "push", "external/repo", 42, 200, false, false, false},
		{"wrong run", "push", "trusted/repo", 43, 200, false, false, false},
		{"API failure", "push", "trusted/repo", 42, 500, false, false, true},
		{"network failure", "push", "trusted/repo", 42, 200, true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := HTTPClient
			t.Cleanup(func() { HTTPClient = original })
			HTTPClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if request.URL.Path != "/repos/trusted/repo/actions/runs/42" {
					t.Fatalf("wrong path: %s", request.URL.Path)
				}
				if tc.networkError {
					return nil, errors.New("offline")
				}
				body := fmt.Sprintf(`{"id":%d,"event":%q,"repository":{"id":1,"full_name":"trusted/repo"},"head_repository":{"id":1,"fork":false,"full_name":%q},"pull_requests":[{"number":9,"head":{"repo":{"id":1}},"base":{"repo":{"id":1}}}]}`, tc.id, tc.event, tc.head)
				return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			app := &App{ReadWorkflowRuns: true, AllowedRepositories: []string{"trusted/repo"}, cachedToken: "test-token", cachedScope: "trusted/repo|administration:write|actions:read", tokenExpires: time.Now().Add(time.Hour)}
			got, err := app.TrustedWorkflowRun(context.Background(), "trusted", "repo", 42)
			if got != tc.want || (err != nil) != tc.wantError {
				t.Fatalf("got %v, %v", got, err)
			}
		})
	}
}
