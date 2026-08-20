package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestGenerateRepoJITConfig(t *testing.T) {
	original := HTTPClient
	t.Cleanup(func() { HTTPClient = original })
	HTTPClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || request.URL.Path != "/repos/trusted/repo/actions/runners/generate-jitconfig" {
			t.Fatalf("unexpected request %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer installation-token" {
			t.Fatalf("unexpected authorization header")
		}
		var body struct {
			Name          string   `json:"name"`
			RunnerGroupID int64    `json:"runner_group_id"`
			Labels        []string `json:"labels"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Name != "runner-1" || body.RunnerGroupID != 1 || len(body.Labels) != 2 {
			t.Fatalf("unexpected JIT request: %#v", body)
		}
		return &http.Response{
			StatusCode: http.StatusCreated,
			Body: io.NopCloser(strings.NewReader(
				`{"runner":{"id":123},"encoded_jit_config":"c2luZ2xlLXVzZS1jb25maWc="}`,
			)),
			Header: make(http.Header),
		}, nil
	})}

	app := &App{cachedToken: "installation-token", cachedScope: "trusted/repo|administration:write", tokenExpires: time.Now().Add(time.Hour), AllowedRepositories: []string{"trusted/repo"}}
	config, err := app.GenerateRepoJITConfig(
		context.Background(), "trusted", "repo", "runner-1", 1, []string{"self-hosted", "chef"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if config.RunnerID != 123 || config.EncodedConfig != "c2luZ2xlLXVzZS1jb25maWc=" {
		t.Fatalf("unexpected JIT config: %#v", config)
	}
}

func TestRepoRunnerOnline(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		wantStatus RunnerStatus
		wantErr    bool
		wantRate   bool
		headers    http.Header
	}{
		{name: "online", statusCode: http.StatusOK, body: `{"id":123,"status":"online"}`, wantStatus: RunnerOnline},
		{name: "offline", statusCode: http.StatusOK, body: `{"id":123,"status":"offline"}`, wantStatus: RunnerOffline},
		{name: "not found", statusCode: http.StatusNotFound, body: `{}`, wantStatus: RunnerMissing},
		{name: "API failure", statusCode: http.StatusInternalServerError, body: `{}`, wantErr: true},
		{name: "primary rate limit", statusCode: http.StatusForbidden, body: `{}`, wantErr: true, wantRate: true,
			headers: http.Header{"X-Ratelimit-Remaining": []string{"0"}, "X-Ratelimit-Reset": []string{"2000000000"}}},
		{name: "secondary rate limit", statusCode: http.StatusTooManyRequests, body: `{}`, wantErr: true, wantRate: true,
			headers: http.Header{"Retry-After": []string{"120"}}},
		{name: "secondary forbidden rate limit", statusCode: http.StatusForbidden, body: `{}`, wantErr: true, wantRate: true,
			headers: http.Header{"Retry-After": []string{"60"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			original := HTTPClient
			t.Cleanup(func() { HTTPClient = original })
			HTTPClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if request.Method != http.MethodGet || request.URL.Path != "/repos/trusted/repo/actions/runners/123" {
					t.Fatalf("unexpected request %s %s", request.Method, request.URL.Path)
				}
				return &http.Response{
					StatusCode: test.statusCode,
					Body:       io.NopCloser(strings.NewReader(test.body)),
					Header:     test.headers,
				}, nil
			})}
			app := &App{
				cachedToken: "installation-token", cachedScope: "trusted/repo|administration:write",
				tokenExpires: time.Now().Add(time.Hour), AllowedRepositories: []string{"trusted/repo"},
			}
			status, err := app.RepoRunnerStatus(context.Background(), "trusted", "repo", 123)
			if (err != nil) != test.wantErr || status != test.wantStatus {
				t.Fatalf("RepoRunnerStatus() = %v, %v; want %v, error=%v", status, err, test.wantStatus, test.wantErr)
			}
			if _, rateLimited := RateLimitReset(err); rateLimited != test.wantRate {
				t.Fatalf("rate-limited error = %v, want %v", rateLimited, test.wantRate)
			}
		})
	}
}

func TestPersistentClientErrorTreatsBusyRunnerStatusesAsRetryable(t *testing.T) {
	for _, status := range []int{http.StatusConflict, http.StatusUnprocessableEntity} {
		err := &APIStatusError{Status: status, Action: "removing busy runner"}
		if PersistentClientError(err) {
			t.Fatalf("status %d was classified as a persistent client error", status)
		}
	}
	if !PersistentClientError(&APIStatusError{Status: http.StatusUnauthorized, Action: "authenticating"}) {
		t.Fatal("401 was not classified as a persistent client error")
	}
}

func TestRunnerMutationAPIsPreserveRateLimits(t *testing.T) {
	for _, test := range []struct {
		name   string
		method string
		call   func(*App) error
	}{
		{name: "generate", method: http.MethodPost, call: func(app *App) error {
			_, err := app.GenerateRepoJITConfig(context.Background(), "trusted", "repo", "runner", 1, []string{"self-hosted"})
			return err
		}},
		{name: "remove", method: http.MethodDelete, call: func(app *App) error {
			return app.RemoveRepoRunner(context.Background(), "trusted", "repo", 123)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			original := HTTPClient
			t.Cleanup(func() { HTTPClient = original })
			HTTPClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if request.Method != test.method {
					t.Fatalf("request method = %s, want %s", request.Method, test.method)
				}
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Body:       io.NopCloser(strings.NewReader(`{}`)),
					Header:     http.Header{"Retry-After": []string{"60"}},
				}, nil
			})}
			app := &App{
				cachedToken: "installation-token", cachedScope: "trusted/repo|administration:write",
				tokenExpires: time.Now().Add(time.Hour), AllowedRepositories: []string{"trusted/repo"},
			}
			if _, rateLimited := RateLimitReset(test.call(app)); !rateLimited {
				t.Fatal("rate limit was collapsed into a generic API error")
			}
		})
	}
}

func TestRetryAfterDoesNotReclassifyUnrelatedClientError(t *testing.T) {
	response := &http.Response{
		StatusCode: http.StatusUnprocessableEntity,
		Header:     http.Header{"Retry-After": []string{"60"}},
	}
	err := apiStatusError(response, "invalid request")
	if _, rateLimited := RateLimitReset(err); rateLimited {
		t.Fatalf("422 Retry-After was classified as a throttle: %v", err)
	}
	var statusErr *APIStatusError
	if !errors.As(err, &statusErr) || statusErr.Status != http.StatusUnprocessableEntity {
		t.Fatalf("422 error = %T %v", err, err)
	}
}

func TestGenerateRepoJITConfigRejectsNonBase64Response(t *testing.T) {
	original := HTTPClient
	t.Cleanup(func() { HTTPClient = original })
	HTTPClient = &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusCreated,
			Body: io.NopCloser(strings.NewReader(
				`{"runner":{"id":123},"encoded_jit_config":"'\nrun-command"}`,
			)),
			Header: make(http.Header),
		}, nil
	})}
	app := &App{cachedToken: "installation-token", cachedScope: "trusted/repo|administration:write", tokenExpires: time.Now().Add(time.Hour), AllowedRepositories: []string{"trusted/repo"}}
	if _, err := app.GenerateRepoJITConfig(context.Background(), "trusted", "repo", "runner-1", 1, []string{"self-hosted"}); err == nil {
		t.Fatal("accepted a non-base64 JIT configuration")
	}
}

func TestGenerateRepoJITConfigRejectsBase64WithNewline(t *testing.T) {
	original := HTTPClient
	t.Cleanup(func() { HTTPClient = original })
	HTTPClient = &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusCreated,
			Body: io.NopCloser(strings.NewReader(
				`{"runner":{"id":123},"encoded_jit_config":"QUFB\nQUJC"}`,
			)),
			Header: make(http.Header),
		}, nil
	})}
	app := &App{
		cachedToken: "installation-token", cachedScope: "trusted/repo|administration:write",
		tokenExpires: time.Now().Add(time.Hour), AllowedRepositories: []string{"trusted/repo"},
	}
	if _, err := app.GenerateRepoJITConfig(context.Background(), "trusted", "repo", "runner-1", 1, []string{"self-hosted"}); err == nil {
		t.Fatal("accepted base64 JIT configuration containing a newline")
	}
}

func TestVerifyWebhookSignature(t *testing.T) {
	payload := []byte(`{"action":"queued"}`)
	secret := []byte("secret")
	if VerifyWebhookSignature(payload, "sha256=invalid", secret, "test") {
		t.Fatal("invalid signature was accepted")
	}
}

func TestVerifyWebhookSignatureEscapesClientIdentifier(t *testing.T) {
	var output bytes.Buffer
	original := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(original) })
	VerifyWebhookSignature([]byte("payload"), "invalid", []byte("secret"), "peer\nSECURITY: forged")
	logged := output.String()
	if strings.Count(logged, "SECURITY:") != 2 {
		// One prefix is the real record and one is quoted attacker text; both
		// must remain on the same physical log line.
		t.Fatalf("unexpected security log: %q", logged)
	}
	if strings.Count(logged, "\n") != 1 {
		t.Fatalf("client identifier injected a log line: %q", logged)
	}
}
