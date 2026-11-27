// SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
// SPDX-License-Identifier: MIT

// Package compute defines the cloud-neutral runner lifecycle contract.
package compute

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"text/template"
	"time"
)

const (
	// DefaultDeadmanShutdownMinutes is the default in-VM dead-man poweroff timeout.
	DefaultDeadmanShutdownMinutes = 180
	// MaxDeadmanShutdownMinutes bounds the in-VM dead-man timer to seven days.
	MaxDeadmanShutdownMinutes = 7 * 24 * 60
)

// Provider owns cloud runner instances for one compute backend.
type Provider interface {
	Provider() string
	FindRunner(context.Context, string) (*RunnerInstance, bool, error)
	CreateRunner(context.Context, RunnerParams) (*RunnerInstance, error)
	DeleteRunner(context.Context, string, string) error
	CleanupRunner(context.Context, string) error
	SweepOrphanedRunners(context.Context, map[string]struct{}, time.Time) (int, error)
}

// ErrOwnershipMismatch marks a provider resource that the configured
// controller cannot safely mutate.
var ErrOwnershipMismatch = errors.New("provider resource ownership mismatch")

// ErrDuplicateInstances marks multiple controller-owned resources for one job.
// Callers must reconcile all of them rather than retrying creation.
var ErrDuplicateInstances = errors.New("duplicate provider instances")

// ErrCreateOutcomeUnknown marks a provider create that may have succeeded even
// though no resource identity was returned. Retrying would risk duplication.
var ErrCreateOutcomeUnknown = errors.New("provider create outcome unknown")

// ErrCapacityUnavailable marks a provider capacity, quota, or spot pool
// exhaustion failure where failover to another subnet, provider, or on-demand
// capacity is safe.
var ErrCapacityUnavailable = errors.New("provider capacity unavailable")

// ErrSpotUnsupported marks a request for spot capacity on a provider that only
// offers on-demand instances.
var ErrSpotUnsupported = errors.New("provider does not support spot instances")

// IsCapacityError reports whether err indicates provider capacity exhaustion.
func IsCapacityError(err error) bool {
	return errors.Is(err, ErrCapacityUnavailable)
}

// IsSpotUnsupported reports whether err indicates spot instances are unsupported.
func IsSpotUnsupported(err error) bool {
	return errors.Is(err, ErrSpotUnsupported)
}

// RejectSpotUnsupported returns a wrapped ErrSpotUnsupported when requested is true.
func RejectSpotUnsupported(provider string, requested bool) error {
	if !requested {
		return nil
	}
	return fmt.Errorf("%w: %s does not support spot instances", ErrSpotUnsupported, provider)
}

// BoolPtr returns a pointer to value.
func BoolPtr(value bool) *bool { return &value }

// ResolveSpot returns the effective spot setting from a provider default and an
// optional per-request override.
func ResolveSpot(defaultSpot bool, override *bool) bool {
	if override != nil {
		return *override
	}
	return defaultSpot
}

// ResolveSpotPreference inspects GitHub runner labels for explicit "spot" or
// "on-demand" provisioning mode selectors. When both labels appear, "on-demand"
// wins so release and deployment jobs are never preempted mid-flight.
func ResolveSpotPreference(labels []string) *bool {
	wantSpot := false
	for _, raw := range labels {
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case "on-demand":
			return BoolPtr(false)
		case "spot":
			wantSpot = true
		}
	}
	if wantSpot {
		return BoolPtr(true)
	}
	return nil
}

var controllerIDRegex = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

// ValidControllerID applies the provider-safe controller ownership identifier
// contract shared by all cloud adapters.
func ValidControllerID(value string) bool {
	return controllerIDRegex.MatchString(value)
}

// RunnerParams contains the non-secret values rendered into runner user data
// and per-job provisioning overrides.
type RunnerParams struct {
	JobKey                  string
	ProvisionEpoch          int
	RunnerName              string
	RunnerJITConfig         string
	RunnerRegistrationToken string
	RunnerRepoURL           string
	RunnerVersion           string
	RunnerSHA256            string
	ChefInstallerSHA256     string
	InstanceSize            string
	DeadmanShutdownMinutes  int
	PrebakedImage           bool
	HTTPSAptMirrors         bool
	EgressAllowlist         []string
	// Spot overrides provider-default spot provisioning: nil uses the provider
	// default, true forces spot/preemptible, and false forces on-demand.
	Spot *bool
	// SpotActive is populated for cloud-init rendering when the instance is
	// actually launched as a spot/preemptible VM.
	SpotActive bool
}

// WithSpot returns a copy of params configured for the effective spot mode so
// cloud-init only installs the preemption watcher on spot instances.
func (p RunnerParams) WithSpot(useSpot bool) RunnerParams {
	p.Spot = BoolPtr(useSpot)
	p.SpotActive = useSpot
	return p
}

// RunnerInstance is a provider-neutral resource identity.
type RunnerInstance struct {
	ID   string
	Name string
}

var safeRepoURLRegex = regexp.MustCompile(`^https://github\.com/[a-zA-Z0-9][a-zA-Z0-9._-]{0,99}/(\.github|[a-zA-Z0-9][a-zA-Z0-9._-]{0,99})$`)

// RenderCloudInit renders the shared, provider-neutral cloud-init template.
func RenderCloudInit(tmpl *template.Template, params RunnerParams) (string, error) {
	if tmpl == nil {
		return "", fmt.Errorf("cloud-init template is not configured")
	}
	if params.DeadmanShutdownMinutes < 0 || params.DeadmanShutdownMinutes > MaxDeadmanShutdownMinutes {
		return "", fmt.Errorf("dead-man shutdown minutes %d must be between 0 and %d", params.DeadmanShutdownMinutes, MaxDeadmanShutdownMinutes)
	}
	if params.RunnerRepoURL != "" && !safeRepoURLRegex.MatchString(params.RunnerRepoURL) {
		return "", fmt.Errorf("invalid runner repository URL %q", params.RunnerRepoURL)
	}
	for _, entry := range params.EgressAllowlist {
		if !ValidEgressEntry(entry) {
			return "", fmt.Errorf("invalid egress allowlist entry %q", entry)
		}
	}
	if params.Spot != nil && *params.Spot {
		params.SpotActive = true
	}
	var userData bytes.Buffer
	if err := tmpl.Execute(&userData, params); err != nil {
		return "", fmt.Errorf("render cloud-init: %w", err)
	}
	return userData.String(), nil
}
