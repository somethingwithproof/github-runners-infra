// SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
// SPDX-License-Identifier: MIT

package webhook

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/thomasvincent/github-runners-infra/internal/budget"
	"github.com/thomasvincent/github-runners-infra/internal/compute"
	gh "github.com/thomasvincent/github-runners-infra/internal/github"
	"github.com/thomasvincent/github-runners-infra/internal/state"
)

type fallbackGitHub struct {
	*fakeGitHub
	regToken string
}

func (f *fallbackGitHub) GenerateRepoRegistrationToken(_ context.Context, _, _ string) (gh.RegistrationToken, error) {
	return gh.RegistrationToken{Token: f.regToken, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func TestBudgetCircuitBreakerBlocksProvisionButAllowsCleanup(t *testing.T) {
	store := openTestStateStore(t)
	ghClient := &fakeGitHub{}
	comp := &captureCompute{fakeCompute: &fakeCompute{}}
	tracker, err := budget.NewTracker(budget.Config{
		DailyCapUSD:         0.01,
		ReservationDuration: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Pre-consume the budget so new provisioning is blocked.
	if err := tracker.Reserve(budget.Reservation{
		Key: "trusted/private-repo:existing", Repository: "trusted/private-repo", HourlyRateUSD: 0.01,
	}); err != nil {
		t.Fatal(err)
	}

	handler, err := NewHandler(Config{
		WebhookSecret:       []byte(testWebhookSecret),
		GitHubClient:        ghClient,
		ComputeClient:       comp,
		BudgetTracker:       tracker,
		DefaultInstanceSize: "m7i.xlarge",
		Store:               store,
		RequiredLabel:       "self-hosted",
		AllowedLabels:       []string{"self-hosted"},
		AllowedRepositories: []string{"trusted/private-repo"},
		RunnerVersion:       "2.336.0",
		RunnerSHA256:        strings.Repeat("a", 64),
		ChefInstallerSHA256: strings.Repeat("b", 64),
		InstallationID:      42,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	key := "trusted/private-repo:700"
	if _, err := store.Create(ctx, state.Record{
		Key: key, DeliveryID: "del-budget-1", JobID: 700,
		Owner: "trusted", Repository: "private-repo", Labels: []string{"self-hosted"}, Provider: "test",
	}); err != nil {
		t.Fatal(err)
	}
	record, kind, err := store.ClaimNext(ctx, time.Now(), 3)
	if err != nil || record == nil || kind != state.WorkProvision {
		t.Fatalf("ClaimNext = %#v %q %v", record, kind, err)
	}
	handler.provision(ctx, *record)
	if comp.created != 0 {
		t.Fatalf("expected budget cap to block CreateRunner, got %d calls", comp.created)
	}
	if snap := tracker.Snapshot(); snap.BudgetBlockedTotal != 1 {
		t.Fatalf("BudgetBlockedTotal = %d, want 1", snap.BudgetBlockedTotal)
	}

	// Cleanup of an existing runner must still succeed even while over cap.
	existingKey := "trusted/private-repo:existing"
	if _, err := store.Create(ctx, state.Record{
		Key: existingKey, DeliveryID: "del-budget-2", JobID: 701,
		Owner: "trusted", Repository: "private-repo", Labels: []string{"self-hosted"}, Provider: "test",
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ClaimNext(ctx, time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProvisioned(ctx, existingKey, "inst-running", 201, "runner"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkCompleted(ctx, existingKey); err != nil {
		t.Fatal(err)
	}
	delRecord, delKind, err := store.ClaimNext(ctx, time.Now(), 3)
	if err != nil || delRecord == nil || delKind != state.WorkDelete {
		t.Fatalf("ClaimNext delete = %#v %q %v", delRecord, delKind, err)
	}
	handler.delete(ctx, *delRecord)
	if len(comp.deleted) != 1 || comp.deleted[0] != "inst-running" {
		t.Fatalf("cleanup blocked by budget: deleted=%v", comp.deleted)
	}
}

func TestHandlerPrebakedEgressAndRegistrationTokenFallback(t *testing.T) {
	store := openTestStateStore(t)
	ghClient := &fallbackGitHub{fakeGitHub: &fakeGitHub{}, regToken: "FALLBACK_TOK"}
	comp := &captureCompute{fakeCompute: &fakeCompute{}}

	handler, err := NewHandler(Config{
		WebhookSecret:       []byte(testWebhookSecret),
		GitHubClient:        ghClient,
		ComputeClient:       comp,
		Store:               store,
		RequiredLabel:       "self-hosted",
		AllowedLabels:       []string{"self-hosted", "chef"},
		AllowedRepositories: []string{"trusted/private-repo"},
		RunnerVersion:       "2.336.0",
		RunnerSHA256:        strings.Repeat("a", 64),
		ChefInstallerSHA256: strings.Repeat("b", 64),
		DisableJITConfig:    true,
		PrebakedImage:       true,
		EgressAllowlist:     []string{"api.github.com", "140.82.112.0/20"},
		InstallationID:      42,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	key := "trusted/private-repo:702"
	if _, err := store.Create(ctx, state.Record{
		Key: key, DeliveryID: "del-fb-1", JobID: 702,
		Owner: "trusted", Repository: "private-repo", Labels: []string{"self-hosted", "chef"}, Provider: "test",
	}); err != nil {
		t.Fatal(err)
	}
	record, _, err := store.ClaimNext(ctx, time.Now(), 3)
	if err != nil || record == nil {
		t.Fatalf("ClaimNext = %#v %v", record, err)
	}
	handler.provision(ctx, *record)
	if !comp.lastParams.PrebakedImage || len(comp.lastParams.EgressAllowlist) != 2 {
		t.Fatalf("unexpected prebaked/egress params: %#v", comp.lastParams)
	}
	if comp.lastParams.RunnerRegistrationToken != "FALLBACK_TOK" || comp.lastParams.RunnerRepoURL != "https://github.com/trusted/private-repo" || comp.lastParams.RunnerJITConfig != "" {
		t.Fatalf("unexpected fallback auth params: %#v", comp.lastParams)
	}

	provisioned, _, err := store.Get(ctx, key)
	if err != nil || provisioned.GitHubRunnerID != 0 || provisioned.GitHubRunnerOwned {
		t.Fatalf("unknown registration identity: %#v, %v", provisioned, err)
	}
	if deleted, err := handler.reconcileProviderFoundRunner(ctx, provisioned); err != nil || deleted || ghClient.statusChecks != 0 {
		t.Fatalf("fallback runner liveness: deleted=%v checks=%d err=%v", deleted, ghClient.statusChecks, err)
	}
	tmpl, err := template.ParseFiles("../../cloud-init/runner.yaml.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := compute.RenderCloudInit(tmpl, comp.lastParams)
	if err != nil || !strings.Contains(rendered, "--labels 'self-hosted,chef'") {
		t.Fatalf("missing custom labels, err=%v", err)
	}
	// Verify RenderCloudInit renders prebaked + egress + fallback cleanly.
	if _, err := compute.RenderCloudInit(nil, comp.lastParams); err == nil {
		t.Fatal("expected nil template error")
	}
}

func TestHandlerLabelBasedSpotAndOnDemandPrecedenceWithSizeAndBudget(t *testing.T) {
	cases := []struct {
		name     string
		labels   []string
		wantSpot *bool
		wantSize string
	}{
		{name: "force spot with size", labels: []string{"self-hosted", "large", "spot"}, wantSpot: compute.BoolPtr(true), wantSize: "m7i.2xlarge"},
		{name: "force on-demand with size", labels: []string{"self-hosted", "large", "on-demand"}, wantSpot: compute.BoolPtr(false), wantSize: "m7i.2xlarge"},
		{name: "both labels on-demand wins", labels: []string{"self-hosted", "spot", "on-demand", "large"}, wantSpot: compute.BoolPtr(false), wantSize: "m7i.2xlarge"},
		{name: "neither label uses default", labels: []string{"self-hosted", "large"}, wantSpot: nil, wantSize: "m7i.2xlarge"},
	}
	for idx, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := openTestStateStore(t)
			comp := &captureCompute{fakeCompute: &fakeCompute{}}
			tracker, err := budget.NewTracker(budget.Config{DailyCapUSD: 100, ReservationDuration: time.Hour})
			if err != nil {
				t.Fatal(err)
			}
			handler, err := NewHandler(Config{
				WebhookSecret:       []byte(testWebhookSecret),
				GitHubClient:        &fakeGitHub{},
				ComputeClient:       comp,
				BudgetTracker:       tracker,
				DefaultInstanceSize: "m7i.xlarge",
				DefaultSpot:         true,
				Store:               store,
				RequiredLabel:       "self-hosted",
				AllowedLabels:       []string{"self-hosted", "large", "spot", "on-demand"},
				LabelSizeMap:        map[string]string{"large": "m7i.2xlarge"},
				AllowedRepositories: []string{"trusted/private-repo"},
				RunnerVersion:       "2.336.0",
				RunnerSHA256:        strings.Repeat("a", 64),
				ChefInstallerSHA256: strings.Repeat("b", 64),
				InstallationID:      42,
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			jobID := int64(800 + idx)
			key := jobKey("trusted/private-repo", jobID)
			if _, err := store.Create(ctx, state.Record{
				Key: key, DeliveryID: "del-spot", JobID: jobID,
				Owner: "trusted", Repository: "private-repo", Labels: tc.labels, Provider: "test",
			}); err != nil {
				t.Fatal(err)
			}
			record, _, err := store.ClaimNext(ctx, time.Now(), 3)
			if err != nil || record == nil {
				t.Fatalf("ClaimNext = %#v %v", record, err)
			}
			handler.provision(ctx, *record)
			if comp.lastParams.InstanceSize != tc.wantSize {
				t.Fatalf("InstanceSize = %q, want %q", comp.lastParams.InstanceSize, tc.wantSize)
			}
			if !reflect.DeepEqual(comp.lastParams.Spot, tc.wantSpot) {
				t.Fatalf("Spot = %v, want %v", comp.lastParams.Spot, tc.wantSpot)
			}
		})
	}
}
