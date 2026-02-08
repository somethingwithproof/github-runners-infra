package digitalocean

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"text/template"
	"time"

	"github.com/digitalocean/godo"
	"golang.org/x/oauth2"
)

// Client wraps the DigitalOcean API client.
type Client struct {
	client          *godo.Client
	cloudInitTmpl   *template.Template
	region          string
	size            string
	image           string
	sshFingerprints []string
}

// Config holds DigitalOcean client configuration.
type Config struct {
	Token           string
	Region          string
	Size            string
	Image           string
	SSHFingerprints []string
	CloudInitPath   string
}

// NewClient creates a new DigitalOcean API client.
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
	RunnerName             string
	RunnerTokenSSMParam    string
	RunnerLabels           string
	RunnerOrg              string
	RunnerRepo             string
	DOToken                string
	RunnerVersion          string
	CallbackSecretSSMParam string
	CallbackURL            string
	ChefInstallerSHA256    string
}

// CreateRunner spins up an ephemeral runner droplet.
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

	log.Printf("Created runner droplet %s (ID: %d)", params.RunnerName, droplet.ID)
	return droplet, nil
}

// DeleteDroplet removes a droplet by ID.
func (c *Client) DeleteDroplet(ctx context.Context, id int) error {
	_, err := c.client.Droplets.Delete(ctx, id)
	return err
}

// ListRunnerDroplets returns all droplets tagged as github-runner.
func (c *Client) ListRunnerDroplets(ctx context.Context) ([]godo.Droplet, error) {
	opt := &godo.ListOptions{PerPage: 200}
	droplets, _, err := c.client.Droplets.ListByTag(ctx, "github-runner", opt)
	if err != nil {
		return nil, fmt.Errorf("list runner droplets: %w", err)
	}
	return droplets, nil
}

// CleanupOldDroplets deletes runner droplets older than maxAge.
func (c *Client) CleanupOldDroplets(ctx context.Context, maxAge time.Duration) (int, error) {
	droplets, err := c.ListRunnerDroplets(ctx)
	if err != nil {
		return 0, err
	}

	cutoff := time.Now().Add(-maxAge)
	deleted := 0

	for _, d := range droplets {
		created, _ := time.Parse(time.RFC3339, d.Created)
		if created.Before(cutoff) {
			log.Printf("Deleting stale runner droplet %s (ID: %d, created: %s)", d.Name, d.ID, d.Created)
			if err := c.DeleteDroplet(ctx, d.ID); err != nil {
				log.Printf("Failed to delete droplet %d: %v", d.ID, err)
				continue
			}
			deleted++
		}
	}

	return deleted, nil
}
