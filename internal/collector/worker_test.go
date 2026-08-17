package collector

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestWorkerStartupCleansStalePartialsAndDiscoversAgeObjects(t *testing.T) {
	fixture := newServiceFixture(t)
	partial := filepath.Join(fixture.spool.path, ".sherpa-upload-0123456789abcdef0123456789abcdef.upload.partial")
	if err := os.WriteFile(partial, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Unix(1, 0)
	if err := os.Chtimes(partial, old, old); err != nil {
		t.Fatal(err)
	}
	pending := createPendingFixture(t, fixture.spool, fixture.ledger, fixture.now.Add(-time.Hour), "startup-discovery")
	fixture.backend.set(pending.ObjectID, true)
	ctx, cancel := context.WithCancel(context.Background())
	worker := newWorker(fixture.spool, fixture.ledger, fixture.service, fixture.status, time.Minute, workerOps{
		now:  fixture.clock,
		wait: func(context.Context, time.Duration) error { cancel(); return context.Canceled },
	})
	if err := worker.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(partial); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale partial remains: %v", err)
	}
	if _, err := os.Stat(pending.EncryptedPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reconciled age remains: %v", err)
	}
	snapshot := fixture.status.Snapshot()
	if snapshot.NewestSuccessfulAt == nil || snapshot.OldestPendingAt != nil {
		t.Fatalf("status = %#v", snapshot)
	}
}

func TestWorkerReconcilesRemoteCommitBeforeLedgerUpdate(t *testing.T) {
	fixture := newServiceFixture(t)
	pending := createPendingFixture(t, fixture.spool, fixture.ledger, fixture.now.Add(-time.Hour), "remote-before-ledger")
	fixture.backend.set(pending.ObjectID, true)
	ctx, cancel := context.WithCancel(context.Background())
	worker := newWorker(fixture.spool, fixture.ledger, fixture.service, fixture.status, time.Minute, workerOps{
		now:  fixture.clock,
		wait: func(context.Context, time.Duration) error { cancel(); return context.Canceled },
	})
	if err := worker.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	record, found, err := fixture.ledger.Get(pending.ObjectID)
	if err != nil || !found || record.StoredAt == nil {
		t.Fatalf("record = %#v found=%v err=%v", record, found, err)
	}
	if fixture.backend.createCalls != 0 {
		t.Fatalf("Create calls = %d", fixture.backend.createCalls)
	}
}

func TestWorkerQueuesMissingObjectsOldestFirst(t *testing.T) {
	fixture := newServiceFixture(t)
	oldest := createPendingFixture(t, fixture.spool, fixture.ledger, fixture.now.Add(-2*time.Hour), "oldest")
	newer := createPendingFixture(t, fixture.spool, fixture.ledger, fixture.now.Add(-time.Hour), "newer")
	ctx, cancel := context.WithCancel(context.Background())
	created := make([]string, 0, 2)
	fixture.backend.onCreate = func(object PendingObject) {
		created = append(created, object.ObjectID)
	}
	removed := 0
	fixture.service.ops.crash = func(point serviceCrashPoint) error {
		if point == crashAfterAgeRemoval {
			removed++
			if removed == 2 {
				cancel()
			}
		}
		return nil
	}
	worker := newWorker(fixture.spool, fixture.ledger, fixture.service, fixture.status, time.Minute, workerOps{
		now: fixture.clock,
		wait: func(ctx context.Context, _ time.Duration) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return nil
		},
	})
	if err := worker.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !reflect.DeepEqual(created, []string{oldest.ObjectID, newer.ObjectID}) {
		t.Fatalf("create order = %v", created)
	}
}

func TestWorkerBackoffStartsAtFiveMinutesAndCapsAtOneHour(t *testing.T) {
	base := 5 * time.Minute
	want := []time.Duration{5 * time.Minute, 5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 40 * time.Minute, time.Hour, time.Hour, time.Hour}
	for retryCount, expected := range want {
		if got := RetryDelay(base, retryCount); got != expected {
			t.Fatalf("RetryDelay(%d) = %v, want %v", retryCount, got, expected)
		}
	}
	for _, invalid := range []struct {
		base  time.Duration
		count int
	}{{0, 1}, {-1, 1}, {base, -1}} {
		if got := RetryDelay(invalid.base, invalid.count); got != 0 {
			t.Fatalf("RetryDelay(%v, %d) = %v", invalid.base, invalid.count, got)
		}
	}
}

func TestWorkerSuccessfulRetryUpdatesLedgerBeforeRemovingAgeFile(t *testing.T) {
	fixture := newServiceFixture(t)
	pending := createPendingFixture(t, fixture.spool, fixture.ledger, fixture.now.Add(-time.Hour), "retry-order")
	checked := false
	ctx, cancel := context.WithCancel(context.Background())
	fixture.service.ops.crash = func(point serviceCrashPoint) error {
		switch point {
		case crashAfterLedgerUpdate:
			record, found, err := fixture.ledger.Get(pending.ObjectID)
			if err != nil || !found || record.StoredAt == nil {
				t.Fatalf("stored record missing: %#v %v %v", record, found, err)
			}
			if _, err := os.Stat(pending.EncryptedPath); err != nil {
				t.Fatalf("age removed before ledger update: %v", err)
			}
			checked = true
		case crashAfterAgeRemoval:
			cancel()
		}
		return nil
	}
	worker := newWorker(fixture.spool, fixture.ledger, fixture.service, fixture.status, time.Minute, workerOps{
		now:  fixture.clock,
		wait: func(context.Context, time.Duration) error { return nil },
	})
	if err := worker.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !checked {
		t.Fatal("ledger-before-removal boundary not observed")
	}
}

func TestWorkerCorruptStateSetsTerminalReadinessError(t *testing.T) {
	fixture := newServiceFixture(t)
	pending := createPendingFixture(t, fixture.spool, fixture.ledger, fixture.now.Add(-time.Hour), "corrupt-state")
	if err := os.WriteFile(filepath.Join(fixture.ledger.path, pending.DigestHex+".json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	worker := NewWorker(fixture.spool, fixture.ledger, fixture.service, fixture.status, time.Minute)
	err := worker.Run(context.Background())
	if err == nil || err.Error() != "collector local state failed" {
		t.Fatalf("Run error = %v", err)
	}
	if !fixture.status.Snapshot().TerminalLocalError || fixture.status.Ready() {
		t.Fatalf("status = %#v ready=%v", fixture.status.Snapshot(), fixture.status.Ready())
	}
}

func TestReadinessPassesDuringStartupGrace(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	tracker := NewStatusTracker(ReadinessConfig{StartedAt: now, StartupGrace: time.Hour, MaxRecoveryAge: 2 * time.Hour}, func() time.Time { return now.Add(30 * time.Minute) })
	tracker.SetSpoolWritable(true)
	if !tracker.Ready() {
		t.Fatal("not ready during no-success startup grace")
	}
}

func TestReadinessFailsAfterGraceWithoutRecoveryPoint(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	tracker := NewStatusTracker(ReadinessConfig{StartedAt: now, StartupGrace: time.Hour, MaxRecoveryAge: 2 * time.Hour}, func() time.Time { return now.Add(time.Hour + time.Nanosecond) })
	tracker.SetSpoolWritable(true)
	if tracker.Ready() {
		t.Fatal("ready after grace without recovery point")
	}
}

func TestReadinessFailsForStaleRecoveryPoint(t *testing.T) {
	started := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	now := started.Add(30 * time.Minute)
	tracker := NewStatusTracker(ReadinessConfig{StartedAt: started, StartupGrace: time.Hour, MaxRecoveryAge: 10 * time.Minute}, func() time.Time { return now })
	tracker.SetSpoolWritable(true)
	tracker.RecordSuccess(started)
	if tracker.Ready() {
		t.Fatal("startup grace hid a stale recorded success")
	}
}

func TestReadinessFailsForOldPendingObject(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	tracker := NewStatusTracker(ReadinessConfig{StartedAt: now.Add(-time.Hour), StartupGrace: 2 * time.Hour, MaxRecoveryAge: 10 * time.Minute}, func() time.Time { return now })
	tracker.SetSpoolWritable(true)
	tracker.RecordPending("sha256:"+testDigestHex, now.Add(-10*time.Minute-time.Nanosecond))
	if tracker.Ready() {
		t.Fatal("ready with old pending object")
	}
}

func TestLivenessRemainsHealthyDuringBackendOutage(t *testing.T) {
	fixture := newServiceFixture(t)
	createPendingFixture(t, fixture.spool, fixture.ledger, fixture.now.Add(-time.Hour), "outage")
	fixture.backend.existsErr = errors.New("offline")
	ctx, cancel := context.WithCancel(context.Background())
	waits := 0
	worker := newWorker(fixture.spool, fixture.ledger, fixture.service, fixture.status, time.Minute, workerOps{
		now: fixture.clock,
		wait: func(context.Context, time.Duration) error {
			waits++
			if waits == 2 {
				cancel()
				return context.Canceled
			}
			return nil
		},
	})
	if err := worker.Run(ctx); err != nil {
		t.Fatalf("backend outage terminated worker: %v", err)
	}
	if waits == 0 || fixture.status.Snapshot().TerminalLocalError {
		t.Fatalf("waits=%d status=%#v", waits, fixture.status.Snapshot())
	}
}

func TestWorkerDoesNotTrustStoredLedgerWithoutRemotePresence(t *testing.T) {
	fixture := newServiceFixture(t)
	pending := createPendingFixture(t, fixture.spool, fixture.ledger, fixture.now.Add(-time.Hour), "stored-ledger-is-not-proof")
	record, _, err := fixture.ledger.Get(pending.ObjectID)
	if err != nil {
		t.Fatal(err)
	}
	storedAt := fixture.now.Add(-30 * time.Minute)
	record.StoredAt = &storedAt
	if err := fixture.ledger.Put(record); err != nil {
		t.Fatal(err)
	}
	if err := fixture.spool.RemoveEncrypted(pending.EncryptedPath); err != nil {
		t.Fatal(err)
	}
	fixture.backend.existsErr = errors.New("offline")
	ctx, cancel := context.WithCancel(context.Background())
	worker := newWorker(fixture.spool, fixture.ledger, fixture.service, fixture.status, time.Minute, workerOps{
		now: fixture.clock,
		wait: func(context.Context, time.Duration) error {
			fixture.backend.existsErr = nil
			fixture.backend.set(pending.ObjectID, true)
			cancel()
			return nil
		},
	})
	if err := worker.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fixture.backend.existsCalls == 0 {
		t.Fatal("stored ledger was accepted without exact remote presence")
	}
	if fixture.status.Snapshot().NewestSuccessfulAt != nil {
		t.Fatal("success was recorded while exact presence was unavailable")
	}
}

func TestWorkerRetriesStoredLedgerCleanupAfterBackendOutage(t *testing.T) {
	fixture := newServiceFixture(t)
	pending := createPendingFixture(t, fixture.spool, fixture.ledger, fixture.now.Add(-time.Hour), "stored-cleanup-outage")
	record, _, err := fixture.ledger.Get(pending.ObjectID)
	if err != nil {
		t.Fatal(err)
	}
	storedAt := fixture.now.Add(-30 * time.Minute)
	record.StoredAt = &storedAt
	if err := fixture.ledger.Put(record); err != nil {
		t.Fatal(err)
	}
	fixture.backend.existsErr = errors.New("offline")
	ctx, cancel := context.WithCancel(context.Background())
	fixture.service.ops.crash = func(point serviceCrashPoint) error {
		if point == crashAfterAgeRemoval {
			cancel()
		}
		return nil
	}
	workerNow := fixture.now
	worker := newWorker(fixture.spool, fixture.ledger, fixture.service, fixture.status, time.Minute, workerOps{
		now: func() time.Time { return workerNow },
		wait: func(context.Context, time.Duration) error {
			fixture.backend.existsErr = nil
			fixture.backend.set(pending.ObjectID, true)
			workerNow = workerNow.Add(time.Minute)
			return nil
		},
	})
	if err := worker.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fixture.status.Snapshot().TerminalLocalError {
		t.Fatal("backend outage during stored cleanup became terminal")
	}
	if _, err := os.Stat(pending.EncryptedPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stored age not removed after recovery: %v", err)
	}
}

func TestWorkerDoesNotReverifyStoredLedgerOnEveryIdleRetry(t *testing.T) {
	fixture := newServiceFixture(t)
	pending := createPendingFixture(t, fixture.spool, fixture.ledger, fixture.now.Add(-time.Hour), "stored-idle-retry")
	record, _, err := fixture.ledger.Get(pending.ObjectID)
	if err != nil {
		t.Fatal(err)
	}
	storedAt := fixture.now.Add(-30 * time.Minute)
	record.StoredAt = &storedAt
	if err := fixture.ledger.Put(record); err != nil {
		t.Fatal(err)
	}
	if err := fixture.spool.RemoveEncrypted(pending.EncryptedPath); err != nil {
		t.Fatal(err)
	}
	fixture.backend.set(pending.ObjectID, true)

	ctx, cancel := context.WithCancel(context.Background())
	waits := 0
	worker := newWorker(fixture.spool, fixture.ledger, fixture.service, fixture.status, time.Minute, workerOps{
		now: fixture.clock,
		wait: func(context.Context, time.Duration) error {
			waits++
			if waits == 2 {
				cancel()
				return context.Canceled
			}
			return nil
		},
	})
	if err := worker.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fixture.backend.existsCalls != 1 {
		t.Fatalf("stored ledger presence checks = %d, want one startup verification", fixture.backend.existsCalls)
	}
	if snapshot := fixture.status.Snapshot(); snapshot.NewestSuccessfulAt == nil || !snapshot.NewestSuccessfulAt.Equal(storedAt) || snapshot.OldestPendingAt != nil {
		t.Fatalf("status = %#v", snapshot)
	}
}

func TestWorkerPersistsRetryMetadataAcrossRestart(t *testing.T) {
	fixture := newServiceFixture(t)
	pending := createPendingFixture(t, fixture.spool, fixture.ledger, fixture.now.Add(-time.Hour), "persisted-backoff")
	record, _, err := fixture.ledger.Get(pending.ObjectID)
	if err != nil {
		t.Fatal(err)
	}
	attempted := fixture.now
	record.LastAttemptAt = &attempted
	record.LatestRetryClass = "backend_failed"
	record.RetryCount = 4
	if err := fixture.ledger.Put(record); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var got time.Duration
	worker := newWorker(fixture.spool, fixture.ledger, fixture.service, fixture.status, 5*time.Minute, workerOps{
		now: fixture.clock,
		wait: func(_ context.Context, delay time.Duration) error {
			got = delay
			cancel()
			return context.Canceled
		},
	})
	if err := worker.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got != 40*time.Minute {
		t.Fatalf("persisted retry delay = %v", got)
	}
}

func TestWorkerContextShutdownIsPrompt(t *testing.T) {
	fixture := newServiceFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- NewWorker(fixture.spool, fixture.ledger, fixture.service, fixture.status, time.Hour).Run(ctx)
	}()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not stop promptly")
	}
}

func TestStatusTrackerSnapshotPointersAreDefensiveAndConcurrent(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	tracker := NewStatusTracker(ReadinessConfig{StartedAt: now.Add(-time.Minute), StartupGrace: time.Hour, MaxRecoveryAge: time.Hour}, func() time.Time { return now })
	tracker.SetSpoolWritable(true)
	tracker.RecordPending("sha256:"+testDigestHex, now.Add(-time.Minute))
	tracker.RecordSuccess(now)
	first := tracker.Snapshot()
	*first.OldestPendingAt = time.Time{}
	*first.NewestSuccessfulAt = time.Time{}
	second := tracker.Snapshot()
	if second.OldestPendingAt == nil || second.OldestPendingAt.IsZero() || second.NewestSuccessfulAt == nil || second.NewestSuccessfulAt.IsZero() {
		t.Fatalf("snapshot pointers alias tracker state: %#v", second)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			id := "sha256:" + testDigestHex
			for j := 0; j < 100; j++ {
				tracker.RecordPending(id, now.Add(time.Duration(index)*time.Second))
				_ = tracker.Snapshot()
				_ = tracker.Ready()
				tracker.RemovePending(id)
			}
		}(i)
	}
	wg.Wait()
}

func TestReadinessFailsClosedForInvalidDurationsAndClockRollback(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	for _, config := range []ReadinessConfig{
		{StartedAt: now, StartupGrace: 0, MaxRecoveryAge: time.Hour},
		{StartedAt: now, StartupGrace: time.Hour, MaxRecoveryAge: 0},
		{StartedAt: now, StartupGrace: -time.Hour, MaxRecoveryAge: time.Hour},
	} {
		tracker := NewStatusTracker(config, func() time.Time { return now })
		tracker.SetSpoolWritable(true)
		if tracker.Ready() {
			t.Fatalf("ready with invalid config %#v", config)
		}
	}
	rollback := NewStatusTracker(ReadinessConfig{StartedAt: now, StartupGrace: time.Hour, MaxRecoveryAge: time.Hour}, func() time.Time { return now.Add(-time.Nanosecond) })
	rollback.SetSpoolWritable(true)
	if rollback.Ready() {
		t.Fatal("ready after clock rollback")
	}
}

func TestWorkerFinalRemovalFailureIsTerminalWithoutRollingBackStoredState(t *testing.T) {
	fixture := newServiceFixture(t)
	pending := createPendingFixture(t, fixture.spool, fixture.ledger, fixture.now.Add(-time.Hour), "remove-failure")
	original := fixture.spool.ops.unlinkat
	fixture.spool.ops.unlinkat = func(dirfd int, path string, flags int) error {
		if stringsHasSuffix(path, ".tar.gz.age") {
			return errors.New("unlink failure")
		}
		return original(dirfd, path, flags)
	}
	_, err := fixture.service.CommitPending(context.Background(), pending)
	if err == nil || err.Error() != "collector local state failed" {
		t.Fatalf("CommitPending error = %v", err)
	}
	record, found, getErr := fixture.ledger.Get(pending.ObjectID)
	if getErr != nil || !found || record.StoredAt == nil || !fixture.backend.has(pending.ObjectID) {
		t.Fatalf("stored truth rolled back: %#v found=%v backend=%v err=%v", record, found, fixture.backend.has(pending.ObjectID), getErr)
	}
	if !fixture.status.Snapshot().TerminalLocalError {
		t.Fatal("final local removal failure did not set terminal readiness")
	}
}

func createPendingFixture(t testing.TB, spool *Spool, ledger *Ledger, received time.Time, seed string) PendingObject {
	t.Helper()
	digest := digestHex([]byte(seed))
	path := filepath.Join(spool.path, digest+".tar.gz.age")
	contents := []byte("encrypted-" + seed)
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, received, received); err != nil {
		t.Fatal(err)
	}
	object := PendingObject{
		ObjectID:      "sha256:" + digest,
		DigestHex:     digest,
		ArchiveName:   "sherpa-" + digest,
		EncryptedPath: path,
		EncryptedSize: int64(len(contents)),
		ReceivedAt:    received,
	}
	if err := ledger.Put(ObjectRecord{
		ObjectID: object.ObjectID, ArchiveName: object.ArchiveName,
		CompressedSize: 1, EncryptedSize: object.EncryptedSize,
		ReceivedAt: received, RetryCount: 0,
	}); err != nil {
		t.Fatal(err)
	}
	return object
}

func stringsHasSuffix(value, suffix string) bool {
	return len(value) >= len(suffix) && value[len(value)-len(suffix):] == suffix
}
