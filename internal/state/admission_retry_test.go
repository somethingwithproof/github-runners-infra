package state

import (
	"context"
	"testing"
	"time"
)

func TestOwnedJITRetryReusesAdmissionSlot(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if err := store.SetMaxLiveRunners(1); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(ctx, Record{Key: "org/repo:1", Owner: "org", Repository: "repo", JobID: 1}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ClaimNext(ctx, time.Now(), 5); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkJITCreated(ctx, "org/repo:1", 42, "runner-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProvisionFailed(ctx, "org/repo:1", "region has no capacity", time.Time{}, 5); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(ctx, Record{Key: "org/repo:2", Owner: "org", Repository: "repo", JobID: 2}); err != nil {
		t.Fatal(err)
	}
	record, kind, err := store.ClaimNext(ctx, time.Now(), 5)
	if err != nil || record == nil || record.Key != "org/repo:1" || kind != WorkProvision {
		t.Fatalf("retry with reserved slot = %#v, %s, %v", record, kind, err)
	}
	// The claimed retry continues to count as one slot, including after its
	// stale registration is removed. A fresh job must remain pending.
	if err := store.ClearJIT(ctx, record.Key); err != nil {
		t.Fatal(err)
	}
	if next, _, err := store.ClaimNext(ctx, time.Now(), 5); err != nil || next != nil {
		t.Fatalf("fresh admission while retry occupies pool = %#v, %v", next, err)
	}
	if err := store.MarkProvisioned(ctx, record.Key, "123", 43, "runner-retry"); err != nil {
		t.Fatal(err)
	}
	if next, _, err := store.ClaimNext(ctx, time.Now(), 5); err != nil || next != nil {
		t.Fatalf("fresh admission after retry provisioned = %#v, %v", next, err)
	}
}
