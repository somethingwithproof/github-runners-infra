// SPDX-FileCopyrightText: 2026 Thomas Vincent <thomasvincent@gmail.com>
// SPDX-License-Identifier: MIT

package webhook

import (
	"context"
	"errors"
	"testing"
	"time"

	gh "github.com/thomasvincent/github-runners-infra/internal/github"
	"github.com/thomasvincent/github-runners-infra/internal/state"
)

func TestOrganizationGitHubRepositoryIdentity(t *testing.T) {
	allowed, err := normalizeAllowedRepositories([]string{"trusted/.github"})
	if err != nil {
		t.Fatal(err)
	}
	handler, _, _, _ := newTestHandler(t)
	handler.allowedRepositories = allowed
	event := testEvent("queued", nil)
	event.Repo.Name = ".github"
	event.Repo.FullName = "trusted/.github"
	if name, targeted, err := handler.validateRepository(event.Repo); err != nil || !targeted || name != "trusted/.github" {
		t.Fatalf(".github identity rejected: %q %v %v", name, targeted, err)
	}
	for _, name := range []string{".", "..", ".github/../other"} {
		if _, err := normalizeAllowedRepositories([]string{"trusted/" + name}); err == nil {
			t.Fatalf("accepted invalid name %q", name)
		}
	}
}

func TestPublicWorkflowAuthorizationBeforePersistence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		trusted bool
		err     error
		runID   int64
		want    int
	}{
		{"trusted", true, nil, 7, 202},
		{"untrusted event", false, nil, 7, 403},
		{"missing run", true, nil, 0, 403},
		{"verification unavailable", false, errors.New("network unavailable"), 7, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler, store, github, _ := newTestHandler(t)
			handler.allowedPublicRepositories = map[string]struct{}{"trusted/private-repo": {}}
			github.workflowTrusted, github.workflowErr = tc.trusted, tc.err
			event := testEvent("queued", []string{"self-hosted", "chef"})
			event.Repo.Private = false
			event.WorkflowJob.RunID = tc.runID
			response := serveEvent(handler, event, "delivery-public-test")
			if response.Code != tc.want {
				t.Fatalf("status %d: %s", response.Code, response.Body.String())
			}
			_, exists, err := store.Get(context.Background(), "trusted/private-repo:42")
			if exists != (tc.want == 202) || err != nil {
				t.Fatalf("persisted = %v, err = %v", exists, err)
			}
		})
	}
}

func TestPublicControllerStillVerifiesOriginAfterVisibilityChange(t *testing.T) {
	handler, store, _, _ := newTestHandler(t)
	handler.allowedPublicRepositories = map[string]struct{}{"trusted/private-repo": {}}
	event := testEvent("queued", []string{"self-hosted", "chef"})
	event.WorkflowJob.RunID = 7
	response := serveEvent(handler, event, "delivery-private-origin")
	if response.Code != 403 {
		t.Fatalf("untrusted origin allowed after visibility change: %d", response.Code)
	}
	if _, exists, err := store.Get(context.Background(), "trusted/private-repo:42"); err != nil || exists {
		t.Fatalf("unverified origin persisted: %v %v", exists, err)
	}
}

func TestPublicRepoUntrustedReassignedCompletionHaltsReplenishment(t *testing.T) {
	for _, tc := range []struct {
		name           string
		reassignedRun  int64
		trusted        bool
		wantReplenish  bool
		wantStatus     state.Status
	}{
		{name: "untrusted reassigned run halts replenishment", reassignedRun: 999, trusted: false, wantReplenish: false, wantStatus: state.StatusDeleted},
		{name: "missing reassigned run ID halts replenishment", reassignedRun: 0, trusted: true, wantReplenish: false, wantStatus: state.StatusDeleted},
		{name: "trusted reassigned run replenishes queued demand", reassignedRun: 888, trusted: true, wantReplenish: true, wantStatus: state.StatusPending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler, store, githubClient, cloud := newTestHandler(t)
			handler.allowedPublicRepositories = map[string]struct{}{"trusted/private-repo": {}}
			ctx := context.Background()

			record := claimedDemand(t, store)
			if err := store.MarkJITCreated(ctx, record.Key, 101, "runner"); err != nil {
				t.Fatal(err)
			}
			if err := store.MarkProvisioned(ctx, record.Key, "500", 101, "runner"); err != nil {
				t.Fatal(err)
			}
			// Deliver reassigned completion via webhook so ReassignedRunID flows end-to-end.
			completedEvent := testEvent("completed", []string{"self-hosted", "chef"})
			completedEvent.Repo.Private = false
			completedEvent.WorkflowJob.ID = 43
			completedEvent.WorkflowJob.RunID = tc.reassignedRun
			completedEvent.WorkflowJob.RunnerID = 101
			completedEvent.WorkflowJob.RunnerName = "eph-trusted-private-repo-0123456789ab"
			resp := serveEvent(handler, completedEvent, "delivery-reassigned-public")
			if resp.Code != 202 {
				t.Fatalf("completed webhook status = %d: %s", resp.Code, resp.Body.String())
			}

			githubClient.workflowTrusted = tc.trusted
			githubClient.jobStates = map[int64]gh.JobStatus{42: gh.JobQueued}

			claimed, kind, err := store.ClaimNext(ctx, time.Now(), 3)
			if err != nil || claimed == nil || kind != state.WorkDelete {
				t.Fatalf("claim delete = %#v %q %v", claimed, kind, err)
			}
			handler.process(ctx, *claimed, kind)

			current, _, err := store.Get(ctx, record.Key)
			if err != nil {
				t.Fatal(err)
			}
			if current.Status != tc.wantStatus || current.RecheckDemand {
				t.Fatalf("post-cleanup state = %#v, want status %s with RecheckDemand=false", current, tc.wantStatus)
			}
			if len(cloud.deleted) != 1 || len(githubClient.removed) != 1 {
				t.Fatalf("owned runner cleanup incomplete: deleted=%v removed=%v", cloud.deleted, githubClient.removed)
			}
			if tc.wantReplenish {
				if current.ProvisionEpoch != 1 {
					t.Fatalf("expected replenished ProvisionEpoch=1, got %d", current.ProvisionEpoch)
				}
			} else if next, _, err := store.ClaimNext(ctx, time.Now(), 3); err != nil || next != nil {
				t.Fatalf("untrusted public reassignment still scheduled work: %#v, %v", next, err)
			}
		})
	}
}
