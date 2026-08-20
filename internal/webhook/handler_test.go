package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thomasvincent/github-runners-infra/internal/compute"
	gh "github.com/thomasvincent/github-runners-infra/internal/github"
	"github.com/thomasvincent/github-runners-infra/internal/state"
)

const testWebhookSecret = "unit-test-webhook-secret-not-a-credential"

type fakeGitHub struct {
	mu             sync.Mutex
	generated      int
	generateErr    error
	removed        []int64
	removeErr      map[int64]error
	runnerStatus   map[int64]gh.RunnerStatus
	runnerStateErr error
	statusChecks   int
}

func (f *fakeGitHub) RepoRunnerStatus(_ context.Context, _, _ string, id int64) (gh.RunnerStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusChecks++
	if f.runnerStateErr != nil {
		return gh.RunnerMissing, f.runnerStateErr
	}
	if f.runnerStatus == nil {
		return gh.RunnerOnline, nil
	}
	return f.runnerStatus[id], nil
}

func (f *fakeGitHub) GenerateRepoJITConfig(_ context.Context, _, _, _ string, _ int64, _ []string) (gh.JITConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.generated++
	if f.generateErr != nil {
		return gh.JITConfig{}, f.generateErr
	}
	return gh.JITConfig{RunnerID: int64(100 + f.generated), EncodedConfig: "encoded-jit"}, nil
}

func (f *fakeGitHub) RemoveRepoRunner(_ context.Context, _, _ string, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, id)
	if f.removeErr != nil {
		return f.removeErr[id]
	}
	return nil
}

type fakeCompute struct {
	mu            sync.Mutex
	created       int
	deleted       []string
	exists        *compute.RunnerInstance
	createErr     error
	createStarted chan struct{}
	deleteErr     error
	cleanupErr    error
	cleanupCalls  int
	findBlock     bool
	findErr       error
	orphans       map[string]time.Time
}

type failMarkProvisionedStore struct {
	state.Store
	failures int
}

type failMarkDeletedStore struct {
	state.Store
	failures int
}

type blockingCreateStore struct {
	state.Store
	started chan struct{}
	release chan struct{}
}

func (s *blockingCreateStore) Create(ctx context.Context, record state.Record) (bool, error) {
	close(s.started)
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-s.release:
		return s.Store.Create(ctx, record)
	}
}

func (s *failMarkDeletedStore) MarkDeleted(ctx context.Context, key string) error {
	if s.failures > 0 {
		s.failures--
		return errors.New("injected deleted-state persistence failure")
	}
	return s.Store.MarkDeleted(ctx, key)
}

func (s *failMarkProvisionedStore) MarkProvisioned(ctx context.Context, key, instanceID string, runnerID int64, runnerName string) error {
	if s.failures > 0 {
		s.failures--
		return errors.New("injected state persistence failure")
	}
	return s.Store.MarkProvisioned(ctx, key, instanceID, runnerID, runnerName)
}

func (f *fakeCompute) Provider() string { return "test" }

func (f *fakeCompute) FindRunner(ctx context.Context, _ string) (*compute.RunnerInstance, bool, error) {
	f.mu.Lock()
	if f.findBlock {
		f.mu.Unlock()
		<-ctx.Done()
		return nil, false, ctx.Err()
	}
	defer f.mu.Unlock()
	if f.findErr != nil {
		return nil, false, f.findErr
	}
	if f.exists == nil {
		return nil, false, nil
	}
	copy := *f.exists
	return &copy, true, nil
}

func (f *fakeCompute) CreateRunner(ctx context.Context, _ compute.RunnerParams) (*compute.RunnerInstance, error) {
	f.mu.Lock()
	f.created++
	createStarted := f.createStarted
	createErr := f.createErr
	f.mu.Unlock()
	if createStarted != nil {
		close(createStarted)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exists = &compute.RunnerInstance{ID: "500", Name: "eph-private-repo-42"}
	if createErr != nil {
		return nil, createErr
	}
	copy := *f.exists
	return &copy, nil
}

func (f *fakeCompute) DeleteRunner(_ context.Context, id, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, id)
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.exists = nil
	return nil
}

func (f *fakeCompute) CleanupRunner(_ context.Context, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleanupCalls++
	if f.cleanupErr != nil {
		return f.cleanupErr
	}
	if f.exists != nil {
		f.deleted = append(f.deleted, f.exists.ID)
		f.exists = nil
	}
	return nil
}

func (f *fakeCompute) SweepOrphanedRunners(_ context.Context, known map[string]struct{}, cutoff time.Time) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	deleted := 0
	for id, created := range f.orphans {
		if _, ok := known[id]; !ok && created.Before(cutoff) {
			delete(f.orphans, id)
			f.deleted = append(f.deleted, id)
			deleted++
		}
	}
	return deleted, nil
}

func TestQueuedDeliveryIsDurableIdempotentAndDeletedOnCompletion(t *testing.T) {
	handler, store, githubClient, computeClient := newTestHandler(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handler.Start(ctx)
	t.Cleanup(func() {
		cancel()
		handler.Wait()
	})

	event := testEvent("queued", []string{"self-hosted", "chef"})
	response := serveEvent(handler, event, "delivery-42")
	if response.Code != http.StatusAccepted {
		t.Fatalf("queued status = %d, body=%q", response.Code, response.Body.String())
	}
	duplicate := serveEvent(handler, event, "delivery-redelivery")
	if duplicate.Code != http.StatusOK || duplicate.Body.String() != "duplicate" {
		t.Fatalf("duplicate response = %d %q", duplicate.Code, duplicate.Body.String())
	}

	waitFor(t, func() bool {
		record, found, _ := store.Get(context.Background(), "trusted/private-repo:42")
		return found && record.Status == state.StatusProvisioned && record.InstanceID == "500"
	})
	githubClient.mu.Lock()
	generated := githubClient.generated
	githubClient.mu.Unlock()
	computeClient.mu.Lock()
	created := computeClient.created
	computeClient.mu.Unlock()
	if generated != 1 || created != 1 {
		t.Fatalf("generated=%d created=%d, want one of each", generated, created)
	}

	event.Action = "completed"
	event.WorkflowJob.RunnerID = 101
	event.WorkflowJob.RunnerName = "eph-private-repo-42"
	completed := serveEvent(handler, event, "")
	if completed.Code != http.StatusAccepted {
		t.Fatalf("completed status = %d, body=%q", completed.Code, completed.Body.String())
	}
	waitFor(t, func() bool {
		record, _, _ := store.Get(context.Background(), "trusted/private-repo:42")
		return record.Status == state.StatusDeleted
	})
	computeClient.mu.Lock()
	defer computeClient.mu.Unlock()
	if len(computeClient.deleted) != 1 || computeClient.deleted[0] != "500" {
		t.Fatalf("deleted instances = %v", computeClient.deleted)
	}
	githubClient.mu.Lock()
	defer githubClient.mu.Unlock()
	if len(githubClient.removed) != 1 || githubClient.removed[0] != 101 {
		t.Fatalf("removed GitHub runners = %v", githubClient.removed)
	}
}

func TestCompletedBeforeQueuedCreatesTombstone(t *testing.T) {
	handler, store, _, computeClient := newTestHandler(t)
	event := testEvent("completed", []string{"self-hosted", "chef"})
	event.WorkflowJob.RunnerName = runnerName(event.Repo.Name, event.WorkflowJob.ID)
	if response := serveEvent(handler, event, "delivery-completed-first"); response.Code != http.StatusAccepted {
		t.Fatalf("completed status = %d", response.Code)
	}
	event.Action = "queued"
	response := serveEvent(handler, event, "delivery-queued-late")
	if response.Code != http.StatusOK || response.Body.String() != "duplicate" {
		t.Fatalf("late queued response = %d %q", response.Code, response.Body.String())
	}
	record, found, err := store.Get(context.Background(), "trusted/private-repo:42")
	if err != nil || !found || record.Status != state.StatusCompleted || !record.DeferDeletion {
		t.Fatalf("completion tombstone = %#v, %v, %v", record, found, err)
	}
	computeClient.mu.Lock()
	defer computeClient.mu.Unlock()
	if computeClient.created != 0 {
		t.Fatalf("created %d runners for completed job", computeClient.created)
	}
}

func TestIgnoresNonTargetEventsAndRejectsPrivilegeEscalationLabels(t *testing.T) {
	handler, store, _, _ := newTestHandler(t)
	public := testEvent("queued", []string{"self-hosted", "chef"})
	public.Repo.Private = false
	if response := serveEvent(handler, public, "delivery-public"); response.Code != http.StatusOK || response.Body.String() != "ignored" {
		t.Fatalf("public repository response = %d %q", response.Code, response.Body.String())
	}
	nonAllowlisted := testEvent("queued", []string{"self-hosted", "chef"})
	nonAllowlisted.Repo.Owner.Login = "other"
	nonAllowlisted.Repo.Name = "repository"
	nonAllowlisted.Repo.FullName = "other/repository"
	if response := serveEvent(handler, nonAllowlisted, "delivery-other-repository"); response.Code != http.StatusOK || response.Body.String() != "ignored" {
		t.Fatalf("non-allowlisted response = %d %q", response.Code, response.Body.String())
	}
	githubHosted := testEvent("queued", []string{"ubuntu-latest"})
	if response := serveEvent(handler, githubHosted, "delivery-github-hosted"); response.Code != http.StatusOK || response.Body.String() != "ignored" {
		t.Fatalf("GitHub-hosted response = %d %q", response.Code, response.Body.String())
	}

	unapproved := testEvent("queued", []string{"self-hosted", "production"})
	if response := serveEvent(handler, unapproved, "delivery-label"); response.Code != http.StatusForbidden {
		t.Fatalf("unapproved label status = %d", response.Code)
	}
	unapproved.Action = "completed"
	if response := serveEvent(handler, unapproved, "delivery-label-completed"); response.Code != http.StatusForbidden {
		t.Fatalf("unapproved completion = %d %q", response.Code, response.Body.String())
	}
	if _, found, err := store.Get(context.Background(), "trusted/private-repo:42"); err != nil || found {
		t.Fatalf("irrelevant completion created state: found=%v err=%v", found, err)
	}
}

func TestCompletionDeletesRunnerThatActuallyExecutedJob(t *testing.T) {
	handler, store, _, _ := newTestHandler(t)
	for _, job := range []struct {
		id       int64
		runnerID int64
	}{
		{id: 71, runnerID: 701},
		{id: 72, runnerID: 702},
	} {
		key := fmt.Sprintf("trusted/private-repo:%d", job.id)
		record := state.Record{
			Key: key, DeliveryID: fmt.Sprintf("delivery-%d", job.id), JobID: job.id,
			Owner: "trusted", Repository: "private-repo", Provider: "test",
		}
		if _, err := store.Create(context.Background(), record); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.ClaimNext(context.Background(), time.Now(), 3); err != nil {
			t.Fatal(err)
		}
		if err := store.MarkJITCreated(context.Background(), key, job.runnerID, fmt.Sprintf("runner-%d", job.runnerID)); err != nil {
			t.Fatal(err)
		}
		if err := store.MarkProvisioned(context.Background(), key, fmt.Sprintf("instance-%d", job.runnerID), job.runnerID, fmt.Sprintf("runner-%d", job.runnerID)); err != nil {
			t.Fatal(err)
		}
	}
	event := testEvent("completed", []string{"self-hosted", "chef"})
	event.WorkflowJob.ID = 71
	event.WorkflowJob.RunnerID = 702
	if response := serveEvent(handler, event, "completion-mismatch"); response.Code != http.StatusAccepted {
		t.Fatalf("completion status = %d %q", response.Code, response.Body.String())
	}
	first, _, err := store.Get(context.Background(), "trusted/private-repo:71")
	if err != nil || first.Status != state.StatusProvisioned {
		t.Fatalf("job-key runner was incorrectly completed: %#v, %v", first, err)
	}
	second, _, err := store.Get(context.Background(), "trusted/private-repo:72")
	if err != nil || second.Status != state.StatusCompleted || second.DeferDeletion {
		t.Fatalf("actual runner was not completed: %#v, %v", second, err)
	}
}

func TestCompletionWithoutJobRecordUsesDurableRunnerOwnership(t *testing.T) {
	handler, store, _, _ := newTestHandler(t)
	record := state.Record{
		Key: "trusted/private-repo:920", DeliveryID: "delivery-920", JobID: 920,
		Owner: "trusted", Repository: "private-repo", Provider: "test",
	}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ClaimNext(context.Background(), time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkJITCreated(context.Background(), record.Key, 9200, "runner-owned"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProvisioned(context.Background(), record.Key, "instance-920", 9200, "runner-owned"); err != nil {
		t.Fatal(err)
	}
	event := testEvent("completed", []string{"self-hosted", "chef"})
	event.WorkflowJob.ID = 921 // Deliberately no job-key record for this event.
	event.WorkflowJob.RunnerID = 9200
	event.WorkflowJob.RunnerName = "name-does-not-drive-ownership"
	if response := serveEvent(handler, event, "completion-owned-runner"); response.Code != http.StatusAccepted {
		t.Fatalf("completion status = %d %q", response.Code, response.Body.String())
	}
	persisted, _, err := store.Get(context.Background(), record.Key)
	if err != nil || persisted.Status != state.StatusCompleted {
		t.Fatalf("owned runner completion = %#v, %v", persisted, err)
	}
}

func TestForeignRunnerCompletionNeverDeregistersRunner(t *testing.T) {
	handler, _, githubClient, _ := newTestHandler(t)
	event := testEvent("completed", []string{"self-hosted", "chef"})
	event.WorkflowJob.ID = 73
	event.WorkflowJob.RunnerID = 4001
	event.WorkflowJob.RunnerName = "permanent-chef-runner"
	if response := serveEvent(handler, event, "foreign-runner-completion"); response.Code != http.StatusOK || response.Body.String() != "ignored" {
		t.Fatalf("completion status = %d %q", response.Code, response.Body.String())
	}
	handler.maxRunnerAge = time.Nanosecond
	time.Sleep(time.Millisecond)
	if err := handler.expireRunners(context.Background()); err != nil {
		t.Fatal(err)
	}
	githubClient.mu.Lock()
	defer githubClient.mu.Unlock()
	if len(githubClient.removed) != 0 {
		t.Fatalf("deregistered unowned GitHub runners: %v", githubClient.removed)
	}
}

func TestUnassignedCancellationUsesShortReclaimTTL(t *testing.T) {
	handler, store, _, _ := newTestHandler(t)
	record := state.Record{
		Key: "trusted/private-repo:80", DeliveryID: "delivery-80", JobID: 80,
		Owner: "trusted", Repository: "private-repo", Provider: "test",
	}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ClaimNext(context.Background(), time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkJITCreated(context.Background(), record.Key, 800, "runner-800"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProvisioned(context.Background(), record.Key, "instance-800", 800, "runner-800"); err != nil {
		t.Fatal(err)
	}
	handler.cancelledRunnerTTL = -time.Second
	event := testEvent("completed", []string{"self-hosted", "chef"})
	event.WorkflowJob.ID = 80
	if response := serveEvent(handler, event, "unassigned-completion"); response.Code != http.StatusAccepted {
		t.Fatalf("completion status = %d %q", response.Code, response.Body.String())
	}
	if err := handler.expireRunners(context.Background()); err != nil {
		t.Fatal(err)
	}
	claimed, kind, err := store.ClaimNext(context.Background(), time.Now(), 3)
	if err != nil || claimed == nil || kind != state.WorkDelete || claimed.Key != record.Key {
		t.Fatalf("short-TTL delete claim = %#v, %q, %v", claimed, kind, err)
	}
}

func TestUnassignedCancellationDoesNotDeleteBusyRunner(t *testing.T) {
	handler, store, githubClient, computeClient := newTestHandler(t)
	record := state.Record{
		Key: "trusted/private-repo:801", DeliveryID: "delivery-801", JobID: 801,
		Owner: "trusted", Repository: "private-repo", Provider: "test",
	}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ClaimNext(context.Background(), time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkJITCreated(context.Background(), record.Key, 801, "runner-801"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProvisioned(context.Background(), record.Key, "instance-801", 801, "runner-801"); err != nil {
		t.Fatal(err)
	}
	handler.cancelledRunnerTTL = -time.Second
	githubClient.removeErr = map[int64]error{801: errors.New("GitHub runner is busy")}
	event := testEvent("completed", []string{"self-hosted", "chef"})
	event.WorkflowJob.ID = 801
	if response := serveEvent(handler, event, "busy-unassigned-completion"); response.Code != http.StatusAccepted {
		t.Fatalf("completion status = %d %q", response.Code, response.Body.String())
	}
	if err := handler.expireRunners(context.Background()); err == nil {
		t.Fatal("busy runner deregistration failure was ignored")
	}
	claimed, kind, err := store.ClaimNext(context.Background(), time.Now(), 3)
	if err != nil || claimed != nil {
		t.Fatalf("busy runner was scheduled for deletion: %#v, %q, %v", claimed, kind, err)
	}
	if len(computeClient.deleted) != 0 {
		t.Fatalf("busy runner instance was deleted: %#v", computeClient.deleted)
	}
}

func TestDeferredDeletionRateLimitPersistsResetWithoutConsumingBudget(t *testing.T) {
	handler, store, githubClient, _ := newTestHandler(t)
	record := state.Record{
		Key: "trusted/private-repo:803", DeliveryID: "delivery-803", JobID: 803,
		Owner: "trusted", Repository: "private-repo", Provider: "test",
	}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ClaimNext(context.Background(), time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkJITCreated(context.Background(), record.Key, 803, "runner-803"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProvisioned(context.Background(), record.Key, "instance-803", 803, "runner-803"); err != nil {
		t.Fatal(err)
	}
	handler.cancelledRunnerTTL = -time.Second
	event := testEvent("completed", []string{"self-hosted", "chef"})
	event.WorkflowJob.ID = 803
	if response := serveEvent(handler, event, "rate-limited-cancellation"); response.Code != http.StatusAccepted {
		t.Fatalf("completion status = %d %q", response.Code, response.Body.String())
	}
	resetAt := time.Now().Add(30 * time.Minute).UTC().Truncate(time.Second)
	githubClient.removeErr = map[int64]error{803: &gh.RateLimitError{Status: http.StatusTooManyRequests, ResetAt: resetAt}}
	if err := handler.expireRunners(context.Background()); err == nil {
		t.Fatal("rate-limited deferred deletion returned no error")
	}
	persisted, _, err := store.Get(context.Background(), record.Key)
	if err != nil || persisted.DeleteAttempts != 0 || !persisted.NextAttemptAt.Equal(resetAt) {
		t.Fatalf("rate-limited reconciliation = %#v, %v", persisted, err)
	}
	if err := handler.expireRunners(context.Background()); err != nil {
		t.Fatalf("backed-off reaper returned error: %v", err)
	}
	if len(githubClient.removed) != 1 {
		t.Fatalf("rate-limited runner removal calls = %v", githubClient.removed)
	}
}

func TestDeferredDeletionBusyRunnerFailuresRemainRetryable(t *testing.T) {
	handler, store, githubClient, _ := newTestHandler(t)
	handler.maxAttempts = 2
	record := state.Record{
		Key: "trusted/private-repo:804", DeliveryID: "delivery-804", JobID: 804,
		Owner: "trusted", Repository: "private-repo", Provider: "test",
	}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ClaimNext(context.Background(), time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkJITCreated(context.Background(), record.Key, 804, "runner-804"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProvisioned(context.Background(), record.Key, "instance-804", 804, "runner-804"); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordCompletion(context.Background(), state.Record{
		Key: record.Key, JobID: record.JobID, Owner: record.Owner, Repository: record.Repository,
		Provider: record.Provider, NextAttemptAt: time.Now().Add(-time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	githubClient.removeErr = map[int64]error{804: &gh.APIStatusError{Status: http.StatusUnprocessableEntity, Action: "removing busy runner"}}
	for attempt := 0; attempt < 2; attempt++ {
		persisted, _, err := store.Get(context.Background(), record.Key)
		if err != nil {
			t.Fatal(err)
		}
		persisted.NextAttemptAt = time.Time{}
		_ = handler.expireRunner(context.Background(), persisted)
	}
	persisted, _, err := store.Get(context.Background(), record.Key)
	if err != nil || persisted.Status != state.StatusCompleted || persisted.ReconcileFailures != 0 || persisted.DeleteAttempts != 0 {
		t.Fatalf("retryable deferred deletion = %#v, %v", persisted, err)
	}
}

func TestProvisionThrottleHasFloorAndBoundedBudget(t *testing.T) {
	handler, store, githubClient, _ := newTestHandler(t)
	handler.maxAttempts = 2
	record := state.Record{
		Key: "trusted/private-repo:provision-throttle", DeliveryID: "delivery-provision-throttle", JobID: 806,
		Owner: "trusted", Repository: "private-repo", Provider: "test", Labels: []string{"self-hosted", "chef"},
	}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	githubClient.generateErr = &gh.RateLimitError{Status: http.StatusTooManyRequests, ResetAt: time.Now().Add(-time.Minute)}
	started := time.Now()
	for attempt := 0; attempt < handler.maxAttempts; attempt++ {
		claimed, kind, err := store.ClaimNext(context.Background(), time.Now().Add(time.Hour), handler.maxAttempts)
		if err != nil || claimed == nil || kind != state.WorkProvision {
			t.Fatalf("provision throttle claim %d = %#v, %q, %v", attempt, claimed, kind, err)
		}
		handler.provision(context.Background(), *claimed)
		persisted, _, err := store.Get(context.Background(), record.Key)
		if err != nil {
			t.Fatal(err)
		}
		if attempt == 0 && (persisted.Status != state.StatusPending || persisted.Attempts != 0 ||
			persisted.ThrottleFailures != 1 || persisted.NextAttemptAt.Before(started.Add(5*time.Second))) {
			t.Fatalf("first provision throttle = %#v", persisted)
		}
	}
	persisted, _, err := store.Get(context.Background(), record.Key)
	if err != nil || persisted.Status != state.StatusOrphaned || persisted.ThrottleFailures != handler.maxAttempts {
		t.Fatalf("exhausted provision throttles = %#v, %v", persisted, err)
	}
}

func TestDeleteThrottleHasFloorAndBoundedBudget(t *testing.T) {
	handler, store, githubClient, _ := newTestHandler(t)
	handler.maxAttempts = 2
	record := state.Record{
		Key: "trusted/private-repo:delete-throttle", DeliveryID: "delivery-delete-throttle", JobID: 807,
		Owner: "trusted", Repository: "private-repo", Provider: "test",
	}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ClaimNext(context.Background(), time.Now(), handler.maxAttempts); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkJITCreated(context.Background(), record.Key, 807, "runner-807"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProvisioned(context.Background(), record.Key, "instance-807", 807, "runner-807"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkCompleted(context.Background(), record.Key); err != nil {
		t.Fatal(err)
	}
	githubClient.removeErr = map[int64]error{807: &gh.RateLimitError{Status: http.StatusTooManyRequests, ResetAt: time.Time{}}}
	started := time.Now()
	for attempt := 0; attempt < handler.maxAttempts; attempt++ {
		claimed, kind, err := store.ClaimNext(context.Background(), time.Now().Add(time.Hour), handler.maxAttempts)
		if err != nil || claimed == nil || kind != state.WorkDelete {
			t.Fatalf("delete throttle claim %d = %#v, %q, %v", attempt, claimed, kind, err)
		}
		handler.delete(context.Background(), *claimed)
		persisted, _, err := store.Get(context.Background(), record.Key)
		if err != nil {
			t.Fatal(err)
		}
		if attempt == 0 && (persisted.Status != state.StatusCompleted || persisted.DeleteAttempts != 0 ||
			persisted.ThrottleFailures != 1 || persisted.NextAttemptAt.Before(started.Add(5*time.Second))) {
			t.Fatalf("first delete throttle = %#v", persisted)
		}
	}
	persisted, _, err := store.Get(context.Background(), record.Key)
	if err != nil || persisted.Status != state.StatusOrphaned || persisted.ThrottleFailures != handler.maxAttempts {
		t.Fatalf("exhausted delete throttles = %#v, %v", persisted, err)
	}
}

func TestReaperIterationHasDeadline(t *testing.T) {
	handler, store, _, computeClient := newTestHandler(t)
	record := state.Record{
		Key: "trusted/private-repo:802", DeliveryID: "delivery-802", JobID: 802,
		Owner: "trusted", Repository: "private-repo", Provider: "test",
	}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ClaimNext(context.Background(), time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProvisioned(context.Background(), record.Key, "instance-802", 802, "runner-802"); err != nil {
		t.Fatal(err)
	}
	computeClient.findBlock = true
	handler.reaperTimeout = 10 * time.Millisecond
	started := time.Now()
	err := handler.reapOnce(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("reaper deadline error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("reaper exceeded its deadline: %s", elapsed)
	}
}

func TestProvisionedRunnerRateLimitBacksOffReaper(t *testing.T) {
	handler, store, githubClient, computeClient := newTestHandler(t)
	record := state.Record{
		Key: "trusted/private-repo:805", DeliveryID: "delivery-805", JobID: 805,
		Owner: "trusted", Repository: "private-repo", Provider: "test",
	}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ClaimNext(context.Background(), time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProvisioned(context.Background(), record.Key, "instance-805", 805, "runner-805"); err != nil {
		t.Fatal(err)
	}
	computeClient.exists = &compute.RunnerInstance{ID: "instance-805", Name: "runner-805"}
	resetAt := time.Now().Add(30 * time.Minute).UTC().Truncate(time.Second)
	githubClient.runnerStateErr = &gh.RateLimitError{Status: http.StatusTooManyRequests, ResetAt: resetAt}
	if err := handler.expireRunners(context.Background()); err == nil {
		t.Fatal("rate-limited registration check returned no error")
	}
	persisted, _, err := store.Get(context.Background(), record.Key)
	if err != nil || persisted.DeleteAttempts != 0 || !persisted.NextAttemptAt.Equal(resetAt) {
		t.Fatalf("rate-limited status reconciliation = %#v, %v", persisted, err)
	}
	if err := handler.expireRunners(context.Background()); err != nil {
		t.Fatalf("backed-off status reaper returned error: %v", err)
	}
	if githubClient.statusChecks != 1 {
		t.Fatalf("rate-limited status calls = %d", githubClient.statusChecks)
	}
}

func TestTransientProviderFailuresNeverOrphanHealthyRunner(t *testing.T) {
	handler, store, _, computeClient := newTestHandler(t)
	record := state.Record{
		Key: "trusted/private-repo:806", DeliveryID: "delivery-806", JobID: 806,
		Owner: "trusted", Repository: "private-repo", Provider: "test",
	}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ClaimNext(context.Background(), time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProvisioned(context.Background(), record.Key, "instance-806", 806, "runner-806"); err != nil {
		t.Fatal(err)
	}
	computeClient.findErr = context.DeadlineExceeded
	for attempt := 0; attempt < 10; attempt++ {
		persisted, _, err := store.Get(context.Background(), record.Key)
		if err != nil {
			t.Fatal(err)
		}
		persisted.NextAttemptAt = time.Time{}
		_, _ = handler.reconcileProvisionedRunner(context.Background(), persisted)
	}
	persisted, _, err := store.Get(context.Background(), record.Key)
	if err != nil || persisted.Status != state.StatusProvisioned || persisted.ReconcileFailures != 0 || persisted.LastError == "" {
		t.Fatalf("transient provider failures = %#v, %v", persisted, err)
	}
}

func TestOrphanedGitHubRunnerCleanupRetriesUntilIdentityCleared(t *testing.T) {
	handler, store, githubClient, computeClient := newTestHandler(t)
	record := state.Record{
		Key: "trusted/private-repo:807", DeliveryID: "delivery-807", JobID: 807,
		Owner: "trusted", Repository: "private-repo", Provider: "test",
	}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ClaimNext(context.Background(), time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkJITCreated(context.Background(), record.Key, 807, "runner-807"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProvisioned(context.Background(), record.Key, "instance-807", 807, "runner-807"); err != nil {
		t.Fatal(err)
	}
	record, _, _ = store.Get(context.Background(), record.Key)
	githubClient.removeErr = map[int64]error{807: errors.New("temporary GitHub failure")}
	if err := handler.markOrphaned(context.Background(), record, compute.ErrOwnershipMismatch); err == nil {
		t.Fatal("initial orphan deregistration failure was hidden")
	}
	githubClient.removeErr = nil
	computeClient.exists = nil
	if err := handler.expireRunners(context.Background()); err != nil {
		t.Fatal(err)
	}
	persisted, _, err := store.Get(context.Background(), record.Key)
	if err != nil || persisted.Status != state.StatusOrphaned || persisted.GitHubRunnerID != 0 || persisted.GitHubRunnerOwned {
		t.Fatalf("orphan GitHub cleanup = %#v, %v", persisted, err)
	}
}

func TestMissingProvisionedInstanceIsRequeued(t *testing.T) {
	handler, store, githubClient, computeClient := newTestHandler(t)
	record := state.Record{
		Key: "trusted/private-repo:81", DeliveryID: "delivery-81", JobID: 81,
		Owner: "trusted", Repository: "private-repo", Provider: "test",
	}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ClaimNext(context.Background(), time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkJITCreated(context.Background(), record.Key, 810, "runner-810"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProvisioned(context.Background(), record.Key, "instance-810", 810, "runner-810"); err != nil {
		t.Fatal(err)
	}
	computeClient.exists = nil
	handler.livenessSettleWindow = 0
	for observation := 1; observation <= 3; observation++ {
		if err := handler.expireRunners(context.Background()); err != nil {
			t.Fatal(err)
		}
		if observation < 3 {
			persisted, _, _ := store.Get(context.Background(), record.Key)
			if persisted.Status != state.StatusProvisioned || persisted.MissingChecks != observation {
				t.Fatalf("premature liveness action after %d observations: %#v", observation, persisted)
			}
		}
	}
	persisted, _, err := store.Get(context.Background(), record.Key)
	if err != nil || persisted.Status != state.StatusPending || persisted.InstanceID != "" || persisted.GitHubRunnerID != 0 {
		t.Fatalf("missing runner reconciliation = %#v, %v", persisted, err)
	}
	githubClient.mu.Lock()
	defer githubClient.mu.Unlock()
	if len(githubClient.removed) != 1 || githubClient.removed[0] != 810 {
		t.Fatalf("removed stale GitHub runners = %v", githubClient.removed)
	}
}

func TestUnregisteredProvisionedInstanceIsRequeued(t *testing.T) {
	handler, store, githubClient, computeClient := newTestHandler(t)
	record := state.Record{
		Key: "trusted/private-repo:811", DeliveryID: "delivery-811", JobID: 811,
		Owner: "trusted", Repository: "private-repo", Provider: "test",
	}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ClaimNext(context.Background(), time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkJITCreated(context.Background(), record.Key, 811, "runner-811"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProvisioned(context.Background(), record.Key, "instance-811", 811, "runner-811"); err != nil {
		t.Fatal(err)
	}
	githubClient.runnerStatus = map[int64]gh.RunnerStatus{811: gh.RunnerOffline}
	computeClient.exists = &compute.RunnerInstance{ID: "instance-811", Name: "runner-811"}
	handler.registrationTimeout = time.Nanosecond
	time.Sleep(time.Millisecond)
	if err := handler.expireRunners(context.Background()); err != nil {
		t.Fatal(err)
	}
	persisted, _, err := store.Get(context.Background(), record.Key)
	if err != nil || persisted.Status != state.StatusPending || persisted.InstanceID != "" || persisted.GitHubRunnerID != 0 {
		t.Fatalf("unregistered runner reconciliation = %#v, %v", persisted, err)
	}
	if len(computeClient.deleted) != 1 || computeClient.deleted[0] != "instance-811" {
		t.Fatalf("deleted instances = %v", computeClient.deleted)
	}
	if len(githubClient.removed) != 1 || githubClient.removed[0] != 811 {
		t.Fatalf("removed GitHub runners = %v", githubClient.removed)
	}
}

func TestPreviouslyRegisteredRunnerIsNotRequeuedWhenOffline(t *testing.T) {
	handler, store, githubClient, computeClient := newTestHandler(t)
	record := state.Record{
		Key: "trusted/private-repo:812", DeliveryID: "delivery-812", JobID: 812,
		Owner: "trusted", Repository: "private-repo", Provider: "test",
	}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ClaimNext(context.Background(), time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkJITCreated(context.Background(), record.Key, 812, "runner-812"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProvisioned(context.Background(), record.Key, "instance-812", 812, "runner-812"); err != nil {
		t.Fatal(err)
	}
	computeClient.exists = &compute.RunnerInstance{ID: "instance-812", Name: "runner-812"}
	if err := handler.expireRunners(context.Background()); err != nil {
		t.Fatal(err)
	}
	githubClient.runnerStatus = map[int64]gh.RunnerStatus{812: gh.RunnerOffline}
	handler.registrationTimeout = time.Nanosecond
	time.Sleep(time.Millisecond)
	if err := handler.expireRunners(context.Background()); err != nil {
		t.Fatal(err)
	}
	persisted, _, err := store.Get(context.Background(), record.Key)
	if err != nil || persisted.Status != state.StatusProvisioned || persisted.RegisteredAt.IsZero() {
		t.Fatalf("previously registered runner = %#v, %v", persisted, err)
	}
	if len(computeClient.deleted) != 0 || len(githubClient.removed) != 0 {
		t.Fatalf("previously registered runner was removed: instances=%v GitHub=%v", computeClient.deleted, githubClient.removed)
	}
	if githubClient.statusChecks != 1 {
		t.Fatalf("registered runner status checks = %d, want one initial registration check", githubClient.statusChecks)
	}
}

func TestFoundOfflineRunnerClearsStaleProviderMisses(t *testing.T) {
	handler, store, githubClient, computeClient := newTestHandler(t)
	record := state.Record{
		Key: "trusted/private-repo:817", DeliveryID: "delivery-817", JobID: 817,
		Owner: "trusted", Repository: "private-repo", Provider: "test",
	}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ClaimNext(context.Background(), time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProvisioned(context.Background(), record.Key, "instance-817", 817, "runner-817"); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordRunnerSeen(context.Background(), record.Key); err != nil {
		t.Fatal(err)
	}
	handler.livenessCheckInterval = 0
	computeClient.exists = nil
	if err := handler.expireRunners(context.Background()); err != nil {
		t.Fatal(err)
	}
	computeClient.exists = &compute.RunnerInstance{ID: "instance-817", Name: "runner-817"}
	githubClient.runnerStatus = map[int64]gh.RunnerStatus{817: gh.RunnerOffline}
	if err := handler.expireRunners(context.Background()); err != nil {
		t.Fatal(err)
	}
	persisted, _, err := store.Get(context.Background(), record.Key)
	if err != nil || persisted.MissingChecks != 0 || !persisted.MissingSince.IsZero() {
		t.Fatalf("found runner retained stale misses: %#v, %v", persisted, err)
	}
}

func TestProviderAndGitHubMissingConfirmationsDoNotCombine(t *testing.T) {
	handler, store, githubClient, computeClient := newTestHandler(t)
	record := state.Record{
		Key: "trusted/private-repo:818", DeliveryID: "delivery-818", JobID: 818,
		Owner: "trusted", Repository: "private-repo", Provider: "test",
	}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ClaimNext(context.Background(), time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkJITCreated(context.Background(), record.Key, 818, "runner-818"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProvisioned(context.Background(), record.Key, "instance-818", 818, "runner-818"); err != nil {
		t.Fatal(err)
	}
	handler.livenessCheckInterval = 0
	handler.livenessSettleWindow = 0
	computeClient.exists = &compute.RunnerInstance{ID: "instance-818", Name: "runner-818"}
	githubClient.runnerStatus = map[int64]gh.RunnerStatus{818: gh.RunnerMissing}
	for observation := 0; observation < handler.livenessConfirmations-1; observation++ {
		if err := handler.expireRunners(context.Background()); err != nil {
			t.Fatal(err)
		}
	}

	computeClient.exists = nil
	if err := handler.expireRunners(context.Background()); err != nil {
		t.Fatal(err)
	}
	persisted, _, err := store.Get(context.Background(), record.Key)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Status != state.StatusProvisioned || persisted.MissingChecks != 1 || persisted.GitHubMissingChecks != 0 {
		t.Fatalf("interleaved missing observations triggered action: %#v", persisted)
	}
	if len(computeClient.deleted) != 0 || len(githubClient.removed) != 0 {
		t.Fatalf("interleaved missing observations mutated resources: instances=%v GitHub=%v", computeClient.deleted, githubClient.removed)
	}
}

func TestMissingGitHubJITRunnerIsScheduledForDeletion(t *testing.T) {
	handler, store, githubClient, computeClient := newTestHandler(t)
	record := state.Record{
		Key: "trusted/private-repo:813", DeliveryID: "delivery-813", JobID: 813,
		Owner: "trusted", Repository: "private-repo", Provider: "test",
	}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ClaimNext(context.Background(), time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkJITCreated(context.Background(), record.Key, 813, "runner-813"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProvisioned(context.Background(), record.Key, "instance-813", 813, "runner-813"); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordRunnerSeen(context.Background(), record.Key); err != nil {
		t.Fatal(err)
	}
	githubClient.runnerStatus = map[int64]gh.RunnerStatus{813: gh.RunnerMissing}
	computeClient.exists = &compute.RunnerInstance{ID: "instance-813", Name: "runner-813"}
	handler.livenessCheckInterval = 0
	handler.livenessSettleWindow = 0
	for observation := 0; observation < handler.livenessConfirmations; observation++ {
		if err := handler.expireRunners(context.Background()); err != nil {
			t.Fatal(err)
		}
		if observation < handler.livenessConfirmations-1 {
			persisted, _, _ := store.Get(context.Background(), record.Key)
			if persisted.Status != state.StatusProvisioned {
				t.Fatalf("single GitHub 404 scheduled deletion: %#v", persisted)
			}
		}
	}
	persisted, _, err := store.Get(context.Background(), record.Key)
	if err != nil || persisted.Status != state.StatusCompleted || persisted.InstanceID != "instance-813" {
		t.Fatalf("missing GitHub runner reconciliation = %#v, %v", persisted, err)
	}
	if len(computeClient.deleted) != 0 || computeClient.created != 0 {
		t.Fatalf("missing GitHub runner was immediately mutated: deleted=%v created=%d", computeClient.deleted, computeClient.created)
	}
}

func TestMissingUnobservedRunnerDuringRegistrationWindowIsPreserved(t *testing.T) {
	handler, store, githubClient, computeClient := newTestHandler(t)
	record := state.Record{
		Key: "trusted/private-repo:missing-booting", DeliveryID: "delivery-missing-booting", JobID: 820,
		Owner: "trusted", Repository: "private-repo", Provider: "test",
	}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ClaimNext(context.Background(), time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkJITCreated(context.Background(), record.Key, 820, "runner-820"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProvisioned(context.Background(), record.Key, "instance-820", 820, "runner-820"); err != nil {
		t.Fatal(err)
	}
	githubClient.runnerStatus = map[int64]gh.RunnerStatus{820: gh.RunnerMissing}
	computeClient.exists = &compute.RunnerInstance{ID: "instance-820", Name: "runner-820"}
	handler.livenessSettleWindow = 0
	for observation := 0; observation < handler.livenessConfirmations; observation++ {
		if err := handler.expireRunners(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	persisted, _, err := store.Get(context.Background(), record.Key)
	if err != nil || persisted.Status != state.StatusProvisioned || persisted.InstanceID != "instance-820" ||
		persisted.GitHubRunnerID != 820 || persisted.GitHubMissingChecks < handler.livenessConfirmations {
		t.Fatalf("booting runner during GitHub lookup grace = %#v, %v", persisted, err)
	}
	if computeClient.created != 0 || len(computeClient.deleted) != 0 || len(githubClient.removed) != 0 {
		t.Fatalf("booting runner was mutated: created=%d deleted=%v removed=%v", computeClient.created, computeClient.deleted, githubClient.removed)
	}
}

func TestMissingUnobservedRunnerAfterRegistrationWindowIsScheduledForDeletion(t *testing.T) {
	handler, store, githubClient, computeClient := newTestHandler(t)
	record := state.Record{
		Key: "trusted/private-repo:missing-fast-job", DeliveryID: "delivery-missing-fast-job", JobID: 819,
		Owner: "trusted", Repository: "private-repo", Provider: "test",
	}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ClaimNext(context.Background(), time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkJITCreated(context.Background(), record.Key, 819, "runner-819"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProvisioned(context.Background(), record.Key, "instance-819", 819, "runner-819"); err != nil {
		t.Fatal(err)
	}
	githubClient.runnerStatus = map[int64]gh.RunnerStatus{819: gh.RunnerMissing}
	computeClient.exists = &compute.RunnerInstance{ID: "instance-819", Name: "runner-819"}
	handler.registrationTimeout = time.Nanosecond
	handler.livenessSettleWindow = 0
	time.Sleep(time.Millisecond)
	for observation := 0; observation < handler.livenessConfirmations; observation++ {
		if err := handler.expireRunners(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	persisted, _, err := store.Get(context.Background(), record.Key)
	if err != nil || persisted.Status != state.StatusCompleted || persisted.InstanceID != "instance-819" {
		t.Fatalf("missing unobserved runner = %#v, %v", persisted, err)
	}
	if computeClient.created != 0 || len(computeClient.deleted) != 0 {
		t.Fatalf("missing unobserved runner was requeued or deleted immediately: created=%d deleted=%v", computeClient.created, computeClient.deleted)
	}
}

func TestDuplicateProviderInstancesAreCleanedAndRequeued(t *testing.T) {
	handler, store, githubClient, computeClient := newTestHandler(t)
	record := state.Record{
		Key: "trusted/private-repo:814", DeliveryID: "delivery-814", JobID: 814,
		Owner: "trusted", Repository: "private-repo", Provider: "test",
	}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ClaimNext(context.Background(), time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkJITCreated(context.Background(), record.Key, 814, "runner-814"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProvisioned(context.Background(), record.Key, "instance-814", 814, "runner-814"); err != nil {
		t.Fatal(err)
	}
	computeClient.exists = &compute.RunnerInstance{ID: "instance-814", Name: "runner-814"}
	computeClient.findErr = compute.ErrDuplicateInstances
	if err := handler.expireRunners(context.Background()); err != nil {
		t.Fatal(err)
	}
	persisted, _, err := store.Get(context.Background(), record.Key)
	if err != nil || persisted.Status != state.StatusPending || persisted.InstanceID != "" {
		t.Fatalf("duplicate reconciliation = %#v, %v", persisted, err)
	}
	if computeClient.cleanupCalls != 1 || len(githubClient.removed) != 1 {
		t.Fatalf("duplicate cleanup calls=%d removed=%v", computeClient.cleanupCalls, githubClient.removed)
	}
}

func TestProviderSweepReclaimsOnlyOldInstancesAbsentFromState(t *testing.T) {
	handler, store, _, computeClient := newTestHandler(t)
	record := state.Record{
		Key: "trusted/private-repo:815", DeliveryID: "delivery-815", JobID: 815,
		Owner: "trusted", Repository: "private-repo", Provider: "test",
	}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ClaimNext(context.Background(), time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProvisioned(context.Background(), record.Key, "instance-known", 815, "runner-815"); err != nil {
		t.Fatal(err)
	}
	orphaned := state.Record{
		Key: "trusted/private-repo:816", DeliveryID: "delivery-816", JobID: 816,
		Owner: "trusted", Repository: "private-repo", Provider: "test",
	}
	if _, err := store.Create(context.Background(), orphaned); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ClaimNext(context.Background(), time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProvisioned(context.Background(), orphaned.Key, "instance-terminal-orphan", 816, "runner-816"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkOrphaned(context.Background(), orphaned.Key, "delete retries exhausted"); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	computeClient.exists = &compute.RunnerInstance{ID: "instance-known", Name: "runner-815"}
	computeClient.orphans = map[string]time.Time{
		"instance-known":           old,
		"instance-orphan":          old,
		"instance-terminal-orphan": old,
		"instance-new":             time.Now(),
	}
	handler.maxRunnerAge = time.Hour
	if err := handler.expireRunners(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := computeClient.orphans["instance-orphan"]; ok {
		t.Fatal("old orphaned provider instance was not reclaimed")
	}
	if _, ok := computeClient.orphans["instance-terminal-orphan"]; ok {
		t.Fatal("terminal orphan record shielded its provider instance")
	}
	if _, ok := computeClient.orphans["instance-known"]; !ok {
		t.Fatal("state-backed provider instance was reclaimed")
	}
	if _, ok := computeClient.orphans["instance-new"]; !ok {
		t.Fatal("young provider instance was reclaimed before the safety cutoff")
	}
}

func TestDeleteNeverRemovesUnownedGitHubRunner(t *testing.T) {
	handler, store, githubClient, _ := newTestHandler(t)
	record := state.Record{
		Key: "trusted/private-repo:unowned-delete", Owner: "trusted", Repository: "private-repo", Provider: "test",
		InstanceID: "instance-unowned", GitHubRunnerID: 999, GitHubRunnerOwned: false,
	}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	handler.delete(context.Background(), record)
	if len(githubClient.removed) != 0 {
		t.Fatalf("removed unowned GitHub runner: %v", githubClient.removed)
	}
}

func TestAmbiguousProviderCreateIsNeverAutomaticallyRetried(t *testing.T) {
	handler, store, githubClient, _ := newTestHandler(t)
	record := state.Record{
		Key: "trusted/private-repo:ambiguous", Owner: "trusted", Repository: "private-repo", Provider: "test",
	}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	claimed, _, err := store.ClaimNext(context.Background(), time.Now(), 3)
	if err != nil || claimed == nil {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	if err := store.MarkJITCreated(context.Background(), record.Key, 1001, "runner-ambiguous"); err != nil {
		t.Fatal(err)
	}
	claimed.GitHubRunnerID = 1001
	claimed.GitHubRunnerOwned = true
	handler.provisionFailed(context.Background(), *claimed, compute.ErrCreateOutcomeUnknown)
	persisted, _, err := store.Get(context.Background(), record.Key)
	if err != nil || persisted.Status != state.StatusOrphaned {
		t.Fatalf("ambiguous create state = %#v, %v", persisted, err)
	}
	if len(githubClient.removed) != 1 || githubClient.removed[0] != 1001 {
		t.Fatalf("removed ambiguous-create JIT runners = %v", githubClient.removed)
	}
	if retry, _, err := store.ClaimNext(context.Background(), time.Now().Add(time.Hour), 3); err != nil || retry != nil {
		t.Fatalf("ambiguous create was retried: %#v, %v", retry, err)
	}
}

func TestGitHubRateLimitDoesNotConsumeProvisionAttempt(t *testing.T) {
	handler, store, _, _ := newTestHandler(t)
	record := state.Record{
		Key: "trusted/private-repo:rate-limited", Owner: "trusted", Repository: "private-repo", Provider: "test",
	}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	claimed, _, err := store.ClaimNext(context.Background(), time.Now(), 3)
	if err != nil || claimed == nil || claimed.Attempts != 1 {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	resetAt := time.Now().Add(30 * time.Minute).UTC().Truncate(time.Second)
	handler.provisionFailed(context.Background(), *claimed, &gh.RateLimitError{Status: http.StatusTooManyRequests, ResetAt: resetAt})
	persisted, _, err := store.Get(context.Background(), record.Key)
	if err != nil || persisted.Status != state.StatusPending || persisted.Attempts != 0 || !persisted.NextAttemptAt.Equal(resetAt) {
		t.Fatalf("rate-limited provision = %#v, %v", persisted, err)
	}
}

func TestCanceledProvisionReturnsToPendingWithoutConsumingAttempt(t *testing.T) {
	handler, store, _, computeClient := newTestHandler(t)
	record := state.Record{
		Key: "trusted/private-repo:canceled-create", DeliveryID: "delivery-canceled-create", JobID: 919,
		Owner: "trusted", Repository: "private-repo", Provider: "test", Labels: []string{"self-hosted"},
	}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	claimed, _, err := store.ClaimNext(context.Background(), time.Now(), 3)
	if err != nil || claimed == nil {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	computeClient.createStarted = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		handler.provision(ctx, *claimed)
		close(done)
	}()
	<-computeClient.createStarted
	cancel()
	<-done
	persisted, _, err := store.Get(context.Background(), record.Key)
	if err != nil || persisted.Status != state.StatusPending || persisted.Attempts != 0 || persisted.ClaimedWork != "" {
		t.Fatalf("canceled provision state = %#v, %v", persisted, err)
	}
}

func TestRequiresDeliveryIDAndStrongConfiguration(t *testing.T) {
	handler, _, _, _ := newTestHandler(t)
	if response := serveEvent(handler, testEvent("queued", []string{"self-hosted", "chef"}), ""); response.Code != http.StatusBadRequest {
		t.Fatalf("missing delivery status = %d", response.Code)
	}

	_, err := NewHandler(Config{WebhookSecret: []byte("short")})
	if err == nil {
		t.Fatal("expected weak configuration to fail closed")
	}
}

func TestLivenessConfirmationConfiguration(t *testing.T) {
	limits, err := resolveHandlerLimits(Config{
		MaxRunnerAge:          time.Hour,
		LivenessSettleWindow:  45 * time.Second,
		LivenessConfirmations: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if limits.livenessSettleWindow != 45*time.Second || limits.livenessConfirmations != 5 {
		t.Fatalf("liveness limits = %#v", limits)
	}
	defaults, err := resolveHandlerLimits(Config{MaxRunnerAge: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if defaults.livenessSettleWindow != 2*time.Minute || defaults.livenessConfirmations != 3 {
		t.Fatalf("default liveness limits = %#v", defaults)
	}
}

func TestWebhookIngestRejectsContentionInsteadOfBlocking(t *testing.T) {
	handler, store, _, _ := newTestHandler(t)
	blocking := &blockingCreateStore{Store: store, started: make(chan struct{}), release: make(chan struct{})}
	handler.store = blocking
	handler.ingestSlots = make(chan struct{}, 1)
	handler.ingestWait = 10 * time.Millisecond
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		firstDone <- serveEvent(handler, testEvent("queued", []string{"self-hosted", "chef"}), "delivery-first")
	}()
	<-blocking.started
	second := serveEvent(handler, testEvent("queued", []string{"self-hosted", "chef"}), "delivery-second")
	if second.Code != http.StatusServiceUnavailable {
		t.Fatalf("contended ingest status = %d %q", second.Code, second.Body.String())
	}
	close(blocking.release)
	if first := <-firstDone; first.Code != http.StatusAccepted {
		t.Fatalf("first ingest status = %d %q", first.Code, first.Body.String())
	}
}

func TestWebhookIngestQueuesNormalConcurrentBurst(t *testing.T) {
	handler, store, _, _ := newTestHandler(t)
	const deliveries = 32
	responses := make(chan *httptest.ResponseRecorder, deliveries)
	var wg sync.WaitGroup
	for index := 0; index < deliveries; index++ {
		wg.Add(1)
		go func(jobID int64) {
			defer wg.Done()
			event := testEvent("queued", []string{"self-hosted", "chef"})
			event.WorkflowJob.ID = jobID
			responses <- serveEvent(handler, event, fmt.Sprintf("burst-%d", jobID))
		}(int64(2000 + index))
	}
	wg.Wait()
	close(responses)
	for response := range responses {
		if response.Code != http.StatusAccepted {
			t.Fatalf("concurrent ingest status = %d %q", response.Code, response.Body.String())
		}
	}
	for index := 0; index < deliveries; index++ {
		key := fmt.Sprintf("trusted/private-repo:%d", 2000+index)
		if _, found, err := store.Get(context.Background(), key); err != nil || !found {
			t.Fatalf("concurrent delivery %s: found=%v err=%v", key, found, err)
		}
	}
}

func TestRunnerNameKeepsUniqueSuffixWhenTruncated(t *testing.T) {
	longRepo := "trusted/" + strings.Repeat("very-long-repository-", 5)
	first := runnerName(longRepo, 1)
	second := runnerName(longRepo, 2)
	if len(first) > 63 || len(second) > 63 {
		t.Fatalf("runner names exceed provider limit: %d, %d", len(first), len(second))
	}
	if first == second {
		t.Fatalf("truncated runner names collided: %q", first)
	}
}

func TestRunnerNameIsPortableAcrossProviders(t *testing.T) {
	name := runnerName("trusted/my_repo.v2", 42)
	if len(name) > 63 {
		t.Fatalf("runner name exceeds provider limit: %d", len(name))
	}
	for _, character := range name {
		if character < 'a' || character > 'z' {
			if character < '0' || character > '9' {
				if character != '-' {
					t.Fatalf("runner name contains provider-unsafe character %q: %q", character, name)
				}
			}
		}
	}
	if strings.ContainsAny(name, "_.") {
		t.Fatalf("runner name retained repository punctuation: %q", name)
	}
}

func TestProvisionRemovesPersistedStaleJITRunnerBeforeRetry(t *testing.T) {
	handler, store, githubClient, _ := newTestHandler(t)
	record := state.Record{
		Key:        "trusted/private-repo:42",
		DeliveryID: "delivery-stale",
		JobID:      42,
		Owner:      "trusted",
		Repository: "private-repo",
		Labels:     []string{"self-hosted", "chef"},
	}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ClaimNext(context.Background(), time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkJITCreated(context.Background(), record.Key, 333, "stale-runner"); err != nil {
		t.Fatal(err)
	}
	persisted, _, err := store.Get(context.Background(), record.Key)
	if err != nil {
		t.Fatal(err)
	}
	handler.provision(context.Background(), persisted)

	githubClient.mu.Lock()
	defer githubClient.mu.Unlock()
	if len(githubClient.removed) != 1 || githubClient.removed[0] != 333 {
		t.Fatalf("removed JIT runners = %v", githubClient.removed)
	}
	if githubClient.generated != 1 {
		t.Fatalf("generated JIT configs = %d, want 1", githubClient.generated)
	}
}

func TestProvisionedStateWriteFailureSchedulesRetry(t *testing.T) {
	handler, store, _, _ := newTestHandler(t)
	handler.store = &failMarkProvisionedStore{Store: store, failures: 1}
	record := state.Record{
		Key: "trusted/private-repo:43", DeliveryID: "delivery-write-failure", JobID: 43,
		Owner: "trusted", Repository: "private-repo", Labels: []string{"self-hosted", "chef"}, Provider: "test",
	}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	claimed, _, err := store.ClaimNext(context.Background(), time.Now(), 3)
	if err != nil || claimed == nil {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	handler.provision(context.Background(), *claimed)
	persisted, _, err := store.Get(context.Background(), record.Key)
	if err != nil || persisted.Status != state.StatusPending || persisted.LastError == "" {
		t.Fatalf("retry state = %#v, %v", persisted, err)
	}
}

func TestCreateErrorPreservesJITForProviderReconciliation(t *testing.T) {
	handler, store, githubClient, computeClient := newTestHandler(t)
	computeClient.createErr = errors.New("ambiguous create timeout")
	record := state.Record{
		Key: "trusted/private-repo:44", DeliveryID: "delivery-timeout", JobID: 44,
		Owner: "trusted", Repository: "private-repo", Labels: []string{"self-hosted", "chef"}, Provider: "test",
	}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	claimed, _, err := store.ClaimNext(context.Background(), time.Now(), 3)
	if err != nil || claimed == nil {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	handler.provision(context.Background(), *claimed)
	persisted, _, err := store.Get(context.Background(), record.Key)
	if err != nil || persisted.Status != state.StatusPending || persisted.GitHubRunnerID == 0 {
		t.Fatalf("post-timeout state = %#v, %v", persisted, err)
	}
	computeClient.createErr = nil
	handler.provision(context.Background(), persisted)
	persisted, _, err = store.Get(context.Background(), record.Key)
	if err != nil || persisted.Status != state.StatusProvisioned || persisted.GitHubRunnerID == 0 {
		t.Fatalf("reconciled state = %#v, %v", persisted, err)
	}
	githubClient.mu.Lock()
	defer githubClient.mu.Unlock()
	if len(githubClient.removed) != 0 {
		t.Fatalf("revoked JIT runners during ambiguous create: %v", githubClient.removed)
	}
}

func TestExhaustedProvisionCleansJITAndReleasesAdmission(t *testing.T) {
	handler, store, githubClient, computeClient := newTestHandler(t)
	handler.maxAttempts = 1
	if err := store.SetMaxLiveRunners(1); err != nil {
		t.Fatal(err)
	}
	computeClient.createErr = errors.New("provider quota exhausted")
	first := state.Record{
		Key: "trusted/private-repo:45", DeliveryID: "delivery-exhausted", JobID: 45,
		Owner: "trusted", Repository: "private-repo", Labels: []string{"self-hosted", "chef"}, Provider: "test",
	}
	if _, err := store.Create(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	claimed, _, err := store.ClaimNext(context.Background(), time.Now(), 1)
	if err != nil || claimed == nil {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	handler.provision(context.Background(), *claimed)
	failed, _, err := store.Get(context.Background(), first.Key)
	if err != nil || failed.Status != state.StatusFailed || failed.GitHubRunnerID != 0 || failed.InstanceID != "" {
		t.Fatalf("exhausted provision state = %#v, %v", failed, err)
	}
	githubClient.mu.Lock()
	if len(githubClient.removed) != 1 {
		githubClient.mu.Unlock()
		t.Fatalf("removed exhausted JIT runners = %v", githubClient.removed)
	}
	githubClient.mu.Unlock()
	second := state.Record{
		Key: "trusted/private-repo:46", DeliveryID: "delivery-after-exhaustion", JobID: 46,
		Owner: "trusted", Repository: "private-repo", Labels: []string{"self-hosted", "chef"}, Provider: "test",
	}
	if _, err := store.Create(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	next, kind, err := store.ClaimNext(context.Background(), time.Now(), 1)
	if err != nil || next == nil || next.Key != second.Key || kind != state.WorkProvision {
		t.Fatalf("admission after exhausted provision = %#v, %q, %v", next, kind, err)
	}
}

func TestReaperContinuesAfterPerRecordFailure(t *testing.T) {
	handler, store, githubClient, _ := newTestHandler(t)
	githubClient.removeErr = map[int64]error{501: errors.New("injected GitHub failure")}
	for i, runnerID := range []int64{501, 502} {
		key := fmt.Sprintf("trusted/private-repo:%d", 50+i)
		record := state.Record{
			Key: key, DeliveryID: fmt.Sprintf("delivery-%d", i), JobID: int64(50 + i),
			Owner: "trusted", Repository: "private-repo", Provider: "test",
		}
		if _, err := store.Create(context.Background(), record); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.ClaimNext(context.Background(), time.Now(), 3); err != nil {
			t.Fatal(err)
		}
		if err := store.MarkJITCreated(context.Background(), key, runnerID, "runner"); err != nil {
			t.Fatal(err)
		}
		if err := store.MarkCompleted(context.Background(), key); err != nil {
			t.Fatal(err)
		}
		if err := store.MarkProvisionFailed(context.Background(), key, "completion race", time.Now(), 3); err != nil {
			t.Fatal(err)
		}
	}
	if err := handler.expireRunners(context.Background()); err == nil {
		t.Fatal("expected aggregate reconciliation error")
	}
	second, _, err := store.Get(context.Background(), "trusted/private-repo:51")
	if err != nil || second.Status != state.StatusDeleted {
		t.Fatalf("second record was blocked by first failure: %#v, %v", second, err)
	}
}

func TestCanceledWorkerDoesNotConsumeProvisionAttempt(t *testing.T) {
	handler, store, _, computeClient := newTestHandler(t)
	record := state.Record{
		Key: "trusted/private-repo:60", DeliveryID: "delivery-canceled", JobID: 60,
		Owner: "trusted", Repository: "private-repo", Provider: "test",
	}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	handler.Start(ctx)
	handler.Wait()
	persisted, _, err := store.Get(context.Background(), record.Key)
	if err != nil || persisted.Status != state.StatusPending || persisted.Attempts != 0 {
		t.Fatalf("canceled worker changed record = %#v, %v", persisted, err)
	}
	computeClient.mu.Lock()
	defer computeClient.mu.Unlock()
	if computeClient.created != 0 {
		t.Fatalf("canceled worker created %d runners", computeClient.created)
	}
}

func TestCanceledDeleteDoesNotConsumeAttempt(t *testing.T) {
	handler, store, _, computeClient := newTestHandler(t)
	record := state.Record{
		Key: "trusted/private-repo:delete-canceled", DeliveryID: "delivery-delete-canceled",
		Owner: "trusted", Repository: "private-repo", Provider: "test",
	}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ClaimNext(context.Background(), time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProvisioned(context.Background(), record.Key, "instance-canceled", 601, "runner-601"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkCompleted(context.Background(), record.Key); err != nil {
		t.Fatal(err)
	}
	claimed, kind, err := store.ClaimNext(context.Background(), time.Now(), 3)
	if err != nil || claimed == nil || kind != state.WorkDelete {
		t.Fatalf("delete claim = %#v, %q, %v", claimed, kind, err)
	}
	computeClient.deleteErr = context.Canceled
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	handler.delete(ctx, *claimed)
	persisted, _, err := store.Get(context.Background(), record.Key)
	if err != nil || persisted.ClaimedWork != "" || persisted.Status != state.StatusCompleted || persisted.DeleteAttempts != 0 {
		t.Fatalf("canceled deletion state = %#v, %v", persisted, err)
	}
}

func TestProviderMismatchIsTerminalForAllWorkKinds(t *testing.T) {
	handler, store, _, computeClient := newTestHandler(t)
	for i, kind := range []state.WorkKind{state.WorkProvision, state.WorkDelete} {
		record := state.Record{
			Key: fmt.Sprintf("trusted/private-repo:mismatch-%d", i), Owner: "trusted", Repository: "private-repo",
			Provider: "other-provider",
		}
		if _, err := store.Create(context.Background(), record); err != nil {
			t.Fatal(err)
		}
		handler.process(context.Background(), record, kind)
		persisted, found, err := store.Get(context.Background(), record.Key)
		if err != nil || !found {
			t.Fatalf("get mismatched %s record: found=%v err=%v", kind, found, err)
		}
		if persisted.Status != state.StatusOrphaned || !strings.Contains(persisted.LastError, "other-provider") {
			t.Fatalf("mismatched %s record = %#v", kind, persisted)
		}
	}
	computeClient.mu.Lock()
	defer computeClient.mu.Unlock()
	if computeClient.created != 0 || len(computeClient.deleted) != 0 {
		t.Fatalf("provider mismatch touched compute: created=%d deleted=%v", computeClient.created, computeClient.deleted)
	}
}

func TestReaperProviderMismatchBecomesTerminalOnce(t *testing.T) {
	handler, store, _, _ := newTestHandler(t)
	record := state.Record{
		Key: "trusted/private-repo:reaper-mismatch", Owner: "trusted", Repository: "private-repo",
		Provider: "other-provider",
	}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkCompleted(context.Background(), record.Key); err != nil {
		t.Fatal(err)
	}
	handler.maxRunnerAge = time.Nanosecond
	time.Sleep(time.Millisecond)
	if err := handler.expireRunners(context.Background()); err == nil {
		t.Fatal("provider mismatch returned no reconciliation error")
	}
	persisted, _, err := store.Get(context.Background(), record.Key)
	if err != nil || persisted.Status != state.StatusOrphaned {
		t.Fatalf("reaper mismatch state = %#v, %v", persisted, err)
	}
	if err := handler.expireRunners(context.Background()); err != nil {
		t.Fatalf("terminal provider mismatch remained in reaper loop: %v", err)
	}
}

func TestUntrackedCleanupExhaustionBecomesOrphaned(t *testing.T) {
	handler, store, _, computeClient := newTestHandler(t)
	handler.maxAttempts = 2
	key := createFailedProvision(t, store, "trusted/private-repo:cleanup-budget")
	computeClient.cleanupErr = errors.New("permanent provider failure")

	if err := handler.expireRunners(context.Background()); err == nil {
		t.Fatal("first cleanup failure was not reported")
	}
	if err := store.MarkDeleteFailed(context.Background(), key, "retry now", time.Now().Add(-time.Second), handler.maxAttempts); err != nil {
		t.Fatal(err)
	}
	if err := handler.expireRunners(context.Background()); err == nil {
		t.Fatal("final cleanup failure was not reported")
	}
	persisted, _, err := store.Get(context.Background(), key)
	if err != nil || persisted.Status != state.StatusOrphaned || persisted.DeleteAttempts != 2 {
		t.Fatalf("exhausted cleanup record = %#v, %v", persisted, err)
	}
	_ = handler.expireRunners(context.Background())
	computeClient.mu.Lock()
	defer computeClient.mu.Unlock()
	if computeClient.cleanupCalls != 2 {
		t.Fatalf("terminal cleanup was retried %d times", computeClient.cleanupCalls)
	}
}

func TestUntrackedOwnershipMismatchBecomesOrphanedImmediately(t *testing.T) {
	handler, store, _, computeClient := newTestHandler(t)
	key := createFailedProvision(t, store, "trusted/private-repo:cleanup-ownership")
	computeClient.cleanupErr = compute.ErrOwnershipMismatch
	if err := handler.expireRunners(context.Background()); !errors.Is(err, compute.ErrOwnershipMismatch) {
		t.Fatalf("ownership cleanup error = %v", err)
	}
	persisted, _, err := store.Get(context.Background(), key)
	if err != nil || persisted.Status != state.StatusOrphaned || persisted.DeleteAttempts != 1 {
		t.Fatalf("ownership cleanup record = %#v, %v", persisted, err)
	}
}

func TestControllerDeadlineDoesNotConsumeUntrackedCleanupBudget(t *testing.T) {
	handler, store, _, computeClient := newTestHandler(t)
	key := createFailedProvision(t, store, "trusted/private-repo:cleanup-deadline")
	computeClient.cleanupErr = context.DeadlineExceeded
	if err := handler.expireRunners(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cleanup deadline error = %v", err)
	}
	persisted, _, err := store.Get(context.Background(), key)
	if err != nil || persisted.Status == state.StatusOrphaned || persisted.DeleteAttempts != 0 || !persisted.NextAttemptAt.After(time.Now()) {
		t.Fatalf("deadline cleanup state = %#v, %v", persisted, err)
	}
}

func TestDeletePersistenceFailureReleasesClaimForRetry(t *testing.T) {
	handler, store, _, _ := newTestHandler(t)
	key := "trusted/private-repo:delete-persistence"
	record := state.Record{Key: key, Owner: "trusted", Repository: "private-repo", Provider: "test"}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	claimed, _, err := store.ClaimNext(context.Background(), time.Now(), 5)
	if err != nil || claimed == nil {
		t.Fatalf("claim provision = %#v, %v", claimed, err)
	}
	if err := store.MarkJITCreated(context.Background(), key, 101, "runner"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProvisioned(context.Background(), key, "instance-1", 101, "runner"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkCompleted(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	claimed, kind, err := store.ClaimNext(context.Background(), time.Now(), 5)
	if err != nil || claimed == nil || kind != state.WorkDelete {
		t.Fatalf("claim delete = %#v, %q, %v", claimed, kind, err)
	}
	handler.store = &failMarkDeletedStore{Store: store, failures: 1}
	handler.delete(context.Background(), *claimed)
	persisted, _, err := store.Get(context.Background(), key)
	if err != nil || persisted.ClaimedWork != "" || persisted.Status != state.StatusCompleted {
		t.Fatalf("failed deletion state = %#v, %v", persisted, err)
	}
	if err := store.MarkDeleteFailed(context.Background(), key, persisted.LastError, time.Now().Add(-time.Second), 5); err != nil {
		t.Fatal(err)
	}
	retry, retryKind, err := store.ClaimNext(context.Background(), time.Now(), 5)
	if err != nil || retry == nil || retryKind != state.WorkDelete {
		t.Fatalf("reclaim delete = %#v, %q, %v", retry, retryKind, err)
	}
}

func createFailedProvision(t *testing.T, store *state.FileStore, key string) string {
	t.Helper()
	record := state.Record{Key: key, Owner: "trusted", Repository: "private-repo", Provider: "test"}
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	claimed, _, err := store.ClaimNext(context.Background(), time.Now(), 1)
	if err != nil || claimed == nil {
		t.Fatalf("claim failed provision: %#v, %v", claimed, err)
	}
	if err := store.MarkProvisionFailed(context.Background(), key, "provision failed", time.Now(), 1); err != nil {
		t.Fatal(err)
	}
	return key
}

func newTestHandler(t *testing.T) (*Handler, *state.FileStore, *fakeGitHub, *fakeCompute) {
	t.Helper()
	store, err := state.OpenFileStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	githubClient := &fakeGitHub{}
	computeClient := &fakeCompute{}
	handler, err := NewHandler(Config{
		WebhookSecret:       []byte(testWebhookSecret),
		GitHubClient:        githubClient,
		ComputeClient:       computeClient,
		Store:               store,
		RequiredLabel:       "self-hosted",
		AllowedLabels:       []string{"self-hosted", "chef"},
		AllowedRepositories: []string{"trusted/private-repo"},
		RunnerVersion:       "2.331.0",
		RunnerSHA256:        strings.Repeat("a", 64),
		ChefInstallerSHA256: strings.Repeat("b", 64),
		WorkerCount:         1,
		MaxLiveRunners:      2,
		PollInterval:        time.Millisecond,
		MaxRunnerAge:        time.Hour,
		CancelledRunnerTTL:  time.Minute,
		InstallationID:      999,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Tests may tune handler timing and fake-compute fields only before Start;
	// mutating them while workers run would race with lifecycle reconciliation.
	return handler, store, githubClient, computeClient
}

func testEvent(action string, labels []string) WorkflowJobEvent {
	event := WorkflowJobEvent{
		Action: action,
		WorkflowJob: WorkflowJob{
			ID:     42,
			Name:   "integration",
			Labels: labels,
		},
		Repo: RepoInfo{
			FullName: "trusted/private-repo",
			Name:     "private-repo",
			Private:  true,
		},
	}
	event.Repo.Owner.Login = "trusted"
	event.Installation.ID = 999
	return event
}

func serveEvent(handler *Handler, event WorkflowJobEvent, deliveryID string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(event)
	request := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(body)))
	request.Header.Set("X-GitHub-Event", "workflow_job")
	request.Header.Set("X-Hub-Signature-256", sign(body))
	if deliveryID != "" {
		request.Header.Set("X-GitHub-Delivery", deliveryID)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func sign(payload []byte) string {
	mac := hmac.New(sha256.New, []byte(testWebhookSecret))
	_, _ = mac.Write(payload)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition was not satisfied before timeout")
}
