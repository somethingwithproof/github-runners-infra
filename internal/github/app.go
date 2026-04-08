package github

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
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
	AppID          int64
	InstallationID int64
	PrivateKey     []byte

	tokenMu      sync.Mutex
	cachedToken  string
	tokenExpires time.Time
}

// GenerateJWT creates a short-lived JWT for GitHub App authentication.
func (a *App) GenerateJWT() (string, error) {
	block, _ := pem.Decode(a.PrivateKey)
	if block == nil {
		return "", fmt.Errorf("failed to decode PEM block")
	}

	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("parse private key: %w", err)
	}

	now := time.Now()
	claims := jwt.RegisteredClaims{
		IssuedAt:  jwt.NewNumericDate(now.Add(-60 * time.Second)),
		ExpiresAt: jwt.NewNumericDate(now.Add(10 * time.Minute)),
		Issuer:    fmt.Sprintf("%d", a.AppID),
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	return token.SignedString(key)
}

// InstallationToken retrieves an installation access token, returning a
// cached token if it is still valid (with a 5-minute safety margin).
func (a *App) InstallationToken() (string, error) {
	a.tokenMu.Lock()
	defer a.tokenMu.Unlock()

	// Return cached token if still valid with 5 min buffer
	if a.cachedToken != "" && time.Now().Before(a.tokenExpires.Add(-5*time.Minute)) {
		return a.cachedToken, nil
	}

	jwtToken, err := a.GenerateJWT()
	if err != nil {
		return "", fmt.Errorf("generate JWT: %w", err)
	}

	url := fmt.Sprintf("https://api.github.com/app/installations/%d/access_tokens", a.InstallationID)
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+jwtToken)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("request installation token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("unexpected status %d requesting installation token", resp.StatusCode)
	}

	var result struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := decodeJSON(resp.Body, &result); err != nil {
		return "", err
	}

	a.cachedToken = result.Token
	a.tokenExpires = result.ExpiresAt

	return result.Token, nil
}
