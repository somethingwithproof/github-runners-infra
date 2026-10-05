package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRouterPreservesSignedBodyAndRejectsUnknownInstallation(t *testing.T) {
	for _, tc := range []struct {
		body string
		want int
	}{
		{`{"installation":{"id":77},"action":"queued"}`, 202},
		{`{"installation":{"id":88}}`, 403},
		{`{"action":"queued"}`, 403},
		{`not-json`, 400},
		{strings.Repeat("x", 1024*1024+1), 400},
	} {
		t.Run(tc.body[:min(30, len(tc.body))], func(t *testing.T) {
			forwarded := false
			router := installationRouter(map[int64]http.Handler{77: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				forwarded = true
				body, err := io.ReadAll(r.Body)
				if err != nil || string(body) != tc.body || r.Header.Get("X-Hub-Signature-256") != "original-signature" {
					t.Fatal("signed request changed")
				}
				w.WriteHeader(202)
			})}, nil)
			request := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(tc.body))
			request.Header.Set("X-Hub-Signature-256", "original-signature")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != tc.want || forwarded != (tc.want == 202) {
				t.Fatalf("status %d forwarded %v", response.Code, forwarded)
			}
		})
	}
}

func TestRepositoryRoutingDoesNotChangeOtherControllers(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"Mantl", `{"installation":{"id":77},"repository":{"full_name":"somethingwithproof/mantl"}}`, "mantl"},
		{"case insensitive identity", `{"installation":{"id":77},"repository":{"full_name":"SomethingWithProof/Mantl"}}`, "mantl"},
		{"other repository", `{"installation":{"id":77},"repository":{"full_name":"somethingwithproof/other"}}`, "legacy"},
		{"missing repository", `{"installation":{"id":77}}`, "legacy"},
		{"other installation", `{"installation":{"id":88},"repository":{"full_name":"somethingwithproof/mantl"}}`, "kadupul"},
		{"unknown installation", `{"installation":{"id":99},"repository":{"full_name":"somethingwithproof/mantl"}}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var selected string
			upstream := func(name string) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, err := io.ReadAll(r.Body)
					if err != nil || string(body) != tc.body || r.Header.Get("X-Hub-Signature-256") != "original-signature" || r.Header.Get("X-GitHub-Delivery") != "delivery" {
						t.Fatal("original signed delivery changed")
					}
					selected = name
					w.WriteHeader(http.StatusAccepted)
				})
			}
			router := installationRouter(map[int64]http.Handler{77: upstream("legacy"), 88: upstream("kadupul")}, map[repositoryRoute]http.Handler{{77, "somethingwithproof/mantl"}: upstream("mantl")})
			request := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(tc.body))
			request.Header.Set("X-Hub-Signature-256", "original-signature")
			request.Header.Set("X-GitHub-Delivery", "delivery")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if selected != tc.want {
				t.Fatalf("selected %q, want %q", selected, tc.want)
			}
			if tc.want == "" && response.Code != http.StatusForbidden {
				t.Fatalf("unknown installation accepted: %d", response.Code)
			}
		})
	}
}
