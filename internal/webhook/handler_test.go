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
	mu        sync.Mutex
	generated int
	removed   []int64
	removeErr map[int64]error
}

func (f *fakeGitHub) GenerateRepoJITConfig(_ context.Context, _, _, _ string, _ int64, _ []string) (gh.JITConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.generated++
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
	mu        sync.Mutex
	created   int
	deleted   []string
	exists    *compute.RunnerInstance
	createErr error
	findBlock bool
}

type failMarkProvisionedStore struct {
	state.Store
	failures int
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
	if f.exists == nil {
		return nil, false, nil
	}
	copy := *f.exists
	return &copy, true, nil
}

func (f *fakeCompute) CreateRunner(_ context.Context, _ compute.RunnerParams) (*compute.RunnerInstance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created++
	f.exists = &compute.RunnerInstance{ID: "500", Name: "eph-private-repo-42"}
	if f.createErr != nil {
		return nil, f.createErr
	}
	copy := *f.exists
	return &copy, nil
}

func (f *fakeCompute) DeleteRunner(_ context.Context, id, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, id)
	f.exists = nil
	return nil
}

func (f *fakeCompute) CleanupRunner(_ context.Context, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.exists != nil {
		f.deleted = append(f.deleted, f.exists.ID)
		f.exists = nil
	}
	return nil
}

func TestQueuedDeliveryIsDurableIdempotentAndDeletedOnCompletion(t *testing.T) {
	handler, store, githubClient, computeClient := newTestHandler(t)
	ctx, cancel := context.WithCancel(context.Background())
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

func TestRejectsPublicRepositoryAndUnapprovedLabels(t *testing.T) {
	handler, store, _, _ := newTestHandler(t)
	public := testEvent("queued", []string{"self-hosted", "chef"})
	public.Repo.Private = false
	if response := serveEvent(handler, public, "delivery-public"); response.Code != http.StatusForbidden {
		t.Fatalf("public repository status = %d", response.Code)
	}

	unapproved := testEvent("queued", []string{"self-hosted", "production"})
	if response := serveEvent(handler, unapproved, "delivery-label"); response.Code != http.StatusForbidden {
		t.Fatalf("unapproved label status = %d", response.Code)
	}
	unapproved.Action = "completed"
	if response := serveEvent(handler, unapproved, "delivery-label-completed"); response.Code != http.StatusOK || response.Body.String() != "ignored" {
		t.Fatalf("irrelevant completion = %d %q", response.Code, response.Body.String())
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

func TestForeignRunnerCompletionNeverDeregistersRunner(t *testing.T) {
	handler, _, githubClient, _ := newTestHandler(t)
	event := testEvent("completed", []string{"self-hosted", "chef"})
	event.WorkflowJob.ID = 73
	event.WorkflowJob.RunnerID = 4001
	event.WorkflowJob.RunnerName = "permanent-chef-runner"
	if response := serveEvent(handler, event, "foreign-runner-completion"); response.Code != http.StatusAccepted {
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

func newTestHandler(t *testing.T) (*Handler, *state.FileStore, *fakeGitHub, *fakeCompute) {
	t.Helper()
	store, err := state.OpenFileStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
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
