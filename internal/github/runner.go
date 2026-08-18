package github

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
)

const githubAPIVersion = "2022-11-28"

// JITConfig is the one-time configuration for a repository-scoped runner.
type JITConfig struct {
	RunnerID      int64
	EncodedConfig string
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
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", githubAPIVersion)

	resp, err := HTTPClient.Do(req)
	if err != nil {
		return JITConfig{}, fmt.Errorf("request repository JIT config: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		return JITConfig{}, fmt.Errorf("unexpected status %d requesting repository JIT config", resp.StatusCode)
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
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	resp, err := HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("remove repository runner: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("unexpected status %d removing repository runner %d", resp.StatusCode, runnerID)
	}
	return nil
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
