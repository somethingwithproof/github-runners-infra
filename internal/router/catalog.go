// SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
// SPDX-License-Identifier: MIT

// Package router provides multi-cloud failover and cost-aware routing across
// multiple compute.Provider backends.
package router

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/thomasvincent/github-runners-infra/internal/compute"
)

// DefaultHourlyRates contains representative on-demand USD/hour reference rates
// keyed by "<provider>:<size>". Callers may override or extend these via
// ROUTER_HOURLY_RATES.
var DefaultHourlyRates = map[string]float64{
	// Vultr Cloud Compute
	"vultr:vc2-2c-4gb":  0.030,
	"vultr:vc2-4c-8gb":  0.071,
	"vultr:vhf-4c-8gb":  0.077,
	"vultr:vhf-8c-16gb": 0.154,
	"vultr:vc2-8c-16gb": 0.143,
	// Linode / Akamai Cloud
	"linode:g6-standard-2":  0.036,
	"linode:g6-standard-4":  0.072,
	"linode:g6-standard-6":  0.144,
	"linode:g6-dedicated-4": 0.108,
	"linode:g6-dedicated-8": 0.216,
	// DigitalOcean
	"digitalocean:s-2vcpu-4gb-amd":  0.042,
	"digitalocean:s-4vcpu-8gb-amd":  0.083,
	"digitalocean:s-8vcpu-16gb-amd": 0.167,
	"digitalocean:c-4":              0.125,
	"digitalocean:c-8":              0.250,
	"digitalocean:c-16":             0.500,
	// AWS EC2 (US regions on-demand reference)
	"aws:m7i.large":   0.1008,
	"aws:m7i.xlarge":  0.2016,
	"aws:m7i.2xlarge": 0.4032,
	"aws:c7i.xlarge":  0.1785,
	"aws:c7i.2xlarge": 0.3570,
	"aws:c7i.4xlarge": 0.7140,
	// Google Compute Engine (us-central1 reference)
	"gcp:e2-standard-4": 0.1340,
	"gcp:n2-standard-4": 0.1942,
	"gcp:n2-standard-8": 0.3885,
	"gcp:c3-standard-4": 0.2088,
	"gcp:c3-standard-8": 0.4176,
	// Azure VMs (West US 2 reference)
	"azure:Standard_D2s_v5": 0.0960,
	"azure:Standard_D4s_v5": 0.1920,
	"azure:Standard_D8s_v5": 0.3840,
	"azure:Standard_F4s_v2": 0.1690,
	"azure:Standard_F8s_v2": 0.3380,
}

var defaultProviderFallbackRates = map[string]float64{
	"vultr":        0.075,
	"linode":       0.080,
	"digitalocean": 0.090,
	"azure":        0.195,
	"gcp":          0.200,
	"aws":          0.205,
}

// DefaultSpotDiscountFactors maps spot-capable providers to representative
// spot-to-on-demand cost ratios used when estimating hourly spend and ranking
// cheapest backends.
var DefaultSpotDiscountFactors = map[string]float64{
	"aws":   0.35,
	"gcp":   0.30,
	"azure": 0.35,
}

const unknownProviderFallbackRate = 0.250

// ProviderSupportsSpot reports whether provider offers spot/preemptible VMs.
func ProviderSupportsSpot(provider string) bool {
	_, ok := DefaultSpotDiscountFactors[strings.ToLower(strings.TrimSpace(provider))]
	return ok
}

// SpotDiscountFactor returns the spot-to-on-demand price multiplier for
// provider (1.0 when provider does not offer spot instances).
func SpotDiscountFactor(provider string) float64 {
	if factor, ok := DefaultSpotDiscountFactors[strings.ToLower(strings.TrimSpace(provider))]; ok && factor > 0 {
		return factor
	}
	return 1.0
}

// HourlyRateForMode returns the USD/hour estimate for (provider, size) in
// either on-demand or spot mode. Explicit ":spot" overrides take precedence
// when spot is true; otherwise the on-demand rate is multiplied by
// SpotDiscountFactor(provider).
func HourlyRateForMode(provider, size string, spot bool, overrides map[string]float64) float64 {
	if !spot {
		return HourlyRate(provider, size, overrides)
	}
	p := strings.ToLower(strings.TrimSpace(provider))
	s := strings.TrimSpace(size)
	if overrides != nil {
		if rate, ok := overrides[p+":"+s+":spot"]; ok {
			return rate
		}
		if rate, ok := overrides[p+":"+strings.ToLower(s)+":spot"]; ok {
			return rate
		}
		if rate, ok := overrides[p+":spot"]; ok {
			return rate
		}
	}
	return HourlyRate(provider, size, overrides) * SpotDiscountFactor(p)
}

// HourlyRate returns the USD/hour estimate for (provider, size), honoring
// explicit overrides first, then DefaultHourlyRates, then provider defaults.
func HourlyRate(provider, size string, overrides map[string]float64) float64 {
	p := strings.ToLower(strings.TrimSpace(provider))
	s := strings.TrimSpace(size)
	exactKey := p + ":" + s
	lowerKey := p + ":" + strings.ToLower(s)

	if overrides != nil {
		if rate, ok := overrides[exactKey]; ok {
			return rate
		}
		if rate, ok := overrides[lowerKey]; ok {
			return rate
		}
		if rate, ok := overrides[p]; ok {
			return rate
		}
	}
	if rate, ok := DefaultHourlyRates[exactKey]; ok {
		return rate
	}
	if rate, ok := DefaultHourlyRates[lowerKey]; ok {
		return rate
	}
	if rate, ok := defaultProviderFallbackRates[p]; ok {
		return rate
	}
	return unknownProviderFallbackRate
}

// ParseHourlyRateOverrides parses comma-separated "provider:size=rate" or
// "provider=rate" entries into a validated map of positive USD/hour rates.
func ParseHourlyRateOverrides(raw string) (map[string]float64, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, nil
	}
	entries := strings.Split(trimmed, ",")
	result := make(map[string]float64, len(entries))
	for _, entry := range entries {
		part := strings.TrimSpace(entry)
		if part == "" {
			return nil, fmt.Errorf("empty entry in hourly rate overrides %q", raw)
		}
		rawKey, rawRate, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("hourly rate override %q must use provider:size=rate format", part)
		}
		key, err := normalizeRateKey(rawKey)
		if err != nil {
			return nil, err
		}
		rate, err := strconv.ParseFloat(strings.TrimSpace(rawRate), 64)
		if err != nil || math.IsNaN(rate) || math.IsInf(rate, 0) || rate <= 0 {
			return nil, fmt.Errorf("invalid hourly rate %q for %q", rawRate, key)
		}
		result[key] = rate
	}
	return result, nil
}

func normalizeRateKey(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	provider, rest, hasRest := strings.Cut(trimmed, ":")
	provider = strings.ToLower(strings.TrimSpace(provider))
	if !compute.ValidLabel(provider) {
		return "", fmt.Errorf("invalid provider %q in hourly rate override", raw)
	}
	if !hasRest {
		return provider, nil
	}
	rest = strings.TrimSpace(rest)
	if strings.EqualFold(rest, "spot") {
		return provider + ":spot", nil
	}
	size, suffix, hasSuffix := strings.Cut(rest, ":")
	size = strings.TrimSpace(size)
	if !compute.ValidInstanceSize(size) {
		return "", fmt.Errorf("invalid instance size %q in hourly rate override %q", size, raw)
	}
	if !hasSuffix {
		return provider + ":" + size, nil
	}
	if !strings.EqualFold(strings.TrimSpace(suffix), "spot") {
		return "", fmt.Errorf("invalid rate suffix %q in hourly rate override %q", suffix, raw)
	}
	return provider + ":" + size + ":spot", nil
}
