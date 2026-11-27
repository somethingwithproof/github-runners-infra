// SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
// SPDX-License-Identifier: MIT

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
	if err := store.DeferDeletionConfirmation(ctx, key, "pending", time.Now().Add(time.Hour), DeferPolicy{MaxAttempts: 5}); err != nil {
		t.Fatal(err)
	}
	current, _, err := store.Get(ctx, key)
	if err != nil || current.Status != StatusDeleting || current.ClaimedWork != WorkDelete || current.DeleteAttempts != 1 {
		t.Fatalf("unaccepted deferral: %#v %v", current, err)
	}
}

func TestUnassignedCancellationDuringActiveDeleteDoesNotDeadlock(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	key := "org/repo:42"
	if _, err := store.Create(ctx, Record{Key: key, DeliveryID: "delivery", JobID: 42, Owner: "org", Repository: "repo", Provider: "digitalocean"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ClaimNext(ctx, time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkJITCreated(ctx, key, 101, "runner"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProvisioned(ctx, key, "123", 101, "runner"); err != nil {
		t.Fatal(err)
	}
	// Cross-job completion triggers WorkDelete with RecheckDemand=true.
	if err := store.RecordCompletion(ctx, Record{
		Key: "org/repo:43", JobID: 43, Owner: "org", Repository: "repo", GitHubRunnerID: 101,
	}); err != nil {
		t.Fatal(err)
	}
	claimed, kind, err := store.ClaimNext(ctx, time.Now(), 3)
	if err != nil || claimed == nil || kind != WorkDelete || !claimed.RecheckDemand {
		t.Fatalf("delete claim: %#v %q %v", claimed, kind, err)
	}
	// Unassigned cancellation for original job arrives while WorkDelete is active.
	if err := store.RecordCompletion(ctx, Record{
		Key: key, JobID: 42, Owner: "org", Repository: "repo",
		NextAttemptAt: time.Now().UTC().Add(5 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	current, _, err := store.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if !current.JobCompleted || current.RecheckDemand || current.Status != StatusDeleting || current.ClaimedWork != WorkDelete || current.DeferDeletion {
		t.Fatalf("active delete corrupted by unassigned cancellation: %#v", current)
	}
	if err := store.MarkDeletionAccepted(ctx, key, "123"); err != nil {
		t.Fatalf("MarkDeletionAccepted failed after concurrent cancellation: %v", err)
	}
	restored, err := store.FinishDeletedRunner(ctx, key, true)
	if err != nil || restored {
		t.Fatalf("FinishDeletedRunner = %v, %v, want false, nil", restored, err)
	}
	final, _, err := store.Get(ctx, key)
	if err != nil || final.Status != StatusDeleted || final.ClaimedWork != "" {
		t.Fatalf("final state = %#v, %v", final, err)
	}
}

func TestDeletionConfirmationEscalatesWhenBudgetExhausted(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	key := "org/repo:confirm-budget"
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
	if err := store.MarkDeletionAccepted(ctx, key, "123"); err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 3; attempt++ {
		if attempt > 1 {
			if _, kind, err := store.ClaimNext(ctx, time.Now().Add(time.Hour), 3); err != nil || kind != WorkDelete {
				t.Fatalf("reclaim %d = %q, %v", attempt, kind, err)
			}
		}
		if err := store.DeferDeletionConfirmation(ctx, key, "still terminating", time.Now().Add(time.Minute), DeferPolicy{MaxAttempts: 3, MaxAge: time.Hour}); err != nil {
			t.Fatal(err)
		}
	}
	current, _, err := store.Get(ctx, key)
	if err != nil || current.Status != StatusOrphaned || current.ConfirmAttempts != 3 || current.InstanceID != "123" || !current.NextAttemptAt.IsZero() {
		t.Fatalf("exhausted confirmation state = %#v, %v", current, err)
	}
}
