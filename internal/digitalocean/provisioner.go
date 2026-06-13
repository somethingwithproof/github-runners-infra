package digitalocean

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/thomasvincent/github-runners-infra/internal/provider"
)

// Client satisfies provider.Provisioner. These adapters keep the existing
// godo-typed methods intact while exposing the cloud-neutral surface the
// handler and cleanup job depend on.
var _ provider.Provisioner = (*Client)(nil)

// Create provisions an ephemeral runner droplet.
func (c *Client) Create(ctx context.Context, params provider.RunnerParams) (provider.Instance, error) {
	droplet, err := c.CreateRunner(ctx, RunnerParams(params))
	if err != nil {
		return provider.Instance{}, err
	}
	return provider.Instance{
		Name: droplet.Name,
		ID:   strconv.Itoa(droplet.ID),
	}, nil
}

// Delete removes a runner droplet by name.
func (c *Client) Delete(ctx context.Context, name string) error {
	droplets, err := c.ListRunnerDroplets(ctx)
	if err != nil {
		return err
	}
	for _, d := range droplets {
		if d.Name == name {
			return c.DeleteDroplet(ctx, d.ID)
		}
	}
	return fmt.Errorf("droplet %q not found", name)
}

// CleanupOld reaps runner droplets older than maxAge.
func (c *Client) CleanupOld(ctx context.Context, maxAge time.Duration) (int, error) {
	return c.CleanupOldDroplets(ctx, maxAge)
}
