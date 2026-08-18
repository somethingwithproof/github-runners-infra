package gcp

import (
	"context"
	"testing"

	gce "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/googleapi"
)

type fakeInstances struct {
	instance *computepb.Instance
	getErr   error
}

func (f *fakeInstances) Get(context.Context, *computepb.GetInstanceRequest) (*computepb.Instance, error) {
	return f.instance, f.getErr
}

func (f *fakeInstances) Insert(context.Context, *computepb.InsertInstanceRequest) (*gce.Operation, error) {
	return nil, nil
}

func (f *fakeInstances) Delete(context.Context, *computepb.DeleteInstanceRequest) (*gce.Operation, error) {
	return nil, nil
}

func (f *fakeInstances) Close() error { return nil }

func TestSpotInstanceSpecIsDeleteOnPreemptionAndPrivateByDefault(t *testing.T) {
	client := &Client{
		zone: "us-central1-a", machineType: "n2-standard-4", sourceImage: "projects/ubuntu/global/images/ubuntu-pinned",
		subnetwork: "projects/p/regions/us-central1/subnetworks/runners", controllerHash: shortHash("primary"), spot: true,
	}
	instance := client.instanceSpec("org/repo:42", "#cloud-config")
	if instance.Scheduling == nil || instance.Scheduling.GetProvisioningModel() != "SPOT" ||
		instance.Scheduling.GetInstanceTerminationAction() != "DELETE" {
		t.Fatalf("unexpected spot scheduling: %#v", instance.Scheduling)
	}
	if len(instance.NetworkInterfaces) != 1 || len(instance.NetworkInterfaces[0].AccessConfigs) != 0 {
		t.Fatal("private-by-default runner unexpectedly requested an external IP")
	}
	if got := instance.Labels[controllerLabel]; got != client.controllerHash {
		t.Fatalf("controller label = %q", got)
	}
}

func TestRequestAndResourceIdentityAreStableAndScoped(t *testing.T) {
	name, id := resourceName("org/repo:1"), requestID("org/repo:1", 1)
	if name != resourceName("org/repo:1") || id != requestID("org/repo:1", 1) {
		t.Fatal("provider identities are not deterministic")
	}
	if name == resourceName("org/repo:2") {
		t.Fatal("different jobs share a provider identity")
	}
	if id == requestID("org/repo:1", 2) {
		t.Fatal("replacement provisioning epoch reused the GCP request ID")
	}
}

func TestFindRunnerHandlesRESTNotFoundAndRejectsForeignLabels(t *testing.T) {
	client := &Client{
		instances: &fakeInstances{getErr: &googleapi.Error{Code: 404}},
		projectID: "project", zone: "zone", controllerHash: shortHash("primary"),
	}
	if instance, found, err := client.FindRunner(context.Background(), "org/repo:1"); err != nil || found || instance != nil {
		t.Fatalf("REST not found = %#v, %v, %v", instance, found, err)
	}
	name := resourceName("org/repo:1")
	client.instances = &fakeInstances{instance: &computepb.Instance{
		Name: &name,
		Labels: map[string]string{
			controllerLabel: shortHash("other"),
			jobLabel:        shortHash("org/repo:1"),
		},
	}}
	if _, _, err := client.FindRunner(context.Background(), "org/repo:1"); err == nil {
		t.Fatal("accepted GCP instance with foreign controller label")
	}
}
