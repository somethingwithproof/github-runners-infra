// Package provider defines the cloud-agnostic contract the webhook handler and
// cleanup job depend on, so DigitalOcean and GCP implementations are
// interchangeable behind the RUNNER_PROVIDER selector.
package provider

import (
	"context"
	"time"
)

// RunnerParams holds the inputs needed to render a runner's startup config and
// register it with GitHub. It is shared across providers; per-cloud fields
// (DOToken) are ignored by providers that do not use them.
type RunnerParams struct {
	RunnerName    string
	RunnerToken   string
	RunnerLabels  string
	RunnerOrg     string
	RunnerRepo    string
	DOToken       string
	RunnerVersion string
}

// Instance identifies a provisioned runner host in a provider-neutral way.
// Name is the stable handle cleanup uses to reap orphans; ID is the provider's
// own identifier for logging.
type Instance struct {
	Name string
	ID   string
}

// Provisioner is the surface both cloud implementations satisfy. Create spins
// up one ephemeral runner host; Delete removes it by name; CleanupOld reaps
// runner hosts older than maxAge, returning the count deleted.
type Provisioner interface {
	Create(ctx context.Context, params RunnerParams) (Instance, error)
	Delete(ctx context.Context, name string) error
	CleanupOld(ctx context.Context, maxAge time.Duration) (int, error)
}
