package digitalocean

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"text/template"
	"time"

	"github.com/digitalocean/godo"
	"github.com/thomasvincent/github-runners-infra/internal/compute"
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
	controllerTag   string
	vpcUUID         string
	firewallID      string
}

// Config holds DigitalOcean client configuration.
type Config struct {
	Token           string
	Region          string
	Size            string
	Image           string
	SSHFingerprints []string
	CloudInitPath   string
	ControllerID    string
	VPCUUID         string
	FirewallID      string
}

func (c *Client) Provider() string { return "digitalocean" }

// NewClient creates a new DigitalOcean API client.
func NewClient(cfg Config) (*Client, error) {
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: cfg.Token})
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 15 * time.Second
	tc := &http.Client{
		Transport: &oauth2.Transport{Source: ts, Base: transport},
		Timeout:   30 * time.Second,
	}
	client := godo.NewClient(tc)

	var tmpl *template.Template
	if cfg.CloudInitPath != "" {
		var err error
		tmpl, err = template.ParseFiles(cfg.CloudInitPath)
		if err != nil {
			return nil, fmt.Errorf("parse cloud-init template: %w", err)
		}
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
	if !compute.ValidControllerID(cfg.ControllerID) {
		return nil, fmt.Errorf("controller ID must contain only letters, numbers, underscores, and hyphens")
	}
	if cfg.VPCUUID == "" || cfg.FirewallID == "" {
		return nil, fmt.Errorf("DigitalOcean VPC UUID and deny-inbound firewall ID are required")
	}

	return &Client{
		client:          client,
		cloudInitTmpl:   tmpl,
		region:          region,
		size:            size,
		image:           image,
		sshFingerprints: cfg.SSHFingerprints,
		controllerTag:   "runner-controller-" + cfg.ControllerID,
		vpcUUID:         cfg.VPCUUID,
		firewallID:      cfg.FirewallID,
	}, nil
}

// CreateRunner spins up an ephemeral runner droplet.
func (c *Client) CreateRunner(ctx context.Context, params compute.RunnerParams) (*compute.RunnerInstance, error) {
	existing, found, err := c.FindRunner(ctx, params.JobKey)
	if err != nil {
		return nil, err
	}
	if found {
		return existing, nil
	}
	firewall, _, err := c.client.Firewalls.Get(ctx, c.firewallID)
	if err != nil {
		return nil, fmt.Errorf("get deny-inbound firewall: %w", err)
	}
	if err := validateRunnerFirewall(firewall, c.controllerTag); err != nil {
		return nil, err
	}
	jobTag := runnerJobTag(params.JobKey)

	userData, err := compute.RenderCloudInit(c.cloudInitTmpl, params)
	if err != nil {
		return nil, err
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
		UserData: userData,
		SSHKeys:  keys,
		Tags:     []string{"github-runner", "ephemeral", c.controllerTag, jobTag},
		VPCUUID:  c.vpcUUID,
	}

	droplet, _, err := c.client.Droplets.Create(ctx, createReq)
	if err != nil {
		return nil, fmt.Errorf("create droplet: %w", err)
	}

	log.Printf("Created runner droplet %s (ID: %d)", params.RunnerName, droplet.ID)
	return &compute.RunnerInstance{ID: fmt.Sprint(droplet.ID), Name: droplet.Name}, nil
}

func validateRunnerFirewall(firewall *godo.Firewall, controllerTag string) error {
	if firewall == nil {
		return fmt.Errorf("DigitalOcean runner firewall response was empty")
	}
	if len(firewall.InboundRules) != 0 {
		return fmt.Errorf("DigitalOcean runner firewall %q must have no inbound rules", firewall.ID)
	}
	for _, tag := range firewall.Tags {
		if tag == controllerTag {
			return nil
		}
	}
	return fmt.Errorf("DigitalOcean runner firewall %q must target controller tag %q", firewall.ID, controllerTag)
}

// FindRunner returns the controller-owned droplet for a durable job key. This
// closes the crash window between provider creation and state persistence.
func (c *Client) FindRunner(ctx context.Context, jobKey string) (*compute.RunnerInstance, bool, error) {
	existing, err := c.listJobDroplets(ctx, jobKey)
	if err != nil {
		return nil, false, err
	}
	if len(existing) == 0 {
		return nil, false, nil
	}
	if len(existing) > 1 {
		return nil, false, fmt.Errorf("multiple DigitalOcean droplets exist for job %s", jobKey)
	}
	droplet := existing[0]
	return &compute.RunnerInstance{ID: fmt.Sprint(droplet.ID), Name: droplet.Name}, true, nil
}

// DeleteRunner removes a droplet only after verifying controller ownership.
func (c *Client) DeleteRunner(ctx context.Context, id, jobKey string) error {
	numericID, err := strconv.Atoi(id)
	if err != nil || numericID <= 0 {
		return fmt.Errorf("invalid DigitalOcean droplet ID %q", id)
	}
	droplet, _, err := c.client.Droplets.Get(ctx, numericID)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("get droplet %s before delete: %w", id, err)
	}
	owned, correctJob := false, false
	for _, tag := range droplet.Tags {
		if tag == c.controllerTag {
			owned = true
		}
		if tag == runnerJobTag(jobKey) {
			correctJob = true
		}
	}
	if !owned || !correctJob {
		return fmt.Errorf("%w: refusing to delete droplet %s without controller and job ownership tags", compute.ErrOwnershipMismatch, id)
	}
	_, err = c.client.Droplets.Delete(ctx, numericID)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("delete owned droplet %s: %w", id, err)
	}
	return nil
}

func (c *Client) CleanupRunner(ctx context.Context, jobKey string) error {
	droplets, err := c.listJobDroplets(ctx, jobKey)
	if err != nil {
		return err
	}
	for _, droplet := range droplets {
		if err := c.DeleteRunner(ctx, fmt.Sprint(droplet.ID), jobKey); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) listJobDroplets(ctx context.Context, jobKey string) ([]godo.Droplet, error) {
	jobTag := runnerJobTag(jobKey)
	existing, _, err := c.client.Droplets.ListByTag(ctx, jobTag, &godo.ListOptions{PerPage: 200})
	if err != nil {
		return nil, fmt.Errorf("look up existing runner droplet: %w", err)
	}
	for _, droplet := range existing {
		owned := false
		for _, tag := range droplet.Tags {
			if tag == c.controllerTag {
				owned = true
				break
			}
		}
		if !owned {
			return nil, fmt.Errorf("%w: job-tagged droplet %d belongs to another controller", compute.ErrOwnershipMismatch, droplet.ID)
		}
	}
	return existing, nil
}

func isNotFound(err error) bool {
	var response *godo.ErrorResponse
	return errors.As(err, &response) && response.Response != nil && response.Response.StatusCode == http.StatusNotFound
}

func runnerJobTag(jobKey string) string {
	jobHash := sha256.Sum256([]byte(jobKey))
	return "runner-job-" + hex.EncodeToString(jobHash[:8])
}
