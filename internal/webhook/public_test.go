package webhook

import (
	"context"
	"errors"
	"testing"
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
