package gcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/proto"

	"github.com/thomasvincent/github-runners-infra/internal/provider"
)

// fakeOperation is a no-op operation used to satisfy the operation interface.
type fakeOperation struct{}

func (fakeOperation) Wait(context.Context, ...gaxCallOption) error { return nil }

// fakeIterator pages over a fixed slice, returning iterator.Done when drained.
type fakeIterator struct {
	items []*computepb.Instance
	idx   int
}

func (f *fakeIterator) Next() (*computepb.Instance, error) {
	if f.idx >= len(f.items) {
		return nil, iterator.Done
	}
	inst := f.items[f.idx]
	f.idx++
	return inst, nil
}

// fakeInstancesAPI records calls and serves canned list results. It is the
// GCP-boundary fake; no network is touched.
type fakeInstancesAPI struct {
	inserted []*computepb.InsertInstanceRequest
	deleted  []*computepb.DeleteInstanceRequest
	list     []*computepb.Instance
	// deleteErr, keyed by instance name, makes Delete fail for that instance so
	// reaper error handling can be exercised.
	deleteErr map[string]error
}

func (f *fakeInstancesAPI) Insert(_ context.Context, req *computepb.InsertInstanceRequest, _ ...gaxCallOption) (operation, error) {
	f.inserted = append(f.inserted, req)
	return fakeOperation{}, nil
}

func (f *fakeInstancesAPI) Delete(_ context.Context, req *computepb.DeleteInstanceRequest, _ ...gaxCallOption) (operation, error) {
	if err := f.deleteErr[req.GetInstance()]; err != nil {
		return nil, err
	}
	f.deleted = append(f.deleted, req)
	return fakeOperation{}, nil
}

func (f *fakeInstancesAPI) List(_ context.Context, _ *computepb.ListInstancesRequest, _ ...gaxCallOption) instanceIterator {
	return &fakeIterator{items: f.list}
}

func newTestClient(t *testing.T, api instancesAPI, cfg Config) *Client {
	t.Helper()
	if cfg.StartupScriptPath == "" {
		path := filepath.Join(t.TempDir(), "startup.tmpl")
		if err := os.WriteFile(path, []byte("name={{.RunnerName}} repo={{.RunnerRepo}} token={{.RunnerToken}}"), 0o600); err != nil {
			t.Fatalf("write template: %v", err)
		}
		cfg.StartupScriptPath = path
	}
	if cfg.Project == "" {
		cfg.Project = "test-project"
	}
	if cfg.Zone == "" {
		cfg.Zone = "us-central1-a"
	}
	c, err := newClientWithAPI(api, cfg)
	if err != nil {
		t.Fatalf("newClientWithAPI: %v", err)
	}
	return c
}

func TestNewClientDefaults(t *testing.T) {
	c := newTestClient(t, &fakeInstancesAPI{}, Config{})

	if c.machineType != defaultMachineType {
		t.Errorf("machineType = %q, want %q", c.machineType, defaultMachineType)
	}
	if c.sourceImage != defaultImage {
		t.Errorf("sourceImage = %q, want %q", c.sourceImage, defaultImage)
	}
	if c.network != defaultNetwork {
		t.Errorf("network = %q, want %q", c.network, defaultNetwork)
	}
}

func TestNewClientRequiresProjectAndZone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.tmpl")
	_ = os.WriteFile(path, []byte("x"), 0o600)

	if _, err := newClientWithAPI(&fakeInstancesAPI{}, Config{Zone: "z", StartupScriptPath: path}); err == nil {
		t.Error("expected error when project is empty")
	}
	if _, err := newClientWithAPI(&fakeInstancesAPI{}, Config{Project: "p", StartupScriptPath: path}); err == nil {
		t.Error("expected error when zone is empty")
	}
	if _, err := newClientWithAPI(&fakeInstancesAPI{}, Config{Project: "p", Zone: "z"}); err == nil {
		t.Error("expected error when startup-script path is empty")
	}
}

func TestCreateBuildsSpotInstance(t *testing.T) {
	api := &fakeInstancesAPI{}
	c := newTestClient(t, api, Config{
		MachineType: "e2-custom-4-8192",
		RunnerSA:    "runner-node@test-project.iam.gserviceaccount.com",
		Subnet:      "regions/us-central1/subnetworks/runners",
	})

	inst, err := c.Create(context.Background(), provider.RunnerParams{
		RunnerName:  "eph-repo-1-1234",
		RunnerRepo:  "org/repo",
		RunnerToken: "AABBCC",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if inst.Name != "eph-repo-1-1234" {
		t.Errorf("instance name = %q", inst.Name)
	}

	if len(api.inserted) != 1 {
		t.Fatalf("expected 1 insert, got %d", len(api.inserted))
	}
	got := api.inserted[0].GetInstanceResource()

	if got.GetScheduling().GetProvisioningModel() != "SPOT" {
		t.Errorf("provisioning model = %q, want SPOT", got.GetScheduling().GetProvisioningModel())
	}
	if !got.GetScheduling().GetPreemptible() {
		t.Error("expected preemptible=true")
	}
	if got.GetScheduling().GetInstanceTerminationAction() != "DELETE" {
		t.Errorf("termination action = %q, want DELETE", got.GetScheduling().GetInstanceTerminationAction())
	}

	// The runner needs an ephemeral external IP for GitHub and GCP API egress.
	ifaces := got.GetNetworkInterfaces()
	if len(ifaces) != 1 || len(ifaces[0].GetAccessConfigs()) != 1 {
		t.Fatalf("expected single interface with one access config, got %+v", ifaces)
	}
	if access := ifaces[0].GetAccessConfigs()[0]; access.GetName() != "External NAT" || access.GetType() != "ONE_TO_ONE_NAT" {
		t.Errorf("unexpected access config: %+v", access)
	}
	if ifaces[0].GetSubnetwork() != "regions/us-central1/subnetworks/runners" {
		t.Errorf("subnet = %q", ifaces[0].GetSubnetwork())
	}

	if sa := got.GetServiceAccounts(); len(sa) != 1 || sa[0].GetEmail() != "runner-node@test-project.iam.gserviceaccount.com" {
		t.Errorf("service account = %+v", sa)
	}

	if got.GetLabels()[labelManagedBy] != managedByValue {
		t.Errorf("missing managed-by label: %+v", got.GetLabels())
	}

	// Startup script rendered from params into the metadata item.
	var startup string
	for _, item := range got.GetMetadata().GetItems() {
		if item.GetKey() == "startup-script" {
			startup = item.GetValue()
		}
	}
	if startup != "name=eph-repo-1-1234 repo=org/repo token=AABBCC" {
		t.Errorf("startup-script = %q", startup)
	}
}

func TestDeleteByName(t *testing.T) {
	api := &fakeInstancesAPI{}
	c := newTestClient(t, api, Config{})

	if err := c.Delete(context.Background(), "eph-repo-9-9"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(api.deleted) != 1 || api.deleted[0].GetInstance() != "eph-repo-9-9" {
		t.Errorf("delete request = %+v", api.deleted)
	}
}

func TestCleanupOldDeletesOnlyStale(t *testing.T) {
	now := time.Now().UTC()
	api := &fakeInstancesAPI{
		list: []*computepb.Instance{
			{Name: proto.String("fresh"), CreationTimestamp: proto.String(now.Add(-5 * time.Minute).Format(time.RFC3339))},
			{Name: proto.String("stale"), CreationTimestamp: proto.String(now.Add(-90 * time.Minute).Format(time.RFC3339))},
			{Name: proto.String("ancient"), CreationTimestamp: proto.String(now.Add(-24 * time.Hour).Format(time.RFC3339))},
		},
	}
	c := newTestClient(t, api, Config{})

	deleted, err := c.CleanupOld(context.Background(), 60*time.Minute)
	if err != nil {
		t.Fatalf("CleanupOld: %v", err)
	}
	if deleted != 2 {
		t.Errorf("deleted = %d, want 2", deleted)
	}

	var names []string
	for _, d := range api.deleted {
		names = append(names, d.GetInstance())
	}
	if len(names) != 2 || names[0] != "stale" || names[1] != "ancient" {
		t.Errorf("deleted names = %v, want [stale ancient]", names)
	}
}

// An instance with an unparseable CreationTimestamp is skipped, not deleted,
// and does not abort the sweep.
func TestCleanupOldSkipsUnparseableTimestamp(t *testing.T) {
	now := time.Now().UTC()
	api := &fakeInstancesAPI{
		list: []*computepb.Instance{
			{Name: proto.String("garbage-ts"), CreationTimestamp: proto.String("not-a-timestamp")},
			{Name: proto.String("stale"), CreationTimestamp: proto.String(now.Add(-90 * time.Minute).Format(time.RFC3339))},
		},
	}
	c := newTestClient(t, api, Config{})

	deleted, err := c.CleanupOld(context.Background(), 60*time.Minute)
	if err != nil {
		t.Fatalf("CleanupOld: %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1 (garbage timestamp skipped)", deleted)
	}
	if len(api.deleted) != 1 || api.deleted[0].GetInstance() != "stale" {
		t.Errorf("deleted = %+v, want only [stale]", api.deleted)
	}
}

// A Delete failure on one stale instance is logged and skipped; the sweep
// continues and the returned count reflects only the successful deletes.
func TestCleanupOldContinuesPastDeleteError(t *testing.T) {
	now := time.Now().UTC()
	api := &fakeInstancesAPI{
		list: []*computepb.Instance{
			{Name: proto.String("doomed"), CreationTimestamp: proto.String(now.Add(-90 * time.Minute).Format(time.RFC3339))},
			{Name: proto.String("ok"), CreationTimestamp: proto.String(now.Add(-90 * time.Minute).Format(time.RFC3339))},
		},
		deleteErr: map[string]error{"doomed": errors.New("boom")},
	}
	c := newTestClient(t, api, Config{})

	deleted, err := c.CleanupOld(context.Background(), 60*time.Minute)
	if err != nil {
		t.Fatalf("CleanupOld returned error, want nil: %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1 (failed delete not counted)", deleted)
	}
	if len(api.deleted) != 1 || api.deleted[0].GetInstance() != "ok" {
		t.Errorf("deleted = %+v, want only [ok]", api.deleted)
	}
}

// Compile-time guarantee that *Client satisfies the shared interface.
var _ provider.Provisioner = (*Client)(nil)
