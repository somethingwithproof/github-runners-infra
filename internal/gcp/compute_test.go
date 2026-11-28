// SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
// SPDX-License-Identifier: MIT

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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/thomasvincent/github-runners-infra/internal/compute"
)

type fakeInstances struct {
	instance     *computepb.Instance
	getErr       error
	insertReq    *computepb.InsertInstanceRequest
	insertReqs   []*computepb.InsertInstanceRequest
	insertOp     operation
	insertOps    []operation
	insertErr    error
	insertErrs   []error
	deleteOp     operation
	deleteErr    error
	deletes      int
	deletedNames []string
	instances    []*computepb.Instance
}

type fakeOperation struct{ err error }

func (f fakeOperation) Wait(context.Context) error { return f.err }

func (f *fakeInstances) Get(context.Context, *computepb.GetInstanceRequest) (*computepb.Instance, error) {
	return f.instance, f.getErr
}

func (f *fakeInstances) Insert(_ context.Context, req *computepb.InsertInstanceRequest) (operation, error) {
	f.insertReq = req
	f.insertReqs = append(f.insertReqs, req)
	if len(f.insertErrs) > 0 {
		err := f.insertErrs[0]
		f.insertErrs = f.insertErrs[1:]
		if err != nil {
			return nil, err
		}
	}
	if len(f.insertOps) > 0 {
		op := f.insertOps[0]
		f.insertOps = f.insertOps[1:]
		return op, nil
	}
	return f.insertOp, f.insertErr
}

func (f *fakeInstances) Delete(_ context.Context, req *computepb.DeleteInstanceRequest) (operation, error) {
	f.deletes++
	f.deletedNames = append(f.deletedNames, req.GetInstance())
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
	instance := client.instanceSpec("org/repo:42", "n2-standard-4", "#cloud-config")
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

func TestDeleteAndSweepIgnoreRESTNotFoundOnOperationWait(t *testing.T) {
	jobKey := "org/repo:rest-404"
	name := resourceName(jobKey)
	created := time.Now().Add(-2 * time.Hour).Format(time.RFC3339)
	instances := &fakeInstances{
		instance: &computepb.Instance{Name: &name, Labels: map[string]string{
			controllerLabel: shortHash("primary"), jobLabel: shortHash(jobKey),
		}},
		instances: []*computepb.Instance{{
			Name: &name, CreationTimestamp: &created,
			Labels: map[string]string{controllerLabel: shortHash("primary")},
		}},
		deleteOp: fakeOperation{err: &googleapi.Error{Code: 404}},
	}
	client := testClient(instances)
	if err := client.DeleteRunner(context.Background(), name, jobKey); err != nil {
		t.Fatalf("DeleteRunner REST 404 wait = %v", err)
	}
	if deleted, err := client.SweepOrphanedRunners(context.Background(), nil, time.Now().Add(-time.Hour)); err != nil || deleted != 1 {
		t.Fatalf("SweepOrphanedRunners REST 404 wait = %d, %v", deleted, err)
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

// An instance whose age cannot be read is kept, but it must not stop the rest
// of the sweep: otherwise one bad record blocks reclamation indefinitely.
func TestSweepOrphanedRunnersKeepsUnknownAgeAndContinues(t *testing.T) {
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
	if err == nil {
		t.Fatal("sweep must still report the unreadable timestamp")
	}
	if deleted != 1 || instances.deletes != 1 {
		t.Fatalf("sweep = %d deleted, %d delete calls; want the valid orphan reclaimed", deleted, instances.deletes)
	}
	for _, name := range instances.deletedNames {
		if name == invalidName {
			t.Fatalf("deleted %s despite an unreadable creation timestamp", name)
		}
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

func TestCreateRunnerAppliesMachineTypeOverride(t *testing.T) {
	instances := &fakeInstances{getErr: &googleapi.Error{Code: 404}, insertOp: fakeOperation{}}
	client := testClient(instances)
	if _, err := client.CreateRunner(context.Background(), compute.RunnerParams{
		JobKey: "org/repo:55", RunnerJITConfig: "jit", InstanceSize: "c3-standard-8",
	}); err != nil {
		t.Fatalf("CreateRunner override error = %v", err)
	}
	want := "zones/zone/machineTypes/c3-standard-8"
	if instances.insertReq == nil || instances.insertReq.GetInstanceResource().GetMachineType() != want {
		t.Fatalf("machine type = %q, want %q", instances.insertReq.GetInstanceResource().GetMachineType(), want)
	}
	if _, err := client.CreateRunner(context.Background(), compute.RunnerParams{
		JobKey: "org/repo:56", RunnerJITConfig: "jit", InstanceSize: "../bad",
	}); err == nil {
		t.Fatal("CreateRunner accepted invalid GCP machine type override")
	}
}

func TestGCPSpotFallsBackOnCapacityError(t *testing.T) {
	instances := &fakeInstances{
		getErr: &googleapi.Error{Code: 404},
		insertOps: []operation{
			fakeOperation{err: errors.New("ZONE_RESOURCE_POOL_EXHAUSTED in us-central1-a")},
			fakeOperation{},
		},
	}
	client := testClient(instances)
	client.spot = true
	client.spotFallbackOnDemand = true
	inst, err := client.CreateRunner(context.Background(), compute.RunnerParams{JobKey: "org/repo:gcp-fb", RunnerJITConfig: "jit"})
	if err != nil || inst == nil {
		t.Fatalf("CreateRunner fallback = %#v, %v", inst, err)
	}
	if len(instances.insertReqs) != 2 {
		t.Fatalf("Insert calls = %d, want 2", len(instances.insertReqs))
	}
	if instances.insertReqs[0].GetInstanceResource().GetScheduling().GetProvisioningModel() != "SPOT" {
		t.Fatal("first attempt did not request SPOT")
	}
	if instances.insertReqs[1].GetInstanceResource().GetScheduling() != nil {
		t.Fatal("on-demand fallback still requested SPOT scheduling")
	}
	if instances.insertReqs[0].GetRequestId() == instances.insertReqs[1].GetRequestId() {
		t.Fatal("fallback reused primary RequestId")
	}
}

func TestGCPResourceExhaustedWithoutFallback(t *testing.T) {
	instances := &fakeInstances{
		getErr:    &googleapi.Error{Code: 404},
		insertErr: status.Error(codes.ResourceExhausted, "RESOURCE_EXHAUSTED"),
	}
	client := testClient(instances)
	client.spot = true
	_, err := client.CreateRunner(context.Background(), compute.RunnerParams{JobKey: "org/repo:gcp-cap"})
	if !compute.IsCapacityError(err) {
		t.Fatalf("error = %v, want ErrCapacityUnavailable", err)
	}
}

func TestGCPStoppedInstanceRetainsNameUntilDeleted(t *testing.T) {
	name := resourceName("org/repo:gcp-term")
	terminated := "TERMINATED"
	instances := &fakeInstances{
		instance: &computepb.Instance{
			Name:   &name,
			Status: &terminated,
			Labels: map[string]string{
				controllerLabel: shortHash("primary"),
				jobLabel:        shortHash("org/repo:gcp-term"),
			},
		},
	}
	client := testClient(instances)
	if _, found, err := client.FindRunner(context.Background(), "org/repo:gcp-term"); err != nil || found {
		t.Fatalf("FindRunner(TERMINATED) = found=%v err=%v, want absent", found, err)
	}
	if absent, err := client.RunnerAbsent(context.Background(), name); err != nil || absent {
		t.Fatalf("RunnerAbsent(TERMINATED) = absent=%v err=%v, want false", absent, err)
	}
	instances.deleteOp = fakeOperation{}
	ctx := context.Background()
	params := compute.RunnerParams{JobKey: "org/repo:gcp-term", RunnerJITConfig: "jit"}
	if _, err := client.CreateRunner(ctx, params); err == nil || instances.insertReq != nil || instances.deletes != 1 {
		t.Fatalf("reused occupied GCP name: deletes=%d insert=%v err=%v", instances.deletes, instances.insertReq, err)
	}
	if err := client.CleanupRunner(ctx, params.JobKey); err != nil || instances.deletes != 2 {
		t.Fatalf("cleanup of stopped instance: deletes=%d err=%v", instances.deletes, err)
	}
	running := "RUNNING"
	instances.instance.Status = &running
	if absent, err := client.RunnerAbsent(context.Background(), name); err != nil || absent {
		t.Fatalf("RunnerAbsent(RUNNING) = absent=%v err=%v, want false", absent, err)
	}
	instances.getErr = status.Error(codes.NotFound, "gone")
	instances.insertOp = fakeOperation{}
	if _, err := client.CreateRunner(ctx, params); err != nil || instances.insertReq == nil {
		t.Fatalf("replacement after deletion: %v", err)
	}

}
