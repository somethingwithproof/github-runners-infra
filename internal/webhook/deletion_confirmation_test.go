package webhook

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/thomasvincent/github-runners-infra/internal/compute"
	"github.com/thomasvincent/github-runners-infra/internal/state"
)

type deletionConfirmationCompute struct {
	*absenceCompute
	excludedID string
}

func (c *deletionConfirmationCompute) CleanupRunnerExcept(ctx context.Context, jobKey, excludedID string) error {
	c.excludedID = excludedID
	return c.CleanupRunner(ctx, jobKey)
}

func TestAcceptedDeletionWaitsForAbsenceAcrossRestart(t *testing.T) {
	ctx := context.Background()
	handler, _, githubClient, client := newTestHandler(t)
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := state.OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	handler.store = store
	handler.maxAttempts = 1
	checker := &deletionConfirmationCompute{absenceCompute: &absenceCompute{fakeCompute: client}}
	handler.computeClient = checker
	if err := store.SetMaxLiveRunners(1); err != nil {
		t.Fatal(err)
	}
	key := "trusted/private-repo:1"
	if _, err := store.Create(ctx, state.Record{Key: key, DeliveryID: "delivery-1", Provider: "test", Owner: "trusted", Repository: "private-repo"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ClaimNext(ctx, time.Now(), 1); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProvisioned(ctx, key, "123", 101, "runner"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkCompleted(ctx, key); err != nil {
		t.Fatal(err)
	}
	claim := func() state.Record {
		t.Helper()
		record, kind, err := store.ClaimNext(ctx, time.Now().Add(time.Hour), 1)
		if err != nil || record == nil || kind != state.WorkDelete {
			t.Fatalf("deletion claim: %#v %q %v", record, kind, err)
		}
		return *record
	}
	handler.delete(ctx, claim())
	current, _, err := store.Get(ctx, key)
	if err != nil || current.DeletionAcceptedID != "123" || current.Status != state.StatusCompleted || current.DeleteAttempts != 0 {
		t.Fatalf("acceptance: %#v %v", current, err)
	}
	if len(client.deleted) != 1 || client.cleanupCalls != 0 || len(githubClient.removed) != 0 {
		t.Fatal("cleanup must wait for provider absence")
	}

	// Restart while the provider still exposes the deleting droplet without tags.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = state.OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	handler.store = store
	client.deleteErr = compute.ErrOwnershipMismatch
	if err := store.SetMaxLiveRunners(1); err != nil {
		t.Fatal(err)
	}
	// A failed confirmation is also read-only and must not release capacity.
	for _, confirmErr := range []error{nil, errors.New("provider unavailable"), context.Canceled, nil} {
		checker.err = confirmErr
		handler.delete(ctx, claim())
		current, _, err = store.Get(ctx, key)
		if err != nil || current.Status != state.StatusCompleted || current.InstanceID != "123" || current.DeletionAcceptedID != "123" || current.DeleteAttempts != 0 {
			t.Fatalf("confirmation: %#v %v", current, err)
		}
	}
	if len(client.deleted) != 1 || client.cleanupCalls != 0 || len(githubClient.removed) != 0 {
		t.Fatal("confirmation retried provider mutation or duplicate cleanup")
	}
	if _, err := store.Create(ctx, state.Record{Key: "trusted/private-repo:2", DeliveryID: "delivery-2", Provider: "test"}); err != nil {
		t.Fatal(err)
	}
	if next, _, err := store.ClaimNext(ctx, time.Now(), 1); err != nil || next != nil {
		t.Fatalf("deleting droplet must still reserve fleet capacity: %#v %v", next, err)
	}

	checker.absent = true
	handler.delete(ctx, claim())
	current, _, err = store.Get(ctx, key)
	if err != nil || current.Status != state.StatusDeleted || current.LastError != "" {
		t.Fatalf("confirmed deletion: %#v %v", current, err)
	}
	if len(client.deleted) != 1 || client.cleanupCalls != 1 || len(githubClient.removed) != 1 {
		t.Fatalf("deletes=%v cleanup=%d deregistration=%v", client.deleted, client.cleanupCalls, githubClient.removed)
	}
	if checker.excludedID != "123" {
		t.Fatalf("duplicate cleanup must exclude confirmed absent ID: %q", checker.excludedID)
	}
	if next, kind, err := store.ClaimNext(ctx, time.Now(), 1); err != nil || next == nil || kind != state.WorkProvision {
		t.Fatalf("confirmed absence must release capacity: %#v %q %v", next, kind, err)
	}
}

func TestUnacceptedDeletionStillRejectsForeignOwnership(t *testing.T) {
	handler, store, _, client := newTestHandler(t)
	handler.computeClient = &absenceCompute{fakeCompute: client}
	client.deleteErr = compute.ErrOwnershipMismatch
	ctx := context.Background()
	key := "trusted/private-repo:1"
	if _, err := store.Create(ctx, state.Record{Key: key, DeliveryID: "delivery-1", Provider: "test"}); err != nil {
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
	record, _, err := store.ClaimNext(ctx, time.Now(), 3)
	if err != nil || record == nil {
		t.Fatalf("claim: %#v %v", record, err)
	}
	handler.delete(ctx, *record)
	current, _, err := store.Get(ctx, key)
	if err != nil || current.Status != state.StatusOrphaned || current.DeletionAcceptedID != "" {
		t.Fatalf("foreign ownership: %#v %v", current, err)
	}
	if client.cleanupCalls != 0 {
		t.Fatal("foreign resource reached duplicate cleanup")
	}
}
