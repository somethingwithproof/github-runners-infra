// SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
// SPDX-License-Identifier: MIT

// Package oci provisions ephemeral GitHub Actions runners on Oracle Cloud
// Infrastructure (OCI) Compute using the stdlib HTTP client and OCI HTTP
// Signature v1 (RSA-SHA256) authentication.
package oci

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/thomasvincent/github-runners-infra/internal/compute"
)

const (
	defaultShape      = "VM.Standard.E4.Flex"
	defaultOCPUs      = 4.0
	defaultMemoryGB   = 16.0
	controllerTagKey  = "github-runners-controller"
	jobTagKey         = "github-runners-job"
	instancesPath     = "/instances"
	instancesBasePath = "/instances/"
)

var (
	// OCI Compute documents preemptible capacity by VM shape family.
	preemptibleShape = regexp.MustCompile(`^VM\.(?:(?:Standard(?:1|2|\.B1|\.E2)|DenseIO[12]|GPU[23])\.[0-9]+|(?:Standard3|Standard\.(?:E[3-6]|A[14])|Optimized3)\.Flex)$`)
	regionRegex      = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)
	instanceIDRegex  = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,254}$`)
)

var _ compute.Provider = (*Client)(nil)

// Config holds Oracle Cloud Infrastructure Compute client configuration.
type Config struct {
	TenancyOCID          string
	UserOCID             string
	Fingerprint          string
	PrivateKeyPath       string
	PrivateKeyPEM        string
	Region               string
	CompartmentOCID      string
	SubnetOCID           string
	AvailabilityDomain   string
	ImageOCID            string
	Shape                string
	OCPUs                float64
	MemoryGB             float64
	SSHPublicKey         string
	AssignPublicIP       bool
	Spot                 bool
	SpotFallbackOnDemand bool
	CloudInitPath        string
	ControllerID         string
	BaseURL              string
	HTTPClient           *http.Client
}

// Client manages OCI Compute instances owned by one controller.
type Client struct {
	httpClient           *http.Client
	signer               *requestSigner
	baseURL              string
	cloudInitTmpl        *template.Template
	compartmentOCID      string
	subnetOCID           string
	availabilityDomain   string
	imageOCID            string
	shape                string
	ocpus                float64
	memoryGB             float64
	sshPublicKey         string
	assignPublicIP       bool
	spot                 bool
	spotFallbackOnDemand bool
	controllerID         string
	controllerTag        string
	recoveryTimeout      time.Duration
	recoveryPoll         time.Duration
}

type ociInstance struct {
	ID             string            `json:"id"`
	DisplayName    string            `json:"displayName"`
	LifecycleState string            `json:"lifecycleState"`
	TimeCreated    string            `json:"timeCreated"`
	FreeformTags   map[string]string `json:"freeformTags"`
}

type shapeConfig struct {
	OCPUs       float64 `json:"ocpus"`
	MemoryInGBs float64 `json:"memoryInGBs"`
}

type sourceDetails struct {
	SourceType string `json:"sourceType"`
	ImageID    string `json:"imageId"`
}

type createVnicDetails struct {
	SubnetID       string `json:"subnetId"`
	AssignPublicIP bool   `json:"assignPublicIp"`
}

type preemptionAction struct {
	Type               string `json:"type"`
	PreserveBootVolume bool   `json:"preserveBootVolume"`
}

type preemptibleInstanceConfig struct {
	PreemptionAction preemptionAction `json:"preemptionAction"`
}

type launchInstanceRequest struct {
	AvailabilityDomain        string                     `json:"availabilityDomain"`
	CompartmentID             string                     `json:"compartmentId"`
	DisplayName               string                     `json:"displayName"`
	Shape                     string                     `json:"shape"`
	ShapeConfig               *shapeConfig               `json:"shapeConfig,omitempty"`
	SourceDetails             sourceDetails              `json:"sourceDetails"`
	CreateVnicDetails         createVnicDetails          `json:"createVnicDetails"`
	Metadata                  map[string]string          `json:"metadata"`
	FreeformTags              map[string]string          `json:"freeformTags"`
	PreemptibleInstanceConfig *preemptibleInstanceConfig `json:"preemptibleInstanceConfig,omitempty"`
}

// Provider returns the provider identifier used in durable state records.
func (c *Client) Provider() string { return "oci" }

// NewClient constructs a validated OCI Compute REST client.
func NewClient(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.TenancyOCID) == "" || strings.TrimSpace(cfg.UserOCID) == "" || strings.TrimSpace(cfg.Fingerprint) == "" {
		return nil, fmt.Errorf("OCI tenancy OCID, user OCID, and key fingerprint are required")
	}
	if strings.TrimSpace(cfg.CompartmentOCID) == "" || strings.TrimSpace(cfg.SubnetOCID) == "" ||
		strings.TrimSpace(cfg.AvailabilityDomain) == "" || strings.TrimSpace(cfg.ImageOCID) == "" {
		return nil, fmt.Errorf("OCI compartment OCID, subnet OCID, availability domain, and image OCID are required")
	}
	region := strings.ToLower(strings.TrimSpace(cfg.Region))
	if !regionRegex.MatchString(region) {
		return nil, fmt.Errorf("invalid OCI region %q", cfg.Region)
	}
	if !compute.ValidControllerID(cfg.ControllerID) {
		return nil, fmt.Errorf("controller ID must contain only letters, numbers, underscores, and hyphens")
	}
	if strings.TrimSpace(cfg.CloudInitPath) == "" {
		return nil, fmt.Errorf("OCI cloud-init template path is required")
	}
	tmpl, err := template.ParseFiles(cfg.CloudInitPath)
	if err != nil {
		return nil, fmt.Errorf("parse cloud-init template: %w", err)
	}

	privateKey, err := loadPrivateKey(cfg.PrivateKeyPath, cfg.PrivateKeyPEM)
	if err != nil {
		return nil, err
	}

	rawShape := strings.TrimSpace(cfg.Shape)
	if rawShape == "" {
		rawShape = defaultShape
	}
	ocpus := cfg.OCPUs
	memGB := cfg.MemoryGB
	if ocpus == 0 {
		ocpus = defaultOCPUs
	}
	if memGB == 0 {
		memGB = defaultMemoryGB
	}
	shape, shapeCfg, err := ParseShapeSpec(rawShape, ocpus, memGB)
	if err != nil {
		return nil, err
	}
	if shapeCfg != nil {
		ocpus = shapeCfg.OCPUs
		memGB = shapeCfg.MemoryInGBs
	}

	defaultURL := fmt.Sprintf("https://iaas.%s.oraclecloud.com/20160918", region)
	baseURL, err := compute.NormalizeBaseURL(cfg.BaseURL, defaultURL, "oci")
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(baseURL, "https://") {
		return nil, fmt.Errorf("OCI endpoint requires HTTPS")
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = compute.DefaultHTTPClient()
	}

	// Each signature is bound to one request target. Never replay launch data on a redirect.
	signedClient := *httpClient
	signedClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	return &Client{
		httpClient:           &signedClient,
		signer:               newRequestSigner(strings.TrimSpace(cfg.TenancyOCID), strings.TrimSpace(cfg.UserOCID), strings.TrimSpace(cfg.Fingerprint), privateKey),
		baseURL:              baseURL,
		cloudInitTmpl:        tmpl,
		compartmentOCID:      strings.TrimSpace(cfg.CompartmentOCID),
		subnetOCID:           strings.TrimSpace(cfg.SubnetOCID),
		availabilityDomain:   strings.TrimSpace(cfg.AvailabilityDomain),
		imageOCID:            strings.TrimSpace(cfg.ImageOCID),
		shape:                shape,
		ocpus:                ocpus,
		memoryGB:             memGB,
		sshPublicKey:         strings.TrimSpace(cfg.SSHPublicKey),
		assignPublicIP:       cfg.AssignPublicIP,
		spot:                 cfg.Spot,
		spotFallbackOnDemand: cfg.SpotFallbackOnDemand,
		controllerID:         cfg.ControllerID,
		controllerTag:        controllerTagKey + "=" + cfg.ControllerID,
		recoveryTimeout:      15 * time.Second,
		recoveryPoll:         2 * time.Second,
	}, nil
}

// ParseShapeSpec parses either a fixed OCI shape ("VM.Standard2.4") or a Flex
// shape ("VM.Standard.E4.Flex" with defaults, or inline "VM.Standard.E4.Flex:4:16").
func ParseShapeSpec(raw string, defaultOCPUsVal, defaultMemGBVal float64) (string, *shapeConfig, error) {
	trimmed := strings.TrimSpace(raw)
	if !compute.ValidSizeSpec(trimmed) {
		return "", nil, fmt.Errorf("invalid OCI shape %q", raw)
	}
	parts := strings.Split(trimmed, ":")
	shapeName := parts[0]
	isFlex := strings.HasSuffix(strings.ToLower(shapeName), ".flex")

	if len(parts) == 3 {
		ocpus, err := parsePositiveFloat(parts[1], "OCPUs")
		if err != nil {
			return "", nil, err
		}
		memGB, err := parsePositiveFloat(parts[2], "memory GB")
		if err != nil {
			return "", nil, err
		}
		return shapeName, &shapeConfig{OCPUs: ocpus, MemoryInGBs: memGB}, nil
	}
	if len(parts) != 1 {
		return "", nil, fmt.Errorf("invalid OCI flex shape spec %q (expected Shape or Shape:ocpus:memoryGB)", raw)
	}
	if !isFlex {
		return shapeName, nil, nil
	}
	if math.IsNaN(defaultOCPUsVal) || math.IsInf(defaultOCPUsVal, 0) || defaultOCPUsVal <= 0 {
		return "", nil, fmt.Errorf("OCI flex shape %q requires positive OCPUs", shapeName)
	}
	if math.IsNaN(defaultMemGBVal) || math.IsInf(defaultMemGBVal, 0) || defaultMemGBVal <= 0 {
		return "", nil, fmt.Errorf("OCI flex shape %q requires positive memory GB", shapeName)
	}
	return shapeName, &shapeConfig{OCPUs: defaultOCPUsVal, MemoryInGBs: defaultMemGBVal}, nil
}

func parsePositiveFloat(raw, label string) (float64, error) {
	val, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(val) || math.IsInf(val, 0) || val <= 0 {
		return 0, fmt.Errorf("invalid OCI %s value %q", label, raw)
	}
	return val, nil
}

// PricingSize expands bare Flex shapes using this client's validated launch dimensions.
func (c *Client) PricingSize(shape string) string {
	if shape == "" {
		shape = c.shape
	}
	if !strings.HasSuffix(strings.ToLower(shape), ".flex") {
		return shape
	}
	return shape + ":" + strconv.FormatFloat(c.ocpus, 'f', -1, 64) + ":" + strconv.FormatFloat(c.memoryGB, 'f', -1, 64)
}

// Health verifies OCI signature authentication and compartment read access.
func (c *Client) Health(ctx context.Context) error {
	query := url.Values{
		"compartmentId": {c.compartmentOCID},
		"limit":         {"1"},
	}
	var instances []ociInstance
	if err := c.doJSON(ctx, http.MethodGet, instancesPath+"?"+query.Encode(), nil, nil, &instances, nil); err != nil {
		return fmt.Errorf("oci health check: %w", err)
	}
	return nil
}

// FindRunner returns the controller-owned active OCI instance for jobKey.
func (c *Client) FindRunner(ctx context.Context, jobKey string) (*compute.RunnerInstance, bool, error) {
	existing, err := c.listJobInstances(ctx, jobKey, "")
	if err != nil {
		return nil, false, err
	}
	return compute.SelectSingleRunner("OCI", jobKey, existing, func(inst ociInstance) *compute.RunnerInstance {
		return &compute.RunnerInstance{ID: inst.ID, Name: inst.DisplayName}
	})
}

// CreateRunner provisions an ephemeral OCI Compute instance (standard or
// preemptible/spot) with ownership freeform tags and cloud-init user data.
func (c *Client) CreateRunner(ctx context.Context, params compute.RunnerParams) (*compute.RunnerInstance, error) {
	desiredName := compute.DeterministicName("oci", params.JobKey, params.ProvisionEpoch)
	if existing, found, err := c.FindRunner(ctx, params.JobKey); err != nil || found {
		return existing, err
	}
	shapeInput := c.shape
	if params.InstanceSize != "" {
		shapeInput = params.InstanceSize
	}
	shape, shapeCfg, err := ParseShapeSpec(shapeInput, c.ocpus, c.memoryGB)
	if err != nil {
		return nil, err
	}

	useSpot := compute.ResolveSpot(c.spot, params.Spot)
	if useSpot && !preemptibleShape.MatchString(shape) {
		if !c.spotFallbackOnDemand {
			return nil, fmt.Errorf("%w: OCI shape %s", compute.ErrSpotUnsupported, shape)
		}
		return c.launchInstance(ctx, params, desiredName, shape, shapeCfg, false, true)
	}
	inst, err := c.launchInstance(ctx, params, desiredName, shape, shapeCfg, useSpot, false)
	if err == nil {
		return inst, nil
	}
	if useSpot && c.spotFallbackOnDemand && compute.IsCapacityError(err) {
		log.Printf("WARN: OCI preemptible capacity unavailable for %s (%v); falling back to on-demand", params.JobKey, err)
		return c.launchInstance(ctx, params, desiredName, shape, shapeCfg, false, true)
	}
	return nil, err
}

func (c *Client) launchInstance(
	ctx context.Context,
	params compute.RunnerParams,
	desiredName, shape string,
	shapeCfg *shapeConfig,
	useSpot, isFallback bool,
) (*compute.RunnerInstance, error) {
	params.HTTPSAptMirrors = true
	userData, err := compute.RenderCloudInit(c.cloudInitTmpl, params.WithSpot(useSpot))
	if err != nil {
		return nil, err
	}
	metadata := map[string]string{
		"user_data": base64.StdEncoding.EncodeToString([]byte(userData)),
	}
	if c.sshPublicKey != "" {
		metadata["ssh_authorized_keys"] = c.sshPublicKey
	}
	reqBody := launchInstanceRequest{
		AvailabilityDomain: c.availabilityDomain,
		CompartmentID:      c.compartmentOCID,
		DisplayName:        desiredName,
		Shape:              shape,
		ShapeConfig:        shapeCfg,
		SourceDetails: sourceDetails{
			SourceType: "image",
			ImageID:    c.imageOCID,
		},
		CreateVnicDetails: createVnicDetails{
			SubnetID:       c.subnetOCID,
			AssignPublicIP: c.assignPublicIP,
		},
		Metadata: metadata,
		FreeformTags: map[string]string{
			controllerTagKey: c.controllerID,
			jobTagKey:        compute.JobTag(params.JobKey),
		},
	}
	if useSpot {
		reqBody.PreemptibleInstanceConfig = &preemptibleInstanceConfig{
			PreemptionAction: preemptionAction{
				Type:               "TERMINATE",
				PreserveBootVolume: false,
			},
		}
	}

	headers := map[string]string{
		"opc-retry-token": retryToken(params.JobKey, params.ProvisionEpoch, isFallback, useSpot),
	}
	var created ociInstance
	if err := c.doJSON(ctx, http.MethodPost, instancesPath, headers, reqBody, &created, nil); err != nil {
		if isCapacityError(err) {
			return nil, fmt.Errorf("%w: create OCI instance: %w", compute.ErrCapacityUnavailable, err)
		}
		return c.handleCreateError(ctx, params.JobKey, desiredName, err)
	}
	if created.ID == "" {
		return nil, fmt.Errorf("%w: OCI API returned no instance ID", compute.ErrCreateOutcomeUnknown)
	}
	log.Printf("Created OCI runner instance %s (ID: %s, shape: %s, preemptible: %t)", desiredName, created.ID, shape, useSpot)
	return &compute.RunnerInstance{ID: created.ID, Name: created.DisplayName}, nil
}

func (c *Client) handleCreateError(ctx context.Context, jobKey, desiredName string, createErr error) (*compute.RunnerInstance, error) {
	return compute.RecoverCreateError(
		ctx, "OCI", desiredName, c.recoveryTimeout, c.recoveryPoll, createErr,
		func(pollCtx context.Context) (*ociInstance, error) {
			instances, err := c.listJobInstances(pollCtx, jobKey, "")
			if err != nil {
				return nil, err
			}
			return compute.FindUniqueByName("OCI", jobKey, desiredName, instances, func(i ociInstance) string { return i.DisplayName })
		},
		func(i *ociInstance) *compute.RunnerInstance {
			return &compute.RunnerInstance{ID: i.ID, Name: i.DisplayName}
		},
	)
}

// RunnerAbsent checks authoritative absence by OCI instance OCID, treating explicit NotFound,
// TERMINATING, and TERMINATED instances as absent.
func (c *Client) RunnerAbsent(ctx context.Context, id string) (bool, error) {
	if !instanceIDRegex.MatchString(id) {
		return false, fmt.Errorf("invalid OCI instance ID %q", id)
	}
	var inst ociInstance
	err := c.doJSON(ctx, http.MethodGet, instancesBasePath+url.PathEscape(id), nil, nil, &inst, nil)
	if err != nil {
		if compute.IsHTTPNotFound(err) {
			return true, nil
		}
		return false, fmt.Errorf("confirm OCI instance %s absence: %w", id, err)
	}
	return isAbsentLifecycleState(inst.LifecycleState), nil
}

// DeleteRunner terminates an OCI instance only after verifying controller and
// job ownership freeform tags.
func (c *Client) DeleteRunner(ctx context.Context, id, jobKey string) error {
	return c.deleteOwned(ctx, id, compute.JobTag(jobKey))
}

// An orphan may have no job tag, but must still belong to this controller.
func (c *Client) deleteOwned(ctx context.Context, id, expectedJobTag string) error {
	if !instanceIDRegex.MatchString(id) {
		return fmt.Errorf("invalid OCI instance ID %q", id)
	}
	path := instancesBasePath + url.PathEscape(id)
	var inst ociInstance
	var responseHeaders http.Header
	if err := c.doJSON(ctx, http.MethodGet, path, nil, nil, &inst, &responseHeaders); err != nil {
		if compute.IsHTTPNotFound(err) {
			return nil
		}
		return fmt.Errorf("get OCI instance %s before delete: %w", id, err)
	}
	if isAbsentLifecycleState(inst.LifecycleState) {
		return nil
	}
	if inst.FreeformTags[controllerTagKey] != c.controllerID || (expectedJobTag != "" && inst.FreeformTags[jobTagKey] != expectedJobTag) {
		var confirm ociInstance
		if confirmErr := c.doJSON(ctx, http.MethodGet, path, nil, nil, &confirm, nil); confirmErr != nil {
			if compute.IsHTTPNotFound(confirmErr) {
				return nil
			}
			return fmt.Errorf("confirm OCI instance %s after ownership mismatch: %w", id, confirmErr)
		}
		if isAbsentLifecycleState(confirm.LifecycleState) {
			return nil
		}
		return fmt.Errorf("%w: refusing to delete OCI instance %s without controller and job ownership tags", compute.ErrOwnershipMismatch, id)
	}
	etag := responseHeaders.Get("ETag")
	if etag == "" {
		return fmt.Errorf("cannot conditionally delete OCI instance %s without an ETag", id)
	}
	if err := c.doJSON(ctx, http.MethodDelete, path+"?preserveBootVolume=false", map[string]string{"If-Match": etag}, nil, nil, nil); err != nil && !compute.IsHTTPNotFound(err) {
		return fmt.Errorf("delete owned OCI instance %s: %w", id, err)
	}
	return nil
}

// CleanupRunner deletes all controller-owned active OCI instances tagged for jobKey.
func (c *Client) CleanupRunner(ctx context.Context, jobKey string) error {
	return c.CleanupRunnerExcept(ctx, jobKey, "")
}

// CleanupRunnerExcept deletes job-tagged active instances other than excludedID.
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

// SweepOrphanedRunners reclaims controller-owned active OCI instances absent
// from known and older than cutoff across all opc-next-page result pages.
func (c *Client) SweepOrphanedRunners(ctx context.Context, known map[string]struct{}, cutoff time.Time) (int, error) {
	instances, err := c.listCompartmentInstances(ctx)
	if err != nil {
		return 0, fmt.Errorf("list controller OCI instances for orphan sweep: %w", err)
	}
	return compute.SweepOrphans(
		ctx, "OCI", c.controllerTag, known, cutoff, instances,
		func(i ociInstance) string { return i.ID },
		instanceTags,
		func(i ociInstance) (time.Time, error) {
			return time.Parse(time.RFC3339Nano, strings.TrimSpace(i.TimeCreated))
		},
		func(delCtx context.Context, id string) error {
			return c.deleteOwned(delCtx, id, "")
		},
	)
}

func (c *Client) listJobInstances(ctx context.Context, jobKey, excludedID string) ([]ociInstance, error) {
	all, err := c.listCompartmentInstances(ctx)
	if err != nil {
		return nil, fmt.Errorf("look up existing OCI runner instance: %w", err)
	}
	wantJob := compute.JobTag(jobKey)
	matched := make([]ociInstance, 0, len(all))
	for _, inst := range all {
		if inst.FreeformTags[jobTagKey] == wantJob {
			matched = append(matched, inst)
		}
	}
	return compute.FilterOwned("OCI", excludedID, c.controllerTag, matched,
		func(i ociInstance) string { return i.ID },
		instanceTags,
	)
}

func (c *Client) listCompartmentInstances(ctx context.Context) ([]ociInstance, error) {
	pageToken := ""
	var active []ociInstance
	for {
		query := url.Values{
			"compartmentId": {c.compartmentOCID},
			"limit":         {"100"},
		}
		if pageToken != "" {
			query.Set("page", pageToken)
		}
		var page []ociInstance
		var respHeaders http.Header
		if err := c.doJSON(ctx, http.MethodGet, instancesPath+"?"+query.Encode(), nil, nil, &page, &respHeaders); err != nil {
			return nil, err
		}
		for _, inst := range page {
			if isAbsentLifecycleState(inst.LifecycleState) {
				continue
			}
			active = append(active, inst)
		}
		next := strings.TrimSpace(respHeaders.Get("opc-next-page"))
		if next == "" || next == pageToken {
			break
		}
		pageToken = next
	}
	return active, nil
}

func (c *Client) doJSON(
	ctx context.Context,
	method, path string,
	headers map[string]string,
	reqBody, respBody any,
	respHeaders *http.Header,
) error {
	return compute.DoJSON(ctx, compute.RESTRequest{
		Provider:        "oci",
		Client:          c.httpClient,
		Method:          method,
		URL:             c.baseURL + path,
		SignRequest:     c.signer.Sign,
		Headers:         headers,
		Body:            reqBody,
		Out:             respBody,
		ResponseHeaders: respHeaders,
		ParseError:      parseOCIError,
	})
}

func parseOCIError(raw []byte) string {
	var payload struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(raw, &payload)
	if payload.Code != "" && payload.Message != "" {
		return payload.Code + ": " + payload.Message
	}
	if payload.Message != "" {
		return payload.Message
	}
	return payload.Code
}

func isCapacityError(err error) bool {
	var apiErr *compute.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	msg := strings.ToLower(apiErr.Message)
	return strings.Contains(msg, "out of host capacity") ||
		strings.Contains(msg, "limitexceeded") ||
		strings.Contains(msg, "quotaexceeded") ||
		strings.Contains(msg, "out of capacity")
}

func isAbsentLifecycleState(state string) bool {
	switch strings.ToUpper(strings.TrimSpace(state)) {
	case "TERMINATED", "TERMINATING":
		return true
	default:
		return false
	}
}

func instanceTags(inst ociInstance) []string {
	tags := make([]string, 0, len(inst.FreeformTags))
	for k, v := range inst.FreeformTags {
		tags = append(tags, k+"="+v)
	}
	return tags
}

func retryToken(jobKey string, epoch int, isFallback, spot bool) string {
	seed := fmt.Sprintf("oci:%s:%d:fb:%t:spot:%t", jobKey, epoch, isFallback, spot)
	sum := sha256.Sum256([]byte(seed))
	return "ghr-" + hex.EncodeToString(sum[:16])
}
