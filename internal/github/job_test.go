package github

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestWorkflowJobStatus(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		code       int
		status     JobStatus
		failure    bool
	}{
		{"queued", `{"id":42,"status":"queued"}`, 200, JobQueued, false},
		{"assigned", `{"id":42,"status":"in_progress"}`, 200, JobInProgress, false},
		{"finished", `{"id":42,"status":"completed"}`, 200, JobCompleted, false},
		{"wrong job", `{"id":43,"status":"queued"}`, 200, "", true},
		{"unknown state", `{"id":42,"status":"waiting"}`, 200, "", true},
		{"malformed response", `{`, 200, "", true},
		{"missing job", `{}`, 404, "", true},
		{"API outage", `{}`, 503, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := HTTPClient
			t.Cleanup(func() { HTTPClient = original })
			HTTPClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if request.Method != http.MethodGet || request.URL.Path != "/repos/trusted/repo/actions/jobs/42" {
					t.Fatalf("unexpected job request")
				}
				if request.Header.Get("Authorization") != "Bearer test-token" {
					t.Fatal("missing scoped authorization")
				}
				return &http.Response{StatusCode: tc.code, Body: io.NopCloser(strings.NewReader(tc.body)), Header: make(http.Header)}, nil
			})}
			app := jobStatusTestApp()
			status, err := app.WorkflowJobStatus(context.Background(), "trusted", "repo", 42)
			if status != tc.status || (err != nil) != tc.failure {
				t.Fatalf("status=%q error=%v", status, err)
			}
		})
	}
}

func jobStatusTestApp() *App {
	return &App{ReadWorkflowRuns: true, AllowedRepositories: []string{"trusted/repo"}, cachedToken: "test-token", cachedScope: "trusted/repo|administration:write|actions:read", tokenExpires: time.Now().Add(time.Hour)}
}

func TestWorkflowJobStatusFailsClosedBeforeOrDuringRequest(t *testing.T) {
	original := HTTPClient
	t.Cleanup(func() { HTTPClient = original })
	calls := 0
	HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("offline") })}
	app := jobStatusTestApp()
	if _, err := app.WorkflowJobStatus(context.Background(), "trusted", "repo", 0); err == nil {
		t.Fatal("invalid job accepted")
	}
	app.ReadWorkflowRuns = false
	if _, err := app.WorkflowJobStatus(context.Background(), "trusted", "repo", 42); err == nil {
		t.Fatal("missing read scope accepted")
	}
	if calls != 0 {
		t.Fatal("invalid configuration contacted GitHub")
	}
	app.ReadWorkflowRuns = true
	if _, err := app.WorkflowJobStatus(context.Background(), "trusted", "repo", 42); err == nil {
		t.Fatal("network failure accepted")
	}
	app.cachedToken = ""
	app.PrivateKey = nil
	if _, err := app.WorkflowJobStatus(context.Background(), "trusted", "repo", 42); err == nil {
		t.Fatal("missing token signing key accepted")
	}
}

func TestWorkflowJobStatusPreservesThrottleDeadline(t *testing.T) {
	original := HTTPClient
	t.Cleanup(func() { HTTPClient = original })
	HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 429, Body: io.NopCloser(strings.NewReader(`{}`)), Header: http.Header{"Retry-After": []string{"120"}}}, nil
	})}
	_, err := jobStatusTestApp().WorkflowJobStatus(context.Background(), "trusted", "repo", 42)
	reset, limited := RateLimitReset(err)
	if !limited || reset.Before(time.Now().Add(time.Minute)) {
		t.Fatalf("throttle deadline lost: %v", err)
	}
}
