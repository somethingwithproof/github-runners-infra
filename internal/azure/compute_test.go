// SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
// SPDX-License-Identifier: MIT

package azure

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"text/template"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v7"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v7"
	"github.com/thomasvincent/github-runners-infra/internal/compute"
)

type fakeOperation struct{ err error }

func (f fakeOperation) Wait(context.Context) error { return f.err }

type fakeNICCreateOperation struct {
	nic armnetwork.Interface
	err error
}

func (f fakeNICCreateOperation) Wait(context.Context) (armnetwork.Interface, error) {
	return f.nic, f.err
}

type fakeVMs struct {
	vm         armcompute.VirtualMachine
	createdVM  armcompute.VirtualMachine
	createdVMs []armcompute.VirtualMachine
	getErr     error
	createErr  error
	createErrs []error
	createOp   operation
	createOps  []operation
	deleteErr  error
	deleteOp   operation
	vms        []*armcompute.VirtualMachine
	deletes    int
}

func (f *fakeVMs) Get(context.Context, string, string) (armcompute.VirtualMachine, error) {
	return f.vm, f.getErr
}

func (f *fakeVMs) List(context.Context, string) ([]*armcompute.VirtualMachine, error) {
	return f.vms, nil
}

func (f *fakeVMs) CreateOrUpdate(_ context.Context, _, _ string, vm armcompute.VirtualMachine) (operation, error) {
	f.createdVM = vm
	f.createdVMs = append(f.createdVMs, vm)
	if len(f.createErrs) > 0 {
		err := f.createErrs[0]
		f.createErrs = f.createErrs[1:]
		if err != nil {
			return nil, err
		}
	}
	if len(f.createOps) > 0 {
		op := f.createOps[0]
		f.createOps = f.createOps[1:]
		return op, nil
	}
	return f.createOp, f.createErr
}

func (f *fakeVMs) Delete(context.Context, string, string) (operation, error) {
	f.deletes++
	return f.deleteOp, f.deleteErr
}

type fakeNICs struct {
	nic         armnetwork.Interface
	nics        []*armnetwork.Interface
	getErr      error
	createErr   error
	createOp    nicCreateOperation
	deleteErr   error
	deleteOp    operation
	deleteCalls int
}

func (f *fakeNICs) Get(context.Context, string, string) (armnetwork.Interface, error) {
	return f.nic, f.getErr
}

func (f *fakeNICs) List(context.Context, string) ([]*armnetwork.Interface, error) {
	return f.nics, nil
}

func (f *fakeNICs) CreateOrUpdate(_ context.Context, _, _ string, nic armnetwork.Interface) (nicCreateOperation, error) {
	f.nic = nic
	f.getErr = nil
	return f.createOp, f.createErr
}

func (f *fakeNICs) Delete(context.Context, string, string) (operation, error) {
	f.deleteCalls++
	return f.deleteOp, f.deleteErr
}

func TestSpotVMSpecDeletesEvictedResourcesAndDisablesPasswords(t *testing.T) {
	client := &Client{
		location: "westus2", vmSize: armcompute.VirtualMachineSizeTypes("Standard_D4s_v5"),
		image:         [4]string{"Canonical", "ubuntu-24_04-lts", "server", "24.04.202601010"},
		adminUsername: "runneradmin", sshPublicKey: "ssh-ed25519 test", controllerID: "primary", spot: true,
	}
	vm := client.vmSpec("org/repo:42", "ghr-test", "/subscriptions/test/nic", armcompute.VirtualMachineSizeTypes("Standard_D4s_v5"), "#cloud-config")
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

func TestOwnershipRejectionUsesSharedSentinel(t *testing.T) {
	if err := validateOwnership("azure VM", "ghr-test", false); !errors.Is(err, compute.ErrOwnershipMismatch) {
		t.Fatalf("ownership error = %v", err)
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

func TestCreateRunnerAndCleanupNICAfterVMFailure(t *testing.T) {
	nicID := "/subscriptions/test/nics/runner"
	nics := &fakeNICs{
		getErr: notFoundError(), createOp: fakeNICCreateOperation{nic: armnetwork.Interface{ID: &nicID}},
		deleteOp: fakeOperation{},
	}
	vms := &fakeVMs{getErr: notFoundError(), createOp: fakeOperation{}}
	client := testAzureClient(vms, nics)
	created, err := client.CreateRunner(context.Background(), compute.RunnerParams{JobKey: "org/repo:1", RunnerJITConfig: "jit"})
	if err != nil || created == nil {
		t.Fatalf("CreateRunner = %#v, %v", created, err)
	}

	nics.getErr = notFoundError()
	nics.deleteCalls = 0
	vms.createErr = errors.New("VM create failed")
	if _, err := client.CreateRunner(context.Background(), compute.RunnerParams{JobKey: "org/repo:2", RunnerJITConfig: "jit"}); err == nil {
		t.Fatal("CreateRunner ignored VM creation failure")
	}
	if nics.deleteCalls != 1 {
		t.Fatalf("VM creation failure cleaned up %d NICs", nics.deleteCalls)
	}
}

func TestDeleteRunnerIsIdempotentAndRejectsForeignOwnership(t *testing.T) {
	vms := &fakeVMs{getErr: notFoundError()}
	nics := &fakeNICs{getErr: notFoundError()}
	client := testAzureClient(vms, nics)
	if err := client.DeleteRunner(context.Background(), resourceName("org/repo:1"), "org/repo:1"); err != nil {
		t.Fatalf("DeleteRunner not found = %v", err)
	}
	vms.getErr = nil
	vms.vm = armcompute.VirtualMachine{Tags: ownershipTags("other", "org/repo:1")}
	if err := client.DeleteRunner(context.Background(), resourceName("org/repo:1"), "org/repo:1"); !errors.Is(err, compute.ErrOwnershipMismatch) {
		t.Fatalf("DeleteRunner foreign ownership error = %v", err)
	}
}

func TestSweepOrphanedRunnersDeletesOldUnknownVM(t *testing.T) {
	created := time.Now().Add(-2 * time.Hour)
	name := "ghr-orphan"
	vms := &fakeVMs{
		vms: []*armcompute.VirtualMachine{{
			Name: &name, Tags: map[string]*string{controllerTag: stringPtr("primary")},
			Properties: &armcompute.VirtualMachineProperties{TimeCreated: &created},
		}},
		deleteOp: fakeOperation{},
	}
	client := testAzureClient(vms, &fakeNICs{getErr: notFoundError()})
	deleted, err := client.SweepOrphanedRunners(context.Background(), nil, time.Now().Add(-time.Hour))
	if err != nil || deleted != 1 || vms.deletes != 1 {
		t.Fatalf("sweep = %d, %v, delete calls=%d", deleted, err, vms.deletes)
	}
}

func TestSweepOrphanedRunnersFailsClosedWithoutCreationTime(t *testing.T) {
	name := "ghr-unknown-age"
	vms := &fakeVMs{vms: []*armcompute.VirtualMachine{{
		Name: &name, Tags: map[string]*string{controllerTag: stringPtr("primary")},
		Properties: &armcompute.VirtualMachineProperties{},
	}}}
	client := testAzureClient(vms, &fakeNICs{getErr: notFoundError()})
	deleted, err := client.SweepOrphanedRunners(context.Background(), nil, time.Now())
	if err == nil || deleted != 0 || vms.deletes != 0 {
		t.Fatalf("missing creation time sweep = %d, %v, delete calls=%d", deleted, err, vms.deletes)
	}
}

func TestSweepOrphanedRunnersDeletesOldUnattachedNIC(t *testing.T) {
	name := "ghr-leaked-nic"
	created := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339Nano)
	nics := &fakeNICs{
		nics: []*armnetwork.Interface{{
			Name:       &name,
			Tags:       map[string]*string{controllerTag: stringPtr("primary"), createdTag: &created},
			Properties: &armnetwork.InterfacePropertiesFormat{},
		}},
		deleteOp: fakeOperation{},
	}
	client := testAzureClient(&fakeVMs{}, nics)
	deleted, err := client.SweepOrphanedRunners(context.Background(), nil, time.Now().Add(-time.Hour))
	if err != nil || deleted != 1 || nics.deleteCalls != 1 {
		t.Fatalf("NIC sweep = %d, %v, delete calls=%d", deleted, err, nics.deleteCalls)
	}
}

func TestSweepOrphanedRunnersFailsClosedForUndatedNIC(t *testing.T) {
	name := "ghr-undated-nic"
	nics := &fakeNICs{nics: []*armnetwork.Interface{{
		Name: &name, Tags: map[string]*string{controllerTag: stringPtr("primary")},
	}}}
	client := testAzureClient(&fakeVMs{}, nics)
	deleted, err := client.SweepOrphanedRunners(context.Background(), nil, time.Now())
	if err == nil || deleted != 0 || nics.deleteCalls != 0 {
		t.Fatalf("undated NIC sweep = %d, %v, delete calls=%d", deleted, err, nics.deleteCalls)
	}
}

func TestConcurrentSweepFindAndCreatePreserveFreshRunner(t *testing.T) {
	jobKey := "org/repo:concurrent"
	name := resourceName(jobKey)
	created := time.Now()
	vm := armcompute.VirtualMachine{
		Name: &name, Tags: ownershipTags("primary", jobKey),
		Properties: &armcompute.VirtualMachineProperties{TimeCreated: &created},
	}
	vms := &fakeVMs{vm: vm, vms: []*armcompute.VirtualMachine{&vm}}
	client := testAzureClient(vms, &fakeNICs{})
	errorsCh := make(chan error, 3)
	go func() { _, _, err := client.FindRunner(context.Background(), jobKey); errorsCh <- err }()
	go func() {
		_, err := client.CreateRunner(context.Background(), compute.RunnerParams{JobKey: jobKey})
		errorsCh <- err
	}()
	go func() {
		_, err := client.SweepOrphanedRunners(context.Background(), nil, time.Now().Add(-time.Hour))
		errorsCh <- err
	}()
	for range 3 {
		if err := <-errorsCh; err != nil {
			t.Fatal(err)
		}
	}
	if vms.deletes != 0 {
		t.Fatalf("concurrent sweep deleted fresh runner %d times", vms.deletes)
	}
}

func testAzureClient(vms virtualMachinesAPI, nics networkInterfacesAPI) *Client {
	return &Client{
		vms: vms, nics: nics, tmpl: template.Must(template.New("runner").Parse("{{.RunnerJITConfig}}")),
		resourceGroup: "group", location: "westus2", vmSize: armcompute.VirtualMachineSizeTypes("Standard_D4s_v5"),
		image: [4]string{"Canonical", "ubuntu", "server", "pinned"}, subnetID: "subnet",
		adminUsername: "runneradmin", sshPublicKey: "ssh-ed25519 test", controllerID: "primary",
	}
}

func notFoundError() error { return &azcore.ResponseError{StatusCode: 404} }

func TestCreateRunnerAppliesVMSizeOverride(t *testing.T) {
	nicID := "/subscriptions/test/nics/runner"
	nics := &fakeNICs{
		getErr: notFoundError(), createOp: fakeNICCreateOperation{nic: armnetwork.Interface{ID: &nicID}},
		deleteOp: fakeOperation{},
	}
	vms := &fakeVMs{getErr: notFoundError(), createOp: fakeOperation{}}
	client := testAzureClient(vms, nics)
	if _, err := client.CreateRunner(context.Background(), compute.RunnerParams{
		JobKey: "org/repo:88", RunnerJITConfig: "jit", InstanceSize: "Standard_D8s_v5",
	}); err != nil {
		t.Fatalf("CreateRunner override error = %v", err)
	}
	if vms.createdVM.Properties == nil || vms.createdVM.Properties.HardwareProfile == nil ||
		vms.createdVM.Properties.HardwareProfile.VMSize == nil ||
		*vms.createdVM.Properties.HardwareProfile.VMSize != armcompute.VirtualMachineSizeTypes("Standard_D8s_v5") {
		t.Fatalf("VM size = %#v, want Standard_D8s_v5", vms.createdVM.Properties)
	}
	if _, err := client.CreateRunner(context.Background(), compute.RunnerParams{
		JobKey: "org/repo:89", RunnerJITConfig: "jit", InstanceSize: "bad size",
	}); err == nil {
		t.Fatal("CreateRunner accepted invalid Azure VM size override")
	}
}

func TestAzureSpotCapacityFallbackAndEvictedAbsence(t *testing.T) {
	t.Run("classifies capacity errors and falls back to on-demand reusing NIC", func(t *testing.T) {
		nicID := "/subscriptions/test/nics/runner"
		nics := &fakeNICs{
			getErr:   notFoundError(),
			createOp: fakeNICCreateOperation{nic: armnetwork.Interface{ID: &nicID}},
			deleteOp: fakeOperation{},
		}
		vms := &fakeVMs{
			getErr: notFoundError(),
			createErrs: []error{
				&azcore.ResponseError{StatusCode: 409, ErrorCode: "SkuNotAvailable"},
				nil,
			},
			createOps: []operation{fakeOperation{}},
		}
		client := testAzureClient(vms, nics)
		client.spot = true
		client.spotFallbackOnDemand = true
		inst, err := client.CreateRunner(context.Background(), compute.RunnerParams{JobKey: "org/repo:az-fb", RunnerJITConfig: "jit"})
		if err != nil || inst == nil {
			t.Fatalf("CreateRunner fallback = %#v, %v", inst, err)
		}
		if len(vms.createdVMs) != 2 {
			t.Fatalf("VM create calls = %d, want 2", len(vms.createdVMs))
		}
		if vms.createdVMs[0].Properties.Priority == nil || *vms.createdVMs[0].Properties.Priority != armcompute.VirtualMachinePriorityTypesSpot {
			t.Fatal("first attempt did not request Spot")
		}
		if vms.createdVMs[1].Properties.Priority != nil {
			t.Fatal("on-demand fallback still requested Spot priority")
		}
		if nics.deleteCalls != 0 {
			t.Fatalf("NIC unexpectedly deleted during successful fallback: %d", nics.deleteCalls)
		}
	})

	t.Run("classifies AllocationFailed and cleans up NIC without fallback", func(t *testing.T) {
		nicID := "/subscriptions/test/nics/runner"
		nics := &fakeNICs{
			getErr:   notFoundError(),
			createOp: fakeNICCreateOperation{nic: armnetwork.Interface{ID: &nicID}},
			deleteOp: fakeOperation{},
		}
		vms := &fakeVMs{
			getErr:    notFoundError(),
			createErr: &azcore.ResponseError{StatusCode: 409, ErrorCode: "AllocationFailed"},
		}
		client := testAzureClient(vms, nics)
		client.spot = true
		_, err := client.CreateRunner(context.Background(), compute.RunnerParams{JobKey: "org/repo:az-cap"})
		if !compute.IsCapacityError(err) || nics.deleteCalls != 1 {
			t.Fatalf("error=%v (isCap=%v), nicDeletes=%d", err, compute.IsCapacityError(err), nics.deleteCalls)
		}
	})

	t.Run("evicted deallocated or deleting VM treated as absent", func(t *testing.T) {
		deallocated := "PowerState/deallocated"
		name := resourceName("org/repo:az-evict")
		vms := &fakeVMs{
			vm: armcompute.VirtualMachine{
				Name: &name,
				Tags: ownershipTags("primary", "org/repo:az-evict"),
				Properties: &armcompute.VirtualMachineProperties{
					InstanceView: &armcompute.VirtualMachineInstanceView{
						Statuses: []*armcompute.InstanceViewStatus{{Code: &deallocated}},
					},
				},
			},
		}
		client := testAzureClient(vms, &fakeNICs{})
		if _, found, err := client.FindRunner(context.Background(), "org/repo:az-evict"); err != nil || found {
			t.Fatalf("FindRunner(deallocated) = found=%v err=%v, want absent", found, err)
		}
		if absent, err := client.RunnerAbsent(context.Background(), name); err != nil || !absent {
			t.Fatalf("RunnerAbsent(deallocated) = absent=%v err=%v, want true", absent, err)
		}
		deleting := "Deleting"
		vms.vm.Properties = &armcompute.VirtualMachineProperties{ProvisioningState: &deleting}
		if absent, err := client.RunnerAbsent(context.Background(), name); err != nil || !absent {
			t.Fatalf("RunnerAbsent(Deleting) = absent=%v err=%v, want true", absent, err)
		}
	})
}
