// SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
// SPDX-License-Identifier: MIT

// Package aws provisions ephemeral GitHub Actions runners on Amazon EC2.
package aws

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"text/template"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
	"github.com/thomasvincent/github-runners-infra/internal/compute"
)

const (
	controllerTagKey = "github-runners/controller"
	jobTagKey        = "github-runners/job"
)

type ec2API interface {
	DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	RunInstances(context.Context, *ec2.RunInstancesInput, ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error)
	TerminateInstances(context.Context, *ec2.TerminateInstancesInput, ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
}

// Config configures the EC2 runner pool. The SDK default credential chain is
// used; static cloud credentials are intentionally not accepted here.
type Config struct {
	Region             string
	AMI                string
	InstanceType       string
	SubnetID           string
	AlternateSubnetIDs []string
	// SecurityGroupIDs are operator/IaC-managed. This adapter defaults to no
	// public IP but cannot infer intended private-network ingress policy.
	SecurityGroupIDs     []string
	InstanceProfileARN   string
	KeyName              string
	CloudInitPath        string
	ControllerID         string
	Spot                 bool
	SpotFallbackOnDemand bool
	ExternalIP           bool
}

// Client owns EC2 resources created for one controller.
type Client struct {
	api                  ec2API
	tmpl                 *template.Template
	ami                  string
	instanceType         types.InstanceType
	subnetID             string
	alternateSubnetIDs   []string
	securityGroupIDs     []string
	instanceProfileARN   string
	keyName              string
	controllerID         string
	spot                 bool
	spotFallbackOnDemand bool
	externalIP           bool
}

var _ compute.Provider = (*Client)(nil)

func (c *Client) Provider() string { return "aws" }

// NewClient creates a client using AWS's standard environment, shared config,
// web identity, ECS, or EC2 role credential chain.
func NewClient(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.Region == "" || cfg.AMI == "" || cfg.InstanceType == "" || cfg.SubnetID == "" || len(cfg.SecurityGroupIDs) == 0 {
		return nil, fmt.Errorf("AWS region, AMI, instance type, subnet ID, and at least one security group are required")
	}
	if !compute.ValidInstanceSize(cfg.InstanceType) {
		return nil, fmt.Errorf("invalid AWS instance type %q", cfg.InstanceType)
	}
	if !compute.ValidControllerID(cfg.ControllerID) {
		return nil, fmt.Errorf("controller ID must contain only letters, numbers, underscores, and hyphens")
	}
	tmpl, err := template.ParseFiles(cfg.CloudInitPath)
	if err != nil {
		return nil, fmt.Errorf("parse cloud-init template: %w", err)
	}
	loaded, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.Region))
	if err != nil {
		return nil, fmt.Errorf("load AWS configuration: %w", err)
	}
	return newClient(ec2.NewFromConfig(loaded), tmpl, cfg), nil
}

func newClient(api ec2API, tmpl *template.Template, cfg Config) *Client {
	return &Client{
		api: api, tmpl: tmpl, ami: cfg.AMI, instanceType: types.InstanceType(cfg.InstanceType),
		subnetID: cfg.SubnetID, alternateSubnetIDs: append([]string(nil), cfg.AlternateSubnetIDs...),
		securityGroupIDs: cfg.SecurityGroupIDs, instanceProfileARN: cfg.InstanceProfileARN,
		keyName: cfg.KeyName, controllerID: cfg.ControllerID,
		spot: cfg.Spot, spotFallbackOnDemand: cfg.SpotFallbackOnDemand, externalIP: cfg.ExternalIP,
	}
}

func (c *Client) FindRunner(ctx context.Context, jobKey string) (*compute.RunnerInstance, bool, error) {
	instances, err := c.listJobInstances(ctx, jobKey)
	if err != nil {
		return nil, false, err
	}
	if len(instances) == 0 {
		return nil, false, nil
	}
	if len(instances) > 1 {
		return nil, false, fmt.Errorf("%w: multiple EC2 instances exist for job %s", compute.ErrDuplicateInstances, jobKey)
	}
	instance := instances[0]
	return &compute.RunnerInstance{ID: *instance.InstanceId, Name: tagValue(instance.Tags, "Name")}, true, nil
}

func (c *Client) listJobInstances(ctx context.Context, jobKey string) ([]types.Instance, error) {
	input := &ec2.DescribeInstancesInput{Filters: []types.Filter{
		{Name: aws.String("tag:" + controllerTagKey), Values: []string{c.controllerID}},
		{Name: aws.String("tag:" + jobTagKey), Values: []string{jobTag(jobKey)}},
		{Name: aws.String("instance-state-name"), Values: []string{"pending", "running", "stopping", "stopped"}},
	}}
	var instances []types.Instance
	for {
		out, err := c.api.DescribeInstances(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("find EC2 runner: %w", err)
		}
		for _, reservation := range out.Reservations {
			for _, instance := range reservation.Instances {
				if instance.InstanceId == nil {
					continue
				}
				if instance.State != nil && isAbsentState(instance.State.Name) {
					continue
				}
				if tagValue(instance.Tags, controllerTagKey) != c.controllerID || tagValue(instance.Tags, jobTagKey) != jobTag(jobKey) {
					return nil, fmt.Errorf("%w: EC2 instance %s lacks controller or job ownership tags", compute.ErrOwnershipMismatch, *instance.InstanceId)
				}
				instances = append(instances, instance)
			}
		}
		if out.NextToken == nil || *out.NextToken == "" {
			return instances, nil
		}
		input.NextToken = out.NextToken
	}
}

func (c *Client) CreateRunner(ctx context.Context, params compute.RunnerParams) (*compute.RunnerInstance, error) {
	if existing, found, err := c.FindRunner(ctx, params.JobKey); err != nil || found {
		return existing, err
	}
	instanceType := c.instanceType
	if params.InstanceSize != "" {
		if !compute.ValidInstanceSize(params.InstanceSize) {
			return nil, fmt.Errorf("invalid AWS instance type %q", params.InstanceSize)
		}
		instanceType = types.InstanceType(params.InstanceSize)
	}
	useSpot := compute.ResolveSpot(c.spot, params.Spot)
	subnets := c.candidateSubnets()
	if len(subnets) == 0 {
		return nil, fmt.Errorf("AWS runner requires at least one nonempty subnet ID")
	}

	inst, err := c.runAcrossSubnets(ctx, params, instanceType, subnets, useSpot, false)
	if err == nil {
		return inst, nil
	}
	if useSpot && c.spotFallbackOnDemand && compute.IsCapacityError(err) {
		if err := params.CheckLaunchBudget(c.Provider(), string(instanceType), false); err != nil {
			return nil, err
		}
		log.Printf("WARN: AWS spot capacity unavailable for %s (%v); falling back to on-demand", params.JobKey, err)
		return c.runAcrossSubnets(ctx, params, instanceType, subnets, false, true)
	}
	return nil, err
}

func (c *Client) candidateSubnets() []string {
	subnets := make([]string, 0, 1+len(c.alternateSubnetIDs))
	seen := make(map[string]struct{}, 1+len(c.alternateSubnetIDs))
	for _, raw := range append([]string{c.subnetID}, c.alternateSubnetIDs...) {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		subnets = append(subnets, id)
	}
	return subnets
}

func (c *Client) runAcrossSubnets(
	ctx context.Context,
	params compute.RunnerParams,
	instanceType types.InstanceType,
	subnets []string,
	useSpot, isFallback bool,
) (*compute.RunnerInstance, error) {
	userData, err := compute.RenderCloudInit(c.tmpl, params.WithSpot(useSpot))
	if err != nil {
		return nil, err
	}
	var lastErr error
	for idx, subnetID := range subnets {
		input := c.buildRunInput(params, instanceType, subnetID, userData, useSpot, idx, isFallback)
		out, runErr := c.api.RunInstances(ctx, input)
		if runErr == nil {
			if len(out.Instances) != 1 || out.Instances[0].InstanceId == nil {
				return nil, fmt.Errorf("create EC2 runner: API returned no instance identity")
			}
			return &compute.RunnerInstance{ID: *out.Instances[0].InstanceId, Name: params.RunnerName}, nil
		}
		lastErr = classifyCreateError(runErr)
		if !compute.IsCapacityError(lastErr) {
			return nil, lastErr
		}
		if idx+1 < len(subnets) {
			log.Printf("WARN: AWS subnet %s capacity unavailable for %s (%v); trying alternate subnet %s", subnetID, params.JobKey, runErr, subnets[idx+1])
		}
	}
	return nil, lastErr
}

func (c *Client) buildRunInput(
	params compute.RunnerParams,
	instanceType types.InstanceType,
	subnetID, userData string,
	useSpot bool,
	subnetIdx int,
	isFallback bool,
) *ec2.RunInstancesInput {
	token := clientTokenForAttempt(params.JobKey, params.ProvisionEpoch, subnetIdx, useSpot, isFallback)
	input := &ec2.RunInstancesInput{
		ImageId: aws.String(c.ami), InstanceType: instanceType, MinCount: aws.Int32(1), MaxCount: aws.Int32(1),
		NetworkInterfaces: []types.InstanceNetworkInterfaceSpecification{{
			AssociatePublicIpAddress: aws.Bool(c.externalIP), DeviceIndex: aws.Int32(0),
			SubnetId: aws.String(subnetID), Groups: append([]string(nil), c.securityGroupIDs...),
		}},
		UserData:    aws.String(base64.StdEncoding.EncodeToString([]byte(userData))),
		ClientToken: aws.String(token),
		MetadataOptions: &types.InstanceMetadataOptionsRequest{
			HttpEndpoint: types.InstanceMetadataEndpointStateEnabled, HttpTokens: types.HttpTokensStateRequired,
			HttpPutResponseHopLimit: aws.Int32(1), InstanceMetadataTags: types.InstanceMetadataTagsStateDisabled,
		},
		TagSpecifications: []types.TagSpecification{{ResourceType: types.ResourceTypeInstance, Tags: []types.Tag{
			{Key: aws.String("Name"), Value: aws.String(params.RunnerName)},
			{Key: aws.String(controllerTagKey), Value: aws.String(c.controllerID)},
			{Key: aws.String(jobTagKey), Value: aws.String(jobTag(params.JobKey))},
		}}},
	}
	if c.instanceProfileARN != "" {
		input.IamInstanceProfile = &types.IamInstanceProfileSpecification{Arn: aws.String(c.instanceProfileARN)}
	}
	if c.keyName != "" {
		input.KeyName = aws.String(c.keyName)
	}
	if useSpot {
		input.InstanceMarketOptions = &types.InstanceMarketOptionsRequest{
			MarketType: types.MarketTypeSpot,
			SpotOptions: &types.SpotMarketOptions{
				InstanceInterruptionBehavior: types.InstanceInterruptionBehaviorTerminate,
				SpotInstanceType:             types.SpotInstanceTypeOneTime,
			},
		}
	}
	return input
}

// RunnerAbsent checks authoritative absence by EC2 instance ID, treating
// missing, shutting-down, and terminated/preempted instances as absent.
func (c *Client) RunnerAbsent(ctx context.Context, instanceID string) (bool, error) {
	out, err := c.api.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{instanceID}})
	if err != nil {
		if isNotFound(err) {
			return true, nil
		}
		return false, fmt.Errorf("confirm EC2 runner %s absence: %w", instanceID, err)
	}
	for i := range out.Reservations {
		if len(out.Reservations[i].Instances) == 0 {
			continue
		}
		inst := out.Reservations[i].Instances[0]
		if inst.State != nil && isAbsentState(inst.State.Name) {
			return true, nil
		}
		return false, nil
	}
	return true, nil
}

func (c *Client) DeleteRunner(ctx context.Context, instanceID, jobKey string) error {
	out, err := c.api.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{instanceID}})
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("get EC2 runner %s before delete: %w", instanceID, err)
	}
	var instance *types.Instance
	for i := range out.Reservations {
		if len(out.Reservations[i].Instances) != 0 {
			instance = &out.Reservations[i].Instances[0]
			break
		}
	}
	if instance == nil {
		return nil
	}
	if instance.State != nil && (instance.State.Name == types.InstanceStateNameShuttingDown || instance.State.Name == types.InstanceStateNameTerminated) {
		return nil
	}
	if tagValue(instance.Tags, controllerTagKey) != c.controllerID || tagValue(instance.Tags, jobTagKey) != jobTag(jobKey) {
		return fmt.Errorf("%w: refusing to delete EC2 instance %s without controller and job ownership tags", compute.ErrOwnershipMismatch, instanceID)
	}
	_, err = c.api.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{instanceID}})
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("terminate owned EC2 runner %s: %w", instanceID, err)
	}
	return nil
}

func isNotFound(err error) bool {
	var apiErr smithy.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode() == "InvalidInstanceID.NotFound"
}

func isAbsentState(state types.InstanceStateName) bool {
	return state == types.InstanceStateNameShuttingDown || state == types.InstanceStateNameTerminated
}

func classifyCreateError(err error) error {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "InsufficientInstanceCapacity", "SpotMaxPriceTooLow", "InstanceLimitExceeded", "MaxSpotInstanceCountExceeded":
			return fmt.Errorf("%w: create EC2 runner: %w", compute.ErrCapacityUnavailable, err)
		}
	}
	return fmt.Errorf("create EC2 runner: %w", err)
}

func (c *Client) CleanupRunner(ctx context.Context, jobKey string) error {
	instances, err := c.listJobInstances(ctx, jobKey)
	if err != nil {
		return err
	}
	for _, instance := range instances {
		if err := c.DeleteRunner(ctx, *instance.InstanceId, jobKey); err != nil {
			return err
		}
	}
	return nil
}

// SweepOrphanedRunners reclaims old controller-owned EC2 instances that no
// longer have a durable lifecycle record.
func (c *Client) SweepOrphanedRunners(ctx context.Context, known map[string]struct{}, cutoff time.Time) (int, error) {
	input := &ec2.DescribeInstancesInput{Filters: []types.Filter{
		{Name: aws.String("tag:" + controllerTagKey), Values: []string{c.controllerID}},
		{Name: aws.String("instance-state-name"), Values: []string{"pending", "running", "stopping", "stopped"}},
	}}
	deleted := 0
	for {
		out, err := c.api.DescribeInstances(ctx, input)
		if err != nil {
			return deleted, fmt.Errorf("list controller EC2 instances for orphan sweep: %w", err)
		}
		for _, reservation := range out.Reservations {
			for _, instance := range reservation.Instances {
				if instance.InstanceId == nil {
					return deleted, fmt.Errorf("controller EC2 instance has no instance ID")
				}
				id := *instance.InstanceId
				if tagValue(instance.Tags, controllerTagKey) != c.controllerID {
					return deleted, fmt.Errorf("%w: EC2 orphan sweep returned unowned instance %s", compute.ErrOwnershipMismatch, id)
				}
				if instance.LaunchTime == nil {
					return deleted, fmt.Errorf("controller-owned EC2 instance %s has no launch time", id)
				}
				if _, ok := known[id]; ok || !instance.LaunchTime.Before(cutoff) {
					continue
				}
				if _, err := c.api.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{id}}); err != nil && !isNotFound(err) {
					return deleted, fmt.Errorf("terminate orphaned controller EC2 instance %s: %w", id, err)
				}
				deleted++
			}
		}
		if out.NextToken == nil || *out.NextToken == "" {
			break
		}
		input.NextToken = out.NextToken
	}
	return deleted, nil
}

func tagValue(tags []types.Tag, key string) string {
	for _, tag := range tags {
		if tag.Key != nil && tag.Value != nil && *tag.Key == key {
			return *tag.Value
		}
	}
	return ""
}

func jobTag(jobKey string) string {
	hash := sha256.Sum256([]byte(jobKey))
	return hex.EncodeToString(hash[:16])
}

func clientToken(jobKey string, epoch int) string {
	hash := sha256.Sum256([]byte(fmt.Sprintf("aws:%s:%d", jobKey, epoch)))
	return "github-runner-" + hex.EncodeToString(hash[:16])
}

func clientTokenForAttempt(jobKey string, epoch, subnetIdx int, useSpot, isFallback bool) string {
	if subnetIdx == 0 && !isFallback {
		return clientToken(jobKey, epoch)
	}
	hash := sha256.Sum256([]byte(fmt.Sprintf("aws:%s:%d:subnet:%d:spot:%t:fb:%t", jobKey, epoch, subnetIdx, useSpot, isFallback)))
	return "github-runner-" + hex.EncodeToString(hash[:16])
}
