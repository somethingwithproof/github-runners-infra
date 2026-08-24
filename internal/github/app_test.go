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

func TestInstallationTokenPreservesRateLimitReset(t *testing.T) {
	for _, test := range []struct {
		name       string
		statusCode int
		header     http.Header
	}{
		{name: "429 retry-after", statusCode: http.StatusTooManyRequests, header: http.Header{"Retry-After": []string{"120"}}},
		{name: "429 retry-after zero", statusCode: http.StatusTooManyRequests, header: http.Header{"Retry-After": []string{"0"}}},
		{name: "403 exhausted quota", statusCode: http.StatusForbidden, header: http.Header{
			"X-Ratelimit-Remaining": []string{"0"},
			"X-Ratelimit-Reset":     []string{"4102444800"},
		}},
		{name: "403 stale reset", statusCode: http.StatusForbidden, header: http.Header{
			"X-Ratelimit-Remaining": []string{"0"},
			"X-Ratelimit-Reset":     []string{"1"},
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			original := HTTPClient
			t.Cleanup(func() { HTTPClient = original })
			HTTPClient = &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: test.statusCode,
					Header:     test.header,
					Body:       io.NopCloser(strings.NewReader(`{}`)),
				}, nil
			})}
			app := &App{AppID: 1, InstallationID: 2, PrivateKey: testPrivateKey(t), AllowedRepositories: []string{"trusted/repo"}}
			_, err := app.InstallationTokenContext(context.Background())
			if _, rateLimited := RateLimitReset(err); !rateLimited {
				t.Fatalf("installation-token throttle was collapsed: %v", err)
			}
		})
	}
}

func TestValidateTokenScopeFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name       string
		statusCode int
		body       string
	}{
		{name: "non-2xx", statusCode: http.StatusForbidden, body: `{}`},
		{name: "missing administration write", statusCode: http.StatusCreated, body: `{
			"token":"scoped-token","expires_at":"2099-01-01T00:00:00Z",
			"permissions":{"administration":"read"},
			"repositories":[{"full_name":"trusted/repo"}]
		}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			original := HTTPClient
			t.Cleanup(func() { HTTPClient = original })
			HTTPClient = &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: test.statusCode, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(test.body)),
				}, nil
			})}
			app := &App{AppID: 1, InstallationID: 2, PrivateKey: testPrivateKey(t), AllowedRepositories: []string{"trusted/repo"}}
			if err := app.ValidateTokenScope(context.Background()); err == nil {
				t.Fatal("ValidateTokenScope accepted an unsafe token response")
			}
		})
	}
}

func TestTokenScopeTrimsRepositoryComponents(t *testing.T) {
	app := &App{AllowedRepositories: []string{" trusted / repo "}}
	names, fullNames, scope, err := app.tokenScope()
	if err != nil || len(names) != 1 || names[0] != "repo" || len(fullNames) != 1 ||
		fullNames[0] != "trusted/repo" || scope != "trusted/repo|administration:write" {
		t.Fatalf("trimmed token scope = names %v, full names %v, scope %q, error %v", names, fullNames, scope, err)
	}
}

func TestGenerateJWTReportsFinalPKCS8ParseError(t *testing.T) {
	app := &App{PrivateKey: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("not-a-key")})}
	if _, err := app.GenerateJWT(); err == nil || !strings.Contains(err.Error(), "PKCS#8") {
		t.Fatalf("GenerateJWT error = %v", err)
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
