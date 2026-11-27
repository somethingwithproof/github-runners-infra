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
	RegistrationLabels      []string
	RunnerVersion           string
	RunnerSHA256            string
	ChefInstallerSHA256     string
	InstanceSize            string
	DeadmanShutdownMinutes  int
	PrebakedImage           bool
	EgressAllowlist         []string
}

// RunnerInstance is a provider-neutral resource identity.
type RunnerInstance struct {
	ID   string
	Name string
}

var safeRepoURLRegex = regexp.MustCompile(`^https://github\.com/[a-zA-Z0-9][a-zA-Z0-9._-]{0,99}/(\.github|[a-zA-Z0-9][a-zA-Z0-9._-]{0,99})$`)

// LabelList returns the comma-separated labels expected by config.sh.
func (p RunnerParams) LabelList() string { return strings.Join(p.RegistrationLabels, ",") }

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
	for _, label := range params.RegistrationLabels {
		if !ValidLabel(label) {
			return "", fmt.Errorf("invalid registration label %q", label)
		}
	}
	for _, entry := range params.EgressAllowlist {
		if !ValidEgressEntry(entry) {
			return "", fmt.Errorf("invalid egress allowlist entry %q", entry)
		}
	}
	var userData bytes.Buffer
	if err := tmpl.Execute(&userData, params); err != nil {
		return "", fmt.Errorf("render cloud-init: %w", err)
	}
	return userData.String(), nil
}
