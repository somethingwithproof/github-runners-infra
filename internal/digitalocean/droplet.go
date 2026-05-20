package digitalocean

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"sync"
	"text/template"
	"time"

	"github.com/digitalocean/godo"
	"github.com/thomasvincent/github-runners-infra/internal/metrics"
	"golang.org/x/oauth2"
)

const maxPages = 50

type Client struct {
	client          *godo.Client
	cloudInitTmpl   *template.Template
	region          string
	size            string
	image           string
	sshFingerprints []string
}

type Config struct {
	Token           string
	Region          string
	Size            string
	Image           string
	SSHFingerprints []string
	CloudInitPath   string
}

func NewClient(cfg Config) (*Client, error) {
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: cfg.Token})
	tc := oauth2.NewClient(context.Background(), ts)
	client := godo.NewClient(tc)

	tmpl, err := template.ParseFiles(cfg.CloudInitPath)
	if err != nil {
		return nil, fmt.Errorf("parse cloud-init template: %w", err)
	}

	region := cfg.Region
	if region == "" {
		region = "nyc3"
	}
	size := cfg.Size
	if size == "" {
		size = "s-4vcpu-8gb"
	}
	image := cfg.Image
	if image == "" {
		image = "ubuntu-24-04-x64"
	}

	return &Client{
		client:          client,
		cloudInitTmpl:   tmpl,
		region:          region,
		size:            size,
		image:           image,
		sshFingerprints: cfg.SSHFingerprints,
	}, nil
}

// RunnerParams holds parameters for cloud-init template rendering.
type RunnerParams struct {
	RunnerName    string
	RunnerToken   string
	RunnerLabels  string
	RunnerOrg     string
	RunnerRepo    string
	DOToken       string // used when callback is not configured
	RunnerVersion string
	DestroyToken  string // scoped self-destruct token (replaces DOToken when set)
	CallbackURL   string // webhook server URL for self-destruct callback
}

func (c *Client) CreateRunner(ctx context.Context, params RunnerParams) (*godo.Droplet, error) {
	var userData bytes.Buffer
	if err := c.cloudInitTmpl.Execute(&userData, params); err != nil {
		return nil, fmt.Errorf("render cloud-init: %w", err)
	}

	var keys []godo.DropletCreateSSHKey
	for _, fp := range c.sshFingerprints {
		keys = append(keys, godo.DropletCreateSSHKey{Fingerprint: fp})
	}

	createReq := &godo.DropletCreateRequest{
		Name:   params.RunnerName,
		Region: c.region,
		Size:   c.size,
		Image: godo.DropletCreateImage{
			Slug: c.image,
		},
		UserData: userData.String(),
		SSHKeys:  keys,
		Tags:     []string{"github-runner", "ephemeral"},
	}

	droplet, _, err := c.client.Droplets.Create(ctx, createReq)
	if err != nil {
		return nil, fmt.Errorf("create droplet: %w", err)
	}

	slog.Info("created runner droplet", "name", params.RunnerName, "droplet_id", droplet.ID)
	return droplet, nil
}

func (c *Client) DeleteDroplet(ctx context.Context, id int) error {
	_, err := c.client.Droplets.Delete(ctx, id)
	return err
}

// Ping verifies the DO API is reachable by listing one droplet.
func (c *Client) Ping(ctx context.Context) error {
	_, _, err := c.client.Droplets.ListByTag(ctx, "github-runner", &godo.ListOptions{PerPage: 1})
	return err
}

func (c *Client) ListRunnerDroplets(ctx context.Context) ([]godo.Droplet, error) {
	var allDroplets []godo.Droplet
	opt := &godo.ListOptions{PerPage: 200}

	for range maxPages {
		droplets, resp, err := c.client.Droplets.ListByTag(ctx, "github-runner", opt)
		if err != nil {
			return nil, fmt.Errorf("list runner droplets: %w", err)
		}
		allDroplets = append(allDroplets, droplets...)

		if resp.Links == nil || resp.Links.IsLastPage() {
			break
		}
		page, err := resp.Links.CurrentPage()
		if err != nil {
			break
		}
		opt.Page = page + 1
	}

	return allDroplets, nil
}

// CleanupOldDroplets deletes runner droplets older than maxAge.
// Deletions run in parallel with bounded concurrency.
func (c *Client) CleanupOldDroplets(ctx context.Context, maxAge time.Duration) (int, error) {
	droplets, err := c.ListRunnerDroplets(ctx)
	if err != nil {
		return 0, err
	}

	metrics.ActiveDroplets.Set(float64(len(droplets)))

	cutoff := time.Now().Add(-maxAge)
	var stale []godo.Droplet
	for _, d := range droplets {
		created, err := time.Parse(time.RFC3339, d.Created)
		if err != nil {
			slog.Warn("cannot parse creation time, skipping", "droplet_id", d.ID, "created", d.Created)
			continue
		}
		if created.Before(cutoff) {
			stale = append(stale, d)
		}
	}

	if len(stale) == 0 {
		return 0, nil
	}

	var (
		mu      sync.Mutex
		deleted int
		wg      sync.WaitGroup
		sem     = make(chan struct{}, 10) // parallel deletion concurrency
	)

	for _, d := range stale {
		wg.Add(1)
		sem <- struct{}{}
		go func(d godo.Droplet) {
			defer wg.Done()
			defer func() { <-sem }()
			slog.Info("deleting stale droplet", "name", d.Name, "droplet_id", d.ID, "created", d.Created)
			if err := c.DeleteDroplet(ctx, d.ID); err != nil {
				slog.Error("failed to delete droplet", "droplet_id", d.ID, "error", err)
				return
			}
			metrics.CleanupDeleted.Inc()
			mu.Lock()
			deleted++
			mu.Unlock()
		}(d)
	}
	wg.Wait()

	return deleted, nil
}
