package webhook

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/thomasvincent/github-runners-infra/internal/state"
)

type absenceCompute struct {
	*fakeCompute
	absent bool
	err    error
	checks int
}

func (c *absenceCompute) RunnerAbsent(_ context.Context, _ string) (bool, error) {
	c.checks++
	return c.absent, c.err
}

func TestOrphanCapacityReleasedOnlyAfterExactProviderAbsence(t *testing.T) {
	for _, tc := range []struct {
		name     string
		absent   bool
		err      error
		provider string
	}{
		{name: "confirmed absent", absent: true, provider: "test"},
		{name: "existing foreign droplet", provider: "test"},
		{name: "API failure", err: errors.New("provider unavailable"), provider: "test"},
		{name: "different provider", absent: true, provider: "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler, store, _, client := newTestHandler(t)
			checker := &absenceCompute{fakeCompute: client, absent: tc.absent, err: tc.err}
			handler.computeClient = checker
			ctx := context.Background()
			if err := store.SetMaxLiveRunners(1); err != nil {
				t.Fatal(err)
			}
			record := state.Record{Key: "trusted/private-repo:1", DeliveryID: "delivery-1", Provider: tc.provider}
			if _, err := store.Create(ctx, record); err != nil {
				t.Fatal(err)
			}
			if _, _, err := store.ClaimNext(ctx, time.Now(), 5); err != nil {
				t.Fatal(err)
			}
			if err := store.MarkProvisioned(ctx, record.Key, "123", 101, "runner"); err != nil {
				t.Fatal(err)
			}
			if err := store.MarkOrphaned(ctx, record.Key, "ownership mismatch"); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Create(ctx, state.Record{Key: "trusted/private-repo:2", DeliveryID: "delivery-2", Provider: "test"}); err != nil {
				t.Fatal(err)
			}
			if next, _, err := store.ClaimNext(ctx, time.Now(), 5); next != nil || err != nil {
				t.Fatalf("orphan must reserve capacity: %#v %v", next, err)
			}
			errs := handler.reconcileOrphanedInstances(ctx)
			if (len(errs) != 0) != (tc.err != nil) {
				t.Fatalf("errors: %v", errs)
			}
			released := tc.absent && tc.err == nil && tc.provider == "test"
			persisted, _, err := store.Get(ctx, record.Key)
			if err != nil || (persisted.InstanceID == "") != released || persisted.Status != state.StatusOrphaned || persisted.GitHubRunnerID != 101 || persisted.LastError != "ownership mismatch" {
				t.Fatalf("orphan audit identity: %#v %v", persisted, err)
			}
			if len(client.deleted) != 0 {
				t.Fatal("absence reconciliation mutated provider resources")
			}
			if tc.provider == "other" && checker.checks != 0 {
				t.Fatal("queried wrong provider")
			}
			next, kind, err := store.ClaimNext(ctx, time.Now(), 5)
			if err != nil || (next != nil) != released {
				t.Fatalf("admission: %#v %q %v", next, kind, err)
			}
		})
	}
}
