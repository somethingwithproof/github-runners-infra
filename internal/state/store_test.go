package state

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFileStoreMigratesLegacyDropletID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	legacy := diskState{Records: map[string]Record{
		"org/repo:1": {Key: "org/repo:1", DropletID: 123, Status: StatusProvisioned},
	}}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	record, found, err := store.Get(context.Background(), "org/repo:1")
	if err != nil || !found || record.InstanceID != "123" || record.DropletID != 0 || record.Provider != "digitalocean" {
		t.Fatalf("legacy migration = %#v, %v, %v", record, found, err)
	}
}

func TestKnownInstanceIDsExcludesOrphanedRecords(t *testing.T) {
	store := openTestStore(t)
	record := Record{Key: "org/repo:orphaned-instance", DeliveryID: "delivery-orphaned", Provider: "aws"}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ClaimNext(context.Background(), time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProvisioned(context.Background(), record.Key, "i-orphaned", 1, "runner"); err != nil {
		t.Fatal(err)
	}
	known, err := store.KnownInstanceIDs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := known["i-orphaned"]; !ok {
		t.Fatal("active instance was absent from known IDs")
	}
	if err := store.MarkOrphaned(context.Background(), record.Key, "operator attention required"); err != nil {
		t.Fatal(err)
	}
	known, err = store.KnownInstanceIDs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := known["i-orphaned"]; ok {
		t.Fatal("orphaned instance was shielded from provider sweep")
	}
}

func TestMissingConfirmationsAreIsolatedBySource(t *testing.T) {
	store := openTestStore(t)
	record := Record{Key: "org/repo:missing-sources", DeliveryID: "delivery-missing", Provider: "aws"}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ClaimNext(context.Background(), time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProvisioned(context.Background(), record.Key, "i-missing", 1, "runner"); err != nil {
		t.Fatal(err)
	}
	for observation := 0; observation < 2; observation++ {
		if confirmed, err := store.ObserveGitHubRunnerMissing(context.Background(), record.Key, time.Now(), 0, 3); err != nil || confirmed {
			t.Fatalf("GitHub observation %d = %v, %v", observation, confirmed, err)
		}
	}
	if confirmed, err := store.ObserveRunnerMissing(context.Background(), record.Key, time.Now(), 0, 3); err != nil || confirmed {
		t.Fatalf("provider observation inherited GitHub confirmations: %v, %v", confirmed, err)
	}
	persisted, _, err := store.Get(context.Background(), record.Key)
	if err != nil || persisted.GitHubMissingChecks != 2 || persisted.MissingChecks != 1 {
		t.Fatalf("isolated missing counters = %#v, %v", persisted, err)
	}
}

func TestListExpiredPreservesDeletionBackoff(t *testing.T) {
	store, err := OpenFileStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	record := Record{Key: "org/repo:delete-backoff", DeliveryID: "delivery-backoff", Provider: "aws"}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkCompleted(context.Background(), record.Key); err != nil {
		t.Fatal(err)
	}
	retryAt := time.Now().Add(time.Hour)
	if err := store.MarkDeleteFailed(context.Background(), record.Key, "transient", retryAt, 5); err != nil {
		t.Fatal(err)
	}
	expired, err := store.ListExpired(context.Background(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 0 {
		t.Fatalf("deletion backoff was ignored: %#v", expired)
	}
}

func TestRecoverRecordNeverPromotesRunnerOwnership(t *testing.T) {
	record := Record{Key: "org/repo:foreign", GitHubRunnerID: 99, GitHubRunnerOwned: false, Provider: "aws"}
	recovered, keep, _ := recoverRecord(record.Key, record, time.Now(), 24*time.Hour)
	if !keep || recovered.GitHubRunnerOwned {
		t.Fatalf("recovery promoted runner ownership: %#v", recovered)
	}
}

func TestFileStoreRejectsSecondWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	first, err := OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFileStore(path); err == nil {
		t.Fatal("second writer acquired the lifecycle state lock")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := OpenFileStore(path)
	if err != nil {
		t.Fatalf("lock was not released on close: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestFileStoreLifecycleAndIdempotency(t *testing.T) {
	store := openTestStore(t)
	path := store.path
	record := Record{Key: "org/repo:42", DeliveryID: "delivery-1", JobID: 42, Owner: "org", Repository: "repo"}
	created, err := store.Create(context.Background(), record)
	if err != nil || !created {
		t.Fatalf("first create = %v, %v", created, err)
	}
	created, err = store.Create(context.Background(), record)
	if err != nil || created {
		t.Fatalf("duplicate create = %v, %v", created, err)
	}

	claimed, kind, err := store.ClaimNext(context.Background(), time.Now(), 3)
	if err != nil || claimed == nil || kind != WorkProvision || claimed.Attempts != 1 {
		t.Fatalf("provision claim = %#v, %q, %v", claimed, kind, err)
	}
	if err := store.MarkProvisioned(context.Background(), record.Key, "100", 200, "runner"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkCompleted(context.Background(), record.Key); err != nil {
		t.Fatal(err)
	}
	claimed, kind, err = store.ClaimNext(context.Background(), time.Now(), 3)
	if err != nil || claimed == nil || kind != WorkDelete || claimed.InstanceID != "100" {
		t.Fatalf("delete claim = %#v, %q, %v", claimed, kind, err)
	}
	if err := store.MarkDeleted(context.Background(), record.Key); err != nil {
		t.Fatal(err)
	}
	final, found, err := store.Get(context.Background(), record.Key)
	if err != nil || !found || final.Status != StatusDeleted {
		t.Fatalf("final record = %#v, %v, %v", final, found, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode = %o, want 600", info.Mode().Perm())
	}
}

func TestFileStoreRecoversInterruptedClaim(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	record := Record{Key: "org/repo:7", DeliveryID: "delivery-7"}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ClaimNext(context.Background(), time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkJITCreated(context.Background(), record.Key, 77, "runner-77"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	claimed, kind, err := reopened.ClaimNext(context.Background(), time.Now(), 3)
	if err != nil || claimed == nil || kind != WorkProvision || claimed.Attempts != 1 {
		t.Fatalf("recovered claim = %#v, %q, %v", claimed, kind, err)
	}
	if claimed.GitHubRunnerID != 77 {
		t.Fatalf("recovery lost JIT runner ownership: %#v", claimed)
	}
}

func TestRecoveryRetriesFinalInFlightAttempts(t *testing.T) {
	t.Run("provision", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "state.json")
		store, err := OpenFileStore(path)
		if err != nil {
			t.Fatal(err)
		}
		record := Record{Key: "org/repo:final-provision", DeliveryID: "delivery-final-provision"}
		if _, err := store.Create(context.Background(), record); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.ClaimNext(context.Background(), time.Now(), 1); err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := OpenFileStore(path)
		if err != nil {
			t.Fatal(err)
		}
		claimed, kind, err := reopened.ClaimNext(context.Background(), time.Now(), 1)
		if err != nil || claimed == nil || kind != WorkProvision || claimed.Attempts != 1 {
			t.Fatalf("recovered final provision = %#v, %q, %v", claimed, kind, err)
		}
	})

	t.Run("delete", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "state.json")
		store, err := OpenFileStore(path)
		if err != nil {
			t.Fatal(err)
		}
		record := Record{Key: "org/repo:final-delete", DeliveryID: "delivery-final-delete"}
		if _, err := store.Create(context.Background(), record); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.ClaimNext(context.Background(), time.Now(), 1); err != nil {
			t.Fatal(err)
		}
		if err := store.MarkProvisioned(context.Background(), record.Key, "instance", 1, "runner"); err != nil {
			t.Fatal(err)
		}
		if err := store.MarkCompleted(context.Background(), record.Key); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.ClaimNext(context.Background(), time.Now(), 1); err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := OpenFileStore(path)
		if err != nil {
			t.Fatal(err)
		}
		claimed, kind, err := reopened.ClaimNext(context.Background(), time.Now(), 1)
		if err != nil || claimed == nil || kind != WorkDelete || claimed.DeleteAttempts != 1 {
			t.Fatalf("recovered final delete = %#v, %q, %v", claimed, kind, err)
		}
	})
}

func TestClaimBackstopTerminatesExhaustedLegacyRecords(t *testing.T) {
	store := openTestStore(t)
	pending := Record{Key: "org/repo:stuck-provision", DeliveryID: "delivery-stuck-provision"}
	if _, err := store.Create(context.Background(), pending); err != nil {
		t.Fatal(err)
	}
	if err := store.update(pending.Key, func(record *Record) { record.Attempts = 3 }); err != nil {
		t.Fatal(err)
	}
	if claimed, _, err := store.ClaimNext(context.Background(), time.Now(), 3); err != nil || claimed != nil {
		t.Fatalf("exhausted pending claim = %#v, %v", claimed, err)
	}
	failed, _, _ := store.Get(context.Background(), pending.Key)
	if failed.Status != StatusFailed {
		t.Fatalf("exhausted pending status = %q", failed.Status)
	}

	completed := Record{Key: "org/repo:stuck-delete", DeliveryID: "delivery-stuck-delete"}
	if _, err := store.Create(context.Background(), completed); err != nil {
		t.Fatal(err)
	}
	if err := store.update(completed.Key, func(record *Record) {
		record.Status = StatusCompleted
		record.InstanceID = "instance"
		record.DeleteAttempts = 3
	}); err != nil {
		t.Fatal(err)
	}
	if claimed, _, err := store.ClaimNext(context.Background(), time.Now(), 3); err != nil || claimed != nil {
		t.Fatalf("exhausted completed claim = %#v, %v", claimed, err)
	}
	orphaned, _, _ := store.Get(context.Background(), completed.Key)
	if orphaned.Status != StatusOrphaned {
		t.Fatalf("exhausted completed status = %q", orphaned.Status)
	}
}

func TestClaimBackstopContinuesToNextEligibleRecord(t *testing.T) {
	store := openTestStore(t)
	exhausted := Record{Key: "org/repo:1-exhausted", DeliveryID: "delivery-exhausted"}
	eligible := Record{Key: "org/repo:2-eligible", DeliveryID: "delivery-eligible"}
	for _, record := range []Record{exhausted, eligible} {
		if _, err := store.Create(context.Background(), record); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.update(exhausted.Key, func(record *Record) { record.Attempts = 3 }); err != nil {
		t.Fatal(err)
	}
	claimed, kind, err := store.ClaimNext(context.Background(), time.Now(), 3)
	if err != nil || claimed == nil || claimed.Key != eligible.Key || kind != WorkProvision {
		t.Fatalf("claim after exhausted record = %#v, %q, %v", claimed, kind, err)
	}
	persisted, _, err := store.Get(context.Background(), exhausted.Key)
	if err != nil || persisted.Status != StatusFailed {
		t.Fatalf("exhausted record = %#v, %v", persisted, err)
	}
}

func TestMissingRunnerRotatesProvisionEpoch(t *testing.T) {
	store := openTestStore(t)
	record := Record{Key: "org/repo:epoch", DeliveryID: "delivery-epoch"}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ClaimNext(context.Background(), time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProvisioned(context.Background(), record.Key, "instance", 1, "runner"); err != nil {
		t.Fatal(err)
	}
	if err := store.RequeueMissingRunner(context.Background(), record.Key, 3); err != nil {
		t.Fatal(err)
	}
	requeued, _, _ := store.Get(context.Background(), record.Key)
	if requeued.ProvisionEpoch != 1 || requeued.Status != StatusPending {
		t.Fatalf("requeued record = %#v", requeued)
	}
}

func TestCompletionDuringProvisioningSchedulesDeletion(t *testing.T) {
	store := openTestStore(t)
	record := Record{Key: "org/repo:9", DeliveryID: "delivery-9"}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ClaimNext(context.Background(), time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkCompleted(context.Background(), record.Key); err != nil {
		t.Fatal(err)
	}
	expired, err := store.ListExpired(context.Background(), time.Now().Add(time.Hour))
	if err != nil || len(expired) != 0 {
		t.Fatalf("in-flight completion exposed to reaper: %#v, %v", expired, err)
	}
	if err := store.MarkProvisioned(context.Background(), record.Key, "99", 88, "runner"); err != nil {
		t.Fatal(err)
	}
	claimed, kind, err := store.ClaimNext(context.Background(), time.Now(), 3)
	if err != nil || claimed == nil || kind != WorkDelete {
		t.Fatalf("delete claim = %#v, %q, %v", claimed, kind, err)
	}
}

func TestCompletionRedeliveryCannotDuplicateDeleteClaim(t *testing.T) {
	store := openTestStore(t)
	record := Record{
		Key: "org/repo:redelivery", DeliveryID: "delivery-redelivery", JobID: 91,
		Owner: "org", Repository: "repo", Provider: "aws",
	}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ClaimNext(context.Background(), time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkJITCreated(context.Background(), record.Key, 901, "runner-901"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProvisioned(context.Background(), record.Key, "instance-901", 901, "runner-901"); err != nil {
		t.Fatal(err)
	}
	completion := Record{
		Key: record.Key, JobID: record.JobID, Owner: record.Owner, Repository: record.Repository,
		Provider: record.Provider, GitHubRunnerID: 901, RunnerName: "runner-901",
	}
	if err := store.RecordCompletion(context.Background(), completion); err != nil {
		t.Fatal(err)
	}
	claimed, kind, err := store.ClaimNext(context.Background(), time.Now(), 3)
	if err != nil || claimed == nil || kind != WorkDelete || claimed.DeleteAttempts != 1 {
		t.Fatalf("first delete claim = %#v, %q, %v", claimed, kind, err)
	}
	if err := store.RecordCompletion(context.Background(), completion); err != nil {
		t.Fatal(err)
	}
	duplicate, duplicateKind, err := store.ClaimNext(context.Background(), time.Now(), 3)
	if err != nil || duplicate != nil {
		t.Fatalf("duplicate delete claim = %#v, %q, %v", duplicate, duplicateKind, err)
	}
	persisted, _, err := store.Get(context.Background(), record.Key)
	if err != nil || persisted.Status != StatusDeleting || persisted.ClaimedWork != WorkDelete || persisted.DeleteAttempts != 1 {
		t.Fatalf("in-flight delete after redelivery = %#v, %v", persisted, err)
	}
}

func TestMatchingCompletionReconcilesOrphanedRunner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	record := Record{
		Key: "org/repo:orphan-completion", DeliveryID: "delivery-orphan-completion", JobID: 92,
		Owner: "org", Repository: "repo", Provider: "aws",
	}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ClaimNext(context.Background(), time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkJITCreated(context.Background(), record.Key, 902, "runner-902"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProvisioned(context.Background(), record.Key, "instance-902", 902, "runner-902"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkOrphaned(context.Background(), record.Key, "busy runner retry budget exhausted"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.RecordCompletion(context.Background(), Record{
		Key: record.Key, JobID: record.JobID, Owner: record.Owner, Repository: record.Repository,
		Provider: record.Provider, GitHubRunnerID: 902,
	}); err != nil {
		t.Fatal(err)
	}
	persisted, _, err := store.Get(context.Background(), record.Key)
	if err != nil || persisted.Status != StatusCompleted || persisted.DeferDeletion {
		t.Fatalf("orphan completion reconciliation = %#v, %v", persisted, err)
	}
}

func TestRecoveryRefundsLegacyCanceledDeleteAttempt(t *testing.T) {
	record := Record{
		Key: "org/repo:legacy-canceled-delete", Status: StatusCompleted,
		DeleteAttempts: 2, LastError: context.Canceled.Error(),
	}
	recovered, keep, changed := recoverRecord(record.Key, record, time.Now(), 24*time.Hour)
	if !keep || !changed || recovered.DeleteAttempts != 1 || recovered.LastError != "" {
		t.Fatalf("legacy canceled deletion recovery = %#v, keep=%v changed=%v", recovered, keep, changed)
	}
}

func TestReleaseClaimDoesNotBurnRetryBudget(t *testing.T) {
	store := openTestStore(t)
	record := Record{Key: "org/repo:shutdown", DeliveryID: "delivery-shutdown"}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	claimed, kind, err := store.ClaimNext(context.Background(), time.Now(), 3)
	if err != nil || claimed == nil || claimed.Attempts != 1 {
		t.Fatalf("claim = %#v, %q, %v", claimed, kind, err)
	}
	if err := store.ReleaseClaim(context.Background(), record.Key, kind); err != nil {
		t.Fatal(err)
	}
	released, _, err := store.Get(context.Background(), record.Key)
	if err != nil || released.Status != StatusPending || released.Attempts != 0 || released.ClaimedWork != "" {
		t.Fatalf("released claim = %#v, %v", released, err)
	}
}

func TestDeferredCancellationRemainsReclaimableAfterInterruptedProvision(t *testing.T) {
	for _, test := range []struct {
		name      string
		interrupt func(*testing.T, *FileStore, string)
	}{
		{
			name: "release claim",
			interrupt: func(t *testing.T, store *FileStore, key string) {
				t.Helper()
				if err := store.ReleaseClaim(context.Background(), key, WorkProvision); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "rate limited work",
			interrupt: func(t *testing.T, store *FileStore, key string) {
				t.Helper()
				if err := store.DeferRateLimitedWork(context.Background(), key, WorkProvision, "throttled", time.Now().Add(time.Hour), 3); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := openTestStore(t)
			if err := store.SetMaxLiveRunners(1); err != nil {
				t.Fatal(err)
			}
			record := Record{Key: "org/repo:cancelled", DeliveryID: "delivery-cancelled", Owner: "org", Repository: "repo"}
			if _, err := store.Create(context.Background(), record); err != nil {
				t.Fatal(err)
			}
			if _, kind, err := store.ClaimNext(context.Background(), time.Now(), 3); err != nil || kind != WorkProvision {
				t.Fatalf("provision claim = %q, %v", kind, err)
			}
			if err := store.MarkJITCreated(context.Background(), record.Key, 700, "runner-700"); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(time.Minute)
			if err := store.RecordCompletion(context.Background(), Record{
				Key: record.Key, Owner: record.Owner, Repository: record.Repository, NextAttemptAt: deadline,
			}); err != nil {
				t.Fatal(err)
			}

			test.interrupt(t, store, record.Key)
			persisted, _, err := store.Get(context.Background(), record.Key)
			if err != nil || persisted.Status != StatusCompleted || !persisted.DeferDeletion {
				t.Fatalf("deferred cancellation = %#v, %v", persisted, err)
			}
			expired, err := store.ListExpired(context.Background(), deadline.Add(time.Second))
			if err != nil || len(expired) != 1 || expired[0].Key != record.Key {
				t.Fatalf("expired cancellations = %#v, %v", expired, err)
			}
			queued := Record{Key: "org/repo:queued", DeliveryID: "delivery-queued", Owner: "org", Repository: "repo"}
			if _, err := store.Create(context.Background(), queued); err != nil {
				t.Fatal(err)
			}
			if claimed, _, err := store.ClaimNext(context.Background(), time.Now(), 3); err != nil || claimed != nil {
				t.Fatalf("deferred cancellation did not hold admission: %#v, %v", claimed, err)
			}
		})
	}
}

func TestRecoveryKeepsDeferredCancellationReclaimable(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "state.json")
	store, err := OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	record := Record{Key: "org/repo:restart-cancelled", DeliveryID: "delivery-restart-cancelled", Owner: "org", Repository: "repo"}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if _, kind, err := store.ClaimNext(context.Background(), time.Now(), 3); err != nil || kind != WorkProvision {
		t.Fatalf("provision claim = %q, %v", kind, err)
	}
	if err := store.RecordCompletion(context.Background(), Record{
		Key: record.Key, Owner: record.Owner, Repository: record.Repository, NextAttemptAt: time.Now().Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	persisted, _, err := reopened.Get(context.Background(), record.Key)
	if err != nil || persisted.Status != StatusCompleted || !persisted.DeferDeletion || persisted.ClaimedWork != "" {
		t.Fatalf("recovered cancellation = %#v, %v", persisted, err)
	}
	expired, err := reopened.ListExpired(context.Background(), time.Now().Add(time.Hour))
	if err != nil || len(expired) != 1 || expired[0].Key != record.Key {
		t.Fatalf("expired cancellations after recovery = %#v, %v", expired, err)
	}
}

func TestLiveRunnerLimitIsAtomicAtClaim(t *testing.T) {
	store := openTestStore(t)
	if err := store.SetMaxLiveRunners(1); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"org/repo:limit-1", "org/repo:limit-2"} {
		if _, err := store.Create(context.Background(), Record{Key: key, DeliveryID: key, Owner: "org", Repository: "repo"}); err != nil {
			t.Fatal(err)
		}
	}
	first, kind, err := store.ClaimNext(context.Background(), time.Now(), 3)
	if err != nil || first == nil || kind != WorkProvision {
		t.Fatalf("first claim = %#v, %q, %v", first, kind, err)
	}
	second, _, err := store.ClaimNext(context.Background(), time.Now(), 3)
	if err != nil || second != nil {
		t.Fatalf("claim above live limit = %#v, %v", second, err)
	}
	if err := store.MarkProvisioned(context.Background(), first.Key, "instance", 1, "runner"); err != nil {
		t.Fatal(err)
	}
	second, _, err = store.ClaimNext(context.Background(), time.Now(), 3)
	if err != nil || second != nil {
		t.Fatalf("claim above provisioned limit = %#v, %v", second, err)
	}
	if err := store.MarkDeleted(context.Background(), first.Key); err != nil {
		t.Fatal(err)
	}
	second, kind, err = store.ClaimNext(context.Background(), time.Now(), 3)
	if err != nil || second == nil || kind != WorkProvision {
		t.Fatalf("claim after capacity release = %#v, %q, %v", second, kind, err)
	}
}

func TestOrphanedInstanceHoldsAdmissionUntilProviderSweep(t *testing.T) {
	store := openTestStore(t)
	if err := store.SetMaxLiveRunners(1); err != nil {
		t.Fatal(err)
	}
	first := Record{Key: "org/repo:orphan", DeliveryID: "delivery-orphan", Owner: "org", Repository: "repo", Provider: "test"}
	second := Record{Key: "org/repo:replacement", DeliveryID: "delivery-replacement", Owner: "org", Repository: "repo", Provider: "test"}
	for _, record := range []Record{first, second} {
		if _, err := store.Create(context.Background(), record); err != nil {
			t.Fatal(err)
		}
	}
	claimed, _, err := store.ClaimNext(context.Background(), time.Now(), 1)
	if err != nil || claimed == nil || claimed.Key != first.Key {
		t.Fatalf("initial claim = %#v, %v", claimed, err)
	}
	if err := store.MarkProvisioned(context.Background(), first.Key, "instance", 1, "runner"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkCompleted(context.Background(), first.Key); err != nil {
		t.Fatal(err)
	}
	if _, kind, err := store.ClaimNext(context.Background(), time.Now(), 1); err != nil || kind != WorkDelete {
		t.Fatalf("delete claim = %q, %v", kind, err)
	}
	if err := store.MarkDeleteFailed(context.Background(), first.Key, "permanent failure", time.Now(), 1); err != nil {
		t.Fatal(err)
	}
	if err := store.ScheduleDeletion(context.Background(), first.Key); err != nil {
		t.Fatal(err)
	}
	orphan, _, _ := store.Get(context.Background(), first.Key)
	if orphan.Status != StatusOrphaned {
		t.Fatalf("orphan was re-enabled: %#v", orphan)
	}
	expired, err := store.ListExpired(context.Background(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range expired {
		if record.Key == first.Key {
			t.Fatalf("orphan remained in reaper loop: %#v", record)
		}
	}
	claimed, kind, err := store.ClaimNext(context.Background(), time.Now(), 1)
	if err != nil || claimed != nil {
		t.Fatalf("orphaned live instance released admission: %#v, %q, %v", claimed, kind, err)
	}
	if released, err := store.ReleaseSweptOrphans(context.Background(), "test", time.Now().Add(time.Hour)); err != nil || released != 1 {
		t.Fatalf("release swept orphan = %d, %v", released, err)
	}
	claimed, kind, err = store.ClaimNext(context.Background(), time.Now(), 1)
	if err != nil || claimed == nil || claimed.Key != second.Key || kind != WorkProvision {
		t.Fatalf("admission after confirmed sweep = %#v, %q, %v", claimed, kind, err)
	}
}

func TestDeletedStateRejectsLateWorkerWrites(t *testing.T) {
	store := openTestStore(t)
	record := Record{Key: "org/repo:terminal", DeliveryID: "delivery-terminal"}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ClaimNext(context.Background(), time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDeleted(context.Background(), record.Key); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProvisioned(context.Background(), record.Key, "late-instance", 99, "late-runner"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProvisionFailed(context.Background(), record.Key, "late failure", time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDeleteFailed(context.Background(), record.Key, "late delete failure", time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	if err := store.ScheduleDeletion(context.Background(), record.Key); err != nil {
		t.Fatal(err)
	}
	terminal, _, err := store.Get(context.Background(), record.Key)
	if err != nil || terminal.Status != StatusDeleted || terminal.InstanceID != "" || terminal.GitHubRunnerID != 0 {
		t.Fatalf("terminal record changed = %#v, %v", terminal, err)
	}
}

func TestCompletionBeforeJITIdentityReconcilesByRunnerID(t *testing.T) {
	store := openTestStore(t)
	completion := Record{
		Key: "org/repo:81", JobID: 81, Owner: "org", Repository: "repo",
		Provider: "aws", GitHubRunnerID: 999,
	}
	if err := store.RecordCompletion(context.Background(), completion); err != nil {
		t.Fatal(err)
	}
	runner := Record{Key: "org/repo:82", DeliveryID: "delivery-82", JobID: 82, Owner: "org", Repository: "repo", Provider: "aws"}
	if _, err := store.Create(context.Background(), runner); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ClaimNext(context.Background(), time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkJITCreated(context.Background(), runner.Key, 999, "runner-999"); err != nil {
		t.Fatal(err)
	}
	reconciled, _, err := store.Get(context.Background(), runner.Key)
	if err != nil || reconciled.Status != StatusCompleted || reconciled.DeferDeletion || reconciled.ClaimedWork != WorkProvision {
		t.Fatalf("reconciled runner = %#v, %v", reconciled, err)
	}
	marker, found, err := store.Get(context.Background(), completionMarkerKey("org", "repo", 999))
	if err != nil || !found || marker.Status != StatusDeleted {
		t.Fatalf("completion marker = %#v, %v, %v", marker, found, err)
	}
}

func TestCompletionPersistenceFailurePreservesRunnerIndex(t *testing.T) {
	store := openTestStore(t)
	record := Record{Key: "org/repo:index", Owner: "org", Repository: "repo", Provider: "aws"}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ClaimNext(context.Background(), time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkJITCreated(context.Background(), record.Key, 77, "runner-77"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkOrphaned(context.Background(), record.Key, "operator cleanup"); err != nil {
		t.Fatal(err)
	}
	identity := runnerIdentityKey("org", "repo", 77)
	if store.runnerKeys[identity] != record.Key {
		t.Fatalf("runner index before failure = %#v", store.runnerKeys)
	}
	if err := os.Remove(store.journalPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(store.journalPath, 0o700); err != nil {
		t.Fatal(err)
	}
	completion := Record{Key: "org/repo:completion", Owner: "org", Repository: "repo", Provider: "aws", GitHubRunnerID: 77}
	if err := store.RecordCompletion(context.Background(), completion); err == nil {
		t.Fatal("RecordCompletion succeeded with an unwritable journal path")
	}
	if store.runnerKeys[identity] != record.Key {
		t.Fatalf("runner index was not rolled back: %#v", store.runnerKeys)
	}
	if _, found, err := store.Get(context.Background(), completion.Key); err != nil || found {
		t.Fatalf("completion record survived failed persistence: found=%v err=%v", found, err)
	}
}

func TestRunnerIdentityIsScopedByRepository(t *testing.T) {
	store := openTestStore(t)
	for _, record := range []Record{
		{Key: "org-a/repo:1", DeliveryID: "delivery-a", Owner: "org-a", Repository: "repo", Provider: "aws"},
		{Key: "org-b/repo:2", DeliveryID: "delivery-b", Owner: "org-b", Repository: "repo", Provider: "aws"},
	} {
		if _, err := store.Create(context.Background(), record); err != nil {
			t.Fatal(err)
		}
		claimed, _, err := store.ClaimNext(context.Background(), time.Now(), 3)
		if err != nil || claimed == nil || claimed.Key != record.Key {
			t.Fatalf("claim = %#v, %v", claimed, err)
		}
		if err := store.MarkJITCreated(context.Background(), record.Key, 5, "runner"); err != nil {
			t.Fatal(err)
		}
		if err := store.MarkProvisioned(context.Background(), record.Key, "instance", 5, "runner"); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.RecordCompletion(context.Background(), Record{
		Key: "org-b/repo:2", Owner: "org-b", Repository: "repo", Provider: "aws", GitHubRunnerID: 5,
	}); err != nil {
		t.Fatal(err)
	}
	first, _, _ := store.Get(context.Background(), "org-a/repo:1")
	second, _, _ := store.Get(context.Background(), "org-b/repo:2")
	if first.Status != StatusProvisioned || second.Status != StatusCompleted {
		t.Fatalf("cross-repository completion: first=%#v second=%#v", first, second)
	}
	if err := store.MarkDeleted(context.Background(), "org-b/repo:2"); err != nil {
		t.Fatal(err)
	}

	if err := store.RecordCompletion(context.Background(), Record{
		Key: "org-b/repo:6", Owner: "org-b", Repository: "repo", Provider: "aws", GitHubRunnerID: 6,
	}); err != nil {
		t.Fatal(err)
	}
	foreign := Record{Key: "org-a/repo:6", DeliveryID: "delivery-a-6", Owner: "org-a", Repository: "repo", Provider: "aws"}
	if _, err := store.Create(context.Background(), foreign); err != nil {
		t.Fatal(err)
	}
	claimed, _, err := store.ClaimNext(context.Background(), time.Now(), 3)
	if err != nil || claimed == nil || claimed.Key != foreign.Key {
		t.Fatalf("foreign claim = %#v, %v", claimed, err)
	}
	if err := store.MarkJITCreated(context.Background(), foreign.Key, 6, "runner-6"); err != nil {
		t.Fatal(err)
	}
	persisted, _, _ := store.Get(context.Background(), foreign.Key)
	if persisted.Status != StatusProvisioning || !persisted.GitHubRunnerOwned {
		t.Fatalf("foreign marker consumed by repository: %#v", persisted)
	}
}

func TestExpiredFailedRecordWithoutInstanceIDIsReconciled(t *testing.T) {
	store := openTestStore(t)
	record := Record{Key: "org/repo:10", DeliveryID: "delivery-10", Provider: "azure"}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if err := store.update(record.Key, func(current *Record) {
		current.Status = StatusFailed
		current.CreatedAt = time.Now().Add(-2 * time.Hour)
	}); err != nil {
		t.Fatal(err)
	}
	expired, err := store.ListExpired(context.Background(), time.Now().Add(-time.Hour))
	if err != nil || len(expired) != 1 || expired[0].InstanceID != "" {
		t.Fatalf("expired records = %#v, %v", expired, err)
	}
}

func TestProvisionedTTLStartsWhenProvisioningFinishes(t *testing.T) {
	store := openTestStore(t)
	record := Record{Key: "org/repo:slow", DeliveryID: "delivery-slow", Provider: "aws"}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if err := store.update(record.Key, func(current *Record) {
		current.CreatedAt = time.Now().Add(-2 * time.Hour)
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ClaimNext(context.Background(), time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProvisioned(context.Background(), record.Key, "i-slow", 12, "runner-slow"); err != nil {
		t.Fatal(err)
	}

	expired, err := store.ListExpired(context.Background(), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 0 {
		t.Fatalf("newly provisioned runner expired from job creation time: %#v", expired)
	}
}

func TestTTLDoesNotResetExhaustedDeletionAttempts(t *testing.T) {
	store := openTestStore(t)
	record := Record{Key: "org/repo:11", DeliveryID: "delivery-11", Provider: "aws"}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	const maxAttempts = 5
	if _, _, err := store.ClaimNext(context.Background(), time.Now(), maxAttempts); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProvisioned(context.Background(), record.Key, "i-11", 11, "runner-11"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkCompleted(context.Background(), record.Key); err != nil {
		t.Fatal(err)
	}
	if _, kind, err := store.ClaimNext(context.Background(), time.Now(), maxAttempts); err != nil || kind != WorkDelete {
		t.Fatalf("initial delete claim kind=%q err=%v", kind, err)
	}
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := store.MarkDeleteFailed(context.Background(), record.Key, "transient", time.Now(), maxAttempts); err != nil {
			t.Fatal(err)
		}
		if err := store.ScheduleDeletion(context.Background(), record.Key); err != nil {
			t.Fatal(err)
		}
		if attempt < maxAttempts {
			claimed, kind, err := store.ClaimNext(context.Background(), time.Now(), maxAttempts)
			if err != nil || claimed == nil || kind != WorkDelete || claimed.DeleteAttempts != attempt+1 {
				t.Fatalf("delete attempt %d claim=%#v kind=%q err=%v", attempt+1, claimed, kind, err)
			}
		}
	}
	claimed, kind, err := store.ClaimNext(context.Background(), time.Now(), maxAttempts)
	if err != nil || claimed != nil || kind != "" {
		t.Fatalf("terminal delete claim=%#v kind=%q err=%v", claimed, kind, err)
	}
	terminal, _, _ := store.Get(context.Background(), record.Key)
	if terminal.Status != StatusOrphaned {
		t.Fatalf("exhausted deletion status = %q", terminal.Status)
	}
}

func TestProvisionFailureAfterCompletionRemainsCleanupEligible(t *testing.T) {
	store := openTestStore(t)
	record := Record{Key: "org/repo:12", DeliveryID: "delivery-12", Provider: "gcp"}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ClaimNext(context.Background(), time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkCompleted(context.Background(), record.Key); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProvisionFailed(context.Background(), record.Key, "ambiguous provider failure", time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	expired, err := store.ListExpired(context.Background(), time.Now().Add(-time.Hour))
	if err != nil || len(expired) != 1 || expired[0].Status != StatusCompleted || expired[0].LastError == "" {
		t.Fatalf("cleanup candidates=%#v err=%v", expired, err)
	}
}

func TestReconciliationDeferralBudgetsBecomeTerminal(t *testing.T) {
	for _, test := range []struct {
		name          string
		countFailure  bool
		countThrottle bool
	}{
		{name: "persistent client failures", countFailure: true},
		{name: "throttle failures", countThrottle: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := openTestStore(t)
			record := Record{Key: "org/repo:reconcile-budget", DeliveryID: "delivery-reconcile-budget"}
			if _, err := store.Create(context.Background(), record); err != nil {
				t.Fatal(err)
			}
			for attempt := 0; attempt < 2; attempt++ {
				if err := store.DeferReconciliation(
					context.Background(), record.Key, "reconciliation failed", time.Now(), 2,
					test.countFailure, test.countThrottle,
				); err != nil {
					t.Fatal(err)
				}
			}
			persisted, _, err := store.Get(context.Background(), record.Key)
			if err != nil || persisted.Status != StatusOrphaned || !persisted.NextAttemptAt.IsZero() {
				t.Fatalf("terminal reconciliation = %#v, %v", persisted, err)
			}
		})
	}
}

func TestRateLimitedCleanupBudgetBecomesTerminal(t *testing.T) {
	store := openTestStore(t)
	record := Record{Key: "org/repo:cleanup-throttle-budget", DeliveryID: "delivery-cleanup-throttle-budget"}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkCompleted(context.Background(), record.Key); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if _, ready, err := store.BeginCleanupAttempt(context.Background(), record.Key, 2); err != nil || !ready {
			t.Fatalf("cleanup attempt %d = ready %v, error %v", attempt+1, ready, err)
		}
		if err := store.DeferRateLimitedCleanup(
			context.Background(), record.Key, "throttled", time.Now().Add(-time.Second), 2, true,
		); err != nil {
			t.Fatal(err)
		}
	}
	persisted, _, err := store.Get(context.Background(), record.Key)
	if err != nil || persisted.Status != StatusOrphaned || persisted.DeleteAttempts != 0 || !persisted.NextAttemptAt.IsZero() {
		t.Fatalf("terminal cleanup throttle = %#v, %v", persisted, err)
	}
}

func TestOrphanCleanupDeferralPreservesTerminalState(t *testing.T) {
	store := openTestStore(t)
	record := Record{Key: "org/repo:orphan-cleanup", DeliveryID: "delivery-orphan-cleanup"}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkOrphaned(context.Background(), record.Key, "operator cleanup"); err != nil {
		t.Fatal(err)
	}
	retryAt := time.Now().Add(time.Minute).UTC()
	if err := store.DeferOrphanCleanup(context.Background(), record.Key, "GitHub unavailable", retryAt); err != nil {
		t.Fatal(err)
	}
	persisted, _, err := store.Get(context.Background(), record.Key)
	if err != nil || persisted.Status != StatusOrphaned || persisted.ReconcileFailures != 1 || !persisted.NextAttemptAt.Equal(retryAt) {
		t.Fatalf("deferred orphan cleanup = %#v, %v", persisted, err)
	}
}

func TestDeletedRecordsArePrunedAfterRetention(t *testing.T) {
	store := openTestStore(t, WithDeletedRetention(24*time.Hour))
	record := Record{Key: "org/repo:13", DeliveryID: "delivery-13", Provider: "azure"}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDeleted(context.Background(), record.Key); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	aged := store.records[record.Key]
	aged.UpdatedAt = time.Now().Add(-25 * time.Hour)
	store.records[record.Key] = aged
	store.mu.Unlock()
	removed, err := store.PruneDeleted(context.Background())
	if err != nil || removed != 1 {
		t.Fatalf("PruneDeleted() = %d, %v", removed, err)
	}
	if _, found, err := store.Get(context.Background(), record.Key); err != nil || found {
		t.Fatalf("pruned record found=%v err=%v", found, err)
	}
}

func TestCleanupExhaustionRefreshesOrphanRetentionTimestamp(t *testing.T) {
	store := openTestStore(t)
	record := Record{Key: "org/repo:cleanup-exhausted", DeliveryID: "delivery-cleanup-exhausted", Provider: "aws"}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	store.mu.Lock()
	persisted := store.records[record.Key]
	persisted.DeleteAttempts = 1
	persisted.UpdatedAt = old
	store.records[record.Key] = persisted
	store.markDirty(record.Key)
	err := store.saveLocked()
	store.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, ready, err := store.BeginCleanupAttempt(context.Background(), record.Key, 1); err != nil || ready {
		t.Fatalf("exhausted cleanup = ready %v, error %v", ready, err)
	}
	persisted, _, err = store.Get(context.Background(), record.Key)
	if err != nil || persisted.Status != StatusOrphaned || !persisted.UpdatedAt.After(old.Add(time.Hour)) {
		t.Fatalf("orphan retention timestamp = %#v, %v", persisted, err)
	}
}

func openTestStore(t *testing.T, options ...Option) *FileStore {
	t.Helper()
	store, err := OpenFileStore(filepath.Join(t.TempDir(), "state.json"), options...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestRecoveryPersistsRetentionPruning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := OpenFileStore(path, WithDeletedRetention(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	record := Record{Key: "org/repo:recovery-prune", DeliveryID: "delivery-recovery-prune"}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDeleted(context.Background(), record.Key); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	aged := store.records[record.Key]
	aged.UpdatedAt = time.Now().Add(-25 * time.Hour)
	store.records[record.Key] = aged
	store.markDirty(record.Key)
	err = store.saveLocked()
	store.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenFileStore(path, WithDeletedRetention(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := reopened.Get(context.Background(), record.Key); err != nil || found {
		t.Fatalf("record survived recovery prune: found=%v err=%v", found, err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	final, err := OpenFileStore(path, WithDeletedRetention(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = final.Close() })
	if _, found, err := final.Get(context.Background(), record.Key); err != nil || found {
		t.Fatalf("recovery prune was not durable: found=%v err=%v", found, err)
	}
}

func TestClosedStoreRejectsWrites(t *testing.T) {
	store, err := OpenFileStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = store.Create(context.Background(), Record{Key: "org/repo:closed", DeliveryID: "delivery-closed"})
	if err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("write after Close = %v", err)
	}
}

func TestJournalCompactionPreservesState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	record := Record{Key: "org/repo:journal", DeliveryID: "delivery-journal", Provider: "gcp"}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ClaimNext(context.Background(), time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	err = store.compactLocked()
	store.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	journalInfo, err := os.Stat(path + ".wal")
	if err != nil || journalInfo.Size() != 0 || journalInfo.Mode().Perm() != 0o600 {
		t.Fatalf("compacted journal = %#v, %v", journalInfo, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	persisted, found, err := reopened.Get(context.Background(), record.Key)
	if err != nil || !found || persisted.Status != StatusPending || persisted.Attempts != 0 || persisted.ClaimedWork != "" {
		t.Fatalf("compacted recovery = %#v, %v, %v", persisted, found, err)
	}
}

func TestOpenRepairsTornFinalJournalEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	record := Record{Key: "org/repo:torn", DeliveryID: "delivery-torn", Provider: "azure"}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	journal, err := os.OpenFile(path+".wal", os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.WriteString(`{"records":{"org/repo:partial":{"key":"org/repo`); err != nil {
		_ = journal.Close()
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	persisted, found, err := reopened.Get(context.Background(), record.Key)
	if err != nil || !found || persisted.Status != StatusPending {
		t.Fatalf("pre-tear state = %#v, %v, %v", persisted, found, err)
	}
	data, err := os.ReadFile(path + ".wal")
	if err != nil || (len(data) != 0 && data[len(data)-1] != '\n') {
		t.Fatalf("repaired journal tail = %q, %v", data, err)
	}
}

func TestOpenNormalizesCompleteJournalEntryWithoutNewline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	first := Record{Key: "org/repo:first", DeliveryID: "delivery-first", Provider: "aws"}
	if _, err := store.Create(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	journalPath := path + ".wal"
	data, err := os.ReadFile(journalPath)
	if err != nil || len(data) == 0 || data[len(data)-1] != '\n' {
		t.Fatalf("journal before truncation = %q, %v", data, err)
	}
	if err := os.Truncate(journalPath, int64(len(data)-1)); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	second := Record{Key: "org/repo:second", DeliveryID: "delivery-second", Provider: "aws"}
	if _, err := reopened.Create(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	final, err := OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{first.Key, second.Key} {
		if _, found, err := final.Get(context.Background(), key); err != nil || !found {
			t.Fatalf("record %s after delimiter repair: found=%v err=%v", key, found, err)
		}
	}
}
