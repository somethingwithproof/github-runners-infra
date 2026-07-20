// Package gcp provisions ephemeral GitHub Actions runners on Compute Engine
// Spot VMs. It mirrors the digitalocean package surface and satisfies
// provider.Provisioner so the webhook handler and cleanup job stay
// cloud-agnostic.
package gcp

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"text/template"
	"time"

	compute "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/proto"

	"github.com/thomasvincent/github-runners-infra/internal/provider"
)

// Labels applied to every runner instance so cleanup can find orphans by label,
// mirroring the DigitalOcean "github-runner"/"ephemeral" tags.
const (
	labelManagedBy = "managed-by"
	labelRole      = "role"
	managedByValue = "github-runners-infra"
	roleValue      = "ephemeral-runner"
)

// runnerFilter selects only instances this autoscaler manages.
const runnerFilter = "labels." + labelManagedBy + "=" + managedByValue

// instancesAPI is the subset of *compute.InstancesClient this provisioner uses.
// It exists so unit tests can inject a fake at the GCP boundary; the real client
// satisfies it directly.
type instancesAPI interface {
	Insert(ctx context.Context, req *computepb.InsertInstanceRequest, opts ...gaxCallOption) (operation, error)
	Delete(ctx context.Context, req *computepb.DeleteInstanceRequest, opts ...gaxCallOption) (operation, error)
	List(ctx context.Context, req *computepb.ListInstancesRequest, opts ...gaxCallOption) instanceIterator
}

// operation is the subset of *compute.Operation callers wait on.
type operation interface {
	Wait(ctx context.Context, opts ...gaxCallOption) error
}

// instanceIterator is the subset of *compute.InstanceIterator callers page over.
// It returns iterator.Done when exhausted.
type instanceIterator interface {
	Next() (*computepb.Instance, error)
}

// Client provisions runner VMs on Compute Engine.
type Client struct {
	instances   instancesAPI
	startupTmpl *template.Template
	project     string
	zone        string
	machineType string
	runnerSA    string
	network     string
	subnet      string
	sourceImage string
}

// Config holds Compute Engine client configuration.
type Config struct {
	Project           string
	Zone              string
	MachineType       string
	RunnerSA          string
	Network           string
	Subnet            string
	SourceImage       string
	StartupScriptPath string
}

const (
	defaultMachineType = "e2-custom-4-8192"
	defaultImage       = "projects/ubuntu-os-cloud/global/images/family/ubuntu-2404-lts-amd64"
	defaultNetwork     = "global/networks/default"
)

// NewClient creates a Compute Engine runner provisioner. The instances client
// is created against application-default credentials; callers that need a fake
// (tests) use newClientWithAPI instead.
func NewClient(ctx context.Context, cfg Config) (*Client, error) {
	raw, err := compute.NewInstancesRESTClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("create instances client: %w", err)
	}
	return newClientWithAPI(restAPI{raw}, cfg)
}

func newClientWithAPI(api instancesAPI, cfg Config) (*Client, error) {
	if cfg.Project == "" {
		return nil, fmt.Errorf("GCP project is required")
	}
	if cfg.Zone == "" {
		return nil, fmt.Errorf("GCP zone is required")
	}
	if cfg.StartupScriptPath == "" {
		return nil, fmt.Errorf("GCP startup-script path is required")
	}

	tmpl, err := template.ParseFiles(cfg.StartupScriptPath)
	if err != nil {
		return nil, fmt.Errorf("parse startup-script template: %w", err)
	}

	machineType := cfg.MachineType
	if machineType == "" {
		machineType = defaultMachineType
	}
	image := cfg.SourceImage
	if image == "" {
		image = defaultImage
	}
	network := cfg.Network
	if network == "" {
		network = defaultNetwork
	}

	return &Client{
		instances:   api,
		startupTmpl: tmpl,
		project:     cfg.Project,
		zone:        cfg.Zone,
		machineType: machineType,
		runnerSA:    cfg.RunnerSA,
		network:     network,
		subnet:      cfg.Subnet,
		sourceImage: image,
	}, nil
}

// Create spins up a SPOT runner instance for one job. The instance has an
// ephemeral external IP for outbound access and runs the rendered startup script (installs Docker + the runner,
// registers --ephemeral, runs one job, exits), and is labelled so cleanup can
// reap it. Teardown is control-plane-driven: the webhook deletes the VM on the
// job's completed event, with the reaper as backstop.
func (c *Client) Create(ctx context.Context, params provider.RunnerParams) (provider.Instance, error) {
	var startup bytes.Buffer
	if err := c.startupTmpl.Execute(&startup, params); err != nil {
		return provider.Instance{}, fmt.Errorf("render startup-script: %w", err)
	}

	instance := c.buildInstance(params.RunnerName, startup.String())

	op, err := c.instances.Insert(ctx, &computepb.InsertInstanceRequest{
		Project:          c.project,
		Zone:             c.zone,
		InstanceResource: instance,
	})
	if err != nil {
		return provider.Instance{}, fmt.Errorf("insert instance: %w", err)
	}
	if err := op.Wait(ctx); err != nil {
		return provider.Instance{}, fmt.Errorf("wait insert instance: %w", err)
	}

	log.Printf("Created runner instance %s (zone: %s)", params.RunnerName, c.zone)
	return provider.Instance{Name: params.RunnerName, ID: params.RunnerName}, nil
}

func (c *Client) buildInstance(name, startupScript string) *computepb.Instance {
	machineType := fmt.Sprintf("zones/%s/machineTypes/%s", c.zone, c.machineType)

	netIface := &computepb.NetworkInterface{
		Network: proto.String(c.network),
		AccessConfigs: []*computepb.AccessConfig{
			{
				Name: proto.String("External NAT"),
				Type: proto.String("ONE_TO_ONE_NAT"),
			},
		},
	}
	if c.subnet != "" {
		netIface.Subnetwork = proto.String(c.subnet)
	}

	inst := &computepb.Instance{
		Name:        proto.String(name),
		MachineType: proto.String(machineType),
		Labels: map[string]string{
			labelManagedBy: managedByValue,
			labelRole:      roleValue,
		},
		Scheduling: &computepb.Scheduling{
			ProvisioningModel:         proto.String("SPOT"),
			Preemptible:               proto.Bool(true),
			AutomaticRestart:          proto.Bool(false),
			InstanceTerminationAction: proto.String("DELETE"),
		},
		Disks: []*computepb.AttachedDisk{
			{
				Boot:       proto.Bool(true),
				AutoDelete: proto.Bool(true),
				InitializeParams: &computepb.AttachedDiskInitializeParams{
					SourceImage: proto.String(c.sourceImage),
				},
			},
		},
		NetworkInterfaces: []*computepb.NetworkInterface{netIface},
		Metadata: &computepb.Metadata{
			Items: []*computepb.Items{
				{Key: proto.String("startup-script"), Value: proto.String(startupScript)},
			},
		},
	}

	if c.runnerSA != "" {
		inst.ServiceAccounts = []*computepb.ServiceAccount{
			{
				Email: proto.String(c.runnerSA),
				// Least privilege: the runner only writes logs. Its SA has just
				// logging.logWriter in IAM, so a compromised CI job cannot mint
				// broad-scoped tokens. Teardown is control-plane-driven.
				Scopes: []string{"https://www.googleapis.com/auth/logging.write"},
			},
		}
	}

	return inst
}

// Delete removes a runner instance by name.
func (c *Client) Delete(ctx context.Context, name string) error {
	op, err := c.instances.Delete(ctx, &computepb.DeleteInstanceRequest{
		Project:  c.project,
		Zone:     c.zone,
		Instance: name,
	})
	if err != nil {
		return fmt.Errorf("delete instance: %w", err)
	}
	if err := op.Wait(ctx); err != nil {
		return fmt.Errorf("wait delete instance: %w", err)
	}
	return nil
}

// ListRunnerInstances returns all instances labelled as managed runners,
// paginating through every page.
func (c *Client) ListRunnerInstances(ctx context.Context) ([]*computepb.Instance, error) {
	it := c.instances.List(ctx, &computepb.ListInstancesRequest{
		Project: c.project,
		Zone:    c.zone,
		Filter:  proto.String(runnerFilter),
	})

	var all []*computepb.Instance
	for {
		inst, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("list runner instances: %w", err)
		}
		all = append(all, inst)
	}
	return all, nil
}

// CleanupOld deletes runner instances older than maxAge.
func (c *Client) CleanupOld(ctx context.Context, maxAge time.Duration) (int, error) {
	instances, err := c.ListRunnerInstances(ctx)
	if err != nil {
		return 0, err
	}

	cutoff := time.Now().Add(-maxAge)
	deleted := 0

	for _, inst := range instances {
		created, err := time.Parse(time.RFC3339, inst.GetCreationTimestamp())
		if err != nil {
			continue
		}
		if created.Before(cutoff) {
			name := inst.GetName()
			log.Printf("Deleting stale runner instance %s (created: %s)", name, inst.GetCreationTimestamp())
			if err := c.Delete(ctx, name); err != nil {
				log.Printf("Failed to delete instance %s: %v", name, err)
				continue
			}
			deleted++
		}
	}

	return deleted, nil
}
