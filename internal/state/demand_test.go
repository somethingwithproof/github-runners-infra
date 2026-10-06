package state

import (
	"context"
	"testing"
	"time"
)

func TestDemandDeferralPreservesConcurrentCompletion(t *testing.T) {
	for _, completed := range []bool{false, true} {
		name := "retry queued demand"
		if completed {
			name = "completion wins"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := openTestStore(t)
			key := "org/repo:42"
			if _, err := store.Create(ctx, Record{Key: key}); err != nil {
				t.Fatal(err)
			}
			if _, kind, err := store.ClaimNext(ctx, time.Now(), 3); err != nil || kind != WorkProvision {
				t.Fatalf("claim = %v, %v", kind, err)
			}
			if err := store.MarkJITCreated(ctx, key, 101, "runner"); err != nil {
				t.Fatal(err)
			}
			if completed {
				if err := store.MarkCompleted(ctx, key); err != nil {
					t.Fatal(err)
				}
			}
			retryAt := time.Now().Add(time.Hour)
			if err := store.DeferProvisioningDemand(ctx, key, "unconfirmed demand", retryAt); err != nil {
				t.Fatal(err)
			}
			record, _, err := store.Get(ctx, key)
			if err != nil || record.ClaimedWork != "" || record.Attempts != 0 || record.GitHubRunnerID != 101 {
				t.Fatalf("claim or registration lost: %#v, %v", record, err)
			}
			if completed {
				if record.Status != StatusCompleted || !record.NextAttemptAt.IsZero() {
					t.Fatalf("completion resurrected: %#v", record)
				}
			} else if record.Status != StatusPending || !record.NextAttemptAt.Equal(retryAt) || !record.DeferDeletion {
				t.Fatalf("retry not safely deferred: %#v", record)
			}
		})
	}
}
