// SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
// SPDX-License-Identifier: MIT

package compute

import (
	"context"
	"errors"
	"net/http"
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
