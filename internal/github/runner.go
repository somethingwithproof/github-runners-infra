package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

func decodeJSON(r io.Reader, v any) error {
	return json.NewDecoder(r).Decode(v)
}

// GenerateRunnerToken creates a short-lived registration token for an org runner.
func (a *App) GenerateRunnerToken(org string) (string, error) {
	token, err := a.InstallationToken()
	if err != nil {
		return "", fmt.Errorf("get installation token: %w", err)
	}

	url := fmt.Sprintf("https://api.github.com/orgs/%s/actions/runners/registration-token", org)
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("request runner token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("unexpected status %d requesting runner token", resp.StatusCode)
	}

	var result struct {
		Token string `json:"token"`
	}
	if err := decodeJSON(resp.Body, &result); err != nil {
		return "", err
	}
	return result.Token, nil
}

// GenerateRepoRunnerToken creates a registration token for a specific repo.
func (a *App) GenerateRepoRunnerToken(owner, repo string) (string, error) {
	token, err := a.InstallationToken()
	if err != nil {
		return "", fmt.Errorf("get installation token: %w", err)
	}

	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/actions/runners/registration-token", owner, repo)
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("request runner token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("unexpected status %d requesting repo runner token", resp.StatusCode)
	}

	var result struct {
		Token string `json:"token"`
	}
	if err := decodeJSON(resp.Body, &result); err != nil {
		return "", err
	}
	return result.Token, nil
}

// VerifyWebhookSignature checks the HMAC-SHA256 signature of a webhook payload.
// Logs failed attempts with client IP for security monitoring. (#10)
func VerifyWebhookSignature(payload []byte, signature string, secret []byte, clientIP string) bool {
	if !strings.HasPrefix(signature, "sha256=") {
		log.Printf("SECURITY: invalid signature format from %s", clientIP)
		return false
	}

	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)
	expected := hex.EncodeToString(mac.Sum(nil))

	valid := hmac.Equal([]byte(signature[7:]), []byte(expected))
	if !valid {
		log.Printf("SECURITY: signature mismatch from %s", clientIP)
	}
	return valid
}
