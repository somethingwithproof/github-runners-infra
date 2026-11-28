// SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
// SPDX-License-Identifier: MIT

package router

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/thomasvincent/github-runners-infra/internal/compute"
)

// Strategy controls how MultiCloudRouter orders candidate providers for new jobs.
type Strategy string

const (
	// StrategyPriority tries providers in their configured list order.
	StrategyPriority Strategy = "priority"
	// StrategyCheapest sorts providers ascending by estimated USD/hour rate,
	// breaking ties by configured priority order.
	StrategyCheapest Strategy = "cheapest"
)

// ErrAllProvidersExhausted indicates every candidate provider failed to create a runner.
var ErrAllProvidersExhausted = errors.New("all configured compute providers failed")

// Backend binds a named provider adapter with its default instance size for
// cost estimation.
type Backend struct {
	Name        string
	DefaultSize string
	Provider    compute.Provider
}

// Config configures a MultiCloudRouter.
type Config struct {
	Strategy    Strategy
	Backends    []Backend
	HourlyRates map[string]float64
}

type absenceChecker interface {
	RunnerAbsent(context.Context, string) (bool, error)
}

type cleanupExcepter interface {
	CleanupRunnerExcept(context.Context, string, string) error
}

var _ compute.Provider = (*MultiCloudRouter)(nil)

// MultiCloudRouter implements compute.Provider by routing jobs across multiple
// cloud backends with cost-aware ordering, automatic failover, and per-provider
// resource ownership tracking via composite "<provider>:<id>" instance IDs.
type MultiCloudRouter struct {
	strategy    Strategy
	backends    []Backend
	byName      map[string]Backend
	hourlyRates map[string]float64
}

// New constructs a validated MultiCloudRouter.
func New(cfg Config) (*MultiCloudRouter, error) {
	if len(cfg.Backends) == 0 {
		return nil, fmt.Errorf("multi-cloud router requires at least one backend")
	}
	strategy := cfg.Strategy
	if strategy == "" {
		strategy = StrategyPriority
	}
	if strategy != StrategyPriority && strategy != StrategyCheapest {
		return nil, fmt.Errorf("unsupported router strategy %q (expected priority or cheapest)", strategy)
	}

	backends := make([]Backend, 0, len(cfg.Backends))
	byName := make(map[string]Backend, len(cfg.Backends))
	for _, b := range cfg.Backends {
		if b.Provider == nil {
			return nil, fmt.Errorf("multi-cloud backend %q has nil provider", b.Name)
		}
		name := strings.ToLower(strings.TrimSpace(b.Name))
		if name == "" {
			name = strings.ToLower(strings.TrimSpace(b.Provider.Provider()))
		}
		if name == "" || strings.Contains(name, ":") {
			return nil, fmt.Errorf("invalid multi-cloud backend name %q", b.Name)
		}
		if _, dup := byName[name]; dup {
			return nil, fmt.Errorf("duplicate multi-cloud backend %q", name)
		}
		entry := Backend{
			Name:        name,
			DefaultSize: strings.TrimSpace(b.DefaultSize),
			Provider:    b.Provider,
		}
		backends = append(backends, entry)
		byName[name] = entry
	}

	rates := make(map[string]float64, len(cfg.HourlyRates))
	for k, v := range cfg.HourlyRates {
		rates[k] = v
	}

	return &MultiCloudRouter{
		strategy:    strategy,
		backends:    backends,
		byName:      byName,
		hourlyRates: rates,
	}, nil
}

// Provider returns the router's provider identifier for durable state records.
func (r *MultiCloudRouter) Provider() string { return "multi" }

// EstimateHourlyRate returns the estimated USD/hour rate of the highest-ranked
// candidate backend for the given optional size override.
func (r *MultiCloudRouter) EstimateHourlyRate(sizeOverride string) (providerName, size string, hourlyUSD float64) {
	ordered := r.OrderedBackends(sizeOverride)
	top := ordered[0]
	effectiveSize := sizeOverride
	if effectiveSize == "" {
		effectiveSize = top.DefaultSize
	}
	return top.Name, effectiveSize, HourlyRate(top.Name, effectiveSize, r.hourlyRates)
}

// OrderedBackends returns backends ordered by the configured routing strategy.
func (r *MultiCloudRouter) OrderedBackends(sizeOverride string) []Backend {
	ordered := make([]Backend, len(r.backends))
	copy(ordered, r.backends)
	if r.strategy != StrategyCheapest {
		return ordered
	}
	rateOf := func(b Backend) float64 {
		size := sizeOverride
		if size == "" {
			size = b.DefaultSize
		}
		return HourlyRate(b.Name, size, r.hourlyRates)
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		return rateOf(ordered[i]) < rateOf(ordered[j])
	})
	return ordered
}

// FindRunner searches all configured backends for an instance owning jobKey.
func (r *MultiCloudRouter) FindRunner(ctx context.Context, jobKey string) (*compute.RunnerInstance, bool, error) {
	var foundInstances []*compute.RunnerInstance
	for _, b := range r.backends {
		inst, found, err := b.Provider.FindRunner(ctx, jobKey)
		if err != nil {
			return nil, false, fmt.Errorf("find runner on %s: %w", b.Name, err)
		}
		if found && inst != nil {
			foundInstances = append(foundInstances, &compute.RunnerInstance{
				ID:   EncodeCompositeID(b.Name, inst.ID),
				Name: inst.Name,
			})
		}
	}
	if len(foundInstances) == 0 {
		return nil, false, nil
	}
	if len(foundInstances) > 1 {
		return nil, false, fmt.Errorf("%w: runner for job %s exists on %d providers", compute.ErrDuplicateInstances, jobKey, len(foundInstances))
	}
	return foundInstances[0], true, nil
}

// CreateRunner provisions a runner on the highest-ranked backend, failing over
// to subsequent backends on capacity, quota, or transient provider errors.
func (r *MultiCloudRouter) CreateRunner(ctx context.Context, params compute.RunnerParams) (*compute.RunnerInstance, error) {
	if existing, found, err := r.FindRunner(ctx, params.JobKey); err != nil || found {
		return existing, err
	}

	candidates := r.OrderedBackends(params.InstanceSize)
	var attemptErrs []error
	for i, b := range candidates {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if params.AuthorizeLaunch != nil {
			size := params.InstanceSize
			if size == "" {
				size = b.DefaultSize
			}
			if err := params.AuthorizeLaunch(b.Name, size, false); err != nil {
				return nil, err
			}
		}
		inst, err := b.Provider.CreateRunner(ctx, params)
		if err == nil {
			return &compute.RunnerInstance{
				ID:   EncodeCompositeID(b.Name, inst.ID),
				Name: inst.Name,
			}, nil
		}
		// Never fail over on cancellation, ownership mismatch, or ambiguous
		// create outcomes where retrying on another cloud risks duplicate VMs.
		if !shouldFailover(ctx, err) {
			return nil, fmt.Errorf("provider %s create failed (no failover): %w", b.Name, err)
		}
		attemptErrs = append(attemptErrs, fmt.Errorf("%s: %w", b.Name, err))
		if i+1 < len(candidates) {
			log.Printf("WARN: provider %s failed for %s (%v); failing over to %s", b.Name, params.JobKey, err, candidates[i+1].Name)
		}
	}
	return nil, fmt.Errorf("%w: %w", ErrAllProvidersExhausted, errors.Join(attemptErrs...))
}

// DeleteRunner routes deletion to the backend encoded in compositeID.
func (r *MultiCloudRouter) DeleteRunner(ctx context.Context, compositeID, jobKey string) error {
	backend, rawID, err := r.resolveBackend(compositeID)
	if err != nil {
		return err
	}
	return backend.Provider.DeleteRunner(ctx, rawID, jobKey)
}

// RunnerAbsent confirms instance absence on the owning backend when supported.
func (r *MultiCloudRouter) RunnerAbsent(ctx context.Context, compositeID string) (bool, error) {
	backend, rawID, err := r.resolveBackend(compositeID)
	if err != nil {
		return false, err
	}
	checker, ok := backend.Provider.(absenceChecker)
	if !ok {
		return true, nil
	}
	return checker.RunnerAbsent(ctx, rawID)
}

// CleanupRunner removes any controller-owned instances for jobKey across all backends.
func (r *MultiCloudRouter) CleanupRunner(ctx context.Context, jobKey string) error {
	return r.CleanupRunnerExcept(ctx, jobKey, "")
}

// CleanupRunnerExcept removes job-tagged instances across all backends,
// excluding the confirmed-absent compositeID on its owning backend.
func (r *MultiCloudRouter) CleanupRunnerExcept(ctx context.Context, jobKey, excludedCompositeID string) error {
	excludedProvider, excludedRawID, hasExcluded := ParseCompositeID(excludedCompositeID)
	var errs []error
	for _, b := range r.backends {
		if hasExcluded && b.Name == excludedProvider {
			if cleaner, ok := b.Provider.(cleanupExcepter); ok {
				if err := cleaner.CleanupRunnerExcept(ctx, jobKey, excludedRawID); err != nil {
					errs = append(errs, fmt.Errorf("cleanup %s: %w", b.Name, err))
				}
				continue
			}
		}
		if err := b.Provider.CleanupRunner(ctx, jobKey); err != nil {
			errs = append(errs, fmt.Errorf("cleanup %s: %w", b.Name, err))
		}
	}
	return errors.Join(errs...)
}

// SweepOrphanedRunners partitions known composite IDs per provider and sweeps
// every configured backend.
func (r *MultiCloudRouter) SweepOrphanedRunners(ctx context.Context, known map[string]struct{}, cutoff time.Time) (int, error) {
	knownByProvider := make(map[string]map[string]struct{}, len(r.backends))
	for compositeID := range known {
		providerName, rawID, ok := ParseCompositeID(compositeID)
		if !ok {
			continue
		}
		set := knownByProvider[providerName]
		if set == nil {
			set = make(map[string]struct{})
			knownByProvider[providerName] = set
		}
		set[rawID] = struct{}{}
	}

	totalDeleted := 0
	var errs []error
	for _, b := range r.backends {
		deleted, err := b.Provider.SweepOrphanedRunners(ctx, knownByProvider[b.Name], cutoff)
		totalDeleted += deleted
		if err != nil {
			errs = append(errs, fmt.Errorf("sweep %s: %w", b.Name, err))
		}
	}
	return totalDeleted, errors.Join(errs...)
}

func (r *MultiCloudRouter) resolveBackend(compositeID string) (Backend, string, error) {
	providerName, rawID, ok := ParseCompositeID(compositeID)
	if !ok {
		return Backend{}, "", fmt.Errorf("%w: invalid multi-cloud instance ID %q", compute.ErrOwnershipMismatch, compositeID)
	}
	backend, found := r.byName[providerName]
	if !found {
		return Backend{}, "", fmt.Errorf("%w: instance %q belongs to unconfigured provider %q", compute.ErrOwnershipMismatch, compositeID, providerName)
	}
	return backend, rawID, nil
}

// EncodeCompositeID prefixes a provider-native instance ID with its backend name.
func EncodeCompositeID(providerName, rawID string) string {
	return strings.ToLower(strings.TrimSpace(providerName)) + ":" + rawID
}

// ParseCompositeID splits a "<provider>:<rawID>" composite identifier.
func ParseCompositeID(compositeID string) (providerName, rawID string, ok bool) {
	p, id, found := strings.Cut(compositeID, ":")
	if !found || p == "" || id == "" {
		return "", "", false
	}
	return strings.ToLower(p), id, true
}

func shouldFailover(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, compute.ErrCreateOutcomeUnknown) ||
		errors.Is(err, compute.ErrOwnershipMismatch) ||
		errors.Is(err, compute.ErrDuplicateInstances) {
		return false
	}
	return true
}
