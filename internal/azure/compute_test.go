package azure

import (
	"encoding/base64"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v7"
)

func TestSpotVMSpecDeletesEvictedResourcesAndDisablesPasswords(t *testing.T) {
	client := &Client{
		location: "westus2", vmSize: armcompute.VirtualMachineSizeTypes("Standard_D4s_v5"),
		image:         [4]string{"Canonical", "ubuntu-24_04-lts", "server", "24.04.202601010"},
		adminUsername: "runneradmin", sshPublicKey: "ssh-ed25519 test", controllerID: "primary", spot: true,
	}
	vm := client.vmSpec("org/repo:42", "ghr-test", "/subscriptions/test/nic", "#cloud-config")
	if vm.Properties.Priority == nil || *vm.Properties.Priority != armcompute.VirtualMachinePriorityTypesSpot ||
		vm.Properties.EvictionPolicy == nil || *vm.Properties.EvictionPolicy != armcompute.VirtualMachineEvictionPolicyTypesDelete {
		t.Fatalf("unexpected spot settings: %#v", vm.Properties)
	}
	if vm.Properties.BillingProfile == nil || vm.Properties.BillingProfile.MaxPrice == nil || *vm.Properties.BillingProfile.MaxPrice != -1 {
		t.Fatal("default Azure spot price cap is not the on-demand ceiling")
	}
	if vm.Properties.OSProfile.LinuxConfiguration.DisablePasswordAuthentication == nil ||
		!*vm.Properties.OSProfile.LinuxConfiguration.DisablePasswordAuthentication {
		t.Fatal("password authentication was not disabled")
	}
	decoded, err := base64.StdEncoding.DecodeString(*vm.Properties.OSProfile.CustomData)
	if err != nil || string(decoded) != "#cloud-config" {
		t.Fatalf("invalid cloud-init custom data: %q, %v", decoded, err)
	}
	if vm.Properties.StorageProfile.OSDisk.DeleteOption == nil ||
		*vm.Properties.StorageProfile.OSDisk.DeleteOption != armcompute.DiskDeleteOptionTypesDelete {
		t.Fatal("OS disk is not configured for deletion")
	}
}

func TestImageMustBePinned(t *testing.T) {
	_, err := NewClient(Config{
		SubscriptionID: "s", ResourceGroup: "r", Location: "westus2", VMSize: "Standard_D4s_v5",
		Image: "Canonical:ubuntu:server:latest", SubnetID: "subnet", AdminUsername: "runneradmin",
		SSHPublicKey: "ssh-ed25519 test", CloudInitPath: "unused", ControllerID: "primary",
	})
	if err == nil {
		t.Fatal("accepted an unpinned Azure image")
	}
}
