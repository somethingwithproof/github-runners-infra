package github

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	githubAPIVersion = "2022-11-28"
	bearerPrefix     = "Bearer "
	acceptGitHubJSON = "application/vnd.github+json"
)

// JITConfig is the one-time configuration for a repository-scoped runner.
type JITConfig struct {
	RunnerID      int64
	EncodedConfig string
}

// RunnerStatus distinguishes an unregistered runner from a JIT runner that
// GitHub has already removed after its one job.
type RunnerStatus int

const (
	RunnerMissing RunnerStatus = iota
	RunnerOffline
	RunnerOnline
)

// RateLimitError preserves the earliest safe retry time from GitHub.
type RateLimitError struct {
	ResetAt time.Time
	Status  int
}

// APIStatusError preserves non-rate-limit HTTP status for retry policy.
type APIStatusError struct {
	Status int
	Action string
}

func (e *APIStatusError) Error() string {
	return fmt.Sprintf("unexpected status %d %s", e.Status, e.Action)
}

// PersistentClientError reports a non-throttle 4xx response which will not be
// repaired by retrying through a transient provider outage.
func PersistentClientError(err error) bool {
	var apiErr *APIStatusError
	return errors.As(err, &apiErr) && apiErr.Status >= 400 && apiErr.Status < 500 &&
		apiErr.Status != http.StatusRequestTimeout && apiErr.Status != http.StatusConflict &&
		apiErr.Status != http.StatusUnprocessableEntity && apiErr.Status != http.StatusTooManyRequests
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("GitHub API rate limited request with status %d until %s", e.Status, e.ResetAt.Format(time.RFC3339))
}

// RateLimitReset extracts a GitHub throttle deadline from an error chain.
func RateLimitReset(err error) (time.Time, bool) {
	var rateLimit *RateLimitError
	if !errors.As(err, &rateLimit) {
		return time.Time{}, false
	}
	return rateLimit.ResetAt, true
}

// RepoRunnerStatus returns GitHub's current view of a repository runner.
func (a *App) RepoRunnerStatus(ctx context.Context, owner, repo string, runnerID int64) (RunnerStatus, error) {
	token, err := a.InstallationTokenContext(ctx)
	if err != nil {
		return RunnerMissing, fmt.Errorf("get installation token: %w", err)
	}
	endpoint := fmt.Sprintf(
		"https://api.github.com/repos/%s/%s/actions/runners/%d",
		url.PathEscape(owner), url.PathEscape(repo), runnerID,
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return RunnerMissing, err
	}
	req.Header.Set("Authorization", bearerPrefix+token)
	req.Header.Set("Accept", acceptGitHubJSON)
	req.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	resp, err := HTTPClient.Do(req)
	if err != nil {
		return RunnerMissing, fmt.Errorf("get repository runner status: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return RunnerMissing, nil
	}
	if resp.StatusCode != http.StatusOK {
		return RunnerMissing, apiStatusError(resp, fmt.Sprintf("getting repository runner %d", runnerID))
	}
	var runner struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
	}
	if err := decodeJSON(resp.Body, &runner); err != nil {
		return RunnerMissing, fmt.Errorf("decode repository runner status: %w", err)
	}
	if runner.ID != runnerID {
		return RunnerMissing, fmt.Errorf("GitHub returned runner ID %d while querying %d", runner.ID, runnerID)
	}
	if runner.Status == "online" {
		return RunnerOnline, nil
	}
	return RunnerOffline, nil
}

// GenerateRepoJITConfig creates a one-job runner configuration. The encoded
// configuration is safe to give only to the runner being provisioned; it does
// not grant DigitalOcean or GitHub App control-plane access.
func (a *App) GenerateRepoJITConfig(ctx context.Context, owner, repo, name string, runnerGroupID int64, labels []string) (JITConfig, error) {
	token, err := a.InstallationTokenContext(ctx)
	if err != nil {
		return JITConfig{}, fmt.Errorf("get installation token: %w", err)
	}

	payload, err := json.Marshal(struct {
		Name          string   `json:"name"`
		RunnerGroupID int64    `json:"runner_group_id"`
		Labels        []string `json:"labels"`
		WorkFolder    string   `json:"work_folder"`
	}{
		Name:          name,
		RunnerGroupID: runnerGroupID,
		Labels:        labels,
		WorkFolder:    "_work",
	})
	if err != nil {
		return JITConfig{}, fmt.Errorf("encode JIT request: %w", err)
	}

	endpoint := fmt.Sprintf(
		"https://api.github.com/repos/%s/%s/actions/runners/generate-jitconfig",
		url.PathEscape(owner), url.PathEscape(repo),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return JITConfig{}, err
	}
	req.Header.Set("Authorization", bearerPrefix+token)
	req.Header.Set("Accept", acceptGitHubJSON)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", githubAPIVersion)

	resp, err := HTTPClient.Do(req)
	if err != nil {
		return JITConfig{}, fmt.Errorf("request repository JIT config: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		return JITConfig{}, apiStatusError(resp, "requesting repository JIT config")
	}

	var result struct {
		Runner struct {
			ID int64 `json:"id"`
		} `json:"runner"`
		EncodedConfig string `json:"encoded_jit_config"`
	}
	if err := decodeJSON(resp.Body, &result); err != nil {
		return JITConfig{}, fmt.Errorf("decode repository JIT config: %w", err)
	}
	if result.Runner.ID == 0 || result.EncodedConfig == "" {
		return JITConfig{}, fmt.Errorf("GitHub returned an incomplete repository JIT config")
	}
	decoded, err := base64.StdEncoding.DecodeString(result.EncodedConfig)
	if err != nil || len(decoded) == 0 || base64.StdEncoding.EncodeToString(decoded) != result.EncodedConfig {
		return JITConfig{}, fmt.Errorf("GitHub returned an invalid base64 repository JIT config")
	}
	return JITConfig{RunnerID: result.Runner.ID, EncodedConfig: result.EncodedConfig}, nil
}

func decodeJSON(r io.Reader, v any) error {
	return json.NewDecoder(r).Decode(v)
}

// RemoveRepoRunner removes a repository-scoped runner by its GitHub ID.
func (a *App) RemoveRepoRunner(ctx context.Context, owner, repo string, runnerID int64) error {
	token, err := a.InstallationTokenContext(ctx)
	if err != nil {
		return fmt.Errorf("get installation token: %w", err)
	}
	endpoint := fmt.Sprintf(
		"https://api.github.com/repos/%s/%s/actions/runners/%d",
		url.PathEscape(owner), url.PathEscape(repo), runnerID,
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", bearerPrefix+token)
	req.Header.Set("Accept", acceptGitHubJSON)
	req.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	resp, err := HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("remove repository runner: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		return apiStatusError(resp, fmt.Sprintf("removing repository runner %d", runnerID))
	}
	return nil
}

func apiStatusError(response *http.Response, action string) error {
	if response.StatusCode == http.StatusTooManyRequests ||
		(response.StatusCode == http.StatusForbidden &&
			(response.Header.Get("Retry-After") != "" || response.Header.Get("X-RateLimit-Remaining") == "0")) {
		return &RateLimitError{ResetAt: retryAtFromHeaders(response.Header), Status: response.StatusCode}
	}
	return &APIStatusError{Status: response.StatusCode, Action: action}
}

func retryAtFromHeaders(header http.Header) time.Time {
	now := time.Now().UTC()
	if value := header.Get("Retry-After"); value != "" {
		if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
			return now.Add(time.Duration(seconds) * time.Second)
		}
		if parsed, err := http.ParseTime(value); err == nil {
			return parsed.UTC()
		}
	}
	if unixSeconds, err := strconv.ParseInt(header.Get("X-RateLimit-Reset"), 10, 64); err == nil && unixSeconds > 0 {
		return time.Unix(unixSeconds, 0).UTC()
	}
	return now.Add(time.Minute)
}

// VerifyWebhookSignature checks the HMAC-SHA256 signature of a webhook payload.
// Logs failed attempts with client IP for security monitoring. (#10)
func VerifyWebhookSignature(payload []byte, signature string, secret []byte, clientIP string) bool {
	if !strings.HasPrefix(signature, "sha256=") {
		log.Printf("SECURITY: invalid signature format from %q", clientIP)
		return false
	}

	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)
	expected := hex.EncodeToString(mac.Sum(nil))

	valid := hmac.Equal([]byte(signature[7:]), []byte(expected))
	if !valid {
		log.Printf("SECURITY: signature mismatch from %q", clientIP)
	}
	return valid
}
