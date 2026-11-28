// SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"maps"
	"net/http"
	"slices"

	"github.com/thomasvincent/github-runners-infra/internal/budget"
	"github.com/thomasvincent/github-runners-infra/internal/state"
)

func newMetricsServer(addr string, store *state.FileStore, tracker *budget.Tracker, limit int, provider string, orphanCleanups func() uint64) *http.Server {
	if addr == "" {
		return nil
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		snap := store.Metrics(provider)
		writeBudgetMetrics(w, tracker)
		_, _ = fmt.Fprintf(w, "# TYPE runner_live gauge\nrunner_live %d\n# TYPE runner_live_limit gauge\nrunner_live_limit %d\n", snap.Live, limit)
		_, _ = fmt.Fprintln(w, "# TYPE runner_records gauge")
		providers := slices.Sorted(maps.Keys(snap.Providers))
		for _, p := range providers {
			for _, status := range []state.Status{state.StatusPending, state.StatusProvisioning, state.StatusProvisioned, state.StatusCompleted, state.StatusDeleting, state.StatusDeleted, state.StatusFailed, state.StatusOrphaned} {
				_, _ = fmt.Fprintf(w, "runner_records{provider=%q,state=%q} %d\n", p, status, snap.Providers[p].Records[status])
			}
		}
		for _, metric := range []struct {
			name  string
			value func(state.ProviderCounters) uint64
		}{
			{"runner_provision_failures_total", func(c state.ProviderCounters) uint64 { return c.ProvisionFailures }},
			{"runner_delete_failures_total", func(c state.ProviderCounters) uint64 { return c.DeleteFailures }},
			{"runner_rate_limit_deferrals_total", func(c state.ProviderCounters) uint64 { return c.RateLimitDeferrals }},
		} {
			_, _ = fmt.Fprintf(w, "# TYPE %s counter\n", metric.name)
			for _, p := range providers {
				_, _ = fmt.Fprintf(w, "%s{provider=%q} %d\n", metric.name, p, metric.value(snap.Providers[p].ProviderCounters))
			}
		}
		_, _ = fmt.Fprintf(w, "# TYPE runner_orphan_cleanups_total counter\nrunner_orphan_cleanups_total %d\n", orphanCleanups())
	})
	return newHTTPServer(addr, mux)
}
