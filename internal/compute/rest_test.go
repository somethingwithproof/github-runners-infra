// SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
// SPDX-License-Identifier: MIT

package compute

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRESTHelpers(t *testing.T) {
	url, err := NormalizeBaseURL("", "https://api.example.com/v2", "test")
	if err != nil || url != "https://api.example.com/v2" {
		t.Fatalf("default NormalizeBaseURL = %q, %v", url, err)
	}
	url, err = NormalizeBaseURL("https://custom.example.com/v2/", "https://api.example.com/v2", "test")
	if err != nil || url != "https://custom.example.com/v2" {
		t.Fatalf("custom NormalizeBaseURL = %q, %v", url, err)
	}
	if _, err := NormalizeBaseURL("ftp://bad.example.com", "https://api.example.com", "test"); err == nil {
		t.Fatal("accepted non-HTTP scheme")
	}

	notFound := &APIError{Provider: "test", StatusCode: http.StatusNotFound, Message: "missing"}
	if !IsHTTPNotFound(notFound) || IsAmbiguousCreateError(notFound) {
		t.Fatalf("404 classification wrong: notFound=%v ambiguous=%v", IsHTTPNotFound(notFound), IsAmbiguousCreateError(notFound))
	}
	if !IsAmbiguousCreateError(&APIError{Provider: "test", StatusCode: http.StatusServiceUnavailable}) {
		t.Fatal("503 should be ambiguous")
	}
	if !IsAmbiguousCreateError(context.DeadlineExceeded) {
		t.Fatal("a timed-out POST may have created the instance")
	}
	if IsAmbiguousCreateError(context.Canceled) {
		t.Fatal("canceled should not be ambiguous")
	}

	if !HasTag([]string{"a", "b"}, "b") || HasTag([]string{"a"}, "c") {
		t.Fatal("HasTag mismatch")
	}
	if JobTag("org/r:1") == JobTag("org/r:2") || DeterministicName("vultr", "org/r:1", 1) == DeterministicName("vultr", "org/r:1", 2) {
		t.Fatal("deterministic tag/name collision")
	}
}

func TestReconcileAmbiguousCreateConfirmedAbsentAndRecovered(t *testing.T) {
	t.Run("confirmed absent", func(t *testing.T) {
		calls := 0
		res, absent, err := ReconcileAmbiguousCreate(context.Background(), "test", "ghr-1", time.Millisecond, func(context.Context) (*string, error) {
			calls++
			return nil, nil
		})
		if err != nil || res != nil || !absent || calls != ConfirmedAbsenceObservations {
			t.Fatalf("absent result = %v %v %v calls=%d", res, absent, err, calls)
		}
	})

	t.Run("recovers after transient error", func(t *testing.T) {
		calls := 0
		found := "inst-1"
		res, absent, err := ReconcileAmbiguousCreate(context.Background(), "test", "ghr-1", time.Millisecond, func(context.Context) (*string, error) {
			calls++
			if calls == 1 {
				return nil, errors.New("temporary")
			}
			return &found, nil
		})
		if err != nil || absent || res == nil || *res != found {
			t.Fatalf("recovered result = %v %v %v", res, absent, err)
		}
	})
}

func TestSweepContinuesAfterResourceError(t *testing.T) {
	failure := errors.New("delete rejected")
	var attempted []string
	deleted, err := SweepOrphans(context.Background(), "test", "owned", nil, time.Now(), []string{"first", "second"},
		func(id string) string { return id }, func(string) []string { return []string{"owned"} },
		func(string) (time.Time, error) { return time.Time{}, nil }, func(_ context.Context, id string) error {
			attempted = append(attempted, id)
			if id == "first" {
				return failure
			}
			return nil
		})
	if !errors.Is(err, failure) || deleted != 1 || len(attempted) != 2 || attempted[1] != "second" {
		t.Fatalf("deleted=%d, attempts=%v, err=%v", deleted, attempted, err)
	}
}

func TestProviderBaseURLRequiresCleanHTTPS(t *testing.T) {
	for _, url := range []string{"http://api.example.com", "https://user@example.com", "https://api.example.com/v2?", "https://api.example.com/v2?view=page", "https://api.example.com/v2#", "https://api.example.com/v4#fragment"} {
		if _, err := NormalizeBaseURL(url, "", "test"); err == nil {
			t.Errorf("accepted %q", url)
		}
	}
}

func TestDoJSONRequiresExpectedBodyAndPreservesErrorStatus(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/missing":
			w.Header().Set("Content-Length", "20")
			w.WriteHeader(http.StatusNotFound)
		case "/list":
			_, _ = w.Write([]byte("[]"))
		}
	}))
	defer server.Close()
	var list []string
	request := func(path string, out any) error {
		return DoJSON(context.Background(), RESTRequest{Provider: "test", Client: server.Client(), URL: server.URL + path, Method: http.MethodGet, Out: out})
	}
	if err := request("/missing", &list); !IsHTTPNotFound(err) {
		t.Fatalf("truncated 404 = %v", err)
	}
	if err := request("/empty", &list); err == nil {
		t.Fatal("accepted empty inventory response")
	}
	if err := request("/empty", nil); err != nil {
		t.Fatal(err)
	}
	if err := request("/list", &list); err != nil || list == nil {
		t.Fatalf("empty JSON list = %v %v", list, err)
	}
}
