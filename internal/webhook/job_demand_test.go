package webhook

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/thomasvincent/github-runners-infra/internal/compute"
	gh "github.com/thomasvincent/github-runners-infra/internal/github"
	"github.com/thomasvincent/github-runners-infra/internal/state"
)

func claimedDemand(t *testing.T, store *state.FileStore) state.Record {
	t.Helper()
	record := state.Record{Key: "trusted/private-repo:42", JobID: 42, Owner: "trusted", Repository: "private-repo", Provider: "test", Labels: []string{"self-hosted", "chef"}}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	next, kind, err := store.ClaimNext(context.Background(), time.Now(), 3)
	if err != nil || next == nil || kind != state.WorkProvision {
		t.Fatalf("claim: %v %v", kind, err)
	}
	return *next
}

func TestDelayedQueuedEventCannotAllocateForAssignedOrCompletedJob(t *testing.T) {
	for _, status := range []gh.JobStatus{gh.JobInProgress, gh.JobCompleted} {
		t.Run(string(status), func(t *testing.T) {
			handler, store, githubClient, cloud := newTestHandler(t)
			record := claimedDemand(t, store)
			githubClient.jobStates = map[int64]gh.JobStatus{42: status}
			handler.process(context.Background(), record, state.WorkProvision)
			current, _, err := store.Get(context.Background(), record.Key)
			if err != nil || current.Status != state.StatusDeleted || current.ClaimedWork != "" {
				t.Fatalf("stale claim retained: %#v %v", current, err)
			}
			if githubClient.generated != 0 || cloud.created != 0 || len(cloud.deleted) != 0 {
				t.Fatal("obsolete job mutated runner capacity")
			}
		})
	}
}

func TestDemandLostDuringJITCreationRemovesRegistrationWithoutCreatingVM(t *testing.T) {
	handler, store, githubClient, cloud := newTestHandler(t)
	record := claimedDemand(t, store)
	githubClient.jobStateSequence = []gh.JobStatus{gh.JobQueued, gh.JobCompleted}
	handler.process(context.Background(), record, state.WorkProvision)
	current, _, err := store.Get(context.Background(), record.Key)
	if err != nil || current.Status != state.StatusDeleted || current.GitHubRunnerID != 0 {
		t.Fatalf("registration retained: %#v %v", current, err)
	}
	if githubClient.generated != 1 || len(githubClient.removed) != 1 || githubClient.removed[0] != 101 || cloud.created != 0 {
		t.Fatal("JIT-to-VM demand race was not closed")
	}
}

func TestUncertainDemandRefundsClaimWithoutAllocating(t *testing.T) {
	handler, store, githubClient, cloud := newTestHandler(t)
	record := claimedDemand(t, store)
	githubClient.jobStateErr = errors.New("GitHub unavailable")
	handler.process(context.Background(), record, state.WorkProvision)
	current, _, err := store.Get(context.Background(), record.Key)
	if err != nil || current.Status != state.StatusPending || current.ClaimedWork != "" || current.Attempts != 0 || !current.NextAttemptAt.After(time.Now()) {
		t.Fatalf("uncertain demand consumed claim: %#v %v", current, err)
	}
	if githubClient.generated != 0 || cloud.created != 0 || len(cloud.deleted) != 0 {
		t.Fatal("uncertain demand mutated capacity")
	}
}

func TestDemandLostToBusyJITKeepsRegistrationAndDefers(t *testing.T) {
	handler, store, githubClient, cloud := newTestHandler(t)
	record := claimedDemand(t, store)
	githubClient.jobStateSequence = []gh.JobStatus{gh.JobQueued, gh.JobCompleted}
	githubClient.removeErr = map[int64]error{101: &gh.APIStatusError{Status: http.StatusUnprocessableEntity, Action: "removing busy runner"}}
	handler.process(context.Background(), record, state.WorkProvision)
	current, _, err := store.Get(context.Background(), record.Key)
	if err != nil || current.Status != state.StatusPending || current.GitHubRunnerID != 101 || current.Attempts != 0 || current.ClaimedWork != "" {
		t.Fatalf("busy registration discarded: %#v %v", current, err)
	}
	if cloud.created != 0 || len(cloud.deleted) != 0 {
		t.Fatal("busy runner triggered provider mutation")
	}
}

func TestCompletedOriginalJobOnlyReclaimsItsIdleRunner(t *testing.T) {
	handler, store, githubClient, cloud := newTestHandler(t)
	record := claimedDemand(t, store)
	if err := store.MarkJITCreated(context.Background(), record.Key, 101, "runner"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProvisioned(context.Background(), record.Key, "500", 101, "runner"); err != nil {
		t.Fatal(err)
	}
	record, _, _ = store.Get(context.Background(), record.Key)
	cloud.exists = &compute.RunnerInstance{ID: "500", Name: "runner"}
	githubClient.jobStates = map[int64]gh.JobStatus{42: gh.JobCompleted}
	githubClient.removeErr = map[int64]error{101: &gh.APIStatusError{Status: 422, Action: "removing busy runner"}}
	if _, err := handler.reconcileProviderFoundRunner(context.Background(), record); err == nil {
		t.Fatal("busy removal unexpectedly succeeded")
	}
	current, _, _ := store.Get(context.Background(), record.Key)
	if current.Status != state.StatusProvisioned || len(cloud.deleted) != 0 {
		t.Fatal("original completion killed a different busy job")
	}
	githubClient.removeErr = nil
	if _, err := handler.reconcileProviderFoundRunner(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	next, kind, err := store.ClaimNext(context.Background(), time.Now(), 3)
	if err != nil || next == nil || kind != state.WorkDelete {
		t.Fatalf("idle runner not scheduled for owned cleanup: %v", err)
	}
	handler.process(context.Background(), *next, kind)
	current, _, _ = store.Get(context.Background(), record.Key)
	if current.Status != state.StatusDeleted || len(cloud.deleted) == 0 {
		t.Fatal("completed idle runner retained provider capacity")
	}
}

func TestUnconfirmedOriginalJobRetainsProvisionedRunner(t *testing.T) {
	handler, store, githubClient, cloud := newTestHandler(t)
	record := claimedDemand(t, store)
	if err := store.MarkProvisioned(context.Background(), record.Key, "500", 101, "runner"); err != nil {
		t.Fatal(err)
	}
	record, _, _ = store.Get(context.Background(), record.Key)
	githubClient.jobStateErr = errors.New("GitHub unavailable")
	if _, err := handler.reconcileProviderFoundRunner(context.Background(), record); err == nil {
		t.Fatal("unconfirmed demand unexpectedly succeeded")
	}
	current, _, _ := store.Get(context.Background(), record.Key)
	if current.Status != state.StatusProvisioned || len(githubClient.removed) != 0 || len(cloud.deleted) != 0 {
		t.Fatal("API outage reclaimed an active runner")
	}
}
