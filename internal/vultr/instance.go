// SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
// SPDX-License-Identifier: MIT

// Package vultr provisions ephemeral GitHub Actions runners on Vultr Cloud Compute.
package vultr

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"text/template"
	"time"

	"github.com/thomasvincent/github-runners-infra/internal/compute"
)

const (
	defaultBaseURL    = "https://api.vultr.com/v2"
	defaultRegion     = "ewr"
	defaultPlan       = "vc2-4c-8gb"
	defaultOSID       = 2284 // Ubuntu 24.04 LTS x64
	instancesPath     = "/instances"
	instancesBasePath = "/instances/"
)

var instanceIDRegex = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]{0,63}$`)

var _ compute.Provider = (*Client)(nil)

// Config holds Vultr Cloud Compute client configuration.
type Config struct {
	APIKey          string
	Region          string
	Plan            string
	OSID            int
	SnapshotID      string
	SSHKeyIDs       []string
	FirewallGroupID string
	VPCID           string
	CloudInitPath   string
	ControllerID    string
	BaseURL         string
	HTTPClient      *http.Client
}

// Client manages Vultr instances created for one controller.
type Client struct {
	httpClient      *http.Client
	baseURL         string
	apiKey          string
	cloudInitTmpl   *template.Template
	region          string
	plan            string
	osID            int
	snapshotID      string
	sshKeyIDs       []string
	firewallGroupID string
	vpcID           string
	controllerTag   string
	recoveryTimeout time.Duration
	recoveryPoll    time.Duration
}

type vultrInstance struct {
	ID          string   `json:"id"`
	Label       string   `json:"label"`
	Region      string   `json:"region"`
	Plan        string   `json:"plan"`
	Status      string   `json:"status"`
	PowerStatus string   `json:"power_status"`
	DateCreated string   `json:"date_created"`
	Tags        []string `json:"tags"`
}

type listInstancesResponse struct {
	Instances []vultrInstance `json:"instances"`
	Meta      struct {
		Total int `json:"total"`
		Links struct {
			Next string `json:"next"`
			Prev string `json:"prev"`
		} `json:"links"`
	} `json:"meta"`
}

type instanceResponse struct {
	Instance vultrInstance `json:"instance"`
}

type createInstanceRequest struct {
	Region          string   `json:"region"`
	Plan            string   `json:"plan"`
	OSID            int      `json:"os_id,omitempty"`
	SnapshotID      string   `json:"snapshot_id,omitempty"`
	Label           string   `json:"label"`
	Hostname        string   `json:"hostname"`
	UserData        string   `json:"user_data"`
	Tags            []string `json:"tags"`
	SSHKeyIDs       []string `json:"sshkey_id,omitempty"`
	FirewallGroupID string   `json:"firewall_group_id,omitempty"`
	AttachVPC       []string `json:"attach_vpc,omitempty"`
	Backups         string   `json:"backups"`
	EnableIPv6      bool     `json:"enable_ipv6"`
}

func (c *Client) Provider() string { return "vultr" }

// NewClient constructs a Vultr API v2 compute client.
func NewClient(cfg Config) (*Client, error) {
	apiKey := strings.TrimSpace(cfg.APIKey)
	if apiKey == "" {
		return nil, fmt.Errorf("vultr API key is required")
	}
	if !compute.ValidControllerID(cfg.ControllerID) {
		return nil, fmt.Errorf("controller ID must contain only letters, numbers, underscores, and hyphens")
	}
	if strings.TrimSpace(cfg.CloudInitPath) == "" {
		return nil, fmt.Errorf("vultr cloud-init template path is required")
	}
	tmpl, err := template.ParseFiles(cfg.CloudInitPath)
	if err != nil {
		return nil, fmt.Errorf("parse cloud-init template: %w", err)
	}

	region := strings.TrimSpace(cfg.Region)
	if region == "" {
		region = defaultRegion
	}
	plan := strings.TrimSpace(cfg.Plan)
	if plan == "" {
		plan = defaultPlan
	}
	if !compute.ValidInstanceSize(plan) {
		return nil, fmt.Errorf("invalid Vultr plan %q", plan)
	}

	snapshotID := strings.TrimSpace(cfg.SnapshotID)
	osID := cfg.OSID
	if snapshotID == "" && osID == 0 {
		osID = defaultOSID
	}
	if snapshotID == "" && osID < 0 {
		return nil, fmt.Errorf("vultr OS ID must be positive")
	}

	baseURL, err := compute.NormalizeBaseURL(cfg.BaseURL, defaultBaseURL, "vultr")
	if err != nil {
		return nil, err
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = compute.DefaultHTTPClient()
	}

	return &Client{
		httpClient:      httpClient,
		baseURL:         baseURL,
		apiKey:          apiKey,
		cloudInitTmpl:   tmpl,
		region:          region,
		plan:            plan,
		osID:            osID,
		snapshotID:      snapshotID,
		sshKeyIDs:       append([]string(nil), cfg.SSHKeyIDs...),
		firewallGroupID: strings.TrimSpace(cfg.FirewallGroupID),
		vpcID:           strings.TrimSpace(cfg.VPCID),
		controllerTag:   "runner-controller-" + cfg.ControllerID,
		recoveryTimeout: 15 * time.Second,
		recoveryPoll:    2 * time.Second,
	}, nil
}

// Health verifies API connectivity and credentials using a bounded read-only
// instance query scoped to this controller's tag.
func (c *Client) Health(ctx context.Context) error {
	query := url.Values{"tag": {c.controllerTag}, "per_page": {"1"}}
	var resp listInstancesResponse
	if err := c.doJSON(ctx, http.MethodGet, instancesPath+"?"+query.Encode(), nil, &resp); err != nil {
		return fmt.Errorf("vultr health check: %w", err)
	}
	return nil
}

// FindRunner returns the controller-owned Vultr instance for jobKey.
func (c *Client) FindRunner(ctx context.Context, jobKey string) (*compute.RunnerInstance, bool, error) {
	existing, err := c.listJobInstances(ctx, jobKey, "")
	if err != nil {
		return nil, false, err
	}
	if len(existing) == 0 {
		return nil, false, nil
	}
	if len(existing) > 1 {
		return nil, false, fmt.Errorf("%w: multiple Vultr instances exist for job %s", compute.ErrDuplicateInstances, jobKey)
	}
	inst := existing[0]
	return &compute.RunnerInstance{ID: inst.ID, Name: inst.Label}, true, nil
}

// CreateRunner provisions an ephemeral Vultr instance with ownership tags.
func (c *Client) CreateRunner(ctx context.Context, params compute.RunnerParams) (*compute.RunnerInstance, error) {
	desiredName := compute.DeterministicName("vultr", params.JobKey, params.ProvisionEpoch)
	if existing, found, err := c.FindRunner(ctx, params.JobKey); err != nil || found {
		return existing, err
	}

	plan := c.plan
	if params.InstanceSize != "" {
		if !compute.ValidInstanceSize(params.InstanceSize) {
			return nil, fmt.Errorf("invalid Vultr plan %q", params.InstanceSize)
		}
		plan = params.InstanceSize
	}

	userData, err := compute.RenderCloudInit(c.cloudInitTmpl, params)
	if err != nil {
		return nil, err
	}

	reqBody := c.buildCreateRequest(desiredName, plan, params.JobKey, userData)
	var resp instanceResponse
	if err := c.doJSON(ctx, http.MethodPost, instancesPath, reqBody, &resp); err != nil {
		return c.handleCreateError(ctx, params.JobKey, desiredName, err)
	}
	if resp.Instance.ID == "" {
		return nil, fmt.Errorf("%w: Vultr API returned no instance ID", compute.ErrCreateOutcomeUnknown)
	}
	log.Printf("Created Vultr runner instance %s (ID: %s, plan: %s)", desiredName, resp.Instance.ID, plan)
	return &compute.RunnerInstance{ID: resp.Instance.ID, Name: resp.Instance.Label}, nil
}

func (c *Client) buildCreateRequest(name, plan, jobKey, userData string) createInstanceRequest {
	req := createInstanceRequest{
		Region:          c.region,
		Plan:            plan,
		Label:           name,
		Hostname:        name,
		UserData:        base64.StdEncoding.EncodeToString([]byte(userData)),
		Tags:            []string{c.controllerTag, compute.JobTag(jobKey)},
		SSHKeyIDs:       c.sshKeyIDs,
		FirewallGroupID: c.firewallGroupID,
		Backups:         "disabled",
		EnableIPv6:      false,
	}
	if c.snapshotID != "" {
		req.SnapshotID = c.snapshotID
	} else {
		req.OSID = c.osID
	}
	if c.vpcID != "" {
		req.AttachVPC = []string{c.vpcID}
	}
	return req
}

func (c *Client) handleCreateError(ctx context.Context, jobKey, desiredName string, createErr error) (*compute.RunnerInstance, error) {
	if !compute.IsAmbiguousCreateError(createErr) {
		return nil, fmt.Errorf("create Vultr instance: %w", createErr)
	}
	recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.recoveryTimeout)
	defer cancel()
	recovered, confirmedAbsent, recoveryErr := compute.ReconcileAmbiguousCreate(
		recoveryCtx, "Vultr", desiredName, c.recoveryPoll,
		func(pollCtx context.Context) (*vultrInstance, error) {
			return c.findJobInstanceByName(pollCtx, jobKey, desiredName)
		},
	)
	if recoveryErr != nil {
		return nil, errors.Join(fmt.Errorf("create Vultr instance: %w", createErr), fmt.Errorf("reconcile ambiguous create: %w", recoveryErr))
	}
	if recovered != nil {
		return &compute.RunnerInstance{ID: recovered.ID, Name: recovered.Label}, nil
	}
	if confirmedAbsent {
		return nil, fmt.Errorf("create Vultr instance after confirmed absence: %w", createErr)
	}
	return nil, fmt.Errorf("%w: create Vultr instance: %v", compute.ErrCreateOutcomeUnknown, createErr)
}

// RunnerAbsent checks authoritative absence by instance ID without mutating it.
func (c *Client) RunnerAbsent(ctx context.Context, id string) (bool, error) {
	if !instanceIDRegex.MatchString(id) {
		return false, fmt.Errorf("invalid Vultr instance ID %q", id)
	}
	var resp instanceResponse
	err := c.doJSON(ctx, http.MethodGet, instancesBasePath+url.PathEscape(id), nil, &resp)
	if err != nil {
		if compute.IsHTTPNotFound(err) {
			return true, nil
		}
		return false, fmt.Errorf("confirm Vultr instance %s absence: %w", id, err)
	}
	return false, nil
}

// DeleteRunner deletes a Vultr instance only after verifying controller and job ownership tags.
func (c *Client) DeleteRunner(ctx context.Context, id, jobKey string) error {
	if !instanceIDRegex.MatchString(id) {
		return fmt.Errorf("invalid Vultr instance ID %q", id)
	}
	path := instancesBasePath + url.PathEscape(id)
	var resp instanceResponse
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		if compute.IsHTTPNotFound(err) {
			return nil
		}
		return fmt.Errorf("get Vultr instance %s before delete: %w", id, err)
	}
	if !compute.HasTag(resp.Instance.Tags, c.controllerTag) || !compute.HasTag(resp.Instance.Tags, compute.JobTag(jobKey)) {
		var confirm instanceResponse
		if confirmErr := c.doJSON(ctx, http.MethodGet, path, nil, &confirm); confirmErr != nil {
			if compute.IsHTTPNotFound(confirmErr) {
				return nil
			}
			return fmt.Errorf("confirm Vultr instance %s after ownership mismatch: %w", id, confirmErr)
		}
		return fmt.Errorf("%w: refusing to delete Vultr instance %s without controller and job ownership tags", compute.ErrOwnershipMismatch, id)
	}
	if err := c.doJSON(ctx, http.MethodDelete, path, nil, nil); err != nil && !compute.IsHTTPNotFound(err) {
		return fmt.Errorf("delete owned Vultr instance %s: %w", id, err)
	}
	return nil
}

// CleanupRunner deletes all controller-owned Vultr instances tagged for jobKey.
func (c *Client) CleanupRunner(ctx context.Context, jobKey string) error {
	return c.CleanupRunnerExcept(ctx, jobKey, "")
}

// CleanupRunnerExcept deletes job-tagged instances other than excludedID.
func (c *Client) CleanupRunnerExcept(ctx context.Context, jobKey, excludedID string) error {
	instances, err := c.listJobInstances(ctx, jobKey, excludedID)
	if err != nil {
		return err
	}
	for _, inst := range instances {
		if err := c.DeleteRunner(ctx, inst.ID, jobKey); err != nil {
			return err
		}
	}
	return nil
}

// SweepOrphanedRunners reclaims controller-owned instances absent from known and older than cutoff.
func (c *Client) SweepOrphanedRunners(ctx context.Context, known map[string]struct{}, cutoff time.Time) (int, error) {
	instances, err := c.listInstancesByTag(ctx, c.controllerTag)
	if err != nil {
		return 0, fmt.Errorf("list controller Vultr instances for orphan sweep: %w", err)
	}
	deleted := 0
	var parseErrs []error
	for _, inst := range instances {
		if !compute.HasTag(inst.Tags, c.controllerTag) {
			continue
		}
		if _, ok := known[inst.ID]; ok {
			continue
		}
		created, err := time.Parse(time.RFC3339, inst.DateCreated)
		if err != nil {
			parseErrs = append(parseErrs, fmt.Errorf("parse creation time for controller Vultr instance %s: %w", inst.ID, err))
			continue
		}
		if !created.Before(cutoff) {
			continue
		}
		if err := c.doJSON(ctx, http.MethodDelete, instancesBasePath+url.PathEscape(inst.ID), nil, nil); err != nil && !compute.IsHTTPNotFound(err) {
			return deleted, fmt.Errorf("delete orphaned controller Vultr instance %s: %w", inst.ID, err)
		}
		deleted++
	}
	return deleted, errors.Join(parseErrs...)
}

func (c *Client) findJobInstanceByName(ctx context.Context, jobKey, name string) (*vultrInstance, error) {
	instances, err := c.listJobInstances(ctx, jobKey, "")
	if err != nil {
		return nil, err
	}
	var match *vultrInstance
	for i := range instances {
		if instances[i].Label != name {
			continue
		}
		if match != nil {
			return nil, fmt.Errorf("multiple Vultr instances named %q exist for job %s", name, jobKey)
		}
		match = &instances[i]
	}
	return match, nil
}

func (c *Client) listJobInstances(ctx context.Context, jobKey, excludedID string) ([]vultrInstance, error) {
	existing, err := c.listInstancesByTag(ctx, compute.JobTag(jobKey))
	if err != nil {
		return nil, fmt.Errorf("look up existing Vultr runner instance: %w", err)
	}
	verified := make([]vultrInstance, 0, len(existing))
	for _, inst := range existing {
		if excludedID != "" && inst.ID == excludedID {
			continue
		}
		if !compute.HasTag(inst.Tags, c.controllerTag) {
			return nil, fmt.Errorf("%w: job-tagged Vultr instance %s belongs to another controller", compute.ErrOwnershipMismatch, inst.ID)
		}
		verified = append(verified, inst)
	}
	return verified, nil
}

func (c *Client) listInstancesByTag(ctx context.Context, tag string) ([]vultrInstance, error) {
	cursor := ""
	var all []vultrInstance
	for {
		query := url.Values{"tag": {tag}, "per_page": {"100"}}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		var page listInstancesResponse
		if err := c.doJSON(ctx, http.MethodGet, instancesPath+"?"+query.Encode(), nil, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Instances...)
		next := strings.TrimSpace(page.Meta.Links.Next)
		if next == "" || next == cursor {
			break
		}
		cursor = next
	}
	return all, nil
}

func (c *Client) doJSON(ctx context.Context, method, path string, reqBody, respBody any) error {
	return compute.DoJSON(ctx, compute.RESTRequest{
		Provider:    "vultr",
		Client:      c.httpClient,
		Method:      method,
		URL:         c.baseURL + path,
		BearerToken: c.apiKey,
		Body:        reqBody,
		Out:         respBody,
		ParseError:  parseVultrError,
	})
}

func parseVultrError(raw []byte) string {
	var payload struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(raw, &payload)
	return payload.Error
}
