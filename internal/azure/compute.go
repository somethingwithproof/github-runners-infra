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

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v7"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v7"
	"github.com/thomasvincent/github-runners-infra/internal/compute"
)

const (
	controllerTag         = "github-runners-controller"
	azureOnDemandPriceCap = -1.0 // Exact SDK sentinel; not a computed currency value.
)

// Config configures an Azure VM runner pool. DefaultAzureCredential is used;
// client secrets and certificates are intentionally not accepted here.
type Config struct {
	SubscriptionID string
	ResourceGroup  string
	Location       string
	VMSize         string
	Image          string // publisher:offer:sku:version; version must be pinned
	SubnetID       string
	AdminUsername  string
	SSHPublicKey   string
	CloudInitPath  string
	ControllerID   string
	Spot           bool
}

// Client owns Azure VMs and NICs created for one controller.
type Client struct {
	vms           *armcompute.VirtualMachinesClient
	nics          *armnetwork.InterfacesClient
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
		vms: vms, nics: nics, tmpl: tmpl, resourceGroup: cfg.ResourceGroup, location: cfg.Location,
		vmSize: armcompute.VirtualMachineSizeTypes(cfg.VMSize), subnetID: cfg.SubnetID,
		adminUsername: cfg.AdminUsername, sshPublicKey: cfg.SSHPublicKey,
		controllerID: cfg.ControllerID, spot: cfg.Spot,
	}
	copy(client.image[:], parts)
	return client, nil
}

func (c *Client) FindRunner(ctx context.Context, jobKey string) (*compute.RunnerInstance, bool, error) {
	name := resourceName(jobKey)
	vm, err := c.vms.Get(ctx, c.resourceGroup, name, nil)
	if err != nil {
		if isNotFound(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("find Azure runner: %w", err)
	}
	if tagValue(vm.Tags, controllerTag) != c.controllerID || tagValue(vm.Tags, "github-runners-job") != jobHash(jobKey) {
		return nil, false, fmt.Errorf("azure VM %s exists without expected ownership tags", name)
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
	poller, err := c.vms.BeginCreateOrUpdate(ctx, c.resourceGroup, name, vm, nil)
	if err != nil {
		return nil, fmt.Errorf("create Azure runner: %w", err)
	}
	if _, err := poller.PollUntilDone(ctx, nil); err != nil {
		return nil, fmt.Errorf("wait for Azure runner creation: %w", err)
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
	vm, err := c.vms.Get(ctx, c.resourceGroup, instanceID, nil)
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("get Azure runner %s before delete: %w", instanceID, err)
	}
	if err == nil {
		if tagValue(vm.Tags, controllerTag) != c.controllerID || tagValue(vm.Tags, "github-runners-job") != jobHash(jobKey) {
			return fmt.Errorf("%w: refusing to delete VM %s without controller and job ownership tags", compute.ErrOwnershipMismatch, instanceID)
		}
		poller, deleteErr := c.vms.BeginDelete(ctx, c.resourceGroup, instanceID, nil)
		if deleteErr != nil && !isNotFound(deleteErr) {
			return fmt.Errorf("delete owned Azure runner %s: %w", instanceID, deleteErr)
		}
		if deleteErr == nil {
			if _, waitErr := poller.PollUntilDone(ctx, nil); waitErr != nil && !isNotFound(waitErr) {
				return fmt.Errorf("wait for Azure runner deletion: %w", waitErr)
			}
		}
	}
	return c.deleteNIC(ctx, instanceID+"-nic", jobKey)
}

func (c *Client) CleanupRunner(ctx context.Context, jobKey string) error {
	return c.DeleteRunner(ctx, resourceName(jobKey), jobKey)
}

func (c *Client) ensureNIC(ctx context.Context, vmName, jobKey string) (string, error) {
	name := vmName + "-nic"
	existing, err := c.nics.Get(ctx, c.resourceGroup, name, nil)
	if err == nil {
		if tagValue(existing.Tags, controllerTag) != c.controllerID || existing.ID == nil {
			return "", fmt.Errorf("azure NIC %s exists without controller ownership tag", name)
		}
		return *existing.ID, nil
	}
	if !isNotFound(err) {
		return "", fmt.Errorf("find Azure runner NIC: %w", err)
	}
	dynamic := armnetwork.IPAllocationMethodDynamic
	poller, err := c.nics.BeginCreateOrUpdate(ctx, c.resourceGroup, name, armnetwork.Interface{
		Location: stringPtr(c.location), Tags: ownershipTags(c.controllerID, jobKey),
		Properties: &armnetwork.InterfacePropertiesFormat{IPConfigurations: []*armnetwork.InterfaceIPConfiguration{{
			Name: stringPtr("primary"), Properties: &armnetwork.InterfaceIPConfigurationPropertiesFormat{
				PrivateIPAllocationMethod: &dynamic, Subnet: &armnetwork.Subnet{ID: stringPtr(c.subnetID)},
			},
		}}},
	}, nil)
	if err != nil {
		return "", fmt.Errorf("create Azure runner NIC: %w", err)
	}
	created, err := poller.PollUntilDone(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("wait for Azure runner NIC creation: %w", err)
	}
	if created.ID == nil {
		return "", fmt.Errorf("create Azure runner NIC: API returned no resource ID")
	}
	return *created.ID, nil
}

func (c *Client) deleteNIC(ctx context.Context, name, jobKey string) error {
	nic, err := c.nics.Get(ctx, c.resourceGroup, name, nil)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("get Azure runner NIC %s before delete: %w", name, err)
	}
	if tagValue(nic.Tags, controllerTag) != c.controllerID || tagValue(nic.Tags, "github-runners-job") != jobHash(jobKey) {
		return fmt.Errorf("%w: refusing to delete NIC %s without controller and job ownership tags", compute.ErrOwnershipMismatch, name)
	}
	poller, err := c.nics.BeginDelete(ctx, c.resourceGroup, name, nil)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("delete owned Azure runner NIC %s: %w", name, err)
	}
	if _, err := poller.PollUntilDone(ctx, nil); err != nil && !isNotFound(err) {
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
	return map[string]*string{controllerTag: stringPtr(controllerID), "github-runners-job": stringPtr(jobHash(jobKey))}
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
