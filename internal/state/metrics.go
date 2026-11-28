// SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
// SPDX-License-Identifier: MIT

package state

import "strings"

// ProviderCounters count persisted lifecycle events since this process started.
type ProviderCounters struct {
	ProvisionFailures, DeleteFailures, RateLimitDeferrals uint64
}

// ProviderMetrics groups job counts and lifecycle events under one provider label.
type ProviderMetrics struct {
	Records map[Status]int
	ProviderCounters
}

// MetricsSnapshot contains aggregate runner state and fleet admission usage.
type MetricsSnapshot struct {
	Providers map[string]*ProviderMetrics
	Live      int
}

func (r Record) countsAsLive() bool {
	if r.Status == StatusDeleted || (r.Status == StatusOrphaned && r.InstanceID == "") {
		return false
	}
	return r.InstanceID != "" || (r.GitHubRunnerOwned && r.GitHubRunnerID != 0) || r.ClaimedWork == WorkProvision || r.Status == StatusProvisioning
}

func providerLabel(provider string) string {
	switch provider {
	case "digitalocean", "aws", "azure", "gcp", "linode", "vultr", "oci", "multi":
		return provider
	}
	return "unknown"
}

// countLifecycleEvents runs under the store lock after a successful mutation.
func (s *FileStore) countLifecycleEvents(before, after Record, failure WorkKind) {
	throttled := after.ThrottleFailures > before.ThrottleFailures
	if failure == "" && !throttled {
		return
	}
	if s.metrics == nil {
		s.metrics = make(map[string]ProviderCounters)
	}
	provider := providerLabel(after.Provider)
	counts := s.metrics[provider]
	if before.Status == StatusOrphaned {
		failure = ""
	}
	switch failure {
	case WorkProvision:
		counts.ProvisionFailures++
	case WorkDelete:
		counts.DeleteFailures++
	}
	if throttled {
		counts.RateLimitDeferrals++
	}
	s.metrics[provider] = counts
}

// Metrics returns a point-in-time copy. Completion markers are deduplication
// entries and do not represent runner jobs.
func (s *FileStore) Metrics(configuredProvider string) MetricsSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := MetricsSnapshot{Providers: make(map[string]*ProviderMetrics)}
	group := func(provider string) *ProviderMetrics {
		label := providerLabel(provider)
		if snap.Providers[label] == nil {
			snap.Providers[label] = &ProviderMetrics{Records: make(map[Status]int), ProviderCounters: s.metrics[label]}
		}
		return snap.Providers[label]
	}
	group(configuredProvider)
	for provider := range s.metrics {
		group(provider)
	}
	for key, record := range s.records {
		if strings.HasPrefix(key, completionMarkerPrefix) {
			continue
		}
		group(record.Provider).Records[record.Status]++
		if record.countsAsLive() {
			snap.Live++
		}
	}
	return snap
}
