package state

import (
	"context"
	"testing"
	"time"
)

func TestDeletionAcceptanceIsBoundToClaimAndExactInstance(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	key := "org/repo:1"
	if _, err := store.Create(ctx, Record{Key: key, DeliveryID: "delivery", Provider: "digitalocean"}); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDeletionAccepted(ctx, key, "123"); err == nil {
		t.Fatal("accepted an unclaimed resource")
	}
	if _, _, err := store.ClaimNext(ctx, time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProvisioned(ctx, key, "123", 101, "runner"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkCompleted(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, kind, err := store.ClaimNext(ctx, time.Now(), 3); err != nil || kind != WorkDelete {
		t.Fatalf("claim: %q %v", kind, err)
	}
	if err := store.MarkDeletionAccepted(ctx, key, "456"); err == nil {
		t.Fatal("accepted a different instance")
	}
	if err := store.MarkDeletionAccepted(ctx, key, "123"); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after the acceptance journal entry, before deferral.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenFileStore(store.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	current, _, err := reopened.Get(ctx, key)
	if err != nil || current.DeletionAcceptedID != "123" || current.Status != StatusCompleted || current.DeleteAttempts != 0 {
		t.Fatalf("restart: %#v %v", current, err)
	}
	if err := reopened.MarkProvisioned(ctx, key, "456", 102, "replacement"); err != nil {
		t.Fatal(err)
	}
	current, _, err = reopened.Get(ctx, key)
	if err != nil || current.DeletionAcceptedID != "" {
		t.Fatalf("replacement inherited deletion acceptance: %#v %v", current, err)
	}
}

func TestConfirmationDeferralCannotRefundUnacceptedDeletion(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	key := "org/repo:1"
	if _, err := store.Create(ctx, Record{Key: key, DeliveryID: "delivery", Provider: "digitalocean"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ClaimNext(ctx, time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProvisioned(ctx, key, "123", 101, "runner"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkCompleted(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ClaimNext(ctx, time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	if err := store.DeferDeletionConfirmation(ctx, key, "pending", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	current, _, err := store.Get(ctx, key)
	if err != nil || current.Status != StatusDeleting || current.ClaimedWork != WorkDelete || current.DeleteAttempts != 1 {
		t.Fatalf("unaccepted deferral: %#v %v", current, err)
	}
}
