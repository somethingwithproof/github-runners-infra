package github

import (
	"bytes"
	"context"
	"encoding/json"
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
