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

// Runner represents a GitHub Actions self-hosted runner.
type Runner struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"` // "online" or "offline"
}

// ListRepoRunners returns all self-hosted runners for a repository.
func (a *App) ListRepoRunners(owner, repo string) ([]Runner, error) {
	token, err := a.InstallationToken()
	if err != nil {
		return nil, fmt.Errorf("get installation token: %w", err)
	}

	var all []Runner
	page := 1
	for {
		url := fmt.Sprintf("https://api.github.com/repos/%s/%s/actions/runners?per_page=100&page=%d", owner, repo, page)
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "token "+token)
		req.Header.Set("Accept", "application/vnd.github+json")

		resp, err := HTTPClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("list repo runners: %w", err)
		}

		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("unexpected status %d listing repo runners", resp.StatusCode)
		}

		var result struct {
			TotalCount int      `json:"total_count"`
			Runners    []Runner `json:"runners"`
		}
		err = decodeJSON(resp.Body, &result)
		_ = resp.Body.Close()
		if err != nil {
			return nil, err
		}

		all = append(all, result.Runners...)
		if len(all) >= result.TotalCount {
			break
		}
		page++
	}
	return all, nil
}

// RemoveRepoRunner deletes a self-hosted runner from a repository.
func (a *App) RemoveRepoRunner(owner, repo string, runnerID int64) error {
	token, err := a.InstallationToken()
	if err != nil {
		return fmt.Errorf("get installation token: %w", err)
	}

	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/actions/runners/%d", owner, repo, runnerID)
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("remove runner: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("unexpected status %d removing runner %d", resp.StatusCode, runnerID)
	}
	return nil
}

// RemoveOfflineRepoRunners removes all offline runners from a repository.
// Returns the number of runners removed.
func (a *App) RemoveOfflineRepoRunners(owner, repo string) (int, error) {
	runners, err := a.ListRepoRunners(owner, repo)
	if err != nil {
		return 0, err
	}

	removed := 0
	for _, r := range runners {
		if r.Status == "offline" {
			log.Printf("Removing offline runner %s (ID: %d) from %s/%s", r.Name, r.ID, owner, repo)
			if err := a.RemoveRepoRunner(owner, repo, r.ID); err != nil {
				log.Printf("Failed to remove runner %d: %v", r.ID, err)
				continue
			}
			removed++
		}
	}
	return removed, nil
}

// ListInstallationRepos returns all repositories accessible to this installation.
func (a *App) ListInstallationRepos() ([][2]string, error) {
	token, err := a.InstallationToken()
	if err != nil {
		return nil, fmt.Errorf("get installation token: %w", err)
	}

	var repos [][2]string // [owner, name] pairs
	page := 1
	for {
		url := fmt.Sprintf("https://api.github.com/installation/repositories?per_page=100&page=%d", page)
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "token "+token)
		req.Header.Set("Accept", "application/vnd.github+json")

		resp, err := HTTPClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("list installation repos: %w", err)
		}

		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("unexpected status %d listing installation repos", resp.StatusCode)
		}

		var result struct {
			TotalCount   int `json:"total_count"`
			Repositories []struct {
				Owner struct {
					Login string `json:"login"`
				} `json:"owner"`
				Name string `json:"name"`
			} `json:"repositories"`
		}
		err = decodeJSON(resp.Body, &result)
		_ = resp.Body.Close()
		if err != nil {
			return nil, err
		}

		for _, r := range result.Repositories {
			repos = append(repos, [2]string{r.Owner.Login, r.Name})
		}
		if len(repos) >= result.TotalCount {
			break
		}
		page++
	}
	return repos, nil
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
