package webhook

import (
	"context"
	"errors"
	"testing"
	"time"

	gh "github.com/thomasvincent/github-runners-infra/internal/github"
	"github.com/thomasvincent/github-runners-infra/internal/state"
)

func reassignedCompletion(t *testing.T, store *state.FileStore) state.Record {
	t.Helper()
	ctx := context.Background()
	record := claimedDemand(t, store)
	if err := store.MarkJITCreated(ctx, record.Key, 101, "runner"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProvisioned(ctx, record.Key, "500", 101, "runner"); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordCompletion(ctx, state.Record{Key: "trusted/private-repo:43", JobID: 43, Owner: record.Owner, Repository: record.Repository, GitHubRunnerID: 101}); err != nil {
		t.Fatal(err)
	}
	next, kind, err := store.ClaimNext(ctx, time.Now(), 3)
	if err != nil || next == nil || kind != state.WorkDelete || !next.RecheckDemand {
		t.Fatalf("reassigned completion claim: %#v %v %v", next, kind, err)
	}
	return *next
}

func TestReassignedRunnerCleanupReplenishesOnlyQueuedOriginalDemand(t *testing.T) {
	for _, status := range []gh.JobStatus{gh.JobQueued, gh.JobInProgress, gh.JobCompleted} {
		t.Run(string(status), func(t *testing.T) {
			handler, store, githubClient, cloud := newTestHandler(t)
			record := reassignedCompletion(t, store)
			githubClient.jobStates = map[int64]gh.JobStatus{42: status}
			handler.process(context.Background(), record, state.WorkDelete)
			current, _, err := store.Get(context.Background(), record.Key)
			if err != nil || len(cloud.deleted) != 1 || len(githubClient.removed) != 1 {
				t.Fatalf("owned cleanup incomplete: %#v %v", current, err)
			}
			if status != gh.JobQueued {
				if current.Status != state.StatusDeleted {
					t.Fatal("assigned/completed original demand was restored")
				}
				return
			}
			if current.Status != state.StatusPending || current.InstanceID != "" || current.GitHubRunnerID != 0 || current.RecheckDemand || current.ProvisionEpoch != 1 {
				t.Fatalf("queued original demand lost or retained old identity: %#v", current)
			}
			next, kind, err := store.ClaimNext(context.Background(), time.Now(), 3)
			if err != nil || next == nil || next.Key != record.Key || kind != state.WorkProvision {
				t.Fatalf("replacement not claimable: %#v %v", next, err)
			}
			handler.process(context.Background(), *next, kind)
			if githubClient.generated != 1 || cloud.created != 1 {
				t.Fatal("legitimate remaining queued job did not receive replacement capacity")
			}
		})
	}
}

func TestReassignedCleanupDefersUnknownDemandWithoutLosingIt(t *testing.T) {
	handler, store, githubClient, cloud := newTestHandler(t)
	record := reassignedCompletion(t, store)
	githubClient.jobStateErr = errors.New("GitHub unavailable")
	handler.process(context.Background(), record, state.WorkDelete)
	current, _, err := store.Get(context.Background(), record.Key)
	if err != nil || current.Status != state.StatusCompleted || !current.RecheckDemand || current.ClaimedWork != "" || current.DeleteAttempts != 0 || !current.NextAttemptAt.After(time.Now()) {
		t.Fatalf("unconfirmed demand was lost: %#v %v", current, err)
	}
	if cloud.created != 0 || githubClient.generated != 0 {
		t.Fatal("uncertain demand allocated replacement capacity")
	}
}

func TestOriginalCompletionWinsOverStaleQueuedReassignmentObservation(t *testing.T) {
	handler, store, githubClient, cloud := newTestHandler(t)
	record := reassignedCompletion(t, store)
	if err := store.RecordCompletion(context.Background(), state.Record{Key: record.Key, JobID: 42, Owner: record.Owner, Repository: record.Repository, GitHubRunnerID: 202}); err != nil {
		t.Fatal(err)
	}
	githubClient.jobStates = map[int64]gh.JobStatus{42: gh.JobQueued}
	handler.process(context.Background(), record, state.WorkDelete)
	current, _, err := store.Get(context.Background(), record.Key)
	if err != nil || current.Status != state.StatusDeleted || !current.JobCompleted || cloud.created != 0 {
		t.Fatalf("authoritative original completion was resurrected: %#v %v", current, err)
	}
}

func TestKnownCompletedJobCannotAllocateDespiteStaleQueuedAPI(t *testing.T) {
	handler, store, githubClient, cloud := newTestHandler(t)
	record := claimedDemand(t, store)
	if err := store.RecordCompletion(context.Background(), state.Record{Key: record.Key, JobID: 42, Owner: record.Owner, Repository: record.Repository, GitHubRunnerID: 202}); err != nil {
		t.Fatal(err)
	}
	githubClient.jobStates = map[int64]gh.JobStatus{42: gh.JobQueued}
	handler.process(context.Background(), record, state.WorkProvision)
	if githubClient.generated != 0 || cloud.created != 0 {
		t.Fatal("stale queued API resurrected authoritative completed demand")
	}
}

func TestReassignedDemandCannotReplenishBeforeProviderAbsence(t *testing.T) {
	handler, store, githubClient, cloud := newTestHandler(t)
	record := reassignedCompletion(t, store)
	checker := &deletionConfirmationCompute{absenceCompute: &absenceCompute{fakeCompute: cloud}}
	handler.computeClient = checker
	githubClient.jobStateSequence = []gh.JobStatus{gh.JobQueued}
	handler.process(context.Background(), record, state.WorkDelete)
	current, _, err := store.Get(context.Background(), record.Key)
	if err != nil || current.Status != state.StatusCompleted || current.DeletionAcceptedID != "500" || !current.RecheckDemand {
		t.Fatalf("unconfirmed absence lost demand: %#v %v", current, err)
	}
	if len(githubClient.jobStateSequence) != 1 || len(githubClient.removed) != 0 || cloud.created != 0 {
		t.Fatal("demand decision or replacement preceded confirmed provider absence")
	}
	checker.absent = true
	next, kind, err := store.ClaimNext(context.Background(), time.Now().Add(time.Hour), 3)
	if err != nil || next == nil || kind != state.WorkDelete {
		t.Fatalf("confirmation claim: %#v %v", next, err)
	}
	handler.process(context.Background(), *next, kind)
	current, _, _ = store.Get(context.Background(), record.Key)
	if current.Status != state.StatusPending || len(cloud.deleted) != 1 || checker.excludedID != "500" {
		t.Fatal("confirmed cleanup did not restore demand safely")
	}
}
