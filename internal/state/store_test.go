package state

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
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
	store, err := OpenFileStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
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

func TestMissingRunnerRotatesProvisionEpoch(t *testing.T) {
	store, err := OpenFileStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
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
	store, err := OpenFileStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
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
	store, err := OpenFileStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
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

func TestReleaseClaimDoesNotBurnRetryBudget(t *testing.T) {
	store, err := OpenFileStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
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

func TestLiveRunnerLimitIsAtomicAtClaim(t *testing.T) {
	store, err := OpenFileStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
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

func TestOrphanedDeletionIsTerminalAndReleasesAdmission(t *testing.T) {
	store, err := OpenFileStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetMaxLiveRunners(1); err != nil {
		t.Fatal(err)
	}
	first := Record{Key: "org/repo:orphan", DeliveryID: "delivery-orphan", Owner: "org", Repository: "repo"}
	second := Record{Key: "org/repo:replacement", DeliveryID: "delivery-replacement", Owner: "org", Repository: "repo"}
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
	if err != nil || claimed == nil || claimed.Key != second.Key || kind != WorkProvision {
		t.Fatalf("admission after orphan = %#v, %q, %v", claimed, kind, err)
	}
}

func TestDeletedStateRejectsLateWorkerWrites(t *testing.T) {
	store, err := OpenFileStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
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
	store, err := OpenFileStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
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

func TestRunnerIdentityIsScopedByRepository(t *testing.T) {
	store, err := OpenFileStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
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
	store, err := OpenFileStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
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
	store, err := OpenFileStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
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
	store, err := OpenFileStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
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
	store, err := OpenFileStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
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

func TestDeletedRecordsArePrunedAfterRetention(t *testing.T) {
	store, err := OpenFileStore(filepath.Join(t.TempDir(), "state.json"), WithDeletedRetention(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
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
