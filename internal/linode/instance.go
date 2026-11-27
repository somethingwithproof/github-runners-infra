// SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
// SPDX-License-Identifier: MIT

// Package linode provisions ephemeral GitHub Actions runners on Akamai Cloud / Linode.
package linode

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/thomasvincent/github-runners-infra/internal/compute"
)

const (
	defaultBaseURL      = "https://api.linode.com/v4"
	defaultRegion       = "us-east"
	defaultInstanceType = "g6-standard-4"
	defaultImage        = "linode/ubuntu24.04"
	instancesPath       = "/linode/instances"
	instancesBasePath   = "/linode/instances/"
)

var _ compute.Provider = (*Client)(nil)

// Config holds Linode / Akamai Cloud API v4 client configuration.
type Config struct {
	Token          string
	Region         string
	InstanceType   string
	Image          string
	AuthorizedKeys []string
	FirewallID     int
	VPCSubnetID    int
	CloudInitPath  string
	ControllerID   string
	BaseURL        string
	HTTPClient     *http.Client
}

// Client manages Linode instances created for one controller.
type Client struct {
	httpClient      *http.Client
	baseURL         string
	token           string
	cloudInitTmpl   *template.Template
	region          string
	instanceType    string
	image           string
	authorizedKeys  []string
	firewallID      int
	vpcSubnetID     int
	controllerTag   string
	recoveryTimeout time.Duration
	recoveryPoll    time.Duration
}

type linodeInstance struct {
	ID      int      `json:"id"`
	Label   string   `json:"label"`
	Region  string   `json:"region"`
	Type    string   `json:"type"`
	Image   string   `json:"image"`
	Status  string   `json:"status"`
	Created string   `json:"created"`
	Tags    []string `json:"tags"`
}

type listInstancesResponse struct {
	Data    []linodeInstance `json:"data"`
	Page    int              `json:"page"`
	Pages   int              `json:"pages"`
	Results int              `json:"results"`
}

type linodeMetadata struct {
	UserData string `json:"user_data"`
}

type linodeInterfaceIPv4 struct {
	NAT string `json:"nat_1_1"`
}

type linodeInterface struct {
	IPv4     *linodeInterfaceIPv4 `json:"ipv4,omitempty"`
	Purpose  string               `json:"purpose"`
	SubnetID int                  `json:"subnet_id,omitempty"`
}

type createInstanceRequest struct {
	Region         string            `json:"region"`
	Type           string            `json:"type"`
	Image          string            `json:"image"`
	Label          string            `json:"label"`
	RootPass       string            `json:"root_pass"`
	AuthorizedKeys []string          `json:"authorized_keys,omitempty"`
	Tags           []string          `json:"tags"`
	FirewallID     int               `json:"firewall_id,omitempty"`
	Interfaces     []linodeInterface `json:"interfaces,omitempty"`
	Metadata       *linodeMetadata   `json:"metadata,omitempty"`
	Booted         bool              `json:"booted"`
	BackupsEnabled bool              `json:"backups_enabled"`
	PrivateIP      bool              `json:"private_ip"`
}

func (c *Client) Provider() string { return "linode" }

// NewClient constructs a Linode API v4 compute client.
func NewClient(cfg Config) (*Client, error) {
	base, err := compute.InitRESTBase(compute.RESTBaseConfig{
		Provider:       "linode",
		TokenName:      "API token",
		Token:          cfg.Token,
		ControllerID:   cfg.ControllerID,
		CloudInitPath:  cfg.CloudInitPath,
		Region:         cfg.Region,
		DefaultRegion:  defaultRegion,
		Size:           cfg.InstanceType,
		DefaultSize:    defaultInstanceType,
		SizeLabel:      "Linode instance type",
		BaseURL:        cfg.BaseURL,
		DefaultBaseURL: defaultBaseURL,
		HTTPClient:     cfg.HTTPClient,
	})
	if err != nil {
		return nil, err
	}

	image := strings.TrimSpace(cfg.Image)
	if image == "" {
		image = defaultImage
	}
	if cfg.FirewallID < 0 {
		return nil, fmt.Errorf("linode firewall ID must be positive when set")
	}
	if cfg.VPCSubnetID < 0 {
		return nil, fmt.Errorf("linode VPC subnet ID must be positive when set")
	}

	return &Client{
		httpClient:      base.HTTPClient,
		baseURL:         base.BaseURL,
		token:           base.Token,
		cloudInitTmpl:   base.CloudInitTmpl,
		region:          base.Region,
		instanceType:    base.Size,
		image:           image,
		authorizedKeys:  append([]string(nil), cfg.AuthorizedKeys...),
		firewallID:      cfg.FirewallID,
		vpcSubnetID:     cfg.VPCSubnetID,
		controllerTag:   base.ControllerTag,
		recoveryTimeout: 15 * time.Second,
		recoveryPoll:    2 * time.Second,
	}, nil
}

// Health verifies API connectivity and credentials using a bounded read-only
// query filtered to this controller's ownership tag.
func (c *Client) Health(ctx context.Context) error {
	filterHeader, err := tagFilterHeader(c.controllerTag)
	if err != nil {
		return err
	}
	var resp listInstancesResponse
	if err := c.doJSON(ctx, http.MethodGet, instancesPath+"?page=1&page_size=25", filterHeader, nil, &resp); err != nil {
		return fmt.Errorf("linode health check: %w", err)
	}
	return nil
}

// FindRunner returns the controller-owned Linode instance for jobKey.
func (c *Client) FindRunner(ctx context.Context, jobKey string) (*compute.RunnerInstance, bool, error) {
	existing, err := c.listJobInstances(ctx, jobKey, "")
	if err != nil {
		return nil, false, err
	}
	return compute.SelectSingleRunner("Linode", jobKey, existing, func(inst linodeInstance) *compute.RunnerInstance {
		return &compute.RunnerInstance{ID: strconv.Itoa(inst.ID), Name: inst.Label}
	})
}

// CreateRunner provisions an ephemeral Linode instance with ownership tags and cloud-init metadata.
func (c *Client) CreateRunner(ctx context.Context, params compute.RunnerParams) (*compute.RunnerInstance, error) {
	desiredName := compute.DeterministicName("linode", params.JobKey, params.ProvisionEpoch)
	if existing, found, err := c.FindRunner(ctx, params.JobKey); err != nil || found {
		return existing, err
	}
	instanceType, err := compute.SelectSizeOverride(c.instanceType, params.InstanceSize, "Linode instance type")
	if err != nil {
		return nil, err
	}
	userData, err := compute.RenderCloudInit(c.cloudInitTmpl, params)
	if err != nil {
		return nil, err
	}
	rootPass, err := randomRootPassword()
	if err != nil {
		return nil, fmt.Errorf("generate Linode root password: %w", err)
	}

	reqBody := c.buildCreateRequest(desiredName, instanceType, params.JobKey, rootPass, userData)
	var created linodeInstance
	if err := c.doJSON(ctx, http.MethodPost, instancesPath, "", reqBody, &created); err != nil {
		return c.handleCreateError(ctx, params.JobKey, desiredName, err)
	}
	if created.ID <= 0 {
		return nil, fmt.Errorf("%w: Linode API returned no instance ID", compute.ErrCreateOutcomeUnknown)
	}
	log.Printf("Created Linode runner instance %s (ID: %d, type: %s)", desiredName, created.ID, instanceType)
	return &compute.RunnerInstance{ID: strconv.Itoa(created.ID), Name: created.Label}, nil
}

func (c *Client) buildCreateRequest(name, instanceType, jobKey, rootPass, userData string) createInstanceRequest {
	req := createInstanceRequest{
		Region:         c.region,
		Type:           instanceType,
		Image:          c.image,
		Label:          name,
		RootPass:       rootPass,
		AuthorizedKeys: c.authorizedKeys,
		Tags:           []string{c.controllerTag, compute.JobTag(jobKey)},
		FirewallID:     c.firewallID,
		Metadata:       &linodeMetadata{UserData: base64.StdEncoding.EncodeToString([]byte(userData))},
		Booted:         true,
		BackupsEnabled: false,
		PrivateIP:      false,
	}
	if c.vpcSubnetID > 0 {
		req.Interfaces = []linodeInterface{{Purpose: "vpc", SubnetID: c.vpcSubnetID, IPv4: &linodeInterfaceIPv4{NAT: "any"}}}
	}
	return req
}

func (c *Client) handleCreateError(ctx context.Context, jobKey, desiredName string, createErr error) (*compute.RunnerInstance, error) {
	return compute.RecoverCreateError(
		ctx, "Linode", desiredName, c.recoveryTimeout, c.recoveryPoll, createErr,
		func(pollCtx context.Context) (*linodeInstance, error) {
			instances, err := c.listJobInstances(pollCtx, jobKey, "")
			if err != nil {
				return nil, err
			}
			return compute.FindUniqueByName("Linode", jobKey, desiredName, instances, func(l linodeInstance) string { return l.Label })
		},
		func(l *linodeInstance) *compute.RunnerInstance {
			return &compute.RunnerInstance{ID: strconv.Itoa(l.ID), Name: l.Label}
		},
	)
}

// RunnerAbsent checks authoritative absence by Linode numeric ID without mutating it.
func (c *Client) RunnerAbsent(ctx context.Context, id string) (bool, error) {
	numericID, err := parseInstanceID(id)
	if err != nil {
		return false, err
	}
	var inst linodeInstance
	err = c.doJSON(ctx, http.MethodGet, instancesBasePath+strconv.Itoa(numericID), "", nil, &inst)
	if err != nil {
		if compute.IsHTTPNotFound(err) {
			return true, nil
		}
		return false, fmt.Errorf("confirm Linode instance %s absence: %w", id, err)
	}
	return false, nil
}

// DeleteRunner deletes a Linode instance only after verifying controller and job ownership tags.
func (c *Client) DeleteRunner(ctx context.Context, id, jobKey string) error {
	numericID, err := parseInstanceID(id)
	if err != nil {
		return err
	}
	path := instancesBasePath + strconv.Itoa(numericID)
	var inst linodeInstance
	if err := c.doJSON(ctx, http.MethodGet, path, "", nil, &inst); err != nil {
		if compute.IsHTTPNotFound(err) {
			return nil
		}
		return fmt.Errorf("get Linode instance %s before delete: %w", id, err)
	}
	if !compute.HasTag(inst.Tags, c.controllerTag) || !compute.HasTag(inst.Tags, compute.JobTag(jobKey)) {
		var confirm linodeInstance
		if confirmErr := c.doJSON(ctx, http.MethodGet, path, "", nil, &confirm); confirmErr != nil {
			if compute.IsHTTPNotFound(confirmErr) {
				return nil
			}
			return fmt.Errorf("confirm Linode instance %s after ownership mismatch: %w", id, confirmErr)
		}
		return fmt.Errorf("%w: refusing to delete Linode instance %s without controller and job ownership tags", compute.ErrOwnershipMismatch, id)
	}
	if err := c.doJSON(ctx, http.MethodDelete, path, "", nil, nil); err != nil && !compute.IsHTTPNotFound(err) {
		return fmt.Errorf("delete owned Linode instance %s: %w", id, err)
	}
	return nil
}

// CleanupRunner deletes all controller-owned Linode instances tagged for jobKey.
func (c *Client) CleanupRunner(ctx context.Context, jobKey string) error {
	return c.CleanupRunnerExcept(ctx, jobKey, "")
}

// CleanupRunnerExcept deletes job-tagged instances other than excludedID.
func (c *Client) CleanupRunnerExcept(ctx context.Context, jobKey, excludedID string) error {
	instances, err := c.listJobInstances(ctx, jobKey, excludedID)
	if err != nil {
		return err
	}
	var failures []error
	for _, inst := range instances {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		if err := c.DeleteRunner(ctx, strconv.Itoa(inst.ID), jobKey); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// SweepOrphanedRunners reclaims controller-owned Linodes absent from known and older than cutoff.
func (c *Client) SweepOrphanedRunners(ctx context.Context, known map[string]struct{}, cutoff time.Time) (int, error) {
	instances, err := c.listInstancesByTag(ctx, c.controllerTag)
	if err != nil {
		return 0, fmt.Errorf("list controller Linode instances for orphan sweep: %w", err)
	}
	return compute.SweepOrphans(
		ctx, "Linode", c.controllerTag, known, cutoff, instances,
		func(l linodeInstance) string { return strconv.Itoa(l.ID) },
		func(l linodeInstance) []string { return l.Tags },
		func(l linodeInstance) (time.Time, error) { return parseLinodeTime(l.Created) },
		func(delCtx context.Context, id string) error {
			return c.doJSON(delCtx, http.MethodDelete, instancesBasePath+id, "", nil, nil)
		},
	)
}

func (c *Client) listJobInstances(ctx context.Context, jobKey, excludedID string) ([]linodeInstance, error) {
	existing, err := c.listInstancesByTag(ctx, compute.JobTag(jobKey))
	if err != nil {
		return nil, fmt.Errorf("look up existing Linode runner instance: %w", err)
	}
	return compute.FilterOwned("Linode", excludedID, c.controllerTag, existing,
		func(l linodeInstance) string { return strconv.Itoa(l.ID) },
		func(l linodeInstance) []string { return l.Tags },
	)
}

func (c *Client) listInstancesByTag(ctx context.Context, tag string) ([]linodeInstance, error) {
	filterHeader, err := tagFilterHeader(tag)
	if err != nil {
		return nil, err
	}
	var all []linodeInstance
	for pageNum := 1; ; pageNum++ {
		path := fmt.Sprintf("%s?page=%d&page_size=100", instancesPath, pageNum)
		var page listInstancesResponse
		if err := c.doJSON(ctx, http.MethodGet, path, filterHeader, nil, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Data...)
		if page.Pages <= 0 || pageNum >= page.Pages {
			break
		}
	}
	return all, nil
}

func (c *Client) doJSON(ctx context.Context, method, path, filterHeader string, reqBody, respBody any) error {
	var headers map[string]string
	if filterHeader != "" {
		headers = map[string]string{"X-Filter": filterHeader}
	}
	return compute.DoJSON(ctx, compute.RESTRequest{
		Provider:    "linode",
		Client:      c.httpClient,
		Method:      method,
		URL:         c.baseURL + path,
		BearerToken: c.token,
		Headers:     headers,
		Body:        reqBody,
		Out:         respBody,
		ParseError:  parseLinodeError,
	})
}

func parseLinodeError(raw []byte) string {
	var payload struct {
		Errors []struct {
			Reason string `json:"reason"`
		} `json:"errors"`
	}
	_ = json.Unmarshal(raw, &payload)
	var reasons []string
	for _, item := range payload.Errors {
		if reason := strings.TrimSpace(item.Reason); reason != "" {
			reasons = append(reasons, reason)
		}
	}
	return strings.Join(reasons, "; ")
}

func tagFilterHeader(tag string) (string, error) {
	encoded, err := json.Marshal(map[string]string{"tags": tag})
	if err != nil {
		return "", fmt.Errorf("encode Linode tag filter: %w", err)
	}
	return string(encoded), nil
}

func parseInstanceID(id string) (int, error) {
	numericID, err := strconv.Atoi(id)
	if err != nil || numericID <= 0 {
		return 0, fmt.Errorf("invalid Linode instance ID %q", id)
	}
	return numericID, nil
}

func parseLinodeTime(raw string) (time.Time, error) {
	trimmed := strings.TrimSpace(raw)
	if parsed, err := time.Parse(time.RFC3339, trimmed); err == nil {
		return parsed, nil
	}
	return time.ParseInLocation("2006-01-02T15:04:05", trimmed, time.UTC)
}

// randomRootPassword produces an ephemeral throwaway password meeting Linode's
// complexity rules; cloud-init disables root and password SSH login.
func randomRootPassword() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "Aa9!" + base64.RawURLEncoding.EncodeToString(buf), nil
}
