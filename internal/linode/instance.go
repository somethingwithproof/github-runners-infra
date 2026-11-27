// SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
// SPDX-License-Identifier: MIT

// Package linode provisions ephemeral GitHub Actions runners on Akamai Cloud / Linode.
package linode

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/thomasvincent/github-runners-infra/internal/compute"
)

const (
	defaultBaseURL               = "https://api.linode.com/v4"
	defaultRegion                = "us-east"
	defaultInstanceType          = "g6-standard-4"
	defaultImage                 = "linode/ubuntu24.04"
	maxResponseBytes             = 4 * 1024 * 1024
	confirmedAbsenceObservations = 3
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

type apiError struct {
	statusCode int
	message    string
}

func (e *apiError) Error() string {
	if e.message != "" {
		return fmt.Sprintf("linode API status %d: %s", e.statusCode, e.message)
	}
	return fmt.Sprintf("linode API status %d", e.statusCode)
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

type linodeInterface struct {
	Purpose  string `json:"purpose"`
	SubnetID int    `json:"subnet_id,omitempty"`
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
	token := strings.TrimSpace(cfg.Token)
	if token == "" {
		return nil, fmt.Errorf("linode API token is required")
	}
	if !compute.ValidControllerID(cfg.ControllerID) {
		return nil, fmt.Errorf("controller ID must contain only letters, numbers, underscores, and hyphens")
	}
	if strings.TrimSpace(cfg.CloudInitPath) == "" {
		return nil, fmt.Errorf("linode cloud-init template path is required")
	}
	tmpl, err := template.ParseFiles(cfg.CloudInitPath)
	if err != nil {
		return nil, fmt.Errorf("parse cloud-init template: %w", err)
	}

	region := strings.TrimSpace(cfg.Region)
	if region == "" {
		region = defaultRegion
	}
	instanceType := strings.TrimSpace(cfg.InstanceType)
	if instanceType == "" {
		instanceType = defaultInstanceType
	}
	if !compute.ValidInstanceSize(instanceType) {
		return nil, fmt.Errorf("invalid Linode instance type %q", instanceType)
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

	baseURL, err := normalizeBaseURL(cfg.BaseURL)
	if err != nil {
		return nil, err
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.ResponseHeaderTimeout = 15 * time.Second
		transport.TLSHandshakeTimeout = 10 * time.Second
		httpClient = &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second,
		}
	}

	return &Client{
		httpClient:      httpClient,
		baseURL:         baseURL,
		token:           token,
		cloudInitTmpl:   tmpl,
		region:          region,
		instanceType:    instanceType,
		image:           image,
		authorizedKeys:  append([]string(nil), cfg.AuthorizedKeys...),
		firewallID:      cfg.FirewallID,
		vpcSubnetID:     cfg.VPCSubnetID,
		controllerTag:   "runner-controller-" + cfg.ControllerID,
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
	if err := c.doJSON(ctx, http.MethodGet, "/linode/instances?page=1&page_size=25", filterHeader, nil, &resp); err != nil {
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
	if len(existing) == 0 {
		return nil, false, nil
	}
	if len(existing) > 1 {
		return nil, false, fmt.Errorf("%w: multiple Linode instances exist for job %s", compute.ErrDuplicateInstances, jobKey)
	}
	inst := existing[0]
	return &compute.RunnerInstance{ID: strconv.Itoa(inst.ID), Name: inst.Label}, true, nil
}

// CreateRunner provisions an ephemeral Linode instance with ownership tags and cloud-init metadata.
func (c *Client) CreateRunner(ctx context.Context, params compute.RunnerParams) (*compute.RunnerInstance, error) {
	desiredName := instanceName(params.JobKey, params.ProvisionEpoch)
	existing, found, err := c.FindRunner(ctx, params.JobKey)
	if err != nil {
		return nil, err
	}
	if found {
		return existing, nil
	}

	instanceType := c.instanceType
	if params.InstanceSize != "" {
		if !compute.ValidInstanceSize(params.InstanceSize) {
			return nil, fmt.Errorf("invalid Linode instance type %q", params.InstanceSize)
		}
		instanceType = params.InstanceSize
	}

	userData, err := compute.RenderCloudInit(c.cloudInitTmpl, params)
	if err != nil {
		return nil, err
	}
	rootPass, err := randomRootPassword()
	if err != nil {
		return nil, fmt.Errorf("generate Linode root password: %w", err)
	}

	reqBody := createInstanceRequest{
		Region:         c.region,
		Type:           instanceType,
		Image:          c.image,
		Label:          desiredName,
		RootPass:       rootPass,
		AuthorizedKeys: c.authorizedKeys,
		Tags:           []string{c.controllerTag, runnerJobTag(params.JobKey)},
		FirewallID:     c.firewallID,
		Metadata: &linodeMetadata{
			UserData: base64.StdEncoding.EncodeToString([]byte(userData)),
		},
		Booted:         true,
		BackupsEnabled: false,
		PrivateIP:      false,
	}
	if c.vpcSubnetID > 0 {
		reqBody.Interfaces = []linodeInterface{
			{Purpose: "vpc", SubnetID: c.vpcSubnetID},
		}
	}

	var created linodeInstance
	if err := c.doJSON(ctx, http.MethodPost, "/linode/instances", "", reqBody, &created); err != nil {
		if !isAmbiguousCreateError(err) {
			return nil, fmt.Errorf("create Linode instance: %w", err)
		}
		recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.recoveryTimeout)
		defer cancel()
		recovered, confirmedAbsent, recoveryErr := c.reconcileAmbiguousCreate(recoveryCtx, params.JobKey, desiredName)
		if recoveryErr != nil {
			return nil, errors.Join(fmt.Errorf("create Linode instance: %w", err), fmt.Errorf("reconcile ambiguous create: %w", recoveryErr))
		}
		if recovered != nil {
			return &compute.RunnerInstance{ID: strconv.Itoa(recovered.ID), Name: recovered.Label}, nil
		}
		if confirmedAbsent {
			return nil, fmt.Errorf("create Linode instance after confirmed absence: %w", err)
		}
		return nil, fmt.Errorf("%w: create Linode instance: %v", compute.ErrCreateOutcomeUnknown, err)
	}
	if created.ID <= 0 {
		return nil, fmt.Errorf("%w: Linode API returned no instance ID", compute.ErrCreateOutcomeUnknown)
	}
	log.Printf("Created Linode runner instance %s (ID: %d, type: %s)", desiredName, created.ID, instanceType)
	return &compute.RunnerInstance{ID: strconv.Itoa(created.ID), Name: created.Label}, nil
}

// RunnerAbsent checks authoritative absence by Linode numeric ID without mutating it.
func (c *Client) RunnerAbsent(ctx context.Context, id string) (bool, error) {
	numericID, err := parseInstanceID(id)
	if err != nil {
		return false, err
	}
	var inst linodeInstance
	err = c.doJSON(ctx, http.MethodGet, "/linode/instances/"+strconv.Itoa(numericID), "", nil, &inst)
	if err != nil {
		if isNotFound(err) {
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
	path := "/linode/instances/" + strconv.Itoa(numericID)
	var inst linodeInstance
	if err := c.doJSON(ctx, http.MethodGet, path, "", nil, &inst); err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("get Linode instance %s before delete: %w", id, err)
	}
	if !hasTag(inst.Tags, c.controllerTag) || !hasTag(inst.Tags, runnerJobTag(jobKey)) {
		var confirm linodeInstance
		if confirmErr := c.doJSON(ctx, http.MethodGet, path, "", nil, &confirm); confirmErr != nil {
			if isNotFound(confirmErr) {
				return nil
			}
			return fmt.Errorf("confirm Linode instance %s after ownership mismatch: %w", id, confirmErr)
		}
		return fmt.Errorf("%w: refusing to delete Linode instance %s without controller and job ownership tags", compute.ErrOwnershipMismatch, id)
	}
	if err := c.doJSON(ctx, http.MethodDelete, path, "", nil, nil); err != nil {
		if isNotFound(err) {
			return nil
		}
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
	for _, inst := range instances {
		if err := c.DeleteRunner(ctx, strconv.Itoa(inst.ID), jobKey); err != nil {
			return err
		}
	}
	return nil
}

// SweepOrphanedRunners reclaims controller-owned Linodes absent from known and older than cutoff.
func (c *Client) SweepOrphanedRunners(ctx context.Context, known map[string]struct{}, cutoff time.Time) (int, error) {
	instances, err := c.listInstancesByTag(ctx, c.controllerTag)
	if err != nil {
		return 0, fmt.Errorf("list controller Linode instances for orphan sweep: %w", err)
	}
	deleted := 0
	var parseErrs []error
	for _, inst := range instances {
		if !hasTag(inst.Tags, c.controllerTag) {
			continue
		}
		id := strconv.Itoa(inst.ID)
		if _, ok := known[id]; ok {
			continue
		}
		created, err := parseLinodeTime(inst.Created)
		if err != nil {
			parseErrs = append(parseErrs, fmt.Errorf("parse creation time for controller Linode instance %s: %w", id, err))
			continue
		}
		if !created.Before(cutoff) {
			continue
		}
		if err := c.doJSON(ctx, http.MethodDelete, "/linode/instances/"+id, "", nil, nil); err != nil && !isNotFound(err) {
			return deleted, fmt.Errorf("delete orphaned controller Linode instance %s: %w", id, err)
		}
		deleted++
	}
	if len(parseErrs) > 0 {
		return deleted, errors.Join(parseErrs...)
	}
	return deleted, nil
}

func (c *Client) reconcileAmbiguousCreate(ctx context.Context, jobKey, name string) (*linodeInstance, bool, error) {
	observations := 0
	consecutiveClean := 0
	for {
		recovered, err := c.findJobInstanceByName(ctx, jobKey, name)
		observations++
		if recovered != nil {
			return recovered, false, nil
		}
		if err == nil {
			consecutiveClean++
			if consecutiveClean >= confirmedAbsenceObservations {
				return nil, true, nil
			}
		} else if ctx.Err() == nil {
			consecutiveClean = 0
			log.Printf("WARN: Linode ambiguous-create recovery poll %d failed for %s: %v", observations, name, err)
		}
		if ctx.Err() != nil {
			return nil, consecutiveClean >= confirmedAbsenceObservations, nil
		}
		timer := time.NewTimer(c.recoveryPoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, consecutiveClean >= confirmedAbsenceObservations, nil
		case <-timer.C:
		}
	}
}

func (c *Client) findJobInstanceByName(ctx context.Context, jobKey, name string) (*linodeInstance, error) {
	instances, err := c.listJobInstances(ctx, jobKey, "")
	if err != nil {
		return nil, err
	}
	var match *linodeInstance
	for i := range instances {
		if instances[i].Label != name {
			continue
		}
		if match != nil {
			return nil, fmt.Errorf("multiple Linode instances named %q exist for job %s", name, jobKey)
		}
		match = &instances[i]
	}
	return match, nil
}

func (c *Client) listJobInstances(ctx context.Context, jobKey, excludedID string) ([]linodeInstance, error) {
	existing, err := c.listInstancesByTag(ctx, runnerJobTag(jobKey))
	if err != nil {
		return nil, fmt.Errorf("look up existing Linode runner instance: %w", err)
	}
	verified := make([]linodeInstance, 0, len(existing))
	for _, inst := range existing {
		if excludedID != "" && strconv.Itoa(inst.ID) == excludedID {
			continue
		}
		if !hasTag(inst.Tags, c.controllerTag) {
			return nil, fmt.Errorf("%w: job-tagged Linode instance %d belongs to another controller", compute.ErrOwnershipMismatch, inst.ID)
		}
		verified = append(verified, inst)
	}
	return verified, nil
}

func (c *Client) listInstancesByTag(ctx context.Context, tag string) ([]linodeInstance, error) {
	filterHeader, err := tagFilterHeader(tag)
	if err != nil {
		return nil, err
	}
	var all []linodeInstance
	for pageNum := 1; ; pageNum++ {
		path := fmt.Sprintf("/linode/instances?page=%d&page_size=100", pageNum)
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
	var bodyReader io.Reader
	if reqBody != nil {
		encoded, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("marshal Linode request: %w", err)
		}
		bodyReader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return fmt.Errorf("build Linode request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if filterHeader != "" {
		req.Header.Set("X-Filter", filterHeader)
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
	}()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("read Linode response: %w", err)
	}
	if len(raw) > maxResponseBytes {
		return fmt.Errorf("linode response exceeded %d bytes", maxResponseBytes)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var errPayload struct {
			Errors []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
		}
		_ = json.Unmarshal(raw, &errPayload)
		var reasons []string
		for _, item := range errPayload.Errors {
			if reason := strings.TrimSpace(item.Reason); reason != "" {
				reasons = append(reasons, reason)
			}
		}
		return &apiError{statusCode: resp.StatusCode, message: strings.Join(reasons, "; ")}
	}

	if respBody == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, respBody); err != nil {
		return fmt.Errorf("decode Linode response: %w", err)
	}
	return nil
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

func normalizeBaseURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return defaultBaseURL, nil
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		return "", fmt.Errorf("invalid Linode API base URL %q", raw)
	}
	return strings.TrimRight(trimmed, "/"), nil
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

func hasTag(tags []string, want string) bool {
	for _, tag := range tags {
		if tag == want {
			return true
		}
	}
	return false
}

func isNotFound(err error) bool {
	var apiErr *apiError
	return errors.As(err, &apiErr) && apiErr.statusCode == http.StatusNotFound
}

func isAmbiguousCreateError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var apiErr *apiError
	if !errors.As(err, &apiErr) {
		return true
	}
	return apiErr.statusCode >= http.StatusInternalServerError || apiErr.statusCode == http.StatusRequestTimeout
}

func runnerJobTag(jobKey string) string {
	jobHash := sha256.Sum256([]byte(jobKey))
	return "runner-job-" + hex.EncodeToString(jobHash[:8])
}

func instanceName(jobKey string, provisionEpoch int) string {
	resourceHash := sha256.Sum256([]byte("linode:" + jobKey + ":" + strconv.Itoa(provisionEpoch)))
	return "ghr-" + hex.EncodeToString(resourceHash[:16])
}
