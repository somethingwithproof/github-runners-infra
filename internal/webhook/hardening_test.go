// SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
// SPDX-License-Identifier: MIT

package webhook

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/thomasvincent/github-runners-infra/internal/compute"
	gh "github.com/thomasvincent/github-runners-infra/internal/github"
	"github.com/thomasvincent/github-runners-infra/internal/state"
)

func TestServeHTTPRejectsNonPositiveWorkflowJobID(t *testing.T) {
	handler, store, _, _ := newTestHandler(t)
	for _, jobID := range []int64{0, -1, -999} {
		event := testEvent("queued", []string{"self-hosted", "chef"})
		event.WorkflowJob.ID = jobID
		resp := serveEvent(handler, event, "delivery-bad-id")
		if resp.Code != http.StatusBadRequest {
			t.Fatalf("jobID=%d status = %d, want 400", jobID, resp.Code)
		}
		if _, found, err := store.Get(context.Background(), jobKey("trusted/private-repo", jobID)); err != nil || found {
			t.Fatalf("jobID=%d persisted invalid record: found=%v err=%v", jobID, found, err)
		}
	}
}

func TestPersistent404WorkflowJobStatusExhaustsProvisioningDemandBudget(t *testing.T) {
	handler, store, githubClient, cloud := newTestHandler(t)
	handler.maxAttempts = 2
	ctx := context.Background()
	if err := store.SetMaxLiveRunners(1); err != nil {
		t.Fatal(err)
	}
	githubClient.jobStateErr = &gh.APIStatusError{Status: http.StatusNotFound, Action: "getting workflow job status"}

	key := "trusted/private-repo:404"
	if _, err := store.Create(ctx, state.Record{
		Key: key, DeliveryID: "d404", JobID: 404, Owner: "trusted", Repository: "private-repo",
		Provider: "test", Labels: []string{"self-hosted", "chef"},
	}); err != nil {
		t.Fatal(err)
	}

	for attempt := 1; attempt <= 2; attempt++ {
		claimed, kind, err := store.ClaimNext(ctx, time.Now().Add(time.Hour), handler.maxAttempts)
		if err != nil || claimed == nil || kind != state.WorkProvision {
			t.Fatalf("attempt %d claim = %#v %q %v", attempt, claimed, kind, err)
		}
		handler.process(ctx, *claimed, kind)
	}

	persisted, _, err := store.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Status != state.StatusFailed || persisted.DeferDeletion || !persisted.NextAttemptAt.IsZero() {
		t.Fatalf("persistent 404 provisioning demand did not terminate: %#v", persisted)
	}
	if cloud.created != 0 || githubClient.generated != 0 {
		t.Fatalf("404 job mutated capacity: created=%d generated=%d", cloud.created, githubClient.generated)
	}

	// Capacity slot must be freed for subsequent jobs.
	if _, err := store.Create(ctx, state.Record{Key: "trusted/private-repo:405", DeliveryID: "d405", JobID: 405, Provider: "test"}); err != nil {
		t.Fatal(err)
	}
	next, kind, err := store.ClaimNext(ctx, time.Now(), handler.maxAttempts)
	if err != nil || next == nil || next.Key != "trusted/private-repo:405" || kind != state.WorkProvision {
		t.Fatalf("slot leaked after persistent 404: %#v %q %v", next, kind, err)
	}
}

func TestPersistent404WorkflowJobStatusExhaustsReassignedDemandBudget(t *testing.T) {
	handler, store, githubClient, _ := newTestHandler(t)
	handler.maxAttempts = 2
	ctx := context.Background()
	if err := store.SetMaxLiveRunners(1); err != nil {
		t.Fatal(err)
	}
	record := reassignedCompletion(t, store)
	githubClient.jobStateErr = &gh.APIStatusError{Status: http.StatusNotFound, Action: "getting workflow job status"}

	handler.process(ctx, record, state.WorkDelete)
	mid, _, _ := store.Get(ctx, record.Key)
	if mid.Status != state.StatusCompleted || mid.ReconcileFailures != 1 {
		t.Fatalf("first 404 state = %#v", mid)
	}

	next, kind, err := store.ClaimNext(ctx, time.Now().Add(time.Hour), handler.maxAttempts)
	if err != nil || next == nil || kind != state.WorkDelete {
		t.Fatalf("second delete claim = %#v %q %v", next, kind, err)
	}
	handler.process(ctx, *next, kind)

	final, _, err := store.Get(ctx, record.Key)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != state.StatusDeleted || final.RecheckDemand || !final.NextAttemptAt.IsZero() {
		t.Fatalf("persistent 404 reassigned demand did not terminate: %#v", final)
	}

	// Slot must now be free.
	if _, err := store.Create(ctx, state.Record{Key: "trusted/private-repo:99", DeliveryID: "d99", JobID: 99, Provider: "test"}); err != nil {
		t.Fatal(err)
	}
	claimed, kind, err := store.ClaimNext(ctx, time.Now(), handler.maxAttempts)
	if err != nil || claimed == nil || claimed.Key != "trusted/private-repo:99" || kind != state.WorkProvision {
		t.Fatalf("slot leaked after reassigned 404: %#v %q %v", claimed, kind, err)
	}
}

func TestAbsenceCheckerProviderNeverUsesBlindAgeReleaseForLiveOrphans(t *testing.T) {
	handler, store, _, client := newTestHandler(t)
	checker := &absenceCompute{fakeCompute: client, absent: false}
	handler.computeClient = checker
	ctx := context.Background()
	if err := store.SetMaxLiveRunners(1); err != nil {
		t.Fatal(err)
	}

	key := "trusted/private-repo:tag-stripped"
	if _, err := store.Create(ctx, state.Record{Key: key, DeliveryID: "d1", Provider: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ClaimNext(ctx, time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProvisioned(ctx, key, "inst-live", 101, "runner"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkOrphaned(ctx, key, compute.ErrOwnershipMismatch.Error()); err != nil {
		t.Fatal(err)
	}

	// Even when the sweep cutoff is in the future (orphan looks aged) and sweep
	// succeeds with 0 errors, exact-ID RunnerAbsent=false must keep the slot reserved.
	if errs := handler.reconcileLiveness(ctx, time.Now().Add(time.Hour)); len(errs) != 0 {
		t.Fatalf("reconcileLiveness errors = %v", errs)
	}
	persisted, _, err := store.Get(ctx, key)
	if err != nil || persisted.InstanceID != "inst-live" {
		t.Fatalf("blind ReleaseSweptOrphans cleared live orphan: %#v, %v", persisted, err)
	}
	if _, err := store.Create(ctx, state.Record{Key: "trusted/private-repo:next", DeliveryID: "d2", Provider: "test"}); err != nil {
		t.Fatal(err)
	}
	if next, _, err := store.ClaimNext(ctx, time.Now(), 3); err != nil || next != nil {
		t.Fatalf("live orphan slot leaked to new job: %#v, %v", next, err)
	}
}

type sweepHookCompute struct {
	*fakeCompute
	onSweep func()
}

func (c *sweepHookCompute) SweepOrphanedRunners(ctx context.Context, known map[string]struct{}, cutoff time.Time) (int, error) {
	if c.onSweep != nil {
		c.onSweep()
	}
	return c.fakeCompute.SweepOrphanedRunners(ctx, known, cutoff)
}

func TestNonCheckerSweepProtectsOwnershipMismatchAndTOCTOUOrphans(t *testing.T) {
	handler, store, _, client := newTestHandler(t)
	ctx := context.Background()

	// Create an ownership-mismatch orphan and an active deleting instance that
	// transitions to orphaned during SweepOrphanedRunners (TOCTOU window).
	ownershipKey := "trusted/private-repo:own"
	toctouKey := "trusted/private-repo:toctou"
	for _, item := range []struct {
		key  string
		inst string
	}{
		{ownershipKey, "inst-own"},
		{toctouKey, "inst-toctou"},
	} {
		if _, err := store.Create(ctx, state.Record{Key: item.key, DeliveryID: item.key, Provider: "test"}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.ClaimNext(ctx, time.Now(), 3); err != nil {
			t.Fatal(err)
		}
		if err := store.MarkProvisioned(ctx, item.key, item.inst, 10, "runner"); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.MarkOrphaned(ctx, ownershipKey, compute.ErrOwnershipMismatch.Error()); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkCompleted(ctx, toctouKey); err != nil {
		t.Fatal(err)
	}

	// Ensure sweepHookCompute does NOT implement instanceAbsenceChecker.
	handler.computeClient = &sweepHookCompute{
		fakeCompute: client,
		onSweep: func() {
			// Concurrent transition to orphaned while instance was in knownInstances.
			_ = store.MarkOrphaned(ctx, toctouKey, "deletion retry budget exhausted")
		},
	}

	if errs := handler.reconcileLiveness(ctx, time.Now().Add(time.Hour)); len(errs) != 0 {
		t.Fatalf("reconcileLiveness errors = %v", errs)
	}
	for _, tc := range []struct {
		key      string
		wantInst string
	}{
		{ownershipKey, "inst-own"},
		{toctouKey, "inst-toctou"},
	} {
		rec, _, err := store.Get(ctx, tc.key)
		if err != nil || rec.InstanceID != tc.wantInst {
			t.Fatalf("%s InstanceID = %q, want %q (err=%v)", tc.key, rec.InstanceID, tc.wantInst, err)
		}
	}
}

func TestHTTPClientTimeoutConsumesBudgetWhileParentCancelRefundsClaim(t *testing.T) {
	t.Run("provision client timeout backs off and consumes attempt", func(t *testing.T) {
		handler, store, _, cloud := newTestHandler(t)
		record := claimedDemand(t, store)
		cloud.createErr = context.DeadlineExceeded
		handler.provision(context.Background(), record)
		current, _, err := store.Get(context.Background(), record.Key)
		if err != nil {
			t.Fatal(err)
		}
		if current.Status != state.StatusPending || current.Attempts != 1 || !current.NextAttemptAt.After(time.Now()) {
			t.Fatalf("client timeout refunded claim instead of backing off: %#v", current)
		}
	})

	t.Run("provision parent cancel refunds claim without backoff", func(t *testing.T) {
		handler, store, _, cloud := newTestHandler(t)
		record := claimedDemand(t, store)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		cloud.createErr = context.Canceled
		handler.provision(ctx, record)
		current, _, err := store.Get(context.Background(), record.Key)
		if err != nil {
			t.Fatal(err)
		}
		if current.Status != state.StatusPending || current.Attempts != 0 || !current.NextAttemptAt.IsZero() {
			t.Fatalf("parent cancel consumed attempt: %#v", current)
		}
	})

	t.Run("delete client timeout backs off and consumes attempt", func(t *testing.T) {
		handler, store, _, cloud := newTestHandler(t)
		ctx := context.Background()
		record := claimedDemand(t, store)
		if err := store.MarkProvisioned(ctx, record.Key, "inst-del", 101, "runner"); err != nil {
			t.Fatal(err)
		}
		if err := store.MarkCompleted(ctx, record.Key); err != nil {
			t.Fatal(err)
		}
		claimed, kind, err := store.ClaimNext(ctx, time.Now(), 3)
		if err != nil || claimed == nil || kind != state.WorkDelete {
			t.Fatalf("claim delete = %#v %q %v", claimed, kind, err)
		}
		cloud.deleteErr = context.DeadlineExceeded
		handler.delete(ctx, *claimed)
		current, _, err := store.Get(ctx, record.Key)
		if err != nil {
			t.Fatal(err)
		}
		if current.Status != state.StatusCompleted || current.DeleteAttempts != 1 || !current.NextAttemptAt.After(time.Now()) {
			t.Fatalf("delete client timeout refunded claim instead of backing off: %#v", current)
		}
	})
}

func TestStaleReaperSnapshotCannotClearOrScheduleDeletionOfReplacement(t *testing.T) {
	handler, store, githubClient, cloud := newTestHandler(t)
	ctx := context.Background()
	record := claimedDemand(t, store)
	if err := store.MarkJITCreated(ctx, record.Key, 101, "runner-old"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProvisioned(ctx, record.Key, "inst-old", 101, "runner-old"); err != nil {
		t.Fatal(err)
	}
	staleSnapshot, _, err := store.Get(ctx, record.Key)
	if err != nil {
		t.Fatal(err)
	}

	// Replenish the record to epoch 1 with a new instance and JIT runner.
	if err := store.RecordCompletion(ctx, state.Record{
		Key: "trusted/private-repo:43", JobID: 43, Owner: record.Owner, Repository: record.Repository, GitHubRunnerID: 101,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ClaimNext(ctx, time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	if restored, err := store.FinishDeletedRunner(ctx, record.Key, true); err != nil || !restored {
		t.Fatalf("FinishDeletedRunner = %v, %v", restored, err)
	}
	if _, _, err := store.ClaimNext(ctx, time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkJITCreated(ctx, record.Key, 202, "runner-new"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProvisioned(ctx, record.Key, "inst-new", 202, "runner-new"); err != nil {
		t.Fatal(err)
	}

	// Stale reaper snapshot sees original job completed and runs reconcileProviderFoundRunner.
	cloud.exists = &compute.RunnerInstance{ID: "inst-old", Name: "runner-old"}
	githubClient.jobStates = map[int64]gh.JobStatus{42: gh.JobCompleted}
	if _, err := handler.reconcileProviderFoundRunner(ctx, staleSnapshot); err != nil {
		t.Fatalf("reconcileProviderFoundRunner = %v", err)
	}
	// Stale reaper also runs expireRunner against the old snapshot.
	if err := handler.expireRunner(ctx, staleSnapshot); err != nil {
		t.Fatalf("expireRunner = %v", err)
	}

	current, _, err := store.Get(ctx, record.Key)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != state.StatusProvisioned || current.ProvisionEpoch != 1 || current.InstanceID != "inst-new" || current.GitHubRunnerID != 202 || !current.GitHubRunnerOwned {
		t.Fatalf("stale reaper snapshot corrupted replacement runner: %#v", current)
	}
}

func TestDeletionConfirmationExponentialBackoffAndEscalation(t *testing.T) {
	handler, store, _, client := newTestHandler(t)
	handler.maxAttempts = 1
	checker := &deletionConfirmationCompute{absenceCompute: &absenceCompute{fakeCompute: client, absent: false}}
	handler.computeClient = checker
	ctx := context.Background()

	key := "trusted/private-repo:confirm-esc"
	if _, err := store.Create(ctx, state.Record{Key: key, DeliveryID: "d-esc", Provider: "test", Owner: "trusted", Repository: "private-repo"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ClaimNext(ctx, time.Now(), 1); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProvisioned(ctx, key, "inst-stuck", 101, "runner"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkCompleted(ctx, key); err != nil {
		t.Fatal(err)
	}

	var firstBackoff, secondBackoff time.Duration
	for attempt := 1; attempt <= 10; attempt++ {
		claimed, kind, err := store.ClaimNext(ctx, time.Now().Add(time.Hour), 1)
		if err != nil || claimed == nil || kind != state.WorkDelete {
			t.Fatalf("attempt %d claim = %#v %q %v", attempt, claimed, kind, err)
		}
		before := time.Now()
		handler.delete(ctx, *claimed)
		cur, _, err := store.Get(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		switch attempt {
		case 1:
			firstBackoff = cur.NextAttemptAt.Sub(before)
		case 2:
			secondBackoff = cur.NextAttemptAt.Sub(before)
		}
	}
	if secondBackoff <= firstBackoff {
		t.Fatalf("expected exponential backoff: first=%v second=%v", firstBackoff, secondBackoff)
	}
	final, _, err := store.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != state.StatusOrphaned || final.InstanceID != "inst-stuck" || final.ConfirmAttempts != 10 {
		t.Fatalf("stuck deletion confirmation did not escalate to orphaned: %#v", final)
	}
}
