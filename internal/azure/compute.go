// Package azure provisions ephemeral GitHub Actions runners on Azure VMs.
package azure

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"text/template"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v7"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v7"
	"github.com/thomasvincent/github-runners-infra/internal/compute"
)

const (
	jobTagKey             = "github-runners-job"
	controllerTag         = "github-runners-controller"
	createdTag            = "github-runners-created"
	azureOnDemandPriceCap = -1.0 // Exact SDK sentinel; not a computed currency value.
)

type operation interface {
	Wait(context.Context) error
}

type nicCreateOperation interface {
	Wait(context.Context) (armnetwork.Interface, error)
}

type virtualMachinesAPI interface {
	Get(context.Context, string, string) (armcompute.VirtualMachine, error)
	List(context.Context, string) ([]*armcompute.VirtualMachine, error)
	CreateOrUpdate(context.Context, string, string, armcompute.VirtualMachine) (operation, error)
	Delete(context.Context, string, string) (operation, error)
}

type networkInterfacesAPI interface {
	Get(context.Context, string, string) (armnetwork.Interface, error)
	List(context.Context, string) ([]*armnetwork.Interface, error)
	CreateOrUpdate(context.Context, string, string, armnetwork.Interface) (nicCreateOperation, error)
	Delete(context.Context, string, string) (operation, error)
}

type operationFunc func(context.Context) error

func (f operationFunc) Wait(ctx context.Context) error { return f(ctx) }

type nicCreateOperationFunc func(context.Context) (armnetwork.Interface, error)

func (f nicCreateOperationFunc) Wait(ctx context.Context) (armnetwork.Interface, error) {
	return f(ctx)
}

type virtualMachinesClient struct {
	client *armcompute.VirtualMachinesClient
}

func (c *virtualMachinesClient) Get(ctx context.Context, group, name string) (armcompute.VirtualMachine, error) {
	response, err := c.client.Get(ctx, group, name, nil)
	return response.VirtualMachine, err
}

func (c *virtualMachinesClient) List(ctx context.Context, group string) ([]*armcompute.VirtualMachine, error) {
	pager := c.client.NewListPager(group, nil)
	var result []*armcompute.VirtualMachine
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		result = append(result, page.Value...)
	}
	return result, nil
}

func (c *virtualMachinesClient) CreateOrUpdate(ctx context.Context, group, name string, vm armcompute.VirtualMachine) (operation, error) {
	poller, err := c.client.BeginCreateOrUpdate(ctx, group, name, vm, nil)
	if err != nil {
		return nil, err
	}
	return operationFunc(func(waitCtx context.Context) error {
		_, err := poller.PollUntilDone(waitCtx, nil)
		return err
	}), nil
}

func (c *virtualMachinesClient) Delete(ctx context.Context, group, name string) (operation, error) {
	poller, err := c.client.BeginDelete(ctx, group, name, nil)
	if err != nil {
		return nil, err
	}
	return operationFunc(func(waitCtx context.Context) error {
		_, err := poller.PollUntilDone(waitCtx, nil)
		return err
	}), nil
}

type networkInterfacesClient struct{ client *armnetwork.InterfacesClient }

func (c *networkInterfacesClient) Get(ctx context.Context, group, name string) (armnetwork.Interface, error) {
	response, err := c.client.Get(ctx, group, name, nil)
	return response.Interface, err
}

func (c *networkInterfacesClient) List(ctx context.Context, group string) ([]*armnetwork.Interface, error) {
	pager := c.client.NewListPager(group, nil)
	var result []*armnetwork.Interface
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		result = append(result, page.Value...)
	}
	return result, nil
}

func (c *networkInterfacesClient) CreateOrUpdate(ctx context.Context, group, name string, nic armnetwork.Interface) (nicCreateOperation, error) {
	poller, err := c.client.BeginCreateOrUpdate(ctx, group, name, nic, nil)
	if err != nil {
		return nil, err
	}
	return nicCreateOperationFunc(func(waitCtx context.Context) (armnetwork.Interface, error) {
		response, err := poller.PollUntilDone(waitCtx, nil)
		return response.Interface, err
	}), nil
}

func (c *networkInterfacesClient) Delete(ctx context.Context, group, name string) (operation, error) {
	poller, err := c.client.BeginDelete(ctx, group, name, nil)
	if err != nil {
		return nil, err
	}
	return operationFunc(func(waitCtx context.Context) error {
		_, err := poller.PollUntilDone(waitCtx, nil)
		return err
	}), nil
}

// Config configures an Azure VM runner pool. DefaultAzureCredential is used;
// client secrets and certificates are intentionally not accepted here.
type Config struct {
	SubscriptionID string
	ResourceGroup  string
	Location       string
	VMSize         string
	Image          string // publisher:offer:sku:version; version must be pinned
	// Subnet/NSG ingress policy is operator/IaC-managed; VMs receive no public IP.
	SubnetID      string
	AdminUsername string
	SSHPublicKey  string
	CloudInitPath string
	ControllerID  string
	Spot          bool
}

// Client owns Azure VMs and NICs created for one controller.
type Client struct {
	vms           virtualMachinesAPI
	nics          networkInterfacesAPI
	tmpl          *template.Template
	resourceGroup string
	location      string
	vmSize        armcompute.VirtualMachineSizeTypes
	image         [4]string
	subnetID      string
	adminUsername string
	sshPublicKey  string
	controllerID  string
	spot          bool
}

func (c *Client) Provider() string { return "azure" }

func NewClient(cfg Config) (*Client, error) {
	if cfg.SubscriptionID == "" || cfg.ResourceGroup == "" || cfg.Location == "" || cfg.VMSize == "" ||
		cfg.SubnetID == "" || cfg.AdminUsername == "" || cfg.SSHPublicKey == "" {
		return nil, fmt.Errorf("azure subscription, resource group, location, VM size, subnet, admin username, and SSH public key are required")
	}
	if !compute.ValidControllerID(cfg.ControllerID) {
		return nil, fmt.Errorf("controller ID must contain only letters, numbers, underscores, and hyphens")
	}
	parts := strings.Split(cfg.Image, ":")
	if len(parts) != 4 || slicesContainEmpty(parts) || strings.EqualFold(parts[3], "latest") {
		return nil, fmt.Errorf("azure image must be a pinned publisher:offer:sku:version value")
	}
	tmpl, err := template.ParseFiles(cfg.CloudInitPath)
	if err != nil {
		return nil, fmt.Errorf("parse cloud-init template: %w", err)
	}
	credential, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("create Azure credential: %w", err)
	}
	vms, err := armcompute.NewVirtualMachinesClient(cfg.SubscriptionID, credential, nil)
	if err != nil {
		return nil, fmt.Errorf("create Azure Compute client: %w", err)
	}
	nics, err := armnetwork.NewInterfacesClient(cfg.SubscriptionID, credential, nil)
	if err != nil {
		return nil, fmt.Errorf("create Azure Network client: %w", err)
	}
	client := &Client{
		vms: &virtualMachinesClient{client: vms}, nics: &networkInterfacesClient{client: nics}, tmpl: tmpl,
		resourceGroup: cfg.ResourceGroup, location: cfg.Location,
		vmSize: armcompute.VirtualMachineSizeTypes(cfg.VMSize), subnetID: cfg.SubnetID,
		adminUsername: cfg.AdminUsername, sshPublicKey: cfg.SSHPublicKey,
		controllerID: cfg.ControllerID, spot: cfg.Spot,
	}
	copy(client.image[:], parts)
	return client, nil
}

func (c *Client) FindRunner(ctx context.Context, jobKey string) (*compute.RunnerInstance, bool, error) {
	name := resourceName(jobKey)
	vm, err := c.vms.Get(ctx, c.resourceGroup, name)
	if err != nil {
		if isNotFound(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("find Azure runner: %w", err)
	}
	if err := validateOwnership("azure VM", name,
		tagValue(vm.Tags, controllerTag) == c.controllerID && tagValue(vm.Tags, jobTagKey) == jobHash(jobKey)); err != nil {
		return nil, false, err
	}
	return &compute.RunnerInstance{ID: name, Name: name}, true, nil
}

func (c *Client) CreateRunner(ctx context.Context, params compute.RunnerParams) (*compute.RunnerInstance, error) {
	if existing, found, err := c.FindRunner(ctx, params.JobKey); err != nil || found {
		return existing, err
	}
	userData, err := compute.RenderCloudInit(c.tmpl, params)
	if err != nil {
		return nil, err
	}
	name := resourceName(params.JobKey)
	nicID, err := c.ensureNIC(ctx, name, params.JobKey)
	if err != nil {
		return nil, err
	}
	vm := c.vmSpec(params.JobKey, name, nicID, userData)
	op, err := c.vms.CreateOrUpdate(ctx, c.resourceGroup, name, vm)
	if err != nil {
		return nil, c.cleanupNICAfterVMFailure(ctx, name, params.JobKey, fmt.Errorf("create Azure runner: %w", err))
	}
	if err := op.Wait(ctx); err != nil {
		return nil, c.cleanupNICAfterVMFailure(ctx, name, params.JobKey, fmt.Errorf("wait for Azure runner creation: %w", err))
	}
	return &compute.RunnerInstance{ID: name, Name: name}, nil
}

func (c *Client) vmSpec(jobKey, name, nicID, userData string) armcompute.VirtualMachine {
	disablePassword, primary := true, true
	deleteDisk, deleteNIC := armcompute.DiskDeleteOptionTypesDelete, armcompute.DeleteOptionsDelete
	vm := armcompute.VirtualMachine{
		Location: stringPtr(c.location), Tags: ownershipTags(c.controllerID, jobKey),
		Properties: &armcompute.VirtualMachineProperties{
			HardwareProfile: &armcompute.HardwareProfile{VMSize: &c.vmSize},
			StorageProfile: &armcompute.StorageProfile{
				ImageReference: &armcompute.ImageReference{Publisher: &c.image[0], Offer: &c.image[1], SKU: &c.image[2], Version: &c.image[3]},
				OSDisk:         &armcompute.OSDisk{CreateOption: enumPtr(armcompute.DiskCreateOptionTypesFromImage), DeleteOption: &deleteDisk},
			},
			OSProfile: &armcompute.OSProfile{
				ComputerName: stringPtr(name), AdminUsername: stringPtr(c.adminUsername),
				CustomData: stringPtr(base64.StdEncoding.EncodeToString([]byte(userData))),
				LinuxConfiguration: &armcompute.LinuxConfiguration{
					DisablePasswordAuthentication: &disablePassword,
					SSH: &armcompute.SSHConfiguration{PublicKeys: []*armcompute.SSHPublicKey{{
						Path: stringPtr("/home/" + c.adminUsername + "/.ssh/authorized_keys"), KeyData: stringPtr(c.sshPublicKey),
					}}},
				},
			},
			NetworkProfile: &armcompute.NetworkProfile{NetworkInterfaces: []*armcompute.NetworkInterfaceReference{{
				ID: &nicID, Properties: &armcompute.NetworkInterfaceReferenceProperties{Primary: &primary, DeleteOption: &deleteNIC},
			}}},
		},
	}
	if c.spot {
		priority, eviction := armcompute.VirtualMachinePriorityTypesSpot, armcompute.VirtualMachineEvictionPolicyTypesDelete
		// Azure's exact -1 sentinel caps cost at the on-demand price without
		// application-side currency arithmetic.
		maxPrice := azureOnDemandPriceCap
		vm.Properties.Priority = &priority
		vm.Properties.EvictionPolicy = &eviction
		vm.Properties.BillingProfile = &armcompute.BillingProfile{MaxPrice: &maxPrice}
	}
	return vm
}

func (c *Client) DeleteRunner(ctx context.Context, instanceID, jobKey string) error {
	vm, err := c.vms.Get(ctx, c.resourceGroup, instanceID)
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("get Azure runner %s before delete: %w", instanceID, err)
	}
	if err == nil {
		if tagValue(vm.Tags, controllerTag) != c.controllerID || tagValue(vm.Tags, jobTagKey) != jobHash(jobKey) {
			return fmt.Errorf("%w: refusing to delete VM %s without controller and job ownership tags", compute.ErrOwnershipMismatch, instanceID)
		}
		op, deleteErr := c.vms.Delete(ctx, c.resourceGroup, instanceID)
		if deleteErr != nil && !isNotFound(deleteErr) {
			return fmt.Errorf("delete owned Azure runner %s: %w", instanceID, deleteErr)
		}
		if deleteErr == nil {
			if waitErr := op.Wait(ctx); waitErr != nil && !isNotFound(waitErr) {
				return fmt.Errorf("wait for Azure runner deletion: %w", waitErr)
			}
		}
	}
	return c.deleteNIC(ctx, instanceID+"-nic", jobKey)
}

func (c *Client) CleanupRunner(ctx context.Context, jobKey string) error {
	return c.DeleteRunner(ctx, resourceName(jobKey), jobKey)
}

// SweepOrphanedRunners reclaims old controller-owned VMs and unattached NICs
// absent from durable state.
func (c *Client) SweepOrphanedRunners(ctx context.Context, known map[string]struct{}, cutoff time.Time) (int, error) {
	vms, err := c.vms.List(ctx, c.resourceGroup)
	if err != nil {
		return 0, fmt.Errorf("list Azure VMs for orphan sweep: %w", err)
	}
	deleted := 0
	protectedNICs := make(map[string]struct{}, len(known)+len(vms))
	for instanceID := range known {
		protectedNICs[instanceID+"-nic"] = struct{}{}
	}
	for _, vm := range vms {
		if vm == nil || tagValue(vm.Tags, controllerTag) != c.controllerID || vm.Name == nil {
			continue
		}
		name := *vm.Name
		protectedNICs[name+"-nic"] = struct{}{}
		if _, ok := known[name]; ok {
			continue
		}
		if vm.Properties == nil || vm.Properties.TimeCreated == nil {
			return deleted, fmt.Errorf("controller-owned Azure VM %s has no creation timestamp", name)
		}
		if !vm.Properties.TimeCreated.Before(cutoff) {
			continue
		}
		op, err := c.vms.Delete(ctx, c.resourceGroup, name)
		if err != nil {
			if isNotFound(err) {
				continue
			}
			return deleted, fmt.Errorf("delete orphaned controller Azure VM %s: %w", name, err)
		}
		if err := op.Wait(ctx); err != nil && !isNotFound(err) {
			return deleted, fmt.Errorf("wait for orphaned Azure VM deletion: %w", err)
		}
		deleted++
	}
	nics, err := c.nics.List(ctx, c.resourceGroup)
	if err != nil {
		return deleted, fmt.Errorf("list Azure NICs for orphan sweep: %w", err)
	}
	for _, nic := range nics {
		if nic == nil || nic.Name == nil || tagValue(nic.Tags, controllerTag) != c.controllerID {
			continue
		}
		name := *nic.Name
		if _, protected := protectedNICs[name]; protected || (nic.Properties != nil && nic.Properties.VirtualMachine != nil) {
			continue
		}
		created, err := time.Parse(time.RFC3339Nano, tagValue(nic.Tags, createdTag))
		if err != nil {
			return deleted, fmt.Errorf("controller-owned Azure NIC %s has no valid creation timestamp", name)
		}
		if !created.Before(cutoff) {
			continue
		}
		op, err := c.nics.Delete(ctx, c.resourceGroup, name)
		if err != nil {
			if isNotFound(err) {
				continue
			}
			return deleted, fmt.Errorf("delete orphaned controller Azure NIC %s: %w", name, err)
		}
		if err := op.Wait(ctx); err != nil && !isNotFound(err) {
			return deleted, fmt.Errorf("wait for orphaned Azure NIC deletion: %w", err)
		}
		deleted++
	}
	return deleted, nil
}

func (c *Client) ensureNIC(ctx context.Context, vmName, jobKey string) (string, error) {
	name := vmName + "-nic"
	existing, err := c.nics.Get(ctx, c.resourceGroup, name)
	if err == nil {
		if err := validateOwnership("azure NIC", name, tagValue(existing.Tags, controllerTag) == c.controllerID && existing.ID != nil); err != nil {
			return "", err
		}
		return *existing.ID, nil
	}
	if !isNotFound(err) {
		return "", fmt.Errorf("find Azure runner NIC: %w", err)
	}
	dynamic := armnetwork.IPAllocationMethodDynamic
	tags := ownershipTags(c.controllerID, jobKey)
	tags[createdTag] = stringPtr(time.Now().UTC().Format(time.RFC3339Nano))
	op, err := c.nics.CreateOrUpdate(ctx, c.resourceGroup, name, armnetwork.Interface{
		Location: stringPtr(c.location), Tags: tags,
		Properties: &armnetwork.InterfacePropertiesFormat{IPConfigurations: []*armnetwork.InterfaceIPConfiguration{{
			Name: stringPtr("primary"), Properties: &armnetwork.InterfaceIPConfigurationPropertiesFormat{
				PrivateIPAllocationMethod: &dynamic, Subnet: &armnetwork.Subnet{ID: stringPtr(c.subnetID)},
			},
		}}},
	})
	if err != nil {
		return "", fmt.Errorf("create Azure runner NIC: %w", err)
	}
	created, err := op.Wait(ctx)
	if err != nil {
		return "", fmt.Errorf("wait for Azure runner NIC creation: %w", err)
	}
	if created.ID == nil {
		return "", fmt.Errorf("create Azure runner NIC: API returned no resource ID")
	}
	return *created.ID, nil
}

func (c *Client) cleanupNICAfterVMFailure(ctx context.Context, vmName, jobKey string, cause error) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := c.deleteNIC(cleanupCtx, vmName+"-nic", jobKey); err != nil {
		return errors.Join(cause, fmt.Errorf("clean up Azure NIC after VM creation failure: %w", err))
	}
	return cause
}

func validateOwnership(resourceType, name string, owned bool) error {
	if owned {
		return nil
	}
	return fmt.Errorf("%w: %s %s exists without expected ownership tags", compute.ErrOwnershipMismatch, resourceType, name)
}

func (c *Client) deleteNIC(ctx context.Context, name, jobKey string) error {
	nic, err := c.nics.Get(ctx, c.resourceGroup, name)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("get Azure runner NIC %s before delete: %w", name, err)
	}
	if tagValue(nic.Tags, controllerTag) != c.controllerID || tagValue(nic.Tags, jobTagKey) != jobHash(jobKey) {
		return fmt.Errorf("%w: refusing to delete NIC %s without controller and job ownership tags", compute.ErrOwnershipMismatch, name)
	}
	op, err := c.nics.Delete(ctx, c.resourceGroup, name)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("delete owned Azure runner NIC %s: %w", name, err)
	}
	if err := op.Wait(ctx); err != nil && !isNotFound(err) {
		return fmt.Errorf("wait for Azure runner NIC deletion: %w", err)
	}
	return nil
}

func resourceName(jobKey string) string { return "ghr-" + jobHash(jobKey) }

func jobHash(value string) string {
	hash := sha256.Sum256([]byte(value))
	return hex.EncodeToString(hash[:16])
}

func ownershipTags(controllerID, jobKey string) map[string]*string {
	return map[string]*string{controllerTag: stringPtr(controllerID), jobTagKey: stringPtr(jobHash(jobKey))}
}

func tagValue(tags map[string]*string, key string) string {
	if value := tags[key]; value != nil {
		return *value
	}
	return ""
}

func isNotFound(err error) bool {
	var responseErr *azcore.ResponseError
	return errors.As(err, &responseErr) && responseErr.StatusCode == http.StatusNotFound
}

func slicesContainEmpty(values []string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return true
		}
	}
	return false
}

func stringPtr(value string) *string { return &value }
func enumPtr[T ~string](value T) *T  { return &value }
