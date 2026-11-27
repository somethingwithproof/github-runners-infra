// SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
// SPDX-License-Identifier: MIT

package state

import (
	"context"
	"testing"
	"time"
)

func TestStaleReaperGuardCannotMutateReplenishedRunner(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	key := "org/repo:42"
	if _, err := store.Create(ctx, Record{Key: key, DeliveryID: "d1", JobID: 42, Owner: "org", Repository: "repo", Provider: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ClaimNext(ctx, time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkJITCreated(ctx, key, 101, "runner-old"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProvisioned(ctx, key, "inst-old", 101, "runner-old"); err != nil {
		t.Fatal(err)
	}
	staleSnapshot, _, err := store.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	staleGuard := staleSnapshot.Guard()

	// Cross-job completion cleans up old runner and replenishes into epoch 1.
	if err := store.RecordCompletion(ctx, Record{Key: "org/repo:43", JobID: 43, Owner: "org", Repository: "repo", GitHubRunnerID: 101}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ClaimNext(ctx, time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	if restored, err := store.FinishDeletedRunner(ctx, key, true); err != nil || !restored {
		t.Fatalf("FinishDeletedRunner = %v, %v", restored, err)
	}
	if _, _, err := store.ClaimNext(ctx, time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkJITCreated(ctx, key, 202, "runner-new"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProvisioned(ctx, key, "inst-new", 202, "runner-new"); err != nil {
		t.Fatal(err)
	}

	// Every stale-snapshot reaper operation must be a safe no-op on epoch 1.
	if err := store.ClearJIT(ctx, key, staleGuard); err != nil {
		t.Fatal(err)
	}
	if err := store.ScheduleDeletion(ctx, key, staleGuard); err != nil {
		t.Fatal(err)
	}
	if requeued, err := store.RequeueMissingRunner(ctx, key, staleGuard, 3); err != nil || requeued {
		t.Fatalf("stale RequeueMissingRunner = %v, %v", requeued, err)
	}
	if confirmed, err := store.ObserveRunnerMissing(ctx, key, staleGuard, time.Now(), 0, 1); err != nil || confirmed {
		t.Fatalf("stale ObserveRunnerMissing = %v, %v", confirmed, err)
	}
	if confirmed, err := store.ObserveGitHubRunnerMissing(ctx, key, staleGuard, time.Now(), 0, 1); err != nil || confirmed {
		t.Fatalf("stale ObserveGitHubRunnerMissing = %v, %v", confirmed, err)
	}
	if err := store.RecordRunnerSeen(ctx, key, staleGuard); err != nil {
		t.Fatal(err)
	}
	if err := store.DeferReconciliation(ctx, key, staleGuard, "stale failure", time.Now().Add(time.Hour), DeferPolicy{MaxAttempts: 1, CountFailure: true}); err != nil {
		t.Fatal(err)
	}

	current, _, err := store.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != StatusProvisioned || current.ProvisionEpoch != 1 || current.InstanceID != "inst-new" ||
		current.GitHubRunnerID != 202 || !current.GitHubRunnerOwned || !current.RegisteredAt.IsZero() ||
		current.MissingChecks != 0 || current.GitHubMissingChecks != 0 || current.ReconcileFailures != 0 || current.LastError != "" {
		t.Fatalf("stale reaper guard mutated replacement runner: %#v", current)
	}
}

func TestReleaseSweptOrphansProtectsOwnershipMismatchAndTOCTOU(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	oldTime := time.Now().UTC().Add(-2 * time.Hour)
	cutoff := time.Now().UTC().Add(-time.Hour)

	setupOrphan := func(key, instanceID, lastErr string) {
		t.Helper()
		if _, err := store.Create(ctx, Record{Key: key, DeliveryID: key, Provider: "test"}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.ClaimNext(ctx, time.Now(), 3); err != nil {
			t.Fatal(err)
		}
		if err := store.MarkProvisioned(ctx, key, instanceID, 1, "runner"); err != nil {
			t.Fatal(err)
		}
		if err := store.update(key, func(r *Record) { r.ProvisionedAt = oldTime; r.CreatedAt = oldTime }); err != nil {
			t.Fatal(err)
		}
		if err := store.MarkOrphaned(ctx, key, lastErr); err != nil {
			t.Fatal(err)
		}
	}

	setupOrphan("org/repo:ownership", "inst-ownership", "provider resource ownership mismatch: tags stripped")
	setupOrphan("org/repo:known", "inst-known", "deletion retry budget exhausted")
	setupOrphan("org/repo:toctou", "inst-toctou", "deletion retry budget exhausted")
	setupOrphan("org/repo:eligible", "inst-eligible", "deletion retry budget exhausted")

	known := map[string]struct{}{"inst-known": {}}
	eligible := map[string]string{
		"org/repo:ownership": "inst-ownership",
		"org/repo:known":     "inst-known",
		"org/repo:eligible":  "inst-eligible",
	}

	released, err := store.ReleaseSweptOrphans(ctx, "test", cutoff, known, eligible)
	if err != nil || released != 1 {
		t.Fatalf("ReleaseSweptOrphans = %d, %v, want 1", released, err)
	}
	for _, tc := range []struct {
		key        string
		wantInstID string
	}{
		{"org/repo:ownership", "inst-ownership"},
		{"org/repo:known", "inst-known"},
		{"org/repo:toctou", "inst-toctou"},
		{"org/repo:eligible", ""},
	} {
		rec, _, err := store.Get(ctx, tc.key)
		if err != nil || rec.InstanceID != tc.wantInstID {
			t.Fatalf("%s InstanceID = %q, want %q (err=%v)", tc.key, rec.InstanceID, tc.wantInstID, err)
		}
	}
}

func TestRunnerKeysEvictedOnLifecycleTransitions(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	// 1. MarkProvisioned replacing runner ID evicts old identity under s.mu.
	k1 := "org/repo:1"
	if _, err := store.Create(ctx, Record{Key: k1, Owner: "org", Repository: "repo", Provider: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ClaimNext(ctx, time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkJITCreated(ctx, k1, 10, "r10"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProvisioned(ctx, k1, "inst-1", 11, "r11"); err != nil {
		t.Fatal(err)
	}
	if owned, _ := store.OwnsRunner(ctx, "org", "repo", 10); owned {
		t.Fatal("stale runner 10 still indexed after MarkProvisioned replacement")
	}
	if owned, _ := store.OwnsRunner(ctx, "org", "repo", 11); !owned {
		t.Fatal("new runner 11 not indexed by MarkProvisioned")
	}

	// 2. ClearJIT evicts runnerKeys entry.
	rec1, _, _ := store.Get(ctx, k1)
	if err := store.ClearJIT(ctx, k1, rec1.Guard()); err != nil {
		t.Fatal(err)
	}
	if _, exists := store.runnerKeys[runnerIdentityKey("org", "repo", 11)]; exists {
		t.Fatal("ClearJIT left stale entry in runnerKeys")
	}

	// 3. RequeueMissingRunner evicts runnerKeys entry.
	if err := store.MarkProvisioned(ctx, k1, "inst-1b", 12, "r12"); err != nil {
		t.Fatal(err)
	}
	rec1, _, _ = store.Get(ctx, k1)
	if requeued, err := store.RequeueMissingRunner(ctx, k1, rec1.Guard(), 3); err != nil || !requeued {
		t.Fatalf("RequeueMissingRunner = %v, %v", requeued, err)
	}
	if _, exists := store.runnerKeys[runnerIdentityKey("org", "repo", 12)]; exists {
		t.Fatal("RequeueMissingRunner left stale entry in runnerKeys")
	}

	// 4. FinishDeletedRunner evicts runnerKeys entry.
	if _, _, err := store.ClaimNext(ctx, time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProvisioned(ctx, k1, "inst-1c", 13, "r13"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FinishDeletedRunner(ctx, k1, false); err != nil {
		t.Fatal(err)
	}
	if _, exists := store.runnerKeys[runnerIdentityKey("org", "repo", 13)]; exists {
		t.Fatal("FinishDeletedRunner left stale entry in runnerKeys")
	}

	// 5. MarkDeleted evicts runnerKeys entry.
	k2 := "org/repo:2"
	if _, err := store.Create(ctx, Record{Key: k2, Owner: "org", Repository: "repo", Provider: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkJITCreated(ctx, k2, 20, "r20"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDeleted(ctx, k2); err != nil {
		t.Fatal(err)
	}
	if _, exists := store.runnerKeys[runnerIdentityKey("org", "repo", 20)]; exists {
		t.Fatal("MarkDeleted left stale entry in runnerKeys")
	}
}

func TestMarkProvisionedResetsStaleDeferDeletion(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	key := "org/repo:stale-defer"
	if _, err := store.Create(ctx, Record{Key: key, Provider: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ClaimNext(ctx, time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	if err := store.update(key, func(r *Record) { r.DeferDeletion = true }); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProvisioned(ctx, key, "inst-1", 50, "runner-50"); err != nil {
		t.Fatal(err)
	}
	rec, _, err := store.Get(ctx, key)
	if err != nil || rec.Status != StatusProvisioned || rec.DeferDeletion {
		t.Fatalf("MarkProvisioned preserved stale DeferDeletion: %#v, %v", rec, err)
	}
}
