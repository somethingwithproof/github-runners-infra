package github

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// HTTPClient is a shared client with timeouts for all GitHub API calls. (#4)
var HTTPClient = &http.Client{
	Timeout: 30 * time.Second,
	Transport: &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
	},
}

// App represents a GitHub App for authentication.
type App struct {
	AppID               int64
	InstallationID      int64
	PrivateKey          []byte
	AllowedRepositories []string

	tokenMu      sync.Mutex
	cachedToken  string
	tokenExpires time.Time
	cachedScope  string
}

// ValidateTokenScope fetches and verifies a least-privilege installation token
// before any webhook is served.
func (a *App) ValidateTokenScope(ctx context.Context) error {
	_, err := a.InstallationTokenContext(ctx)
	return err
}

// GenerateJWT creates a short-lived JWT for GitHub App authentication.
func (a *App) GenerateJWT() (string, error) {
	block, _ := pem.Decode(a.PrivateKey)
	if block == nil {
		return "", fmt.Errorf("failed to decode PEM block")
	}

	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		parsed, pkcs8Err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if pkcs8Err != nil {
			return "", fmt.Errorf("parse RSA private key as PKCS#1 (%v) or PKCS#8: %w", err, pkcs8Err)
		}
		var ok bool
		key, ok = parsed.(*rsa.PrivateKey)
		if !ok {
			return "", fmt.Errorf("GitHub App private key is not RSA")
		}
	}

	now := time.Now()
	claims := jwt.RegisteredClaims{
		IssuedAt:  jwt.NewNumericDate(now.Add(-60 * time.Second)),
		ExpiresAt: jwt.NewNumericDate(now.Add(9 * time.Minute)),
		Issuer:    fmt.Sprintf("%d", a.AppID),
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	return token.SignedString(key)
}

// InstallationTokenContext retrieves and caches an installation token while
// honoring cancellation from the provisioning lifecycle.
func (a *App) InstallationTokenContext(ctx context.Context) (string, error) {
	repositoryNames, expectedRepositories, scope, err := a.tokenScope()
	if err != nil {
		return "", err
	}
	a.tokenMu.Lock()
	defer a.tokenMu.Unlock()
	if a.cachedToken != "" && a.cachedScope == scope && time.Now().Before(a.tokenExpires.Add(-5*time.Minute)) {
		return a.cachedToken, nil
	}

	jwtToken, err := a.GenerateJWT()
	if err != nil {
		return "", fmt.Errorf("generate JWT: %w", err)
	}

	url := fmt.Sprintf("https://api.github.com/app/installations/%d/access_tokens", a.InstallationID)
	body, err := json.Marshal(struct {
		Repositories []string          `json:"repositories"`
		Permissions  map[string]string `json:"permissions"`
	}{Repositories: repositoryNames, Permissions: map[string]string{"administration": "write"}})
	if err != nil {
		return "", fmt.Errorf("encode installation token scope: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+jwtToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("request installation token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		return "", apiStatusError(resp, "requesting installation token")
	}

	var result struct {
		Token        string            `json:"token"`
		ExpiresAt    time.Time         `json:"expires_at"`
		Permissions  map[string]string `json:"permissions"`
		Repositories []struct {
			FullName string `json:"full_name"`
		} `json:"repositories"`
	}
	if err := decodeJSON(resp.Body, &result); err != nil {
		return "", err
	}
	if result.Token == "" || result.ExpiresAt.IsZero() {
		return "", fmt.Errorf("GitHub returned an incomplete installation token")
	}
	if result.Permissions["administration"] != "write" {
		return "", fmt.Errorf("GitHub installation token lacks administration:write scope")
	}
	actualRepositories := make([]string, 0, len(result.Repositories))
	for _, repository := range result.Repositories {
		actualRepositories = append(actualRepositories, strings.ToLower(repository.FullName))
	}
	sort.Strings(actualRepositories)
	if strings.Join(actualRepositories, ",") != strings.Join(expectedRepositories, ",") {
		return "", fmt.Errorf("GitHub installation token repository scope does not match allowlist")
	}
	a.cachedToken = result.Token
	a.tokenExpires = result.ExpiresAt
	a.cachedScope = scope
	return result.Token, nil
}

func (a *App) tokenScope() ([]string, []string, string, error) {
	if len(a.AllowedRepositories) == 0 || len(a.AllowedRepositories) > 500 {
		return nil, nil, "", fmt.Errorf("installation token requires 1 to 500 allowed repositories")
	}
	names := make([]string, 0, len(a.AllowedRepositories))
	fullNames := make([]string, 0, len(a.AllowedRepositories))
	owner := ""
	seen := make(map[string]struct{}, len(a.AllowedRepositories))
	for _, raw := range a.AllowedRepositories {
		parts := strings.Split(strings.ToLower(strings.TrimSpace(raw)), "/")
		if len(parts) != 2 {
			return nil, nil, "", fmt.Errorf("invalid installation token repository %q", raw)
		}
		parts[0], parts[1] = strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		if parts[0] == "" || parts[1] == "" {
			return nil, nil, "", fmt.Errorf("invalid installation token repository %q", raw)
		}
		if owner != "" && owner != parts[0] {
			return nil, nil, "", fmt.Errorf("one installation token cannot span multiple repository owners")
		}
		owner = parts[0]
		fullName := parts[0] + "/" + parts[1]
		if _, duplicate := seen[fullName]; duplicate {
			continue
		}
		seen[fullName] = struct{}{}
		names = append(names, parts[1])
		fullNames = append(fullNames, fullName)
	}
	sort.Strings(names)
	sort.Strings(fullNames)
	scope := strings.Join(fullNames, ",") + "|administration:write"
	return names, fullNames, scope, nil
}
