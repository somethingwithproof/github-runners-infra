package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestInstallationTokenIsRepositoryAndPermissionScoped(t *testing.T) {
	original := HTTPClient
	t.Cleanup(func() { HTTPClient = original })
	HTTPClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var body struct {
			Repositories []string          `json:"repositories"`
			Permissions  map[string]string `json:"permissions"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if strings.Join(body.Repositories, ",") != "repo-a,repo-b" || body.Permissions["administration"] != "write" {
			t.Fatalf("installation token request = %#v", body)
		}
		return &http.Response{
			StatusCode: http.StatusCreated,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{
				"token":"scoped-token","expires_at":"2099-01-01T00:00:00Z",
				"permissions":{"administration":"write"},
				"repositories":[{"full_name":"trusted/repo-b"},{"full_name":"trusted/repo-a"}]
			}`)),
		}, nil
	})}
	app := &App{
		AppID: 1, InstallationID: 2, PrivateKey: testPrivateKey(t),
		AllowedRepositories: []string{"trusted/repo-b", "trusted/repo-a"},
	}
	if err := app.ValidateTokenScope(context.Background()); err != nil {
		t.Fatalf("ValidateTokenScope = %v", err)
	}
	token, err := app.InstallationTokenContext(context.Background())
	if err != nil || token != "scoped-token" {
		t.Fatalf("InstallationTokenContext = %q, %v", token, err)
	}
}

func TestInstallationTokenRejectsExpandedRepositoryScope(t *testing.T) {
	original := HTTPClient
	t.Cleanup(func() { HTTPClient = original })
	HTTPClient = &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusCreated, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{
				"token":"expanded-token","expires_at":"2099-01-01T00:00:00Z",
				"permissions":{"administration":"write"},
				"repositories":[{"full_name":"trusted/repo"},{"full_name":"trusted/other"}]
			}`)),
		}, nil
	})}
	app := &App{AppID: 1, InstallationID: 2, PrivateKey: testPrivateKey(t), AllowedRepositories: []string{"trusted/repo"}}
	if _, err := app.InstallationTokenContext(context.Background()); err == nil {
		t.Fatal("accepted expanded installation token repository scope")
	}
}

func testPrivateKey(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}
