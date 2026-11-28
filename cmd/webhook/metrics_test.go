// SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thomasvincent/github-runners-infra/internal/state"
)

func TestMetricsListenerIsOptIn(t *testing.T) {
	setValidRuntimeEnvironment(t)
	cfg, err := loadRuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	if newMetricsServer(cfg.metricsAddr, nil, nil, 20, "aws", nil) != nil {
		t.Fatal("unexpected metrics listener")
	}
	t.Setenv("METRICS_ADDR", "127.0.0.1:9090")
	cfg, err = loadRuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	srv := newMetricsServer(cfg.metricsAddr, nil, nil, 20, "aws", nil)
	if srv == nil || srv.Addr != "127.0.0.1:9090" {
		t.Fatal("missing configured metrics server")
	}
	w := httptest.NewRecorder()
	srv.Handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/webhook", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("metrics listener served webhook: %d", w.Code)
	}
}

func TestScrapeRunnerLifecycle(t *testing.T) {
	store, err := state.OpenFileStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	srv := newMetricsServer("localhost:0", store, nil, 3, "aws", func() uint64 { return 0 })
	ctx := t.Context()
	const key = "private-owner/private-project:42"
	check := func(want ...string) {
		t.Helper()
		w := httptest.NewRecorder()
		srv.Handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		text := w.Body.String()
		if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain; version=0.0.4") {
			t.Fatalf("scrape: %d %s", w.Code, w.Header())
		}
		if strings.Contains(text, "private-") {
			t.Fatal("scrape exposed job information")
		}
		for _, sample := range want {
			if !strings.Contains(text, "\n"+sample+"\n") {
				t.Errorf("missing %q in scrape:\n%s", sample, text)
			}
		}
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	claim := func() {
		t.Helper()
		rec, _, err := store.ClaimNext(ctx, time.Now(), 5)
		must(err)
		if rec == nil {
			t.Fatal("no claim")
		}
	}
	_, err = store.Create(ctx, state.Record{Key: key, Owner: "private-owner", Repository: "private-project", Provider: "aws"})
	must(err)
	check("runner_live 0", "runner_live_limit 3", `runner_records{provider="aws",state="pending"} 1`, "budget_blocked_total 0")
	claim()
	must(store.DeferRateLimitedWork(ctx, key, state.WorkProvision, "private-error", time.Now(), 5))
	claim()
	must(store.MarkProvisionFailed(ctx, key, "private-error", time.Now(), 5))
	check(`runner_rate_limit_deferrals_total{provider="aws"} 1`, `runner_provision_failures_total{provider="aws"} 1`)
	claim()
	must(store.MarkProvisioned(ctx, key, "private-instance", 42, "private-runner"))
	check("runner_live 1", `runner_records{provider="aws",state="provisioned"} 1`)
	must(store.MarkCompleted(ctx, key))
	claim()
	must(store.MarkDeleteFailed(ctx, key, "private-error", time.Now(), 5))
	claim()
	must(store.MarkDeleted(ctx, key))
	must(store.MarkDeleteFailed(ctx, key, "late", time.Now(), 5))
	check("runner_live 0", `runner_records{provider="aws",state="deleted"} 1`, `runner_delete_failures_total{provider="aws"} 1`)
}

func TestMetricsBindFailureShutsDownController(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := occupied.Close(); err != nil {
			t.Error(err)
		}
	})
	metrics := newMetricsServer(occupied.Addr().String(), nil, nil, 20, "aws", nil)
	srv := newHTTPServer("127.0.0.1:0", http.NewServeMux())
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	var worker context.Context
	err = serveUntilShutdown(ctx, srv, func(ctx context.Context) { worker = ctx }, func() {}, srv.ListenAndServe, metrics)
	if err == nil || worker.Err() == nil || ctx.Err() != nil {
		t.Fatalf("metrics bind failure: err=%v, worker=%v, timeout=%v", err, worker.Err(), ctx.Err())
	}
}
