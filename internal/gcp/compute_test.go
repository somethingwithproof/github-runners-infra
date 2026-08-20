package gcp

import (
	"context"
	"errors"
	"strings"
	"testing"
	"text/template"
	"time"

	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/googleapi"

	"github.com/thomasvincent/github-runners-infra/internal/compute"
)

type fakeInstances struct {
	instance  *computepb.Instance
	getErr    error
	insertOp  operation
	insertErr error
	deleteOp  operation
	deleteErr error
	deletes   int
	instances []*computepb.Instance
}

type fakeOperation struct{ err error }

func (f fakeOperation) Wait(context.Context) error { return f.err }

func (f *fakeInstances) Get(context.Context, *computepb.GetInstanceRequest) (*computepb.Instance, error) {
	return f.instance, f.getErr
}

func (f *fakeInstances) Insert(context.Context, *computepb.InsertInstanceRequest) (operation, error) {
	return f.insertOp, f.insertErr
}

func (f *fakeInstances) Delete(context.Context, *computepb.DeleteInstanceRequest) (operation, error) {
	f.deletes++
	return f.deleteOp, f.deleteErr
}

func (f *fakeInstances) List(context.Context, *computepb.ListInstancesRequest) ([]*computepb.Instance, error) {
	return f.instances, nil
}

func (f *fakeInstances) Close() error { return nil }

func TestSpotInstanceSpecIsDeleteOnPreemptionAndPrivateByDefault(t *testing.T) {
	client := &Client{
		zone: "us-central1-a", machineType: "n2-standard-4", sourceImage: "projects/ubuntu/global/images/ubuntu-pinned",
		subnetwork: "projects/p/regions/us-central1/subnetworks/runners", controllerHash: shortHash("primary"), spot: true,
		serviceAccountEmail: "runner@project.iam.gserviceaccount.com",
	}
	instance := client.instanceSpec("org/repo:42", "#cloud-config")
	if instance.Scheduling == nil || instance.Scheduling.GetProvisioningModel() != "SPOT" ||
		instance.Scheduling.GetInstanceTerminationAction() != "DELETE" || instance.Scheduling.GetAutomaticRestart() ||
		instance.Scheduling.GetOnHostMaintenance() != "TERMINATE" {
		t.Fatalf("unexpected spot scheduling: %#v", instance.Scheduling)
	}
	if len(instance.NetworkInterfaces) != 1 || len(instance.NetworkInterfaces[0].AccessConfigs) != 0 {
		t.Fatal("private-by-default runner unexpectedly requested an external IP")
	}
	if got := instance.Labels[controllerLabel]; got != client.controllerHash {
		t.Fatalf("controller label = %q", got)
	}
	if len(instance.ServiceAccounts) != 1 || instance.ServiceAccounts[0].GetEmail() != client.serviceAccountEmail ||
		len(instance.ServiceAccounts[0].Scopes) != 1 || instance.ServiceAccounts[0].Scopes[0] != runnerScope {
		t.Fatalf("runner service account = %#v", instance.ServiceAccounts)
	}
}

func TestNewClientRequiresRunnerServiceAccount(t *testing.T) {
	_, err := NewClient(context.Background(), Config{
		ProjectID: "project", Zone: "zone", MachineType: "n2-standard-4", SourceImage: "image",
		Subnetwork: "subnetwork", CloudInitPath: "../../cloud-init/runner.yaml.tmpl", ControllerID: "primary",
	})
	if err == nil || !strings.Contains(err.Error(), "runner service account") {
		t.Fatalf("missing runner service account error = %v", err)
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
	if len(id) != 36 || id[14] != '4' || !strings.ContainsRune("89ab", rune(id[19])) {
		t.Fatalf("request ID is not a canonical v4 UUID: %q", id)
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
	if _, _, err := client.FindRunner(context.Background(), "org/repo:1"); !errors.Is(err, compute.ErrOwnershipMismatch) {
		t.Fatalf("foreign controller error = %v", err)
	}
}

func TestCreateRunnerUsesTestableOperationBoundary(t *testing.T) {
	instances := &fakeInstances{getErr: &googleapi.Error{Code: 404}, insertOp: fakeOperation{}}
	client := testClient(instances)
	created, err := client.CreateRunner(context.Background(), compute.RunnerParams{JobKey: "org/repo:1", RunnerJITConfig: "jit"})
	if err != nil || created == nil || created.ID != resourceName("org/repo:1") {
		t.Fatalf("CreateRunner = %#v, %v", created, err)
	}
	instances.insertOp = fakeOperation{err: errors.New("wait failed")}
	if _, err := client.CreateRunner(context.Background(), compute.RunnerParams{JobKey: "org/repo:2", RunnerJITConfig: "jit"}); err == nil {
		t.Fatal("CreateRunner ignored operation wait failure")
	}
}

func TestDeleteRunnerIsIdempotentAndRejectsForeignOwnership(t *testing.T) {
	instances := &fakeInstances{getErr: &googleapi.Error{Code: 404}}
	client := testClient(instances)
	if err := client.DeleteRunner(context.Background(), resourceName("org/repo:1"), "org/repo:1"); err != nil {
		t.Fatalf("DeleteRunner not found = %v", err)
	}
	name := resourceName("org/repo:1")
	instances.getErr = nil
	instances.instance = &computepb.Instance{Name: &name, Labels: map[string]string{
		controllerLabel: shortHash("other"), jobLabel: shortHash("org/repo:1"),
	}}
	if err := client.DeleteRunner(context.Background(), name, "org/repo:1"); !errors.Is(err, compute.ErrOwnershipMismatch) {
		t.Fatalf("DeleteRunner foreign ownership error = %v", err)
	}
}

func TestCleanupRunnerDeletesOwnedInstance(t *testing.T) {
	jobKey := "org/repo:cleanup"
	name := resourceName(jobKey)
	instances := &fakeInstances{
		instance: &computepb.Instance{Name: &name, Labels: map[string]string{
			controllerLabel: shortHash("primary"), jobLabel: shortHash(jobKey),
		}},
		deleteOp: fakeOperation{},
	}
	client := testClient(instances)
	if err := client.CleanupRunner(context.Background(), jobKey); err != nil {
		t.Fatalf("CleanupRunner() error = %v", err)
	}
	if instances.deletes != 1 {
		t.Fatalf("delete calls = %d, want 1", instances.deletes)
	}
}

func TestSweepOrphanedRunnersDeletesOldUnknownInstance(t *testing.T) {
	created := time.Now().Add(-2 * time.Hour).Format(time.RFC3339)
	name := "ghr-orphan"
	instances := &fakeInstances{
		instances: []*computepb.Instance{{
			Name: &name, CreationTimestamp: &created,
			Labels: map[string]string{controllerLabel: shortHash("primary")},
		}},
		deleteOp: fakeOperation{},
	}
	client := testClient(instances)
	deleted, err := client.SweepOrphanedRunners(context.Background(), nil, time.Now().Add(-time.Hour))
	if err != nil || deleted != 1 || instances.deletes != 1 {
		t.Fatalf("sweep = %d, %v, delete calls=%d", deleted, err, instances.deletes)
	}
}

func TestSweepOrphanedRunnersFailsClosedOnInvalidTimestamp(t *testing.T) {
	invalidCreated := "not-a-timestamp"
	validCreated := time.Now().Add(-2 * time.Hour).Format(time.RFC3339)
	invalidName := "ghr-invalid-time"
	validName := "ghr-valid-orphan"
	instances := &fakeInstances{
		instances: []*computepb.Instance{
			{Name: &invalidName, CreationTimestamp: &invalidCreated, Labels: map[string]string{controllerLabel: shortHash("primary")}},
			{Name: &validName, CreationTimestamp: &validCreated, Labels: map[string]string{controllerLabel: shortHash("primary")}},
		},
		deleteOp: fakeOperation{},
	}
	client := testClient(instances)
	deleted, err := client.SweepOrphanedRunners(context.Background(), nil, time.Now().Add(-time.Hour))
	if err == nil || deleted != 0 || instances.deletes != 0 {
		t.Fatalf("sweep with invalid timestamp = %d, %v, delete calls=%d", deleted, err, instances.deletes)
	}
}

func TestConcurrentSweepFindAndCreatePreserveFreshRunner(t *testing.T) {
	jobKey := "org/repo:concurrent"
	name := resourceName(jobKey)
	created := time.Now().Format(time.RFC3339)
	instance := &computepb.Instance{
		Name: &name, CreationTimestamp: &created,
		Labels: map[string]string{controllerLabel: shortHash("primary"), jobLabel: shortHash(jobKey)},
	}
	instances := &fakeInstances{instance: instance, instances: []*computepb.Instance{instance}}
	client := testClient(instances)
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
	if instances.deletes != 0 {
		t.Fatalf("concurrent sweep deleted fresh runner %d times", instances.deletes)
	}
}

func testClient(instances instancesAPI) *Client {
	return &Client{
		instances: instances, tmpl: template.Must(template.New("runner").Parse("{{.RunnerJITConfig}}")),
		projectID: "project", zone: "zone", machineType: "n2-standard-4", sourceImage: "image",
		subnetwork: "subnetwork", controllerHash: shortHash("primary"), serviceAccountEmail: "runner@project.iam.gserviceaccount.com",
	}
}
