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
	"strings"
	"text/template"
	"time"

	"github.com/digitalocean/godo"
	"github.com/thomasvincent/github-runners-infra/internal/compute"
	"golang.org/x/oauth2"
)

const confirmedAbsenceObservations = 3

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
	recoveryTimeout time.Duration
	recoveryPoll    time.Duration
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
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, fmt.Errorf("DigitalOcean token is required")
	}
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: cfg.Token})
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 15 * time.Second
	tc := &http.Client{
		Transport: &oauth2.Transport{Source: ts, Base: transport},
		Timeout:   30 * time.Second,
	}
	client := godo.NewClient(tc)

	if strings.TrimSpace(cfg.CloudInitPath) == "" {
		return nil, fmt.Errorf("DigitalOcean cloud-init template path is required")
	}
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
		recoveryTimeout: 15 * time.Second,
		recoveryPoll:    2 * time.Second,
	}, nil
}

// CreateRunner spins up an ephemeral runner droplet.
func (c *Client) CreateRunner(ctx context.Context, params compute.RunnerParams) (*compute.RunnerInstance, error) {
	desiredName := dropletName(params.JobKey, params.ProvisionEpoch)
	existing, found, err := c.FindRunner(ctx, params.JobKey)
	if err != nil {
		return nil, err
	}
	if found {
		return existing, nil
	}
	jobTag := runnerJobTag(params.JobKey)
	appliedTags := []string{c.controllerTag, jobTag}
	if err := c.validateRunnerFirewalls(ctx, appliedTags); err != nil {
		return nil, err
	}

	userData, err := compute.RenderCloudInit(c.cloudInitTmpl, params)
	if err != nil {
		return nil, err
	}

	var keys []godo.DropletCreateSSHKey
	for _, fp := range c.sshFingerprints {
		keys = append(keys, godo.DropletCreateSSHKey{Fingerprint: fp})
	}

	createReq := &godo.DropletCreateRequest{
		Name:   desiredName,
		Region: c.region,
		Size:   c.size,
		Image: godo.DropletCreateImage{
			Slug: c.image,
		},
		UserData: userData,
		SSHKeys:  keys,
		Tags:     appliedTags,
		VPCUUID:  c.vpcUUID,
	}

	droplet, _, err := c.client.Droplets.Create(ctx, createReq)
	if err != nil {
		if !isAmbiguousCreateError(err) {
			return nil, fmt.Errorf("create droplet: %w", err)
		}
		recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.recoveryTimeout)
		defer cancel()
		recovered, confirmedAbsent, recoveryErr := c.reconcileAmbiguousCreate(recoveryCtx, params.JobKey, desiredName)
		if recoveryErr != nil {
			return nil, errors.Join(fmt.Errorf("create droplet: %w", err), fmt.Errorf("reconcile ambiguous create: %w", recoveryErr))
		}
		if recovered != nil {
			return &compute.RunnerInstance{ID: fmt.Sprint(recovered.ID), Name: recovered.Name}, nil
		}
		if confirmedAbsent {
			return nil, fmt.Errorf("create droplet after confirmed absence: %w", err)
		}
		return nil, fmt.Errorf("%w: create droplet: %v", compute.ErrCreateOutcomeUnknown, err)
	}

	log.Printf("Created runner droplet %s (ID: %d)", desiredName, droplet.ID)
	return &compute.RunnerInstance{ID: fmt.Sprint(droplet.ID), Name: droplet.Name}, nil
}

func (c *Client) reconcileAmbiguousCreate(ctx context.Context, jobKey, name string) (*godo.Droplet, bool, error) {
	observations := 0
	consecutiveClean := 0
	for {
		recovered, err := c.findJobDropletByName(ctx, jobKey, name)
		observations++
		if recovered != nil {
			return recovered, false, nil
		}
		if err == nil {
			consecutiveClean++
			if consecutiveClean >= confirmedAbsenceObservations {
				return ambiguousRecoveryResult(name, observations, consecutiveClean)
			}
		} else if ctx.Err() == nil {
			consecutiveClean = 0
			log.Printf("WARN: DigitalOcean ambiguous-create recovery poll %d failed for %s: %v", observations, name, err)
		}
		if ctx.Err() != nil {
			return ambiguousRecoveryResult(name, observations, consecutiveClean)
		}
		timer := time.NewTimer(c.recoveryPoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ambiguousRecoveryResult(name, observations, consecutiveClean)
		case <-timer.C:
		}
	}
}

func ambiguousRecoveryResult(name string, observations, consecutiveClean int) (*godo.Droplet, bool, error) {
	if consecutiveClean >= confirmedAbsenceObservations {
		log.Printf("WARN: DigitalOcean create confirmed absent after %d observations for %s", observations, name)
		return nil, true, nil
	}
	log.Printf("WARN: DigitalOcean ambiguous-create recovery exhausted after %d observations for %s", observations, name)
	return nil, false, nil
}

func (c *Client) validateRunnerFirewalls(ctx context.Context, appliedTags []string) error {
	options := &godo.ListOptions{Page: 1, PerPage: 200}
	foundPinned := false
	for {
		firewalls, response, err := c.client.Firewalls.List(ctx, options)
		if err != nil {
			return fmt.Errorf("list DigitalOcean firewalls: %w", err)
		}
		for index := range firewalls {
			firewall := &firewalls[index]
			if firewall.ID == c.firewallID {
				foundPinned = true
				if err := validateRunnerFirewall(firewall, c.controllerTag); err != nil {
					return err
				}
				continue
			}
			if len(firewall.InboundRules) != 0 && sharesTag(firewall.Tags, appliedTags) {
				return fmt.Errorf("DigitalOcean firewall %q adds inbound rules to runner tag", firewall.ID)
			}
		}
		if response == nil || response.Links == nil || response.Links.Pages == nil || response.Links.Pages.Next == "" {
			break
		}
		options.Page++
	}
	if !foundPinned {
		return fmt.Errorf("DigitalOcean deny-inbound firewall %q was not found", c.firewallID)
	}
	return nil
}

func sharesTag(left, right []string) bool {
	for _, candidate := range left {
		for _, applied := range right {
			if candidate == applied {
				return true
			}
		}
	}
	return false
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
		return nil, false, fmt.Errorf("%w: multiple DigitalOcean droplets exist for job %s", compute.ErrDuplicateInstances, jobKey)
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
	existing, err := c.listDropletsByTag(ctx, jobTag)
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

func (c *Client) listDropletsByTag(ctx context.Context, tag string) ([]godo.Droplet, error) {
	options := &godo.ListOptions{Page: 1, PerPage: 200}
	var existing []godo.Droplet
	for {
		page, response, err := c.client.Droplets.ListByTag(ctx, tag, options)
		if err != nil {
			return nil, err
		}
		existing = append(existing, page...)
		if response == nil || response.Links == nil || response.Links.Pages == nil || response.Links.Pages.Next == "" {
			break
		}
		options.Page++
	}
	return existing, nil
}

// SweepOrphanedRunners reclaims old controller-owned droplets that are absent
// from durable state, including after state-volume loss.
func (c *Client) SweepOrphanedRunners(ctx context.Context, known map[string]struct{}, cutoff time.Time) (int, error) {
	droplets, err := c.listDropletsByTag(ctx, c.controllerTag)
	if err != nil {
		return 0, fmt.Errorf("list controller droplets for orphan sweep: %w", err)
	}
	deleted := 0
	for _, droplet := range droplets {
		id := fmt.Sprint(droplet.ID)
		if _, ok := known[id]; ok {
			continue
		}
		created, err := time.Parse(time.RFC3339, droplet.Created)
		if err != nil {
			return deleted, fmt.Errorf("parse creation time for controller droplet %s: %w", id, err)
		}
		if !created.Before(cutoff) {
			continue
		}
		if _, err := c.client.Droplets.Delete(ctx, droplet.ID); err != nil && !isNotFound(err) {
			return deleted, fmt.Errorf("delete orphaned controller droplet %s: %w", id, err)
		}
		deleted++
	}
	return deleted, nil
}

func (c *Client) findJobDropletByName(ctx context.Context, jobKey, name string) (*godo.Droplet, error) {
	droplets, err := c.listJobDroplets(ctx, jobKey)
	if err != nil {
		return nil, err
	}
	var match *godo.Droplet
	for index := range droplets {
		if droplets[index].Name != name {
			continue
		}
		if match != nil {
			return nil, fmt.Errorf("multiple DigitalOcean droplets named %q exist for job %s", name, jobKey)
		}
		match = &droplets[index]
	}
	return match, nil
}

func isNotFound(err error) bool {
	var response *godo.ErrorResponse
	return errors.As(err, &response) && response.Response != nil && response.Response.StatusCode == http.StatusNotFound
}

func isAmbiguousCreateError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var response *godo.ErrorResponse
	if !errors.As(err, &response) || response.Response == nil {
		return true
	}
	status := response.Response.StatusCode
	return status >= http.StatusInternalServerError || status == http.StatusRequestTimeout
}

func runnerJobTag(jobKey string) string {
	jobHash := sha256.Sum256([]byte(jobKey))
	return "runner-job-" + hex.EncodeToString(jobHash[:8])
}

func dropletName(jobKey string, provisionEpoch int) string {
	resourceHash := sha256.Sum256([]byte("digitalocean:" + jobKey + ":" + strconv.Itoa(provisionEpoch)))
	return "ghr-" + hex.EncodeToString(resourceHash[:16])
}
