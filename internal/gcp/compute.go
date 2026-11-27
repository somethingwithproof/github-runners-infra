// SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
// SPDX-License-Identifier: MIT

// Package gcp provisions ephemeral GitHub Actions runners on Compute Engine.
package gcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"text/template"
	"time"

	gce "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/thomasvincent/github-runners-infra/internal/compute"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type instancesAPI interface {
	Get(context.Context, *computepb.GetInstanceRequest) (*computepb.Instance, error)
	Insert(context.Context, *computepb.InsertInstanceRequest) (operation, error)
	Delete(context.Context, *computepb.DeleteInstanceRequest) (operation, error)
	List(context.Context, *computepb.ListInstancesRequest) ([]*computepb.Instance, error)
	Close() error
}

type operation interface {
	Wait(context.Context) error
}

type operationFunc func(context.Context) error

func (f operationFunc) Wait(ctx context.Context) error { return f(ctx) }

type instancesClient struct{ client *gce.InstancesClient }

func (c *instancesClient) Get(ctx context.Context, request *computepb.GetInstanceRequest) (*computepb.Instance, error) {
	return c.client.Get(ctx, request)
}

func (c *instancesClient) Insert(ctx context.Context, request *computepb.InsertInstanceRequest) (operation, error) {
	op, err := c.client.Insert(ctx, request)
	if err != nil {
		return nil, err
	}
	return operationFunc(func(waitCtx context.Context) error { return op.Wait(waitCtx) }), nil
}

func (c *instancesClient) Delete(ctx context.Context, request *computepb.DeleteInstanceRequest) (operation, error) {
	op, err := c.client.Delete(ctx, request)
	if err != nil {
		return nil, err
	}
	return operationFunc(func(waitCtx context.Context) error { return op.Wait(waitCtx) }), nil
}

func (c *instancesClient) List(ctx context.Context, request *computepb.ListInstancesRequest) ([]*computepb.Instance, error) {
	iter := c.client.List(ctx, request)
	var instances []*computepb.Instance
	for {
		instance, err := iter.Next()
		if errors.Is(err, iterator.Done) {
			return instances, nil
		}
		if err != nil {
			return nil, err
		}
		instances = append(instances, instance)
	}
}

func (c *instancesClient) Close() error { return c.client.Close() }

const (
	controllerLabel = "github-runners-controller"
	jobLabel        = "github-runners-job"
	runnerScope     = "https://www.googleapis.com/auth/userinfo.email"
)

// Config configures a zonal Compute Engine runner pool. Application Default
// Credentials are used; credentials are never accepted as configuration.
type Config struct {
	ProjectID   string
	Zone        string
	MachineType string
	SourceImage string
	// Subnetwork firewall policy is operator/IaC-managed; ExternalIP defaults false.
	Subnetwork    string
	CloudInitPath string
	ControllerID  string
	// ServiceAccountEmail identifies a dedicated runner identity whose IAM
	// roles must grant no cloud access beyond the workload's explicit needs.
	ServiceAccountEmail  string
	Spot                 bool
	SpotFallbackOnDemand bool
	ExternalIP           bool
}

// Client owns Compute Engine instances created for one controller.
type Client struct {
	instances            instancesAPI
	tmpl                 *template.Template
	projectID            string
	zone                 string
	machineType          string
	sourceImage          string
	subnetwork           string
	serviceAccountEmail  string
	controllerHash       string
	spot                 bool
	spotFallbackOnDemand bool
	externalIP           bool
}

var _ compute.Provider = (*Client)(nil)

func (c *Client) Provider() string { return "gcp" }

func NewClient(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.ProjectID == "" || cfg.Zone == "" || cfg.MachineType == "" || cfg.SourceImage == "" || cfg.Subnetwork == "" || cfg.ServiceAccountEmail == "" {
		return nil, fmt.Errorf("GCP project, zone, machine type, source image, subnetwork, and runner service account are required")
	}
	if !compute.ValidInstanceSize(cfg.MachineType) {
		return nil, fmt.Errorf("invalid GCP machine type %q", cfg.MachineType)
	}
	if !compute.ValidControllerID(cfg.ControllerID) {
		return nil, fmt.Errorf("controller ID must contain only letters, numbers, underscores, and hyphens")
	}
	tmpl, err := template.ParseFiles(cfg.CloudInitPath)
	if err != nil {
		return nil, fmt.Errorf("parse cloud-init template: %w", err)
	}
	instances, err := gce.NewInstancesRESTClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("create GCP Compute client: %w", err)
	}
	return &Client{
		instances: &instancesClient{client: instances}, tmpl: tmpl, projectID: cfg.ProjectID, zone: cfg.Zone,
		machineType: cfg.MachineType, sourceImage: cfg.SourceImage, subnetwork: cfg.Subnetwork,
		serviceAccountEmail:  cfg.ServiceAccountEmail,
		controllerHash:       shortHash(cfg.ControllerID),
		spot:                 cfg.Spot,
		spotFallbackOnDemand: cfg.SpotFallbackOnDemand,
		externalIP:           cfg.ExternalIP,
	}, nil
}

// Close releases transports owned by the Google client.
func (c *Client) Close() error { return c.instances.Close() }

func (c *Client) FindRunner(ctx context.Context, jobKey string) (*compute.RunnerInstance, bool, error) {
	name := resourceName(jobKey)
	instance, err := c.instances.Get(ctx, &computepb.GetInstanceRequest{Project: c.projectID, Zone: c.zone, Instance: name})
	if err != nil {
		if isNotFound(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("find GCP runner: %w", err)
	}
	if instance.Labels[controllerLabel] != c.controllerHash || instance.Labels[jobLabel] != shortHash(jobKey) {
		return nil, false, fmt.Errorf("%w: GCP instance %s exists without expected ownership labels", compute.ErrOwnershipMismatch, name)
	}
	if isAbsentStatus(instance.GetStatus()) {
		return nil, false, nil
	}
	return &compute.RunnerInstance{ID: name, Name: name}, true, nil
}

// RunnerAbsent checks authoritative absence by GCP instance name, treating
// missing or TERMINATED/preempted instances as absent.
func (c *Client) RunnerAbsent(ctx context.Context, instanceID string) (bool, error) {
	instance, err := c.instances.Get(ctx, &computepb.GetInstanceRequest{Project: c.projectID, Zone: c.zone, Instance: instanceID})
	if err != nil {
		if isNotFound(err) {
			return true, nil
		}
		return false, fmt.Errorf("confirm GCP runner %s absence: %w", instanceID, err)
	}
	return isAbsentStatus(instance.GetStatus()), nil
}

func (c *Client) CreateRunner(ctx context.Context, params compute.RunnerParams) (*compute.RunnerInstance, error) {
	if existing, found, err := c.FindRunner(ctx, params.JobKey); err != nil || found {
		return existing, err
	}
	machineType := c.machineType
	if params.InstanceSize != "" {
		if !compute.ValidInstanceSize(params.InstanceSize) {
			return nil, fmt.Errorf("invalid GCP machine type %q", params.InstanceSize)
		}
		machineType = params.InstanceSize
	}
	useSpot := compute.ResolveSpot(c.spot, params.Spot)
	inst, err := c.insertInstance(ctx, params, machineType, useSpot, requestID(params.JobKey, params.ProvisionEpoch))
	if err == nil {
		return inst, nil
	}
	if useSpot && c.spotFallbackOnDemand && compute.IsCapacityError(err) {
		log.Printf("WARN: GCP spot capacity unavailable for %s (%v); falling back to on-demand", params.JobKey, err)
		return c.insertInstance(ctx, params, machineType, false, requestIDForFallback(params.JobKey, params.ProvisionEpoch))
	}
	return nil, err
}

func (c *Client) insertInstance(
	ctx context.Context,
	params compute.RunnerParams,
	machineType string,
	useSpot bool,
	reqID string,
) (*compute.RunnerInstance, error) {
	userData, err := compute.RenderCloudInit(c.tmpl, params.WithSpot(useSpot))
	if err != nil {
		return nil, err
	}
	name := resourceName(params.JobKey)
	instance := c.instanceSpecForMode(params.JobKey, machineType, userData, useSpot)
	op, err := c.instances.Insert(ctx, &computepb.InsertInstanceRequest{
		Project: c.projectID, Zone: c.zone, InstanceResource: instance,
		RequestId: stringPtr(reqID),
	})
	if err != nil {
		return nil, classifyCreateError("create GCP runner", err)
	}
	if err := op.Wait(ctx); err != nil {
		return nil, classifyCreateError("wait for GCP runner creation", err)
	}
	return &compute.RunnerInstance{ID: name, Name: name}, nil
}

func (c *Client) instanceSpec(jobKey, machineType, userData string) *computepb.Instance {
	return c.instanceSpecForMode(jobKey, machineType, userData, c.spot)
}

func (c *Client) instanceSpecForMode(jobKey, machineType, userData string, useSpot bool) *computepb.Instance {
	name := resourceName(jobKey)
	boot, autoDelete := true, true
	network := &computepb.NetworkInterface{Subnetwork: stringPtr(c.subnetwork)}
	if c.externalIP {
		network.AccessConfigs = []*computepb.AccessConfig{{
			Name: stringPtr("External NAT"), Type: stringPtr("ONE_TO_ONE_NAT"), NetworkTier: stringPtr("PREMIUM"),
		}}
	}
	instance := &computepb.Instance{
		Name: stringPtr(name), MachineType: stringPtr("zones/" + c.zone + "/machineTypes/" + machineType),
		Labels: map[string]string{controllerLabel: c.controllerHash, jobLabel: shortHash(jobKey)},
		Disks: []*computepb.AttachedDisk{{Boot: &boot, AutoDelete: &autoDelete, InitializeParams: &computepb.AttachedDiskInitializeParams{
			SourceImage: stringPtr(c.sourceImage),
		}}},
		NetworkInterfaces: []*computepb.NetworkInterface{network},
		ServiceAccounts: []*computepb.ServiceAccount{{
			Email: stringPtr(c.serviceAccountEmail), Scopes: []string{runnerScope},
		}},
		Metadata: &computepb.Metadata{Items: []*computepb.Items{{Key: stringPtr("user-data"), Value: &userData}}},
	}
	if useSpot {
		automaticRestart := false
		instance.Scheduling = &computepb.Scheduling{
			ProvisioningModel: stringPtr("SPOT"), InstanceTerminationAction: stringPtr("DELETE"),
			AutomaticRestart: &automaticRestart, OnHostMaintenance: stringPtr("TERMINATE"),
		}
	}
	return instance
}

func (c *Client) DeleteRunner(ctx context.Context, instanceID, jobKey string) error {
	instance, err := c.instances.Get(ctx, &computepb.GetInstanceRequest{Project: c.projectID, Zone: c.zone, Instance: instanceID})
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("get GCP runner %s before delete: %w", instanceID, err)
	}
	if instance.Labels[controllerLabel] != c.controllerHash || instance.Labels[jobLabel] != shortHash(jobKey) {
		return fmt.Errorf("%w: refusing to delete GCP instance %s without controller and job ownership labels", compute.ErrOwnershipMismatch, instanceID)
	}
	op, err := c.instances.Delete(ctx, &computepb.DeleteInstanceRequest{Project: c.projectID, Zone: c.zone, Instance: instanceID})
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("delete owned GCP runner %s: %w", instanceID, err)
	}
	if err := op.Wait(ctx); err != nil && status.Code(err) != codes.NotFound {
		return fmt.Errorf("wait for GCP runner deletion: %w", err)
	}
	return nil
}

func isNotFound(err error) bool {
	if status.Code(err) == codes.NotFound {
		return true
	}
	var apiErr *googleapi.Error
	return errors.As(err, &apiErr) && apiErr.Code == 404
}

func isAbsentStatus(instanceStatus string) bool {
	return strings.EqualFold(strings.TrimSpace(instanceStatus), "TERMINATED")
}

func classifyCreateError(action string, err error) error {
	if isCapacityError(err) {
		return fmt.Errorf("%w: %s: %w", compute.ErrCapacityUnavailable, action, err)
	}
	return fmt.Errorf("%s: %w", action, err)
}

func isCapacityError(err error) bool {
	if status.Code(err) == codes.ResourceExhausted {
		return true
	}
	msg := strings.ToUpper(err.Error())
	if strings.Contains(msg, "ZONE_RESOURCE_POOL_EXHAUSTED") || strings.Contains(msg, "RESOURCE_EXHAUSTED") {
		return true
	}
	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) {
		for _, item := range apiErr.Errors {
			reason := strings.ToUpper(item.Reason)
			if strings.Contains(reason, "ZONE_RESOURCE_POOL_EXHAUSTED") || strings.Contains(reason, "RESOURCE_EXHAUSTED") {
				return true
			}
		}
	}
	return false
}

func (c *Client) CleanupRunner(ctx context.Context, jobKey string) error {
	instance, found, err := c.FindRunner(ctx, jobKey)
	if err != nil || !found {
		return err
	}
	return c.DeleteRunner(ctx, instance.ID, jobKey)
}

// SweepOrphanedRunners reclaims old controller-owned instances that are absent
// from durable state.
func (c *Client) SweepOrphanedRunners(ctx context.Context, known map[string]struct{}, cutoff time.Time) (int, error) {
	instances, err := c.instances.List(ctx, &computepb.ListInstancesRequest{Project: c.projectID, Zone: c.zone})
	if err != nil {
		return 0, fmt.Errorf("list GCP instances for orphan sweep: %w", err)
	}
	deleted := 0
	// One instance with an unreadable creation timestamp must not stop the
	// sweep; otherwise a single bad record blocks reclamation indefinitely.
	var parseErrs []error
	for _, instance := range instances {
		if instance == nil || instance.Labels[controllerLabel] != c.controllerHash {
			continue
		}
		name := instance.GetName()
		if _, ok := known[name]; ok {
			continue
		}
		created, err := time.Parse(time.RFC3339, instance.GetCreationTimestamp())
		if err != nil {
			// Age is unknown, so keep the instance rather than delete it blind.
			parseErrs = append(parseErrs, fmt.Errorf("parse creation time for controller GCP instance %s: %w", name, err))
			continue
		}
		if !created.Before(cutoff) {
			continue
		}
		op, err := c.instances.Delete(ctx, &computepb.DeleteInstanceRequest{Project: c.projectID, Zone: c.zone, Instance: name})
		if err != nil {
			if isNotFound(err) {
				continue
			}
			return deleted, fmt.Errorf("delete orphaned controller GCP instance %s: %w", name, err)
		}
		if err := op.Wait(ctx); err != nil && status.Code(err) != codes.NotFound {
			return deleted, fmt.Errorf("wait for orphaned GCP instance deletion: %w", err)
		}
		deleted++
	}
	if len(parseErrs) > 0 {
		return deleted, errors.Join(parseErrs...)
	}
	return deleted, nil
}

func resourceName(jobKey string) string { return "ghr-" + shortHash(jobKey) }

func shortHash(value string) string {
	hash := sha256.Sum256([]byte(value))
	return hex.EncodeToString(hash[:16])
}

func requestID(jobKey string, epoch int) string {
	return deterministicUUID(fmt.Sprintf("gcp:%s:%d", jobKey, epoch))
}

func requestIDForFallback(jobKey string, epoch int) string {
	return deterministicUUID(fmt.Sprintf("gcp:%s:%d:ondemand", jobKey, epoch))
}

func deterministicUUID(seed string) string {
	hash := sha256.Sum256([]byte(seed))
	rawBytes := hash[:16]
	rawBytes[6] = (rawBytes[6] & 0x0f) | 0x40
	rawBytes[8] = (rawBytes[8] & 0x3f) | 0x80
	raw := hex.EncodeToString(rawBytes)
	return raw[:8] + "-" + raw[8:12] + "-" + raw[12:16] + "-" + raw[16:20] + "-" + raw[20:]
}

func stringPtr(value string) *string { return &value }
