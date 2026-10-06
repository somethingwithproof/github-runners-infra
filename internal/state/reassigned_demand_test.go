package state

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestReassignedDemandPersistsAndOriginalCompletionRemainsAuthoritative(t *testing.T) {
	for _, completed := range []bool{false, true} {
		name := "original still queued"
		if completed {
			name = "original completed elsewhere"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "state.json")
			store, err := OpenFileStore(path)
			if err != nil {
				t.Fatal(err)
			}
			key := "org/repo:42"
			if _, err := store.Create(ctx, Record{Key: key, JobID: 42, Owner: "org", Repository: "repo", Labels: []string{"self-hosted"}}); err != nil {
				t.Fatal(err)
			}
			if _, _, err := store.ClaimNext(ctx, time.Now(), 3); err != nil {
				t.Fatal(err)
			}
			if err := store.MarkJITCreated(ctx, key, 101, "runner"); err != nil {
				t.Fatal(err)
			}
			if err := store.MarkProvisioned(ctx, key, "500", 101, "runner"); err != nil {
				t.Fatal(err)
			}
			if err := store.RecordCompletion(ctx, Record{Key: "org/repo:43", JobID: 43, Owner: "org", Repository: "repo", GitHubRunnerID: 101}); err != nil {
				t.Fatal(err)
			}
			if completed {
				if err := store.RecordCompletion(ctx, Record{Key: key, JobID: 42, Owner: "org", Repository: "repo", GitHubRunnerID: 202}); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store, err = OpenFileStore(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			record, kind, err := store.ClaimNext(ctx, time.Now(), 3)
			if err != nil || record == nil || record.Key != key || kind != WorkDelete {
				t.Fatalf("restored cleanup claim: %#v %v", record, err)
			}
			if record.JobCompleted != completed || record.RecheckDemand == completed {
				t.Fatalf("completion/demand distinction lost after restart: %#v", record)
			}
			requeued, err := store.FinishDeletedRunner(ctx, key, true)
			if err != nil || requeued == completed {
				t.Fatalf("finish demand = %v, %v", requeued, err)
			}
			current, _, err := store.Get(ctx, key)
			if err != nil {
				t.Fatal(err)
			}
			if completed {
				if current.Status != StatusDeleted {
					t.Fatal("completed original job resurrected")
				}
			} else if current.Status != StatusPending || current.InstanceID != "" || current.GitHubRunnerID != 0 || current.ProvisionEpoch != 1 || current.Attempts != 0 {
				t.Fatalf("replacement retained old resources or retry budget: %#v", current)
			}
		})
	}
}

func TestLateBoundReassignedCompletionRetainsOriginalDemand(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	key := "org/repo:42"
	if _, err := store.Create(ctx, Record{Key: key, JobID: 42, Owner: "org", Repository: "repo"}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordCompletion(ctx, Record{Key: "org/repo:43", JobID: 43, Owner: "org", Repository: "repo", GitHubRunnerID: 101}); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkJITCreated(ctx, key, 101, "runner"); err != nil {
		t.Fatal(err)
	}
	record, _, err := store.Get(ctx, key)
	if err != nil || record.Status != StatusCompleted || !record.RecheckDemand || record.JobCompleted {
		t.Fatalf("late-bound completion lost queued original: %#v %v", record, err)
	}
}
