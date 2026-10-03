package github

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestPRRunOriginFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		headID, prHeadID, prBaseID int
		fork, omitPR               bool
		want                       bool
	}{
		{"same repository", 1, 1, 1, false, false, true},
		{"fork flag", 1, 1, 1, true, false, false},
		{"different repository ID", 2, 1, 1, false, false, false},
		{"PR head from fork", 1, 2, 1, false, false, false},
		{"different PR base", 1, 1, 2, false, false, false},
		{"missing head identity", 0, 1, 1, false, false, false},
		{"missing PR association", 1, 1, 1, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := HTTPClient
			t.Cleanup(func() { HTTPClient = original })
			HTTPClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				prs := fmt.Sprintf(`[{"number":9,"head":{"repo":{"id":%d}},"base":{"repo":{"id":%d}}}]`, tc.prHeadID, tc.prBaseID)
				if tc.omitPR {
					prs = `[]`
				}
				body := fmt.Sprintf(`{"id":42,"event":"pull_request","repository":{"id":1,"full_name":"trusted/repo"},"head_repository":{"id":%d,"fork":%v,"full_name":"trusted/repo"},"pull_requests":%s}`, tc.headID, tc.fork, prs)
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			app := &App{ReadWorkflowRuns: true, AllowedRepositories: []string{"trusted/repo"}, cachedToken: "test-token", cachedScope: "trusted/repo|administration:write|actions:read", tokenExpires: time.Now().Add(time.Hour)}
			got, err := app.TrustedWorkflowRun(context.Background(), "trusted", "repo", 42)
			if err != nil || got != tc.want {
				t.Fatalf("trusted=%v err=%v", got, err)
			}
		})
	}
}
