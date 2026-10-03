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
			})})
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
