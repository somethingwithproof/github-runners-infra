package state

import (
	"context"
	"testing"
	"time"
)

func TestReleaseAbsentOrphanRequiresMatchingInstanceAndTerminalState(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	record := Record{Key: "org/repo:1", DeliveryID: "delivery-1", Provider: "digitalocean"}
	if _, err := store.Create(ctx, record); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ClaimNext(ctx, time.Now(), 5); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProvisioned(ctx, record.Key, "123", 101, "runner"); err != nil {
		t.Fatal(err)
	}
	if err := store.ReleaseAbsentOrphan(ctx, record.Key, "123"); err != nil {
		t.Fatal(err)
	}
	current, _, _ := store.Get(ctx, record.Key)
	if current.InstanceID != "123" {
		t.Fatal("released active runner")
	}
	if err := store.MarkOrphaned(ctx, record.Key, "ownership mismatch"); err != nil {
		t.Fatal(err)
	}
	if err := store.ReleaseAbsentOrphan(ctx, record.Key, "456"); err != nil {
		t.Fatal(err)
	}
	current, _, _ = store.Get(ctx, record.Key)
	if current.InstanceID != "123" {
		t.Fatal("released different instance")
	}
	if err := store.ReleaseAbsentOrphan(ctx, record.Key, "123"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenFileStore(store.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	})
	current, _, err = reopened.Get(ctx, record.Key)
	if err != nil || current.InstanceID != "" || current.Status != StatusOrphaned || current.GitHubRunnerID != 101 {
		t.Fatalf("durable release: %#v %v", current, err)
	}
}
