package collector

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type ReadinessConfig struct {
	StartedAt      time.Time
	StartupGrace   time.Duration
	MaxRecoveryAge time.Duration
}

type ReadinessSnapshot struct {
	SpoolWritable      bool
	OldestPendingAt    *time.Time
	NewestSuccessfulAt *time.Time
	TerminalLocalError bool
}

type StatusTracker struct {
	mu                 sync.RWMutex
	config             ReadinessConfig
	now                func() time.Time
	spoolWritable      bool
	pending            map[string]time.Time
	newestSuccessful   *time.Time
	terminalLocalError bool
}

func NewStatusTracker(config ReadinessConfig, now func() time.Time) *StatusTracker {
	if now == nil {
		now = time.Now
	}
	return &StatusTracker{config: config, now: now, pending: make(map[string]time.Time)}
}

func (s *StatusTracker) SetSpoolWritable(writable bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.spoolWritable = writable
	s.mu.Unlock()
}

func (s *StatusTracker) RecordPending(objectID string, receivedAt time.Time) {
	if s == nil || objectID == "" || receivedAt.IsZero() {
		return
	}
	s.mu.Lock()
	if current, found := s.pending[objectID]; !found || receivedAt.Before(current) {
		s.pending[objectID] = receivedAt
	}
	s.mu.Unlock()
}

func (s *StatusTracker) RemovePending(objectID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.pending, objectID)
	s.mu.Unlock()
}

func (s *StatusTracker) RecordSuccess(storedAt time.Time) {
	if s == nil || storedAt.IsZero() {
		return
	}
	s.mu.Lock()
	if s.newestSuccessful == nil || storedAt.After(*s.newestSuccessful) {
		s.newestSuccessful = timePointer(storedAt)
	}
	s.mu.Unlock()
}

func (s *StatusTracker) RecordTerminalLocalError() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.terminalLocalError = true
	s.mu.Unlock()
}

func (s *StatusTracker) Snapshot() ReadinessSnapshot {
	if s == nil {
		return ReadinessSnapshot{TerminalLocalError: true}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var oldest *time.Time
	for _, receivedAt := range s.pending {
		if oldest == nil || receivedAt.Before(*oldest) {
			oldest = timePointer(receivedAt)
		}
	}
	var newest *time.Time
	if s.newestSuccessful != nil {
		newest = timePointer(*s.newestSuccessful)
	}
	return ReadinessSnapshot{
		SpoolWritable:      s.spoolWritable,
		OldestPendingAt:    oldest,
		NewestSuccessfulAt: newest,
		TerminalLocalError: s.terminalLocalError,
	}
}

func (s *StatusTracker) Ready() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	config := s.config
	nowFn := s.now
	spoolWritable := s.spoolWritable
	terminal := s.terminalLocalError
	pending := make([]time.Time, 0, len(s.pending))
	for _, receivedAt := range s.pending {
		pending = append(pending, receivedAt)
	}
	var success *time.Time
	if s.newestSuccessful != nil {
		success = timePointer(*s.newestSuccessful)
	}
	s.mu.RUnlock()
	if nowFn == nil || !spoolWritable || terminal || config.StartedAt.IsZero() || config.StartupGrace <= 0 || config.MaxRecoveryAge <= 0 {
		return false
	}
	now := nowFn()
	if now.IsZero() || now.Before(config.StartedAt) {
		return false
	}
	for _, receivedAt := range pending {
		if receivedAt.IsZero() || receivedAt.After(now) || now.Sub(receivedAt) > config.MaxRecoveryAge {
			return false
		}
	}
	if success != nil {
		if success.After(now) || now.Sub(*success) > config.MaxRecoveryAge {
			return false
		}
		return true
	}
	return now.Sub(config.StartedAt) <= config.StartupGrace
}

type workerOps struct {
	now  func() time.Time
	wait func(context.Context, time.Duration) error
}

type Worker struct {
	spool         *Spool
	ledger        *Ledger
	service       *Service
	status        *StatusTracker
	retryInterval time.Duration
	ops           workerOps
}

func NewWorker(spool *Spool, ledger *Ledger, service *Service, status *StatusTracker, retryInterval time.Duration) *Worker {
	return newWorker(spool, ledger, service, status, retryInterval, workerOps{})
}

func newWorker(spool *Spool, ledger *Ledger, service *Service, status *StatusTracker, retryInterval time.Duration, ops workerOps) *Worker {
	if ops.now == nil {
		ops.now = time.Now
	}
	if ops.wait == nil {
		ops.wait = waitForRetry
	}
	return &Worker{spool: spool, ledger: ledger, service: service, status: status, retryInterval: retryInterval, ops: ops}
}

func (w *Worker) Run(ctx context.Context) error {
	if w == nil || w.spool == nil || w.ledger == nil || w.service == nil || w.status == nil || w.retryInterval <= 0 || w.ops.now == nil || w.ops.wait == nil || ctx == nil {
		return errors.New("collector local state failed")
	}
	if _, err := w.spool.CleanupAbandonedPartials(); err != nil {
		return w.terminalFailure()
	}
	if err := w.spool.CheckWritable(); err != nil {
		w.status.SetSpoolWritable(false)
		return w.terminalFailure()
	}
	w.status.SetSpoolWritable(true)

	queue, err := w.reconcileStartup(ctx)
	if err != nil {
		if ctx.Err() != nil && !w.status.Snapshot().TerminalLocalError {
			return nil
		}
		return err
	}
	for {
		if ctx.Err() != nil {
			return nil
		}
		if len(queue) == 0 {
			if err := w.ops.wait(ctx, w.retryInterval); err != nil {
				if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return nil
				}
				return w.terminalFailure()
			}
			queue, err = w.reconcileStartup(ctx)
			if err != nil {
				if ctx.Err() != nil && !w.status.Snapshot().TerminalLocalError {
					return nil
				}
				return err
			}
			continue
		}

		object := queue[0]
		record, found, getErr := w.ledger.Get(object.ObjectID)
		if getErr != nil || !found || !recordMatchesPending(record, object) {
			return w.terminalFailure()
		}
		delay, delayErr := w.retryDelay(record)
		if delayErr != nil {
			return w.terminalFailure()
		}
		if delay > 0 {
			if err := w.ops.wait(ctx, delay); err != nil {
				if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return nil
				}
				return w.terminalFailure()
			}
			continue
		}
		_, commitErr := w.service.CommitPending(ctx, object)
		if commitErr == nil {
			queue = queue[1:]
			continue
		}
		if w.status.Snapshot().TerminalLocalError {
			return errors.New("collector local state failed")
		}
		if ctx.Err() != nil {
			return nil
		}
	}
}

func (w *Worker) reconcileStartup(context.Context) ([]PendingObject, error) {
	w.service.publishMu.Lock()
	defer w.service.publishMu.Unlock()
	w.service.commitMu.Lock()
	defer w.service.commitMu.Unlock()

	records, err := w.ledger.List()
	if err != nil {
		return nil, w.terminalFailure()
	}
	objects, err := w.spool.Discover()
	if err != nil {
		return nil, w.terminalFailure()
	}
	byID := make(map[string]ObjectRecord, len(records))
	objectByID := make(map[string]PendingObject, len(objects))
	for _, record := range records {
		if _, duplicate := byID[record.ObjectID]; duplicate {
			return nil, w.terminalFailure()
		}
		byID[record.ObjectID] = record
	}
	for _, object := range objects {
		if _, duplicate := objectByID[object.ObjectID]; duplicate {
			return nil, w.terminalFailure()
		}
		objectByID[object.ObjectID] = object
	}

	queue := make([]PendingObject, 0, len(records))
	for _, record := range records {
		object, hasObject := objectByID[record.ObjectID]
		if !hasObject {
			if record.StoredAt == nil {
				if err := w.ledger.DeletePending(record.ObjectID); err != nil {
					return nil, w.terminalFailure()
				}
				w.status.RemovePending(record.ObjectID)
				continue
			}
			digest, ok := canonicalDigest(record.ObjectID)
			if !ok {
				return nil, w.terminalFailure()
			}
			object = PendingObject{
				ObjectID: record.ObjectID, DigestHex: digest, ArchiveName: record.ArchiveName,
				EncryptedPath: filepath.Join(w.spool.path, digest+".tar.gz.age"),
				EncryptedSize: record.EncryptedSize, ReceivedAt: record.ReceivedAt,
			}
		} else {
			object.ReceivedAt = record.ReceivedAt
			if !recordMatchesPending(record, object) {
				return nil, w.terminalFailure()
			}
			delete(objectByID, record.ObjectID)
		}
		if record.StoredAt == nil {
			w.status.RecordPending(record.ObjectID, record.ReceivedAt)
		}
		queue = append(queue, object)
	}
	if len(objectByID) != 0 {
		return nil, w.terminalFailure()
	}
	w.sortQueue(queue)
	return queue, nil
}

func (w *Worker) retryDelay(record ObjectRecord) (time.Duration, error) {
	if record.RetryCount == 0 {
		return 0, nil
	}
	if record.LastAttemptAt == nil {
		return 0, errors.New("missing retry time")
	}
	now := w.ops.now()
	if now.IsZero() || now.Before(record.ReceivedAt) || record.LastAttemptAt.Before(record.ReceivedAt) {
		return 0, errors.New("invalid retry clock")
	}
	delay := RetryDelay(w.retryInterval, record.RetryCount)
	if delay <= 0 {
		return 0, errors.New("invalid retry delay")
	}
	due := record.LastAttemptAt.Add(delay)
	if due.Before(*record.LastAttemptAt) {
		return 0, errors.New("retry overflow")
	}
	if !due.After(now) {
		return 0, nil
	}
	return due.Sub(now), nil
}

func (w *Worker) sortQueue(queue []PendingObject) {
	sort.SliceStable(queue, func(i, j int) bool {
		if queue[i].ReceivedAt.Equal(queue[j].ReceivedAt) {
			return queue[i].ObjectID < queue[j].ObjectID
		}
		return queue[i].ReceivedAt.Before(queue[j].ReceivedAt)
	})
}

func RetryDelay(base time.Duration, retryCount int) time.Duration {
	if base <= 0 || retryCount < 0 {
		return 0
	}
	if retryCount <= 1 {
		return minDuration(base, time.Hour)
	}
	shift := retryCount - 1
	if shift >= 63 || base > time.Duration(math.MaxInt64>>shift) {
		return time.Hour
	}
	return minDuration(base*time.Duration(uint64(1)<<shift), time.Hour)
}

func minDuration(left, right time.Duration) time.Duration {
	if left < right {
		return left
	}
	return right
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	if ctx == nil {
		return context.Canceled
	}
	if delay < 0 {
		return errors.New("invalid delay")
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (w *Worker) terminalFailure() error {
	if w != nil && w.status != nil {
		w.status.RecordTerminalLocalError()
	}
	return errors.New("collector local state failed")
}
