package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Status describes the controller-owned lifecycle of a runner instance.
type Status string

const (
	StatusPending      Status = "pending"
	StatusProvisioning Status = "provisioning"
	StatusProvisioned  Status = "provisioned"
	StatusCompleted    Status = "completed"
	StatusDeleting     Status = "deleting"
	StatusDeleted      Status = "deleted"
	StatusFailed       Status = "failed"
	StatusOrphaned     Status = "orphaned"
)

// WorkKind identifies the action claimed by a worker.
type WorkKind string

const (
	WorkProvision WorkKind = "provision"
	WorkDelete    WorkKind = "delete"
)

// Record is the durable source of truth for one workflow job and its runner.
// It intentionally contains no credentials or JIT configuration.
type Record struct {
	Key            string   `json:"key"`
	DeliveryID     string   `json:"delivery_id"`
	JobID          int64    `json:"job_id"`
	Owner          string   `json:"owner"`
	Repository     string   `json:"repository"`
	Labels         []string `json:"labels"`
	Provider       string   `json:"provider"`
	RunnerName     string   `json:"runner_name,omitempty"`
	GitHubRunnerID int64    `json:"github_runner_id,omitempty"`
	// GitHubRunnerOwned proves the ID came from this controller's JIT request.
	GitHubRunnerOwned bool   `json:"github_runner_owned,omitempty"`
	InstanceID        string `json:"instance_id,omitempty"`
	// DropletID is read only to migrate state written before provider-neutral IDs.
	DropletID           int       `json:"droplet_id,omitempty"`
	Status              Status    `json:"status"`
	ClaimedWork         WorkKind  `json:"claimed_work,omitempty"`
	DeferDeletion       bool      `json:"defer_deletion,omitempty"`
	Attempts            int       `json:"attempts"`
	ProvisionEpoch      int       `json:"provision_epoch,omitempty"`
	DeleteAttempts      int       `json:"delete_attempts"`
	ReconcileFailures   int       `json:"reconcile_failures,omitempty"`
	ThrottleFailures    int       `json:"throttle_failures,omitempty"`
	LastError           string    `json:"last_error,omitempty"`
	NextAttemptAt       time.Time `json:"next_attempt_at,omitempty"`
	MissingSince        time.Time `json:"missing_since,omitempty"`
	MissingChecks       int       `json:"missing_checks,omitempty"`
	GitHubMissingSince  time.Time `json:"github_missing_since,omitempty"`
	GitHubMissingChecks int       `json:"github_missing_checks,omitempty"`
	CreatedAt           time.Time `json:"created_at"`
	ProvisionedAt       time.Time `json:"provisioned_at,omitempty"`
	RegisteredAt        time.Time `json:"registered_at,omitempty"`
	ReconciledAt        time.Time `json:"reconciled_at,omitempty"`
	UpdatedAt           time.Time `json:"updated_at"`
}

// Store defines the durable lifecycle operations used by the controller.
type Store interface {
	Create(context.Context, Record) (bool, error)
	RecordCompletion(context.Context, Record) error
	Get(context.Context, string) (Record, bool, error)
	OwnsRunner(context.Context, string, string, int64) (bool, error)
	ClaimNext(context.Context, time.Time, int) (*Record, WorkKind, error)
	SetMaxLiveRunners(int) error
	ReleaseClaim(context.Context, string, WorkKind) error
	DeferRateLimitedWork(context.Context, string, WorkKind, string, time.Time, int) error
	MarkJITCreated(context.Context, string, int64, string) error
	ClearJIT(context.Context, string) error
	MarkProvisioned(context.Context, string, string, int64, string) error
	MarkProvisionFailed(context.Context, string, string, time.Time, int) error
	MarkCompleted(context.Context, string) error
	ScheduleDeletion(context.Context, string) error
	MarkDeleted(context.Context, string) error
	BeginCleanupAttempt(context.Context, string, int) (int, bool, error)
	MarkDeleteFailed(context.Context, string, string, time.Time, int) error
	MarkOrphaned(context.Context, string, string) error
	ListExpired(context.Context, time.Time) ([]Record, error)
	ListProvisioned(context.Context) ([]Record, error)
	ListOrphaned(context.Context) ([]Record, error)
	KnownInstanceIDs(context.Context) (map[string]struct{}, error)
	ReleaseSweptOrphans(context.Context, string, time.Time) (int, error)
	ObserveRunnerMissing(context.Context, string, time.Time, time.Duration, int) (bool, error)
	ObserveGitHubRunnerMissing(context.Context, string, time.Time, time.Duration, int) (bool, error)
	ClearProviderMissing(context.Context, string) error
	ClearGitHubMissing(context.Context, string) error
	RecordRunnerSeen(context.Context, string) error
	ClearRunnerMissing(context.Context, string) error
	DeferReconciliation(context.Context, string, string, time.Time, int, bool, bool) error
	DeferRateLimitedCleanup(context.Context, string, string, time.Time, int, bool) error
	DeferOrphanCleanup(context.Context, string, string, time.Time) error
	RequeueMissingRunner(context.Context, string, int) error
	PruneDeleted(context.Context) (int, error)
}

var _ Store = (*FileStore)(nil)

type diskState struct {
	Records map[string]Record `json:"records"`
}

type journalEntry struct {
	Records map[string]*Record `json:"records"`
}

// FileStore persists constant-size mutation records to an fsynced journal and
// periodically compacts them into an atomically replaced snapshot. It is
// intended for a single controller process.
type FileStore struct {
	mu               sync.Mutex
	lockFile         *os.File
	path             string
	journalPath      string
	records          map[string]Record
	persisted        map[string]Record
	dirtyKeys        map[string]struct{}
	runnerKeys       map[string]string
	deletedRetention time.Duration
	orderedKeys      []string
	keysDirty        bool
	journalEntries   int
	journalBytes     int64
	maxLiveRunners   int
	closed           bool
}

const defaultDeletedRetention = 24 * time.Hour

const completionMarkerPrefix = "_completed_runner:"

// Option configures durable state behavior.
type Option func(*FileStore) error

// WithDeletedRetention sets how long deleted records remain for webhook
// redelivery deduplication before compaction removes them.
func WithDeletedRetention(retention time.Duration) Option {
	return func(store *FileStore) error {
		if retention < 24*time.Hour {
			return fmt.Errorf("deleted record retention must be at least 24 hours")
		}
		store.deletedRetention = retention
		return nil
	}
}

// OpenFileStore opens or creates a state file. Interrupted in-flight work is
// returned to a claimable state so a restarted controller can reconcile it.
func OpenFileStore(path string, options ...Option) (_ *FileStore, err error) {
	if path == "" {
		return nil, errors.New("state file path is required")
	}
	s, err := newFileStore(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = s.Close()
		}
	}()
	for _, option := range options {
		if err := option(s); err != nil {
			return nil, err
		}
	}
	snapshotMissing, err := s.loadSnapshot()
	if err != nil {
		return nil, err
	}
	if err := s.loadJournal(); err != nil {
		return nil, err
	}
	s.persisted = cloneRecords(s.records)
	if s.recoverRecords(time.Now().UTC()) {
		if err := s.saveLocked(); err != nil {
			return nil, err
		}
	}
	s.rebuildRunnerIndexLocked()
	if err := s.finalizeOpen(snapshotMissing); err != nil {
		return nil, err
	}
	return s, nil
}

func newFileStore(path string) (*FileStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	lockFile, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open state lock: %w", err)
	}
	if err := lockFile.Chmod(0o600); err != nil {
		_ = lockFile.Close()
		return nil, fmt.Errorf("secure state lock permissions: %w", err)
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lockFile.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, fmt.Errorf("state file %q is already locked by another controller", path)
		}
		return nil, fmt.Errorf("lock state file: %w", err)
	}

	return &FileStore{
		lockFile: lockFile, path: path, journalPath: path + ".wal", records: make(map[string]Record), persisted: make(map[string]Record),
		dirtyKeys: make(map[string]struct{}), runnerKeys: make(map[string]string),
		deletedRetention: defaultDeletedRetention, keysDirty: true, maxLiveRunners: 20,
	}, nil
}

func (s *FileStore) loadSnapshot() (bool, error) {
	path := s.path
	data, err := os.ReadFile(path)
	snapshotMissing := errors.Is(err, os.ErrNotExist)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("read state file: %w", err)
	}
	if err == nil {
		if err := os.Chmod(path, 0o600); err != nil {
			return false, fmt.Errorf("secure state file permissions: %w", err)
		}
	}
	if len(data) != 0 {
		var persisted diskState
		if err := json.Unmarshal(data, &persisted); err != nil {
			return false, fmt.Errorf("decode state file: %w", err)
		}
		if persisted.Records != nil {
			s.records = persisted.Records
		}
	}
	return snapshotMissing, nil
}

func (s *FileStore) recoverRecords(now time.Time) bool {
	recovered := false
	for key, record := range s.records {
		updated, keep, changed := recoverRecord(key, record, now, s.deletedRetention)
		if !keep {
			delete(s.records, key)
			s.markDirty(key)
			s.keysDirty = true
			recovered = true
			continue
		}
		if changed {
			s.records[key] = updated
			s.markDirty(key)
			recovered = true
		}
	}
	return recovered
}

func recoverRecord(key string, record Record, now time.Time, deletedRetention time.Duration) (Record, bool, bool) {
	if record.Status == StatusDeleted && record.UpdatedAt.Before(now.Add(-deletedRetention)) {
		return record, false, true
	}
	changed := false
	if record.ClaimedWork != "" {
		record.ClaimedWork = ""
		changed = true
	}
	if record.Provider == "" {
		// All state written before provider binding was DigitalOcean-only.
		record.Provider = "digitalocean"
		changed = true
	}
	if record.Status == StatusProvisioned && record.ProvisionedAt.IsZero() {
		record.ProvisionedAt = record.UpdatedAt
		if record.ProvisionedAt.IsZero() {
			record.ProvisionedAt = record.CreatedAt
		}
		changed = true
	}
	changed = recoverInterruptedWork(&record) || changed
	if record.InstanceID == "" && record.DropletID != 0 {
		record.InstanceID = strconv.Itoa(record.DropletID)
		record.DropletID = 0
		changed = true
	}
	if changed {
		record.NextAttemptAt = time.Time{}
		record.UpdatedAt = now
	}
	return record, true, changed
}

func recoverInterruptedWork(record *Record) bool {
	switch record.Status {
	case StatusProvisioning:
		record.Status = StatusPending
		if record.Attempts > 0 {
			record.Attempts--
		}
		return true
	case StatusDeleting:
		record.Status = StatusCompleted
		if record.DeleteAttempts > 0 {
			record.DeleteAttempts--
		}
		return true
	case StatusCompleted:
		// Older controller versions recorded shutdown cancellation as a
		// deletion failure after releasing the work claim. Refund that attempt
		// during migration; cancellation is not a resource lifecycle failure.
		if record.DeleteAttempts > 0 &&
			(record.LastError == context.Canceled.Error() || record.LastError == context.DeadlineExceeded.Error()) {
			record.DeleteAttempts--
			record.LastError = ""
			return true
		}
	default:
		return false
	}
	return false
}

func (s *FileStore) finalizeOpen(snapshotMissing bool) error {
	if s.journalEntries >= 512 || s.journalBytes >= 4*1024*1024 {
		return s.compactLocked()
	}
	if snapshotMissing {
		return s.writeSnapshotLocked()
	}
	return nil
}

// Close releases the exclusive writer lock held for this store.
func (s *FileStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lockFile == nil {
		return nil
	}
	s.closed = true
	lockFile := s.lockFile
	s.lockFile = nil
	unlockErr := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
	closeErr := lockFile.Close()
	return errors.Join(unlockErr, closeErr)
}

func (s *FileStore) Create(_ context.Context, record Record) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.records[record.Key]; exists {
		return false, nil
	}
	now := time.Now().UTC()
	record.Status = StatusPending
	record.CreatedAt = now
	record.UpdatedAt = now
	s.records[record.Key] = clone(record)
	s.markDirty(record.Key)
	s.keysDirty = true
	if err := s.saveLocked(); err != nil {
		delete(s.records, record.Key)
		s.keysDirty = true
		return false, err
	}
	return true, nil
}

// RecordCompletion reconciles an assigned job by GitHub's actual runner ID.
// GitHub does not bind a JIT runner to the queued job that caused its creation,
// so the event job key alone is not a safe deletion target. An event without a
// runner ID records a deferred tombstone which is reclaimed only by TTL.
func (s *FileStore) RecordCompletion(_ context.Context, completion Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	before := make(recordUndo)
	beforeDirty := s.keysDirty
	matchedKey, staleRunnerIdentity := s.reconcileRunnerCompletion(completion, now, before)
	s.reconcileEventCompletion(completion, matchedKey, now, before)
	if err := s.saveLocked(); err != nil {
		s.rollback(before)
		s.keysDirty = beforeDirty
		return err
	}
	if staleRunnerIdentity != "" {
		delete(s.runnerKeys, staleRunnerIdentity)
	}
	return nil
}

type recordUndo map[string]*Record

func (s *FileStore) remember(before recordUndo, key string) {
	if _, recorded := before[key]; recorded {
		return
	}
	record, found := s.records[key]
	if !found {
		before[key] = nil
		return
	}
	copy := clone(record)
	before[key] = &copy
}

func (s *FileStore) reconcileRunnerCompletion(completion Record, now time.Time, before recordUndo) (string, string) {
	runnerID := completion.GitHubRunnerID
	if runnerID == 0 {
		return "", ""
	}
	runnerIdentity := runnerIdentityKey(completion.Owner, completion.Repository, runnerID)
	if key, found := s.runnerKeys[runnerIdentity]; found {
		if record, valid := s.records[key]; valid && completionMatches(record, completion) {
			s.remember(before, key)
			if record.ClaimedWork != WorkDelete {
				record.Status = StatusCompleted
				record.NextAttemptAt = time.Time{}
			}
			record.DeferDeletion = false
			record.UpdatedAt = now
			s.records[key] = record
			s.markDirty(key)
			return key, ""
		}
		s.addCompletionMarker(completion, now, before)
		return "", runnerIdentity
	}
	s.addCompletionMarker(completion, now, before)
	return "", ""
}

func completionMatches(record, completion Record) bool {
	return record.Status != StatusDeleted && record.GitHubRunnerOwned &&
		record.GitHubRunnerID == completion.GitHubRunnerID && strings.EqualFold(record.Owner, completion.Owner) &&
		strings.EqualFold(record.Repository, completion.Repository)
}

func (s *FileStore) addCompletionMarker(completion Record, now time.Time, before recordUndo) {
	marker := clone(completion)
	marker.Key = completionMarkerKey(completion.Owner, completion.Repository, completion.GitHubRunnerID)
	marker.Status = StatusCompleted
	marker.GitHubRunnerOwned = false
	marker.DeferDeletion = true
	marker.ClaimedWork = ""
	marker.CreatedAt = now
	marker.UpdatedAt = now
	s.remember(before, marker.Key)
	s.records[marker.Key] = marker
	s.markDirty(marker.Key)
	s.keysDirty = true
}

func (s *FileStore) reconcileEventCompletion(completion Record, matchedKey string, now time.Time, before recordUndo) {
	if matchedKey == completion.Key {
		return
	}
	existing, found := s.records[completion.Key]
	if !found {
		s.remember(before, completion.Key)
		completion.GitHubRunnerID = 0
		completion.Status = StatusCompleted
		completion.DeferDeletion = true
		completion.ClaimedWork = ""
		completion.CreatedAt = now
		completion.UpdatedAt = now
		s.records[completion.Key] = clone(completion)
		s.markDirty(completion.Key)
		s.keysDirty = true
		return
	}
	if completion.GitHubRunnerID != 0 || existing.Status == StatusDeleted || existing.Status == StatusOrphaned {
		return
	}
	s.remember(before, completion.Key)
	existing.Status = StatusCompleted
	existing.DeferDeletion = true
	existing.NextAttemptAt = completion.NextAttemptAt
	existing.UpdatedAt = now
	s.records[completion.Key] = existing
	s.markDirty(completion.Key)
}

func (s *FileStore) rollback(before recordUndo) {
	for key, record := range before {
		if record == nil {
			delete(s.records, key)
			continue
		}
		s.records[key] = clone(*record)
	}
}

func (s *FileStore) Get(_ context.Context, key string) (Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[key]
	return clone(record), ok, nil
}

// OwnsRunner reports whether the repository runner ID was durably issued by
// this controller.
func (s *FileStore) OwnsRunner(_ context.Context, owner, repository string, runnerID int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key, found := s.runnerKeys[runnerIdentityKey(owner, repository, runnerID)]
	if !found {
		return false, nil
	}
	record, found := s.records[key]
	return found && record.GitHubRunnerOwned && record.GitHubRunnerID == runnerID, nil
}

// SetMaxLiveRunners configures the atomic provisioning admission ceiling.
func (s *FileStore) SetMaxLiveRunners(limit int) error {
	if limit <= 0 {
		return fmt.Errorf("maximum live runners must be positive")
	}
	s.mu.Lock()
	s.maxLiveRunners = limit
	s.mu.Unlock()
	return nil
}

func (s *FileStore) ClaimNext(ctx context.Context, now time.Time, maxAttempts int) (*Record, WorkKind, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}

	keys := s.orderedKeysLocked()
	liveRunners := s.liveRunnerCountLocked()

	for _, key := range keys {
		record := s.records[key]
		before := clone(record)
		if record.NextAttemptAt.After(now) {
			continue
		}
		if record.Status == StatusPending && record.Attempts >= maxAttempts {
			record.Status = StatusFailed
			record.LastError = "provisioning retry budget exhausted"
			record.UpdatedAt = now.UTC()
			s.records[key] = record
			s.markDirty(key)
			if err := s.saveLocked(); err != nil {
				s.records[key] = before
				return nil, "", err
			}
			continue
		}
		if record.Status == StatusCompleted && !record.DeferDeletion && record.InstanceID != "" && record.DeleteAttempts >= maxAttempts {
			record.Status = StatusOrphaned
			record.LastError = "deletion retry budget exhausted; operator cleanup required"
			record.UpdatedAt = now.UTC()
			s.records[key] = record
			s.markDirty(key)
			if err := s.saveLocked(); err != nil {
				s.records[key] = before
				return nil, "", err
			}
			continue
		}
		var kind WorkKind
		switch {
		case record.ClaimedWork == "" && record.Status == StatusPending && record.Attempts < maxAttempts && liveRunners < s.maxLiveRunners:
			record.Status = StatusProvisioning
			record.Attempts++
			kind = WorkProvision
		case record.ClaimedWork == "" && record.Status == StatusCompleted && !record.DeferDeletion && record.InstanceID != "" && record.DeleteAttempts < maxAttempts:
			record.Status = StatusDeleting
			record.DeleteAttempts++
			kind = WorkDelete
		default:
			continue
		}
		record.UpdatedAt = now.UTC()
		record.NextAttemptAt = time.Time{}
		record.ClaimedWork = kind
		s.records[key] = record
		s.markDirty(key)
		if err := s.saveLocked(); err != nil {
			s.records[key] = before
			return nil, "", err
		}
		copy := clone(record)
		return &copy, kind, nil
	}
	return nil, "", nil
}

func (s *FileStore) liveRunnerCountLocked() int {
	count := 0
	for _, record := range s.records {
		if record.Status == StatusDeleted || (record.Status == StatusOrphaned && record.InstanceID == "") {
			continue
		}
		if record.InstanceID != "" || (record.GitHubRunnerOwned && record.GitHubRunnerID != 0) ||
			record.ClaimedWork == WorkProvision || record.Status == StatusProvisioning {
			count++
		}
	}
	return count
}

// ReleaseClaim returns work that was claimed but not started to the queue
// without consuming retry budget. Completion remains authoritative if it
// arrived while the claim was held.
func (s *FileStore) ReleaseClaim(_ context.Context, key string, kind WorkKind) error {
	return s.update(key, func(record *Record) {
		if record.Status == StatusDeleted || record.Status == StatusOrphaned || record.ClaimedWork != kind {
			return
		}
		if kind != WorkProvision && kind != WorkDelete {
			return
		}
		record.ClaimedWork = ""
		record.NextAttemptAt = time.Time{}
		switch kind {
		case WorkProvision:
			releaseProvisionClaim(record)
		case WorkDelete:
			releaseDeleteClaim(record)
		}
	})
}

// DeferRateLimitedWork atomically refunds a claimed lifecycle attempt while
// bounding consecutive throttle responses with a separate terminal budget.
func (s *FileStore) DeferRateLimitedWork(_ context.Context, key string, kind WorkKind, message string, retryAt time.Time, maxAttempts int) error {
	return s.update(key, func(record *Record) {
		if record.Status == StatusDeleted || record.Status == StatusOrphaned || record.ClaimedWork != kind {
			return
		}
		record.ClaimedWork = ""
		switch kind {
		case WorkProvision:
			releaseProvisionClaim(record)
		case WorkDelete:
			releaseDeleteClaim(record)
		}
		if kind == WorkProvision && record.Status == StatusCompleted {
			record.NextAttemptAt = time.Time{}
			return
		}
		record.ThrottleFailures++
		record.LastError = message
		if record.ThrottleFailures >= maxAttempts {
			record.Status = StatusOrphaned
			record.NextAttemptAt = time.Time{}
			return
		}
		record.NextAttemptAt = retryAt.UTC()
	})
}

func releaseProvisionClaim(record *Record) {
	if record.Attempts > 0 {
		record.Attempts--
	}
	if record.Status == StatusProvisioning {
		record.Status = StatusPending
	}
}

func releaseDeleteClaim(record *Record) {
	if record.DeleteAttempts > 0 {
		record.DeleteAttempts--
	}
	if record.Status == StatusDeleting {
		record.Status = StatusCompleted
	}
}

func (s *FileStore) MarkJITCreated(_ context.Context, key string, runnerID int64, runnerName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[key]
	if !ok {
		return fmt.Errorf("state record %q not found", key)
	}
	if record.Status == StatusDeleted || record.Status == StatusOrphaned {
		return nil
	}
	before := clone(record)
	beforeDirty := s.keysDirty
	now := time.Now().UTC()
	record.GitHubRunnerID = runnerID
	record.GitHubRunnerOwned = true
	record.RunnerName = runnerName
	record.UpdatedAt = now
	markerKey := completionMarkerKey(record.Owner, record.Repository, runnerID)
	var markerBefore *Record
	if marker, found := s.records[markerKey]; found && marker.Status != StatusDeleted {
		copy := clone(marker)
		markerBefore = &copy
		record.Status = StatusCompleted
		record.DeferDeletion = false
		record.NextAttemptAt = time.Time{}
		marker.Status = StatusDeleted
		marker.DeferDeletion = false
		marker.UpdatedAt = now
		s.records[markerKey] = marker
		s.markDirty(markerKey)
	}
	s.records[key] = record
	s.markDirty(key)
	if err := s.saveLocked(); err != nil {
		s.records[key] = before
		if markerBefore != nil {
			s.records[markerKey] = clone(*markerBefore)
		}
		s.keysDirty = beforeDirty
		return err
	}
	s.runnerKeys[runnerIdentityKey(record.Owner, record.Repository, runnerID)] = key
	return nil
}

func (s *FileStore) ClearJIT(_ context.Context, key string) error {
	return s.update(key, func(record *Record) {
		if record.Status == StatusDeleted {
			return
		}
		record.GitHubRunnerID = 0
		record.GitHubRunnerOwned = false
	})
}

func (s *FileStore) MarkProvisioned(_ context.Context, key, instanceID string, runnerID int64, runnerName string) error {
	return s.update(key, func(record *Record) {
		if record.Status == StatusDeleted || record.Status == StatusOrphaned {
			return
		}
		record.InstanceID = instanceID
		record.DropletID = 0
		record.GitHubRunnerID = runnerID
		record.GitHubRunnerOwned = true
		record.RunnerName = runnerName
		record.LastError = ""
		record.ThrottleFailures = 0
		if record.ProvisionedAt.IsZero() {
			record.ProvisionedAt = time.Now().UTC()
		}
		record.ClaimedWork = ""
		record.MissingSince = time.Time{}
		record.MissingChecks = 0
		if record.Status != StatusCompleted {
			record.Status = StatusProvisioned
		}
	})
}

// ObserveRunnerMissing requires repeated observations across a settle window
// before provider absence is treated as authoritative.
func (s *FileStore) ObserveRunnerMissing(_ context.Context, key string, now time.Time, settle time.Duration, confirmations int) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[key]
	if !ok {
		return false, fmt.Errorf("state record %q not found", key)
	}
	if record.Status != StatusProvisioned {
		return false, nil
	}
	before := clone(record)
	if record.MissingSince.IsZero() {
		record.MissingSince = now.UTC()
		record.MissingChecks = 1
	} else {
		record.MissingChecks++
	}
	record.UpdatedAt = now.UTC()
	s.records[key] = record
	s.markDirty(key)
	if err := s.saveLocked(); err != nil {
		s.records[key] = before
		return false, err
	}
	return record.MissingChecks >= confirmations && !record.MissingSince.Add(settle).After(now), nil
}

// ObserveGitHubRunnerMissing debounces GitHub absence independently from
// provider inventory absence.
func (s *FileStore) ObserveGitHubRunnerMissing(_ context.Context, key string, now time.Time, settle time.Duration, confirmations int) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[key]
	if !ok {
		return false, fmt.Errorf("state record %q not found", key)
	}
	if record.Status != StatusProvisioned {
		return false, nil
	}
	before := clone(record)
	if record.GitHubMissingSince.IsZero() {
		record.GitHubMissingSince = now.UTC()
		record.GitHubMissingChecks = 1
	} else {
		record.GitHubMissingChecks++
	}
	record.UpdatedAt = now.UTC()
	s.records[key] = record
	s.markDirty(key)
	if err := s.saveLocked(); err != nil {
		s.records[key] = before
		return false, err
	}
	return record.GitHubMissingChecks >= confirmations && !record.GitHubMissingSince.Add(settle).After(now), nil
}

func (s *FileStore) ClearProviderMissing(_ context.Context, key string) error {
	return s.update(key, func(record *Record) {
		record.MissingSince = time.Time{}
		record.MissingChecks = 0
	})
}

func (s *FileStore) ClearGitHubMissing(_ context.Context, key string) error {
	return s.update(key, func(record *Record) {
		record.GitHubMissingSince = time.Time{}
		record.GitHubMissingChecks = 0
	})
}

// RecordRunnerSeen durably records successful GitHub registration and clears a
// partial sequence of provider misses.
func (s *FileStore) RecordRunnerSeen(_ context.Context, key string) error {
	return s.update(key, func(record *Record) {
		if record.Status == StatusProvisioned {
			if record.RegisteredAt.IsZero() {
				record.RegisteredAt = time.Now().UTC()
			}
			record.ReconciledAt = time.Now().UTC()
			record.MissingSince = time.Time{}
			record.MissingChecks = 0
			record.GitHubMissingSince = time.Time{}
			record.GitHubMissingChecks = 0
		}
	})
}

// ClearRunnerMissing resets provider-liveness debounce state without claiming
// that GitHub has observed the JIT runner online.
func (s *FileStore) ClearRunnerMissing(_ context.Context, key string) error {
	return s.update(key, func(record *Record) {
		if record.Status == StatusProvisioned {
			record.ReconciledAt = time.Now().UTC()
			record.MissingSince = time.Time{}
			record.MissingChecks = 0
			record.GitHubMissingSince = time.Time{}
			record.GitHubMissingChecks = 0
			record.DeleteAttempts = 0
			record.ReconcileFailures = 0
			record.ThrottleFailures = 0
			record.LastError = ""
			record.NextAttemptAt = time.Time{}
		}
	})
}

// DeferReconciliation persists reaper backoff. Persistent client-policy
// failures and consecutive throttles have independent terminal budgets;
// infrastructure failures remain retryable.
func (s *FileStore) DeferReconciliation(_ context.Context, key, message string, retryAt time.Time, maxAttempts int, countFailure, countThrottle bool) error {
	return s.update(key, func(record *Record) {
		if record.Status == StatusDeleted || record.Status == StatusOrphaned {
			return
		}
		if countFailure {
			record.ReconcileFailures++
			if record.ReconcileFailures >= maxAttempts {
				record.Status = StatusOrphaned
				record.LastError = message
				record.NextAttemptAt = time.Time{}
				return
			}
		}
		if countThrottle {
			record.ThrottleFailures++
			if record.ThrottleFailures >= maxAttempts {
				record.Status = StatusOrphaned
				record.LastError = message
				record.NextAttemptAt = time.Time{}
				return
			}
		} else {
			record.ThrottleFailures = 0
		}
		record.LastError = message
		record.NextAttemptAt = retryAt.UTC()
	})
}

// DeferRateLimitedCleanup refunds the attempt reserved by BeginCleanupAttempt
// because throttling is not a lifecycle failure.
func (s *FileStore) DeferRateLimitedCleanup(_ context.Context, key, message string, retryAt time.Time, maxAttempts int, countThrottle bool) error {
	return s.update(key, func(record *Record) {
		if record.Status == StatusDeleted || record.Status == StatusOrphaned {
			return
		}
		if record.DeleteAttempts > 0 {
			record.DeleteAttempts--
		}
		record.LastError = message
		if countThrottle {
			record.ThrottleFailures++
			if record.ThrottleFailures >= maxAttempts {
				record.Status = StatusOrphaned
				record.NextAttemptAt = time.Time{}
				return
			}
		} else {
			record.ThrottleFailures = 0
		}
		record.NextAttemptAt = retryAt.UTC()
	})
}

// DeferOrphanCleanup records backoff while keeping an orphan terminal and
// admission-safe.
func (s *FileStore) DeferOrphanCleanup(_ context.Context, key, message string, retryAt time.Time) error {
	return s.update(key, func(record *Record) {
		if record.Status != StatusOrphaned {
			return
		}
		record.ReconcileFailures++
		record.LastError = message
		record.NextAttemptAt = retryAt.UTC()
	})
}

func (s *FileStore) MarkProvisionFailed(_ context.Context, key, message string, retryAt time.Time, maxAttempts int) error {
	return s.update(key, func(record *Record) {
		if record.Status == StatusDeleted || record.Status == StatusOrphaned {
			return
		}
		record.LastError = message
		record.ThrottleFailures = 0
		record.ClaimedWork = ""
		if record.Status == StatusCompleted {
			return
		}
		if record.Attempts >= maxAttempts {
			record.Status = StatusFailed
			return
		}
		record.Status = StatusPending
		record.NextAttemptAt = retryAt.UTC()
	})
}

func (s *FileStore) MarkCompleted(_ context.Context, key string) error {
	return s.update(key, func(record *Record) {
		if record.Status != StatusDeleted && record.Status != StatusOrphaned {
			record.Status = StatusCompleted
			record.NextAttemptAt = time.Time{}
		}
	})
}

// ScheduleDeletion re-enables deletion after retry exhaustion. It is used only
// by TTL reconciliation, which has independently determined the runner is stale.
func (s *FileStore) ScheduleDeletion(_ context.Context, key string) error {
	return s.update(key, func(record *Record) {
		if record.Status == StatusDeleted || record.Status == StatusOrphaned || record.ClaimedWork != "" {
			return
		}
		record.Status = StatusCompleted
		record.DeferDeletion = false
		record.NextAttemptAt = time.Time{}
	})
}

func (s *FileStore) MarkDeleted(_ context.Context, key string) error {
	return s.update(key, func(record *Record) {
		record.Status = StatusDeleted
		record.ClaimedWork = ""
		record.DeferDeletion = false
		record.LastError = ""
		record.NextAttemptAt = time.Time{}
	})
}

// BeginCleanupAttempt atomically consumes retry budget for reaper-owned cleanup.
// It prevents permanently failing untracked resources from being retried forever.
func (s *FileStore) BeginCleanupAttempt(_ context.Context, key string, maxAttempts int) (int, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[key]
	if !ok {
		return 0, false, fmt.Errorf("state record %q not found", key)
	}
	if record.Status == StatusDeleted || record.Status == StatusOrphaned || record.ClaimedWork != "" {
		return record.DeleteAttempts, false, nil
	}
	if !record.NextAttemptAt.IsZero() && record.NextAttemptAt.After(time.Now()) {
		return record.DeleteAttempts, false, nil
	}
	before := clone(record)
	if record.DeleteAttempts >= maxAttempts {
		record.Status = StatusOrphaned
		record.LastError = "cleanup retry budget exhausted; operator cleanup required"
		record.NextAttemptAt = time.Time{}
		record.UpdatedAt = time.Now().UTC()
	} else {
		record.DeleteAttempts++
		record.UpdatedAt = time.Now().UTC()
	}
	s.records[key] = record
	s.markDirty(key)
	if err := s.saveLocked(); err != nil {
		s.records[key] = before
		return before.DeleteAttempts, false, err
	}
	return record.DeleteAttempts, record.Status != StatusOrphaned, nil
}

func (s *FileStore) MarkDeleteFailed(_ context.Context, key, message string, retryAt time.Time, maxAttempts int) error {
	return s.update(key, func(record *Record) {
		if record.Status == StatusDeleted || record.Status == StatusOrphaned {
			return
		}
		record.LastError = message
		record.ThrottleFailures = 0
		record.ClaimedWork = ""
		if record.DeleteAttempts >= maxAttempts {
			record.Status = StatusOrphaned
			return
		}
		record.Status = StatusCompleted
		record.NextAttemptAt = retryAt.UTC()
	})
}

// MarkOrphaned records an operator-actionable terminal resource without
// retrying unsafe provider mutation. A live instance still consumes admission.
func (s *FileStore) MarkOrphaned(_ context.Context, key, message string) error {
	return s.update(key, func(record *Record) {
		record.Status = StatusOrphaned
		record.ClaimedWork = ""
		record.LastError = message
		record.NextAttemptAt = time.Time{}
	})
}

// ListProvisioned returns active records for provider liveness reconciliation.
func (s *FileStore) ListProvisioned(_ context.Context) ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result []Record
	for _, record := range s.records {
		if record.Status == StatusProvisioned {
			result = append(result, clone(record))
		}
	}
	return result, nil
}

// ListOrphaned returns terminal records that may still need GitHub identity
// cleanup before retention pruning.
func (s *FileStore) ListOrphaned(_ context.Context) ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result []Record
	for _, record := range s.records {
		if record.Status == StatusOrphaned {
			result = append(result, clone(record))
		}
	}
	return result, nil
}

// KnownInstanceIDs returns every provider resource still represented by a
// non-deleted lifecycle record. Provider sweeps use this to reclaim resources
// stranded by a lost or replaced state file without touching live work.
func (s *FileStore) KnownInstanceIDs(_ context.Context) (map[string]struct{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make(map[string]struct{})
	for _, record := range s.records {
		if record.InstanceID != "" && record.Status != StatusDeleted && record.Status != StatusOrphaned {
			result[record.InstanceID] = struct{}{}
		}
	}
	return result, nil
}

// ReleaseSweptOrphans clears admission-bearing instance identities only after
// a successful provider sweep has confirmed every old untracked resource is
// absent or deleted.
func (s *FileStore) ReleaseSweptOrphans(_ context.Context, provider string, cutoff time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	before := make(map[string]Record)
	for key, record := range s.records {
		instanceCreatedAt := record.ProvisionedAt
		if instanceCreatedAt.IsZero() {
			instanceCreatedAt = record.CreatedAt
		}
		if record.Status != StatusOrphaned || record.Provider != provider || record.InstanceID == "" || !instanceCreatedAt.Before(cutoff) {
			continue
		}
		before[key] = clone(record)
		record.InstanceID = ""
		record.UpdatedAt = time.Now().UTC()
		s.records[key] = record
		s.markDirty(key)
	}
	if len(before) == 0 {
		return 0, nil
	}
	if err := s.saveLocked(); err != nil {
		for key, record := range before {
			s.records[key] = record
		}
		return 0, err
	}
	return len(before), nil
}

// RequeueMissingRunner clears stale provider and JIT identity after liveness
// reconciliation proves the instance no longer exists.
func (s *FileStore) RequeueMissingRunner(_ context.Context, key string, maxAttempts int) error {
	return s.update(key, func(record *Record) {
		if record.Status != StatusProvisioned {
			return
		}
		record.InstanceID = ""
		record.ProvisionedAt = time.Time{}
		record.RegisteredAt = time.Time{}
		record.ReconciledAt = time.Time{}
		record.ProvisionEpoch++
		record.GitHubRunnerID = 0
		record.GitHubRunnerOwned = false
		record.RunnerName = ""
		record.MissingSince = time.Time{}
		record.MissingChecks = 0
		record.GitHubMissingSince = time.Time{}
		record.GitHubMissingChecks = 0
		record.LastError = "provider instance disappeared; provisioning replacement"
		if record.Attempts >= maxAttempts {
			record.Status = StatusFailed
			return
		}
		record.Status = StatusPending
		record.NextAttemptAt = time.Time{}
	})
}

func (s *FileStore) ListExpired(_ context.Context, cutoff time.Time) ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result []Record
	now := time.Now()
	for _, record := range s.records {
		if record.ClaimedWork != "" {
			continue
		}
		staleProvisioned := record.Status == StatusProvisioned && !record.ProvisionedAt.IsZero() && record.ProvisionedAt.Before(cutoff)
		retryDue := record.NextAttemptAt.IsZero() || !record.NextAttemptAt.After(now)
		staleTerminal := retryDue && (record.Status == StatusFailed || record.Status == StatusCompleted) && record.CreatedAt.Before(cutoff)
		failedCompletionRace := retryDue && record.Status == StatusCompleted && !record.DeferDeletion && record.InstanceID == "" && record.LastError != ""
		failedProvision := retryDue && record.Status == StatusFailed && record.InstanceID == ""
		// A zero retry timestamp means due now everywhere else in lifecycle
		// state. Keep that meaning for deferred cancellations too: interrupted
		// work recovery may clear the timestamp while preserving DeferDeletion.
		deferredCancellationDue := record.Status == StatusCompleted && record.DeferDeletion && retryDue
		if staleProvisioned || staleTerminal || failedCompletionRace || failedProvision || deferredCancellationDue {
			result = append(result, clone(record))
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.Before(result[j].CreatedAt) })
	return result, nil
}

// PruneDeleted compacts records after the configured deduplication window.
func (s *FileStore) PruneDeleted(_ context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().UTC().Add(-s.deletedRetention)
	removed := make(map[string]Record)
	for key, record := range s.records {
		prunableOrphan := record.Status == StatusOrphaned && record.InstanceID == "" && record.GitHubRunnerID == 0
		if (record.Status == StatusDeleted || prunableOrphan) && record.UpdatedAt.Before(cutoff) {
			removed[key] = record
			delete(s.records, key)
			s.markDirty(key)
		}
	}
	if len(removed) == 0 {
		return 0, nil
	}
	s.keysDirty = true
	if err := s.saveLocked(); err != nil {
		for key, record := range removed {
			s.records[key] = record
		}
		s.keysDirty = true
		return 0, err
	}
	return len(removed), nil
}

func (s *FileStore) orderedKeysLocked() []string {
	if s.keysDirty {
		s.orderedKeys = s.orderedKeys[:0]
		for key := range s.records {
			s.orderedKeys = append(s.orderedKeys, key)
		}
		sort.Strings(s.orderedKeys)
		s.keysDirty = false
	}
	return s.orderedKeys
}

func completionMarkerKey(owner, repository string, runnerID int64) string {
	return completionMarkerPrefix + runnerIdentityKey(owner, repository, runnerID)
}

func runnerIdentityKey(owner, repository string, runnerID int64) string {
	return strings.ToLower(owner+"/"+repository) + "#" + strconv.FormatInt(runnerID, 10)
}

func cloneRecords(records map[string]Record) map[string]Record {
	copy := make(map[string]Record, len(records))
	for key, record := range records {
		copy[key] = clone(record)
	}
	return copy
}

func (s *FileStore) update(key string, mutate func(*Record)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[key]
	if !ok {
		return fmt.Errorf("state record %q not found", key)
	}
	before := clone(record)
	mutate(&record)
	record.UpdatedAt = time.Now().UTC()
	s.records[key] = record
	s.markDirty(key)
	if err := s.saveLocked(); err != nil {
		s.records[key] = before
		return err
	}
	return nil
}

func (s *FileStore) saveLocked() error {
	if s.closed {
		return errors.New("state store is closed")
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	changes := make(map[string]*Record, len(s.dirtyKeys))
	for key := range s.dirtyKeys {
		record, found := s.records[key]
		persisted, wasPersisted := s.persisted[key]
		switch {
		case !found && wasPersisted:
			changes[key] = nil
		case found && (!wasPersisted || !reflect.DeepEqual(record, persisted)):
			copy := clone(record)
			changes[key] = &copy
		}
	}
	if len(changes) == 0 {
		clear(s.dirtyKeys)
		return nil
	}
	data, err := json.Marshal(journalEntry{Records: changes})
	if err != nil {
		return fmt.Errorf("encode state journal: %w", err)
	}
	data = append(data, '\n')
	_, statErr := os.Stat(s.journalPath)
	journalMissing := errors.Is(statErr, os.ErrNotExist)
	if statErr != nil && !journalMissing {
		return fmt.Errorf("stat state journal before append: %w", statErr)
	}
	journal, err := os.OpenFile(s.journalPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open state journal: %w", err)
	}
	if err := journal.Chmod(0o600); err != nil {
		_ = journal.Close()
		return fmt.Errorf("secure state journal permissions: %w", err)
	}
	if journalMissing {
		if err := syncDirectory(dir); err != nil {
			_ = journal.Close()
			return err
		}
	}
	info, err := journal.Stat()
	if err != nil {
		_ = journal.Close()
		return fmt.Errorf("stat state journal: %w", err)
	}
	written, err := journal.Write(data)
	if err != nil || written != len(data) {
		writeErr := err
		if writeErr == nil {
			writeErr = io.ErrShortWrite
		}
		rollbackErr := rollbackJournal(journal, info.Size())
		_ = journal.Close()
		return errors.Join(fmt.Errorf("append state journal: %w", writeErr), rollbackErr)
	}
	if err := journal.Sync(); err != nil {
		rollbackErr := rollbackJournal(journal, info.Size())
		_ = journal.Close()
		return errors.Join(fmt.Errorf("sync state journal: %w", err), rollbackErr)
	}
	if err := journal.Close(); err != nil {
		// Sync established durability; a close error must not make callers
		// roll back memory while the journal retains the committed mutation.
		log.Printf("WARN: close synced lifecycle state journal: %v", err)
	}
	for key, record := range changes {
		if record == nil {
			delete(s.persisted, key)
		} else {
			s.persisted[key] = clone(*record)
		}
	}
	clear(s.dirtyKeys)
	s.journalEntries++
	s.journalBytes += int64(len(data))
	if s.journalEntries >= 512 || s.journalBytes >= 4*1024*1024 {
		if err := s.compactLocked(); err != nil {
			// The journal append above is already durable. Compaction is an
			// optimization and must not make the committed mutation look failed.
			log.Printf("WARN: compact lifecycle state journal: %v", err)
		}
	}
	return nil
}

func (s *FileStore) markDirty(key string) {
	s.dirtyKeys[key] = struct{}{}
}

func (s *FileStore) rebuildRunnerIndexLocked() {
	clear(s.runnerKeys)
	for key, record := range s.records {
		identity := runnerIdentityKey(record.Owner, record.Repository, record.GitHubRunnerID)
		if record.Status != StatusDeleted && record.GitHubRunnerOwned && record.GitHubRunnerID != 0 && key != completionMarkerKey(record.Owner, record.Repository, record.GitHubRunnerID) {
			s.runnerKeys[identity] = key
		}
	}
}

func rollbackJournal(journal *os.File, size int64) error {
	if err := journal.Truncate(size); err != nil {
		return fmt.Errorf("roll back partial state journal entry: %w", err)
	}
	if err := journal.Sync(); err != nil {
		return fmt.Errorf("sync rolled-back state journal: %w", err)
	}
	return nil
}

func (s *FileStore) loadJournal() error {
	data, err := os.ReadFile(s.journalPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read state journal: %w", err)
	}
	if err := os.Chmod(s.journalPath, 0o600); err != nil {
		return fmt.Errorf("secure state journal permissions: %w", err)
	}
	lines := bytes.Split(data, []byte{'\n'})
	validBytes := len(data)
	for index, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var entry journalEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			if index == len(lines)-1 && !bytes.HasSuffix(data, []byte{'\n'}) {
				validBytes -= len(line)
				if repairErr := s.repairJournalTail(int64(validBytes)); repairErr != nil {
					return errors.Join(fmt.Errorf("decode torn state journal tail: %w", err), repairErr)
				}
				log.Printf("WARN: discarded torn final lifecycle journal entry")
				break
			}
			return fmt.Errorf("decode state journal entry %d: %w", index+1, err)
		}
		for key, record := range entry.Records {
			if record == nil {
				delete(s.records, key)
			} else {
				s.records[key] = clone(*record)
			}
		}
		s.journalEntries++
	}
	if validBytes != 0 && validBytes == len(data) && !bytes.HasSuffix(data, []byte{'\n'}) {
		if err := s.appendJournalDelimiter(); err != nil {
			return err
		}
		validBytes++
	}
	s.journalBytes = int64(validBytes)
	return nil
}

func (s *FileStore) appendJournalDelimiter() error {
	journal, err := os.OpenFile(s.journalPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open state journal delimiter: %w", err)
	}
	if _, err := journal.Write([]byte{'\n'}); err != nil {
		_ = journal.Close()
		return fmt.Errorf("append state journal delimiter: %w", err)
	}
	if err := journal.Sync(); err != nil {
		_ = journal.Close()
		return fmt.Errorf("sync state journal delimiter: %w", err)
	}
	if err := journal.Close(); err != nil {
		return fmt.Errorf("close state journal delimiter: %w", err)
	}
	return syncDirectory(filepath.Dir(s.journalPath))
}

func (s *FileStore) repairJournalTail(size int64) error {
	journal, err := os.OpenFile(s.journalPath, os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open torn state journal: %w", err)
	}
	if err := rollbackJournal(journal, size); err != nil {
		_ = journal.Close()
		return err
	}
	if err := journal.Close(); err != nil {
		return fmt.Errorf("close repaired state journal: %w", err)
	}
	return syncDirectory(filepath.Dir(s.journalPath))
}

func (s *FileStore) compactLocked() error {
	if err := s.writeSnapshotLocked(); err != nil {
		return err
	}
	journal, err := os.OpenFile(s.journalPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("truncate state journal: %w", err)
	}
	if err := journal.Sync(); err != nil {
		_ = journal.Close()
		return fmt.Errorf("sync compacted state journal: %w", err)
	}
	if err := journal.Close(); err != nil {
		return fmt.Errorf("close compacted state journal: %w", err)
	}
	s.journalEntries = 0
	s.journalBytes = 0
	return nil
}

func (s *FileStore) writeSnapshotLocked() error {
	dir := filepath.Dir(s.path)
	data, err := json.MarshalIndent(diskState{Records: s.records}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state snapshot: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".runner-state-*")
	if err != nil {
		return fmt.Errorf("create temporary state snapshot: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("replace state snapshot: %w", err)
	}
	return syncDirectory(dir)
}

func syncDirectory(dir string) error {
	directory, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open state directory for sync: %w", err)
	}
	defer func() { _ = directory.Close() }()
	if err := directory.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) {
		return fmt.Errorf("sync state directory: %w", err)
	}
	return nil
}

func clone(record Record) Record {
	record.Labels = append([]string(nil), record.Labels...)
	return record
}
