// SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
// SPDX-License-Identifier: MIT

package router

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thomasvincent/github-runners-infra/internal/compute"
)

type stubProvider struct {
	name          string
	mu            sync.Mutex
	instances     map[string]*compute.RunnerInstance
	createErr     error
	createCalls   int
	deletedIDs    []string
	cleanedJobs   []string
	sweptKnown    map[string]struct{}
	sweepCount    int
	sweepErr      error
	absentByID    map[string]bool
	excludedClean string
}

func newStubProvider(name string) *stubProvider {
	return &stubProvider{
		name:       name,
		instances:  make(map[string]*compute.RunnerInstance),
		absentByID: make(map[string]bool),
	}
}

func (s *stubProvider) Provider() string { return s.name }

func (s *stubProvider) FindRunner(_ context.Context, jobKey string) (*compute.RunnerInstance, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst, ok := s.instances[jobKey]
	if !ok {
		return nil, false, nil
	}
	cp := *inst
	return &cp, true, nil
}

func (s *stubProvider) CreateRunner(_ context.Context, params compute.RunnerParams) (*compute.RunnerInstance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.createCalls++
	if s.createErr != nil {
		return nil, s.createErr
	}
	inst := &compute.RunnerInstance{
		ID:   fmt.Sprintf("%s-%d", s.name, s.createCalls),
		Name: params.RunnerName,
	}
	s.instances[params.JobKey] = inst
	cp := *inst
	return &cp, nil
}

func (s *stubProvider) DeleteRunner(_ context.Context, id, jobKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deletedIDs = append(s.deletedIDs, id)
	delete(s.instances, jobKey)
	s.absentByID[id] = true
	return nil
}

func (s *stubProvider) RunnerAbsent(_ context.Context, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.absentByID[id], nil
}

func (s *stubProvider) CleanupRunner(_ context.Context, jobKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanedJobs = append(s.cleanedJobs, jobKey)
	delete(s.instances, jobKey)
	return nil
}

func (s *stubProvider) CleanupRunnerExcept(_ context.Context, jobKey, excludedID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.excludedClean = excludedID
	s.cleanedJobs = append(s.cleanedJobs, jobKey)
	return nil
}

func (s *stubProvider) SweepOrphanedRunners(_ context.Context, known map[string]struct{}, _ time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweptKnown = make(map[string]struct{}, len(known))
	for k := range known {
		s.sweptKnown[k] = struct{}{}
	}
	return s.sweepCount, s.sweepErr
}

func TestCheapestSelectionAndOverrides(t *testing.T) {
	awsProv := newStubProvider("aws")
	vultrProv := newStubProvider("vultr")
	linodeProv := newStubProvider("linode")

	r, err := New(Config{
		Strategy: StrategyCheapest,
		Backends: []Backend{
			{Name: "aws", DefaultSize: "m7i.xlarge", Provider: awsProv},
			{Name: "linode", DefaultSize: "g6-standard-4", Provider: linodeProv},
			{Name: "vultr", DefaultSize: "vc2-4c-8gb", Provider: vultrProv},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	inst, err := r.CreateRunner(context.Background(), compute.RunnerParams{JobKey: "org/repo:1", RunnerName: "r1"})
	if err != nil {
		t.Fatalf("CreateRunner error = %v", err)
	}
	if !strings.HasPrefix(inst.ID, "vultr:") || vultrProv.createCalls != 1 || awsProv.createCalls != 0 {
		t.Fatalf("expected cheapest provider vultr, got %#v (vultr=%d aws=%d)", inst, vultrProv.createCalls, awsProv.createCalls)
	}

	// Custom hourly rate override flips cheapest to aws.
	overrides, err := ParseHourlyRateOverrides("aws:m7i.xlarge=0.02, vultr:vc2-4c-8gb=0.09")
	if err != nil {
		t.Fatal(err)
	}
	r2, err := New(Config{
		Strategy:    StrategyCheapest,
		HourlyRates: overrides,
		Backends: []Backend{
			{Name: "vultr", DefaultSize: "vc2-4c-8gb", Provider: newStubProvider("vultr")},
			{Name: "aws", DefaultSize: "m7i.xlarge", Provider: newStubProvider("aws")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	providerName, _, rate := r2.EstimateHourlyRate("")
	if providerName != "aws" || rate != 0.02 {
		t.Fatalf("EstimateHourlyRate = %q %v, want aws 0.02", providerName, rate)
	}
}

func TestFailoverOnQuotaAndCapacityErrors(t *testing.T) {
	vultrProv := newStubProvider("vultr")
	vultrProv.createErr = errors.New("vultr API status 503: insufficient capacity")
	linodeProv := newStubProvider("linode")
	linodeProv.createErr = errors.New("linode API status 429: quota exceeded")
	doProv := newStubProvider("digitalocean")

	r, err := New(Config{
		Strategy: StrategyPriority,
		Backends: []Backend{
			{Name: "vultr", DefaultSize: "vc2-4c-8gb", Provider: vultrProv},
			{Name: "linode", DefaultSize: "g6-standard-4", Provider: linodeProv},
			{Name: "digitalocean", DefaultSize: "s-4vcpu-8gb-amd", Provider: doProv},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	inst, err := r.CreateRunner(context.Background(), compute.RunnerParams{JobKey: "org/repo:2", RunnerName: "r2"})
	if err != nil {
		t.Fatalf("failover CreateRunner error = %v", err)
	}
	if inst.ID != "digitalocean:digitalocean-1" {
		t.Fatalf("expected digitalocean failover instance, got %q", inst.ID)
	}
	if vultrProv.createCalls != 1 || linodeProv.createCalls != 1 || doProv.createCalls != 1 {
		t.Fatalf("calls vultr=%d linode=%d do=%d", vultrProv.createCalls, linodeProv.createCalls, doProv.createCalls)
	}
}

func TestExhaustionAndNoFailoverOnAmbiguousOutcome(t *testing.T) {
	t.Run("all exhausted", func(t *testing.T) {
		p1 := newStubProvider("vultr")
		p1.createErr = errors.New("quota exceeded")
		p2 := newStubProvider("linode")
		p2.createErr = errors.New("region full")
		r, err := New(Config{
			Backends: []Backend{{Name: "vultr", Provider: p1}, {Name: "linode", Provider: p2}},
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = r.CreateRunner(context.Background(), compute.RunnerParams{JobKey: "org/repo:3"})
		if !errors.Is(err, ErrAllProvidersExhausted) || !strings.Contains(err.Error(), "quota exceeded") || !strings.Contains(err.Error(), "region full") {
			t.Fatalf("exhaustion error = %v", err)
		}
	})

	t.Run("no failover on ambiguous create outcome", func(t *testing.T) {
		p1 := newStubProvider("vultr")
		p1.createErr = compute.ErrCreateOutcomeUnknown
		p2 := newStubProvider("linode")
		r, err := New(Config{
			Backends: []Backend{{Name: "vultr", Provider: p1}, {Name: "linode", Provider: p2}},
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = r.CreateRunner(context.Background(), compute.RunnerParams{JobKey: "org/repo:4"})
		if !errors.Is(err, compute.ErrCreateOutcomeUnknown) || p2.createCalls != 0 {
			t.Fatalf("ambiguous outcome error=%v p2 calls=%d", err, p2.createCalls)
		}
	})
}

func TestCrossProviderDeleteCleanupAndSweep(t *testing.T) {
	vultrProv := newStubProvider("vultr")
	vultrProv.sweepCount = 2
	linodeProv := newStubProvider("linode")
	linodeProv.sweepCount = 1

	r, err := New(Config{
		Backends: []Backend{
			{Name: "vultr", Provider: vultrProv},
			{Name: "linode", Provider: linodeProv},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	inst, err := r.CreateRunner(context.Background(), compute.RunnerParams{JobKey: "org/repo:10", RunnerName: "r10"})
	if err != nil {
		t.Fatal(err)
	}
	found, ok, err := r.FindRunner(context.Background(), "org/repo:10")
	if err != nil || !ok || found.ID != inst.ID {
		t.Fatalf("FindRunner = %#v, %v, %v", found, ok, err)
	}

	if err := r.DeleteRunner(context.Background(), inst.ID, "org/repo:10"); err != nil {
		t.Fatalf("DeleteRunner error = %v", err)
	}
	if len(vultrProv.deletedIDs) != 1 || vultrProv.deletedIDs[0] != "vultr-1" || len(linodeProv.deletedIDs) != 0 {
		t.Fatalf("deleted vultr=%v linode=%v", vultrProv.deletedIDs, linodeProv.deletedIDs)
	}
	absent, err := r.RunnerAbsent(context.Background(), inst.ID)
	if err != nil || !absent {
		t.Fatalf("RunnerAbsent = %v, %v", absent, err)
	}

	if err := r.CleanupRunnerExcept(context.Background(), "org/repo:10", inst.ID); err != nil {
		t.Fatalf("CleanupRunnerExcept error = %v", err)
	}
	if vultrProv.excludedClean != "vultr-1" {
		t.Fatalf("excludedClean = %q, want vultr-1", vultrProv.excludedClean)
	}

	known := map[string]struct{}{
		"vultr:v-keep":  {},
		"linode:l-keep": {},
	}
	deleted, err := r.SweepOrphanedRunners(context.Background(), known, time.Now().Add(-time.Hour))
	if err != nil || deleted != 3 {
		t.Fatalf("SweepOrphanedRunners = %d, %v", deleted, err)
	}
	if _, ok := vultrProv.sweptKnown["v-keep"]; !ok || len(vultrProv.sweptKnown) != 1 {
		t.Fatalf("vultr sweptKnown = %#v", vultrProv.sweptKnown)
	}
	if _, ok := linodeProv.sweptKnown["l-keep"]; !ok || len(linodeProv.sweptKnown) != 1 {
		t.Fatalf("linode sweptKnown = %#v", linodeProv.sweptKnown)
	}

	// Reject malformed or unknown provider composite ID.
	if err := r.DeleteRunner(context.Background(), "no-prefix", "org/repo:10"); !errors.Is(err, compute.ErrOwnershipMismatch) {
		t.Fatalf("malformed ID error = %v", err)
	}
	if err := r.DeleteRunner(context.Background(), "aws:i-123", "org/repo:10"); !errors.Is(err, compute.ErrOwnershipMismatch) {
		t.Fatalf("unconfigured provider ID error = %v", err)
	}
}

func TestParseHourlyRateOverridesValidation(t *testing.T) {
	for _, bad := range []string{
		"vultr",
		"vultr:vc2-4c-8gb=-1",
		"vultr:vc2-4c-8gb=0",
		"vultr:vc2-4c-8gb=NaN",
		"vultr:vc2-4c-8gb=Inf",
		"vultr:bad/size=0.05",
		",vultr=0.05",
	} {
		if _, err := ParseHourlyRateOverrides(bad); err == nil {
			t.Fatalf("ParseHourlyRateOverrides(%q) unexpectedly succeeded", bad)
		}
	}
}
