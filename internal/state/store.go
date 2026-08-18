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
	DropletID      int       `json:"droplet_id,omitempty"`
	Status         Status    `json:"status"`
	ClaimedWork    WorkKind  `json:"claimed_work,omitempty"`
	DeferDeletion  bool      `json:"defer_deletion,omitempty"`
	Attempts       int       `json:"attempts"`
	ProvisionEpoch int       `json:"provision_epoch,omitempty"`
	DeleteAttempts int       `json:"delete_attempts"`
	LastError      string    `json:"last_error,omitempty"`
	NextAttemptAt  time.Time `json:"next_attempt_at,omitempty"`
	MissingSince   time.Time `json:"missing_since,omitempty"`
	MissingChecks  int       `json:"missing_checks,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	ProvisionedAt  time.Time `json:"provisioned_at,omitempty"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// Store defines the durable lifecycle operations used by the controller.
type Store interface {
	Create(context.Context, Record) (bool, error)
	RecordCompletion(context.Context, Record) error
	Get(context.Context, string) (Record, bool, error)
	ClaimNext(context.Context, time.Time, int) (*Record, WorkKind, error)
	SetMaxLiveRunners(int) error
	ReleaseClaim(context.Context, string, WorkKind) error
	MarkJITCreated(context.Context, string, int64, string) error
	ClearJIT(context.Context, string) error
	MarkProvisioned(context.Context, string, string, int64, string) error
	MarkProvisionFailed(context.Context, string, string, time.Time, int) error
	MarkCompleted(context.Context, string) error
	ScheduleDeletion(context.Context, string) error
	MarkDeleted(context.Context, string) error
	MarkDeleteFailed(context.Context, string, string, time.Time, int) error
	MarkOrphaned(context.Context, string, string) error
	ListExpired(context.Context, time.Time) ([]Record, error)
	ListProvisioned(context.Context) ([]Record, error)
	ObserveRunnerMissing(context.Context, string, time.Time, time.Duration, int) (bool, error)
	RecordRunnerSeen(context.Context, string) error
	RequeueMissingRunner(context.Context, string, int) error
	PruneDeleted(context.Context) (int, error)
}

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

	s := &FileStore{
		lockFile: lockFile, path: path, journalPath: path + ".wal", records: make(map[string]Record), persisted: make(map[string]Record),
		dirtyKeys: make(map[string]struct{}), runnerKeys: make(map[string]string),
		deletedRetention: defaultDeletedRetention, keysDirty: true, maxLiveRunners: 20,
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
	data, err := os.ReadFile(path)
	snapshotMissing := errors.Is(err, os.ErrNotExist)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read state file: %w", err)
	}
	if err == nil {
		if err := os.Chmod(path, 0o600); err != nil {
			return nil, fmt.Errorf("secure state file permissions: %w", err)
		}
	}
	if len(data) != 0 {
		var persisted diskState
		if err := json.Unmarshal(data, &persisted); err != nil {
			return nil, fmt.Errorf("decode state file: %w", err)
		}
		if persisted.Records != nil {
			s.records = persisted.Records
		}
	}
	if err := s.loadJournal(); err != nil {
		return nil, err
	}
	s.persisted = cloneRecords(s.records)

	recovered := false
	for key, record := range s.records {
		if record.Status == StatusDeleted && record.UpdatedAt.Before(time.Now().Add(-s.deletedRetention)) {
			delete(s.records, key)
			recovered = true
			continue
		}
		recordRecovered := false
		if record.ClaimedWork != "" {
			record.ClaimedWork = ""
			recordRecovered = true
		}
		if record.Provider == "" {
			// All state written before provider binding was DigitalOcean-only.
			record.Provider = "digitalocean"
			recordRecovered = true
		}
		if record.Status == StatusProvisioned && record.ProvisionedAt.IsZero() {
			record.ProvisionedAt = record.UpdatedAt
			if record.ProvisionedAt.IsZero() {
				record.ProvisionedAt = record.CreatedAt
			}
			recordRecovered = true
		}
		if record.GitHubRunnerID != 0 && !record.GitHubRunnerOwned && !strings.HasPrefix(key, completionMarkerPrefix) {
			// State predating the ownership field only persisted IDs returned by
			// this controller's GenerateRepoJITConfig call.
			record.GitHubRunnerOwned = true
			recordRecovered = true
		}
		switch record.Status {
		case StatusProvisioning:
			record.Status = StatusPending
			if record.Attempts > 0 {
				record.Attempts--
			}
			recordRecovered = true
		case StatusDeleting:
			record.Status = StatusCompleted
			if record.DeleteAttempts > 0 {
				record.DeleteAttempts--
			}
			recordRecovered = true
		}
		if recordRecovered {
			record.NextAttemptAt = time.Time{}
			record.UpdatedAt = time.Now().UTC()
			s.records[key] = record
			s.markDirty(key)
			recovered = true
		}
		if record.InstanceID == "" && record.DropletID != 0 {
			record.InstanceID = strconv.Itoa(record.DropletID)
			record.DropletID = 0
			record.UpdatedAt = time.Now().UTC()
			s.records[key] = record
			s.markDirty(key)
			recovered = true
		}
	}
	if recovered {
		if err := s.saveLocked(); err != nil {
			return nil, err
		}
	}
	s.rebuildRunnerIndexLocked()
	if s.journalEntries >= 512 || s.journalBytes >= 4*1024*1024 {
		if err := s.compactLocked(); err != nil {
			return nil, err
		}
	} else if snapshotMissing {
		if err := s.writeSnapshotLocked(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Close releases the exclusive writer lock held for this store.
func (s *FileStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lockFile == nil {
		return nil
	}
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
	before := make(map[string]*Record)
	remember := func(key string) {
		if _, recorded := before[key]; recorded {
			return
		}
		if record, found := s.records[key]; found {
			copy := clone(record)
			before[key] = &copy
		} else {
			before[key] = nil
		}
	}
	beforeDirty := s.keysDirty
	runnerID := completion.GitHubRunnerID
	runnerIdentity := runnerIdentityKey(completion.Owner, completion.Repository, runnerID)
	matchedKey := ""
	if runnerID != 0 {
		if key, found := s.runnerKeys[runnerIdentity]; found {
			if record, valid := s.records[key]; valid && record.Status != StatusDeleted && record.Status != StatusOrphaned && record.GitHubRunnerOwned &&
				record.GitHubRunnerID == runnerID && strings.EqualFold(record.Owner, completion.Owner) &&
				strings.EqualFold(record.Repository, completion.Repository) {
				remember(key)
				if record.ClaimedWork != WorkDelete {
					record.Status = StatusCompleted
					record.NextAttemptAt = time.Time{}
				}
				record.DeferDeletion = false
				record.UpdatedAt = now
				s.records[key] = record
				s.markDirty(key)
				matchedKey = key
			} else {
				delete(s.runnerKeys, runnerIdentity)
			}
		}
		if matchedKey == "" {
			marker := clone(completion)
			marker.Key = completionMarkerKey(completion.Owner, completion.Repository, runnerID)
			marker.Status = StatusCompleted
			marker.GitHubRunnerOwned = false
			marker.DeferDeletion = true
			marker.ClaimedWork = ""
			marker.CreatedAt = now
			marker.UpdatedAt = now
			remember(marker.Key)
			s.records[marker.Key] = marker
			s.markDirty(marker.Key)
			s.keysDirty = true
		}
	}

	if matchedKey != completion.Key {
		existing, found := s.records[completion.Key]
		switch {
		case !found:
			remember(completion.Key)
			completion.GitHubRunnerID = 0
			completion.Status = StatusCompleted
			completion.DeferDeletion = true
			completion.ClaimedWork = ""
			completion.CreatedAt = now
			completion.UpdatedAt = now
			s.records[completion.Key] = clone(completion)
			s.markDirty(completion.Key)
			s.keysDirty = true
		case runnerID == 0 && existing.Status != StatusDeleted && existing.Status != StatusOrphaned:
			remember(completion.Key)
			existing.Status = StatusCompleted
			existing.DeferDeletion = true
			existing.NextAttemptAt = completion.NextAttemptAt
			existing.UpdatedAt = now
			s.records[completion.Key] = existing
			s.markDirty(completion.Key)
		}
	}
	if err := s.saveLocked(); err != nil {
		for key, record := range before {
			if record == nil {
				delete(s.records, key)
			} else {
				s.records[key] = clone(*record)
			}
		}
		s.keysDirty = beforeDirty
		return err
	}
	return nil
}

func (s *FileStore) Get(_ context.Context, key string) (Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[key]
	return clone(record), ok, nil
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
			return nil, "", nil
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
			return nil, "", nil
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
		if record.Status == StatusDeleted || record.Status == StatusOrphaned {
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
		record.ClaimedWork = ""
		record.NextAttemptAt = time.Time{}
		switch kind {
		case WorkProvision:
			if record.Attempts > 0 {
				record.Attempts--
			}
			if record.Status == StatusProvisioning {
				record.Status = StatusPending
			}
		case WorkDelete:
			if record.DeleteAttempts > 0 {
				record.DeleteAttempts--
			}
			if record.Status == StatusDeleting {
				record.Status = StatusCompleted
			}
		}
	})
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
		if record.Status == StatusDeleted || record.Status == StatusOrphaned {
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

// RecordRunnerSeen clears a partial sequence of provider misses.
func (s *FileStore) RecordRunnerSeen(_ context.Context, key string) error {
	return s.update(key, func(record *Record) {
		if record.Status == StatusProvisioned {
			record.MissingSince = time.Time{}
			record.MissingChecks = 0
		}
	})
}

func (s *FileStore) MarkProvisionFailed(_ context.Context, key, message string, retryAt time.Time, maxAttempts int) error {
	return s.update(key, func(record *Record) {
		if record.Status == StatusDeleted || record.Status == StatusOrphaned {
			return
		}
		record.LastError = message
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

func (s *FileStore) MarkDeleteFailed(_ context.Context, key, message string, retryAt time.Time, maxAttempts int) error {
	return s.update(key, func(record *Record) {
		if record.Status == StatusDeleted || record.Status == StatusOrphaned {
			return
		}
		record.LastError = message
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
// retrying unsafe deletion or consuming fleet admission capacity.
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

// RequeueMissingRunner clears stale provider and JIT identity after liveness
// reconciliation proves the instance no longer exists.
func (s *FileStore) RequeueMissingRunner(_ context.Context, key string, maxAttempts int) error {
	return s.update(key, func(record *Record) {
		if record.Status != StatusProvisioned {
			return
		}
		record.InstanceID = ""
		record.ProvisionedAt = time.Time{}
		record.ProvisionEpoch++
		record.GitHubRunnerID = 0
		record.GitHubRunnerOwned = false
		record.RunnerName = ""
		record.MissingSince = time.Time{}
		record.MissingChecks = 0
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
	for _, record := range s.records {
		if record.ClaimedWork != "" {
			continue
		}
		staleProvisioned := record.Status == StatusProvisioned && !record.ProvisionedAt.IsZero() && record.ProvisionedAt.Before(cutoff)
		staleTerminal := (record.Status == StatusFailed || record.Status == StatusCompleted) && record.CreatedAt.Before(cutoff)
		failedCompletionRace := record.Status == StatusCompleted && !record.DeferDeletion && record.InstanceID == "" && record.LastError != ""
		failedProvision := record.Status == StatusFailed && record.InstanceID == ""
		deferredCancellationDue := record.Status == StatusCompleted && record.DeferDeletion &&
			!record.NextAttemptAt.IsZero() && !record.NextAttemptAt.After(time.Now())
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
		if record.Status == StatusDeleted && record.UpdatedAt.Before(cutoff) {
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
	journal, err := os.OpenFile(s.journalPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open state journal: %w", err)
	}
	if err := journal.Chmod(0o600); err != nil {
		_ = journal.Close()
		return fmt.Errorf("secure state journal permissions: %w", err)
	}
	if err := syncDirectory(dir); err != nil {
		_ = journal.Close()
		return err
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
		if record.Status != StatusDeleted && record.Status != StatusOrphaned && record.GitHubRunnerOwned && record.GitHubRunnerID != 0 && key != completionMarkerKey(record.Owner, record.Repository, record.GitHubRunnerID) {
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
