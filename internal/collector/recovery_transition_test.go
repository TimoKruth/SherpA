package collector

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"sherpa/internal/recoveryarchive"
)

func TestCommitPendingConcurrentSameObjectIsIdempotent(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	spoolOps := defaultSpoolOps()
	unlink := spoolOps.unlinkat
	spoolOps.unlinkat = func(dirFD int, name string, flags int) error {
		if completeAgePattern.MatchString(name) {
			once.Do(func() {
				close(entered)
				<-release
			})
		}
		return unlink(dirFD, name, flags)
	}
	fixture := newTransitionFixture(t, spoolOps, &fakeBackend{objects: make(map[string]bool), createVisible: true}, nil)
	pending := createPendingFixture(t, fixture.spool, fixture.ledger, fixture.now.Add(-time.Hour), "same-object-barrier")

	results := make(chan error, 2)
	go func() { _, err := fixture.service.CommitPending(context.Background(), pending); results <- err }()
	<-entered
	go func() { _, err := fixture.service.CommitPending(context.Background(), pending); results <- err }()
	close(release)
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Fatalf("CommitPending caller %d: %v", i, err)
		}
	}
	record, found, err := fixture.ledger.Get(pending.ObjectID)
	if err != nil || !found || record.StoredAt == nil {
		t.Fatalf("record = %#v found=%v err=%v", record, found, err)
	}
	if fixture.status.Snapshot().TerminalLocalError {
		t.Fatal("idempotent duplicate set terminal readiness")
	}
}

func TestCommitPendingCancellationWhileWaitingMakesNoBackendCall(t *testing.T) {
	backend := &commitBarrierBackend{objects: make(map[string]bool), entered: make(chan struct{}), release: make(chan struct{})}
	fixture := newTransitionFixture(t, defaultSpoolOps(), backend, nil)
	pending := createPendingFixture(t, fixture.spool, fixture.ledger, fixture.now.Add(-time.Hour), "cancel-waiting-commit")
	firstDone := make(chan error, 1)
	go func() { _, err := fixture.service.CommitPending(context.Background(), pending); firstDone <- err }()
	<-backend.entered
	ctx, cancel := context.WithCancel(context.Background())
	secondDone := make(chan error, 1)
	secondStarted := make(chan struct{})
	go func() {
		close(secondStarted)
		_, err := fixture.service.CommitPending(ctx, pending)
		secondDone <- err
	}()
	<-secondStarted
	time.Sleep(40 * time.Millisecond)
	cancel()
	close(backend.release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first CommitPending: %v", err)
	}
	if err := <-secondDone; err == nil || err.Error() != "collector backend cancelled" {
		t.Fatalf("canceled CommitPending error = %v", err)
	}
	backend.mu.Lock()
	existsCalls := backend.existsCalls
	backend.mu.Unlock()
	if existsCalls != 2 {
		t.Fatalf("Exists calls = %d, canceled waiter reached backend", existsCalls)
	}
}

func TestCommitPendingStoredMissingLocalIsIdempotent(t *testing.T) {
	fixture := newTransitionFixture(t, defaultSpoolOps(), &fakeBackend{objects: make(map[string]bool), createVisible: true}, nil)
	pending := createPendingFixture(t, fixture.spool, fixture.ledger, fixture.now.Add(-time.Hour), "stored-missing-local")
	if _, err := fixture.service.CommitPending(context.Background(), pending); err != nil {
		t.Fatalf("first CommitPending: %v", err)
	}
	existing, err := fixture.service.CommitPending(context.Background(), pending)
	if err != nil {
		t.Fatalf("second CommitPending: %v", err)
	}
	if !existing {
		t.Fatal("idempotent stored commit did not return existing")
	}
	if fixture.status.Snapshot().TerminalLocalError {
		t.Fatal("stored idempotent commit set terminal readiness")
	}
}

func TestCommitPendingFailureCannotRegressConcurrentSuccess(t *testing.T) {
	backend := &firstExistsFailureBackend{objects: make(map[string]bool), calls: make(chan int, 8)}
	nowEntered := make(chan struct{})
	releaseFailure := make(chan struct{})
	var nowCalls atomic.Int32
	fixture := newTransitionFixture(t, defaultSpoolOps(), backend, func(encryptor *Encryptor, _ func() time.Time) serviceOps {
		return serviceOps{
			now: func() time.Time {
				if nowCalls.Add(1) == 1 {
					close(nowEntered)
					<-releaseFailure
				}
				return time.Date(2026, 7, 21, 13, 0, 0, 0, time.UTC)
			},
			encryptFile: encryptor.EncryptFile,
		}
	})
	pending := createPendingFixture(t, fixture.spool, fixture.ledger, fixture.now.Add(-time.Hour), "stale-failure-race")

	firstDone := make(chan error, 1)
	go func() { _, err := fixture.service.CommitPending(context.Background(), pending); firstDone <- err }()
	<-nowEntered
	secondDone := make(chan error, 1)
	go func() { _, err := fixture.service.CommitPending(context.Background(), pending); secondDone <- err }()

	select {
	case call := <-backend.calls:
		if call != 2 {
			t.Fatalf("unexpected backend call %d", call)
		}
		if err := <-secondDone; err != nil {
			t.Fatalf("concurrent success: %v", err)
		}
	case <-time.After(40 * time.Millisecond):
		// Whole-transaction locking correctly keeps the second caller out.
	}
	close(releaseFailure)
	if err := <-firstDone; err == nil || err.Error() != "collector backend unavailable" {
		t.Fatalf("failed caller error = %v", err)
	}
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatalf("second caller: %v", err)
		}
	default:
		if err := <-secondDone; err != nil {
			t.Fatalf("second caller: %v", err)
		}
	}
	record, found, err := fixture.ledger.Get(pending.ObjectID)
	if err != nil || !found || record.StoredAt == nil {
		t.Fatalf("stored state regressed: %#v found=%v err=%v", record, found, err)
	}
	if record.RetryCount == 0 {
		t.Fatal("actual failed attempt was not retained in canonical metadata")
	}
	if fixture.status.Snapshot().TerminalLocalError {
		t.Fatal("stale failure race set terminal readiness")
	}
}

func TestWorkerAndDuplicateIngestSerializeWholeTransition(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	spoolOps := defaultSpoolOps()
	unlink := spoolOps.unlinkat
	spoolOps.unlinkat = func(dirFD int, name string, flags int) error {
		if completeAgePattern.MatchString(name) {
			once.Do(func() {
				close(entered)
				<-release
			})
		}
		return unlink(dirFD, name, flags)
	}
	backend := &fakeBackend{objects: make(map[string]bool), createVisible: true}
	fixture := newTransitionFixture(t, spoolOps, backend, nil)
	archive := validRecoveryArchive(t, "worker-duplicate-ingest")
	backend.existsErr = errors.New("initial outage")
	if _, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive))); err == nil {
		t.Fatal("initial Ingest succeeded during outage")
	}
	backend.existsErr = nil
	objects, err := fixture.spool.Discover()
	if err != nil || len(objects) != 1 {
		t.Fatalf("Discover = %#v, %v", objects, err)
	}
	pending := objects[0]
	originalRecord, found, err := fixture.ledger.Get(pending.ObjectID)
	if err != nil || !found {
		t.Fatalf("pending ledger found=%v err=%v", found, err)
	}
	pending.ReceivedAt = originalRecord.ReceivedAt

	workerDone := make(chan error, 1)
	go func() { _, err := fixture.service.CommitPending(context.Background(), pending); workerDone <- err }()
	<-entered
	ingestDone := make(chan error, 1)
	go func() {
		result, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive)))
		if err == nil && result.Status != ResultExisting {
			err = errors.New("duplicate did not return existing")
		}
		ingestDone <- err
	}()
	close(release)
	if err := <-workerDone; err != nil {
		t.Fatalf("worker commit: %v", err)
	}
	if err := <-ingestDone; err != nil {
		t.Fatalf("duplicate ingest: %v", err)
	}
	updated, found, err := fixture.ledger.Get(pending.ObjectID)
	if err != nil || !found || updated.StoredAt == nil || updated.RetryCount != originalRecord.RetryCount || updated.LatestRetryClass != originalRecord.LatestRetryClass || updated.LastAttemptAt == nil || originalRecord.LastAttemptAt == nil || !updated.LastAttemptAt.Equal(*originalRecord.LastAttemptAt) || !updated.ReceivedAt.Equal(originalRecord.ReceivedAt) {
		t.Fatalf("duplicate metadata changed: original=%#v updated=%#v found=%v err=%v", originalRecord, updated, found, err)
	}
	if fixture.status.Snapshot().TerminalLocalError {
		t.Fatal("worker/ingest duplicate set terminal readiness")
	}
}

func TestIngestValidationFailureCleansOwnedPlaintext(t *testing.T) {
	fixture := newTransitionFixture(t, defaultSpoolOps(), &fakeBackend{objects: make(map[string]bool), createVisible: true}, nil)
	_, err := fixture.service.Ingest(context.Background(), testReadCloser([]byte("invalid")), 7)
	if err == nil || err.Error() != "collector archive invalid" {
		t.Fatalf("Ingest error = %v", err)
	}
	if entries := spoolEntryNames(t, fixture.spool.path); len(entries) != 0 {
		t.Fatalf("validation failure entries = %v", entries)
	}
}

func TestIngestEncryptFailureBeforeCreateCleansOwnership(t *testing.T) {
	fixture := newTransitionFixture(t, defaultSpoolOps(), &fakeBackend{objects: make(map[string]bool), createVisible: true}, func(*Encryptor, func() time.Time) serviceOps {
		return serviceOps{encryptFile: func(context.Context, string, string) (int64, error) {
			return 0, errors.New("encrypt before create")
		}}
	})
	archive := validRecoveryArchive(t, "encrypt-before-create")
	_, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive)))
	if err == nil || err.Error() != "collector encryption failed" {
		t.Fatalf("Ingest error = %v", err)
	}
	if entries := spoolEntryNames(t, fixture.spool.path); len(entries) != 0 {
		t.Fatalf("encryption failure entries = %v", entries)
	}
}

func TestIngestMidWriteCancellationCleansOwnership(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	fixture := newTransitionFixture(t, defaultSpoolOps(), &fakeBackend{objects: make(map[string]bool), createVisible: true}, func(*Encryptor, func() time.Time) serviceOps {
		return serviceOps{encryptFile: func(_ context.Context, _, partialPath string) (int64, error) {
			if err := os.WriteFile(partialPath, []byte("partial ciphertext"), 0o600); err != nil {
				return 0, err
			}
			cancel()
			return 0, context.Canceled
		}}
	})
	archive := validRecoveryArchive(t, "midwrite-cancel")
	_, err := fixture.service.Ingest(ctx, testReadCloser(archive), int64(len(archive)))
	if err == nil || err.Error() != "collector ingestion canceled" {
		t.Fatalf("Ingest error = %v", err)
	}
	if entries := spoolEntryNames(t, fixture.spool.path); len(entries) != 0 {
		t.Fatalf("cancellation entries = %v", entries)
	}
}

func TestIngestCleanupFailureIsTerminalAndRedacted(t *testing.T) {
	spoolOps := defaultSpoolOps()
	unlink := spoolOps.unlinkat
	spoolOps.unlinkat = func(dirFD int, name string, flags int) error {
		if partialAgePattern.MatchString(name) {
			return errors.New("private cleanup path")
		}
		return unlink(dirFD, name, flags)
	}
	fixture := newTransitionFixture(t, spoolOps, &fakeBackend{objects: make(map[string]bool), createVisible: true}, func(*Encryptor, func() time.Time) serviceOps {
		return serviceOps{encryptFile: func(_ context.Context, _, partialPath string) (int64, error) {
			if err := os.WriteFile(partialPath, []byte("partial"), 0o600); err != nil {
				return 0, err
			}
			return 0, errors.New("private encrypt failure")
		}}
	})
	archive := validRecoveryArchive(t, "cleanup-failure")
	_, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive)))
	if err == nil || err.Error() != "collector local state failed" {
		t.Fatalf("Ingest error = %v", err)
	}
	if !fixture.status.Snapshot().TerminalLocalError {
		t.Fatal("cleanup failure did not set terminal readiness")
	}
}

func TestIngestCleanupFsyncFailureIsTerminal(t *testing.T) {
	spoolOps := defaultSpoolOps()
	fsync := spoolOps.fsync
	var fsyncCalls atomic.Int32
	spoolOps.fsync = func(fd int) error {
		if fsyncCalls.Add(1) == 2 {
			return errors.New("private cleanup sync failure")
		}
		return fsync(fd)
	}
	fixture := newTransitionFixture(t, spoolOps, &fakeBackend{objects: make(map[string]bool), createVisible: true}, func(*Encryptor, func() time.Time) serviceOps {
		return serviceOps{encryptFile: func(_ context.Context, _, partialPath string) (int64, error) {
			if err := os.WriteFile(partialPath, []byte("partial"), 0o600); err != nil {
				return 0, err
			}
			return 0, errors.New("private encrypt failure")
		}}
	})
	archive := validRecoveryArchive(t, "cleanup-fsync-failure")
	_, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive)))
	if err == nil || err.Error() != "collector local state failed" {
		t.Fatalf("Ingest error = %v", err)
	}
	if entries := spoolEntryNames(t, fixture.spool.path); len(entries) != 0 {
		t.Fatalf("cleanup fsync failure entries = %v", entries)
	}
	if !fixture.status.Snapshot().TerminalLocalError {
		t.Fatal("cleanup fsync failure did not set terminal readiness")
	}
}

func TestIngestEncryptionFailureReleasesActiveReservation(t *testing.T) {
	var calls atomic.Int32
	fixture := newTransitionFixture(t, defaultSpoolOps(), &fakeBackend{objects: make(map[string]bool), createVisible: true}, func(encryptor *Encryptor, _ func() time.Time) serviceOps {
		return serviceOps{encryptFile: func(ctx context.Context, sourcePath, partialPath string) (int64, error) {
			if calls.Add(1) == 1 {
				return 0, errors.New("first failure")
			}
			return encryptor.EncryptFile(ctx, sourcePath, partialPath)
		}}
	})
	archive := validRecoveryArchive(t, "release-active")
	if _, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive))); err == nil {
		t.Fatal("first Ingest succeeded")
	}
	if _, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive))); err != nil {
		t.Fatalf("retry Ingest: %v", err)
	}
}

func TestSpoolEncryptedPathsDiscardsRecentAbandonedPartialAfterReopen(t *testing.T) {
	dir := newPrivateDir(t)
	spool, err := OpenSpool(dir, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	partial, _, err := spool.EncryptedPaths("sha256:" + testDigestHex)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(partial, []byte("recent abandoned"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := spool.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSpool(dir, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	gotPartial, _, err := reopened.EncryptedPaths("sha256:" + testDigestHex)
	if err != nil {
		t.Fatalf("EncryptedPaths after reopen: %v", err)
	}
	if gotPartial != partial {
		t.Fatalf("partial = %q, want %q", gotPartial, partial)
	}
	if _, err := os.Stat(partial); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recent abandoned partial remains: %v", err)
	}
}

func TestWorkerStartupHonorsPersistedRetryDueBeforeRemote(t *testing.T) {
	backend := &fakeBackend{objects: make(map[string]bool), createVisible: true}
	fixture := newTransitionFixture(t, defaultSpoolOps(), backend, nil)
	pending := createPendingFixture(t, fixture.spool, fixture.ledger, fixture.now.Add(-time.Hour), "persisted-due-gate")
	record, _, err := fixture.ledger.Get(pending.ObjectID)
	if err != nil {
		t.Fatal(err)
	}
	attempted := fixture.now
	record.RetryCount = 2
	record.LastAttemptAt = &attempted
	record.LatestRetryClass = "backend_failed"
	if err := fixture.ledger.Put(record); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	waits := 0
	worker := newWorker(fixture.spool, fixture.ledger, fixture.service, fixture.status, 5*time.Minute, workerOps{
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
	if backend.existsCalls != 0 || backend.createCalls != 0 {
		t.Fatalf("backend calls before due: exists=%d create=%d", backend.existsCalls, backend.createCalls)
	}
	updated, _, err := fixture.ledger.Get(pending.ObjectID)
	if err != nil || updated.RetryCount != 2 || updated.LastAttemptAt == nil || !updated.LastAttemptAt.Equal(attempted) {
		t.Fatalf("retry metadata changed without attempt: %#v err=%v", updated, err)
	}
}

func TestWorkerOldestPendingBlocksNewerDue(t *testing.T) {
	backend := &fakeBackend{objects: make(map[string]bool), createVisible: true}
	fixture := newTransitionFixture(t, defaultSpoolOps(), backend, nil)
	oldest := createPendingFixture(t, fixture.spool, fixture.ledger, fixture.now.Add(-2*time.Hour), "oldest-backed-off")
	createPendingFixture(t, fixture.spool, fixture.ledger, fixture.now.Add(-time.Hour), "newer-due")
	record, _, err := fixture.ledger.Get(oldest.ObjectID)
	if err != nil {
		t.Fatal(err)
	}
	attempted := fixture.now
	record.RetryCount = 1
	record.LastAttemptAt = &attempted
	record.LatestRetryClass = "backend_failed"
	if err := fixture.ledger.Put(record); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var waited time.Duration
	worker := newWorker(fixture.spool, fixture.ledger, fixture.service, fixture.status, 5*time.Minute, workerOps{
		now: fixture.clock,
		wait: func(_ context.Context, delay time.Duration) error {
			waited = delay
			cancel()
			return context.Canceled
		},
	})
	if err := worker.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if waited != 5*time.Minute {
		t.Fatalf("oldest wait = %v", waited)
	}
	if backend.existsCalls != 0 || backend.createCalls != 0 {
		t.Fatalf("newer object bypassed oldest: exists=%d create=%d", backend.existsCalls, backend.createCalls)
	}
}

func TestWorkerContextCancellationWhileOldestBackoffWaits(t *testing.T) {
	backend := &fakeBackend{objects: make(map[string]bool), createVisible: true}
	fixture := newTransitionFixture(t, defaultSpoolOps(), backend, nil)
	pending := createPendingFixture(t, fixture.spool, fixture.ledger, fixture.now.Add(-time.Hour), "cancel-oldest-wait")
	record, _, err := fixture.ledger.Get(pending.ObjectID)
	if err != nil {
		t.Fatal(err)
	}
	attempted := fixture.now
	record.RetryCount = 1
	record.LastAttemptAt = &attempted
	record.LatestRetryClass = "backend_failed"
	if err := fixture.ledger.Put(record); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- NewWorker(fixture.spool, fixture.ledger, fixture.service, fixture.status, 5*time.Minute).Run(ctx)
	}()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker timer did not stop on cancellation")
	}
	if backend.existsCalls != 0 || backend.createCalls != 0 {
		t.Fatalf("backend called while waiting: exists=%d create=%d", backend.existsCalls, backend.createCalls)
	}
}

func TestWorkerCannotDeleteInFlightPendingPublication(t *testing.T) {
	pendingWritten := make(chan struct{})
	releasePublication := make(chan struct{})
	var once sync.Once
	backend := &fakeBackend{objects: make(map[string]bool), createVisible: true}
	fixture := newTransitionFixture(t, defaultSpoolOps(), backend, func(encryptor *Encryptor, now func() time.Time) serviceOps {
		return serviceOps{
			now: now, encryptFile: encryptor.EncryptFile,
			crash: func(point serviceCrashPoint) error {
				if point == crashAfterPendingLedger {
					once.Do(func() { close(pendingWritten) })
					<-releasePublication
				}
				return nil
			},
		}
	})
	archive := validRecoveryArchive(t, "in-flight-pending")
	ingestDone := make(chan error, 1)
	go func() {
		_, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive)))
		ingestDone <- err
	}()
	<-pendingWritten
	ctx, cancel := context.WithCancel(context.Background())
	workerDone := make(chan error, 1)
	worker := newWorker(fixture.spool, fixture.ledger, fixture.service, fixture.status, time.Minute, workerOps{
		now: fixture.clock,
		wait: func(context.Context, time.Duration) error {
			cancel()
			return context.Canceled
		},
	})
	go func() { workerDone <- worker.Run(ctx) }()
	select {
	case err := <-workerDone:
		t.Fatalf("worker observed in-flight pending publication: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
	close(releasePublication)
	if err := <-ingestDone; err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if err := <-workerDone; err != nil {
		t.Fatalf("Run: %v", err)
	}
	objectID := "sha256:" + digestHex(archive)
	record, found, err := fixture.ledger.Get(objectID)
	if err != nil || !found || record.StoredAt == nil {
		t.Fatalf("record = %#v found=%v err=%v", record, found, err)
	}
	if fixture.status.Snapshot().TerminalLocalError {
		t.Fatal("in-flight publication caused terminal readiness")
	}
}

func TestIngestPendingLedgerSyncFailureDeletesPublishedPending(t *testing.T) {
	ledgerOps := defaultLedgerOps()
	fsync := ledgerOps.fsync
	var fsyncCalls atomic.Int32
	ledgerOps.fsync = func(fd int) error {
		if fsyncCalls.Add(1) == 2 {
			return errors.New("directory sync failure")
		}
		return fsync(fd)
	}
	fixture := newTransitionFixtureWithLedgerOps(t, defaultSpoolOps(), ledgerOps, &fakeBackend{objects: make(map[string]bool), createVisible: true}, nil)
	archive := validRecoveryArchive(t, "pending-sync-failure")
	_, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive)))
	if err == nil || err.Error() != "collector local state failed" {
		t.Fatalf("Ingest error = %v", err)
	}
	if entries := spoolEntryNames(t, fixture.spool.path); len(entries) != 0 {
		t.Fatalf("pending sync failure entries = %v", entries)
	}
	if records, err := fixture.ledger.List(); err != nil || len(records) != 0 {
		t.Fatalf("published pending record remains = %#v err=%v", records, err)
	}
}

func TestIngestPendingLedgerPrecedesAgePublication(t *testing.T) {
	fixture := newTransitionFixture(t, defaultSpoolOps(), &fakeBackend{objects: make(map[string]bool), createVisible: true}, func(encryptor *Encryptor, now func() time.Time) serviceOps {
		return serviceOps{now: now, encryptFile: encryptor.EncryptFile, crash: crashAt(crashAfterPendingLedger)}
	})
	archive := validRecoveryArchive(t, "pending-before-age")
	_, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive)))
	if err == nil || err.Error() != "collector operation interrupted" {
		t.Fatalf("Ingest error = %v", err)
	}
	objects, discoverErr := fixture.spool.Discover()
	if discoverErr != nil || len(objects) != 0 {
		t.Fatalf("complete objects before rename = %#v err=%v", objects, discoverErr)
	}
	records, listErr := fixture.ledger.List()
	if listErr != nil || len(records) != 1 {
		t.Fatalf("pending ledger = %#v err=%v", records, listErr)
	}
	if records[0].CompressedSize != int64(len(archive)) || records[0].EncryptedSize <= 0 || records[0].StoredAt != nil {
		t.Fatalf("pending ledger fields = %#v", records[0])
	}
}

func TestIngestRenameFailureCleansNewPendingRecordAndOwnedFiles(t *testing.T) {
	spoolOps := defaultSpoolOps()
	spoolOps.renameNoReplace = func(int, string, int, string) error { return errors.New("rename failure") }
	fixture := newTransitionFixture(t, spoolOps, &fakeBackend{objects: make(map[string]bool), createVisible: true}, nil)
	archive := validRecoveryArchive(t, "rename-cleanup")
	_, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive)))
	if err == nil || err.Error() != "collector local state failed" {
		t.Fatalf("Ingest error = %v", err)
	}
	if entries := spoolEntryNames(t, fixture.spool.path); len(entries) != 0 {
		t.Fatalf("rename failure entries = %v", entries)
	}
	if records, err := fixture.ledger.List(); err != nil || len(records) != 0 {
		t.Fatalf("orphan ledger = %#v err=%v", records, err)
	}
}

func TestWorkerCleansOrphanPendingLedgerAndPartial(t *testing.T) {
	backend := &fakeBackend{objects: make(map[string]bool), createVisible: true}
	fixture := newTransitionFixture(t, defaultSpoolOps(), backend, nil)
	received := fixture.now.Add(-time.Hour)
	record := validTestRecord(received)
	if err := fixture.ledger.Put(record); err != nil {
		t.Fatal(err)
	}
	partial := filepath.Join(fixture.spool.path, testDigestHex+".tar.gz.age.partial")
	if err := os.WriteFile(partial, []byte("recent partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	worker := newWorker(fixture.spool, fixture.ledger, fixture.service, fixture.status, time.Minute, workerOps{
		now:  fixture.clock,
		wait: func(context.Context, time.Duration) error { cancel(); return context.Canceled },
	})
	if err := worker.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(partial); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan partial remains: %v", err)
	}
	if _, found, err := fixture.ledger.Get(record.ObjectID); err != nil || found {
		t.Fatalf("orphan pending record found=%v err=%v", found, err)
	}
	if backend.existsCalls != 0 || backend.createCalls != 0 {
		t.Fatalf("backend called for orphan: exists=%d create=%d", backend.existsCalls, backend.createCalls)
	}
}

func TestWorkerMissingLedgerAgeIsTerminalWithoutRemoteCall(t *testing.T) {
	backend := &fakeBackend{objects: make(map[string]bool), createVisible: true}
	fixture := newTransitionFixture(t, defaultSpoolOps(), backend, nil)
	digest := digestHex([]byte("missing-ledger-age"))
	path := filepath.Join(fixture.spool.path, digest+".tar.gz.age")
	if err := os.WriteFile(path, []byte("ciphertext"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := NewWorker(fixture.spool, fixture.ledger, fixture.service, fixture.status, time.Minute).Run(context.Background())
	if err == nil || err.Error() != "collector local state failed" {
		t.Fatalf("Run error = %v", err)
	}
	if backend.existsCalls != 0 || backend.createCalls != 0 {
		t.Fatalf("backend called for corrupt state: exists=%d create=%d", backend.existsCalls, backend.createCalls)
	}
}

func TestLedgerDeletePendingRefusesStoredRecord(t *testing.T) {
	ledger := openTestLedger(t, defaultLedgerOps())
	record := validTestRecord(time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC))
	storedAt := record.ReceivedAt.Add(time.Minute)
	record.StoredAt = &storedAt
	if err := ledger.Put(record); err != nil {
		t.Fatal(err)
	}
	if err := ledger.DeletePending(record.ObjectID); err == nil || err.Error() != "collector stored ledger record protected" {
		t.Fatalf("DeletePending error = %v", err)
	}
	if _, found, err := ledger.Get(record.ObjectID); err != nil || !found {
		t.Fatalf("stored record found=%v err=%v", found, err)
	}
}

func TestWorkerNeverDeletesStoredOrphanLedger(t *testing.T) {
	backend := &fakeBackend{objects: make(map[string]bool), createVisible: true}
	fixture := newTransitionFixture(t, defaultSpoolOps(), backend, nil)
	record := validTestRecord(fixture.now.Add(-time.Hour))
	stored := fixture.now.Add(-time.Minute)
	record.StoredAt = &stored
	if err := fixture.ledger.Put(record); err != nil {
		t.Fatal(err)
	}
	backend.set(record.ObjectID, true)
	ctx, cancel := context.WithCancel(context.Background())
	worker := newWorker(fixture.spool, fixture.ledger, fixture.service, fixture.status, time.Minute, workerOps{
		now:  fixture.clock,
		wait: func(context.Context, time.Duration) error { cancel(); return context.Canceled },
	})
	if err := worker.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, found, err := fixture.ledger.Get(record.ObjectID); err != nil || !found {
		t.Fatalf("stored orphan was deleted: found=%v err=%v", found, err)
	}
}

func TestServiceNineCrashBoundariesRestartToExactState(t *testing.T) {
	points := []serviceCrashPoint{
		crashAfterUploadPartial,
		crashAfterAgePartial,
		crashAfterPendingLedger,
		crashAfterAgeRename,
		crashAfterPlaintextRemoval,
		crashAfterRemoteCreate,
		crashAfterRemotePresence,
		crashAfterLedgerUpdate,
		crashAfterAgeRemoval,
	}
	for _, point := range points {
		t.Run(string(point), func(t *testing.T) {
			backend := &fakeBackend{objects: make(map[string]bool), createVisible: true}
			presenceQueries := 0
			fixture := newTransitionFixture(t, defaultSpoolOps(), backend, func(encryptor *Encryptor, now func() time.Time) serviceOps {
				return serviceOps{
					now: now, encryptFile: encryptor.EncryptFile,
					crash: func(got serviceCrashPoint) error {
						if got == crashAfterRemotePresence && point == crashAfterRemotePresence {
							presenceQueries++
							if presenceQueries < 2 {
								return nil
							}
						}
						if got == point {
							return errors.New("simulated crash")
						}
						return nil
					},
				}
			})
			archive := validRecoveryArchive(t, "nine-crash-"+string(point))
			objectID := "sha256:" + digestHex(archive)
			_, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive)))
			if err == nil || err.Error() != "collector operation interrupted" {
				t.Fatalf("Ingest error = %v", err)
			}
			spoolPath, ledgerPath := fixture.spool.path, fixture.ledger.path
			if err := fixture.spool.Close(); err != nil {
				t.Fatal(err)
			}
			if err := fixture.ledger.Close(); err != nil {
				t.Fatal(err)
			}
			restartedSpool, err := OpenSpool(spoolPath, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			defer restartedSpool.Close()
			restartedLedger, err := OpenLedger(ledgerPath)
			if err != nil {
				t.Fatal(err)
			}
			defer restartedLedger.Close()
			status := NewStatusTracker(ReadinessConfig{StartedAt: fixture.now, StartupGrace: time.Hour, MaxRecoveryAge: 24 * time.Hour}, fixture.clock)
			restartedService := newService(restartedSpool, restartedLedger, fixture.service.encryptor, backend, status, recoveryarchive.DefaultLimits(), serviceOps{now: fixture.clock})
			ctx, cancel := context.WithCancel(context.Background())
			worker := newWorker(restartedSpool, restartedLedger, restartedService, status, time.Minute, workerOps{
				now:  fixture.clock,
				wait: func(context.Context, time.Duration) error { cancel(); return context.Canceled },
			})
			if err := worker.Run(ctx); err != nil {
				t.Fatalf("restart Run: %v", err)
			}
			if entries := spoolEntryNames(t, spoolPath); len(entries) != 0 {
				t.Fatalf("restart spool entries = %v", entries)
			}
			record, found, getErr := restartedLedger.Get(objectID)
			early := point == crashAfterUploadPartial || point == crashAfterAgePartial || point == crashAfterPendingLedger
			if early {
				if getErr != nil || found {
					t.Fatalf("early crash ledger found=%v record=%#v err=%v", found, record, getErr)
				}
				if backend.existsCalls != 0 || backend.createCalls != 0 {
					t.Fatalf("early crash backend calls: exists=%d create=%d", backend.existsCalls, backend.createCalls)
				}
				return
			}
			if getErr != nil || !found || record.StoredAt == nil {
				t.Fatalf("recovered record = %#v found=%v err=%v", record, found, getErr)
			}
			if record.CompressedSize != int64(len(archive)) || record.EncryptedSize <= 0 {
				t.Fatalf("recovered exact sizes = %#v", record)
			}
			if !backend.has(objectID) {
				t.Fatal("remote object absent after recovery")
			}
			if backend.createCalls != 1 {
				t.Fatalf("Create calls = %d", backend.createCalls)
			}
		})
	}
}

type transitionFixture struct {
	spool   *Spool
	ledger  *Ledger
	backend Backend
	status  *StatusTracker
	service *Service
	now     time.Time
	clock   func() time.Time
}

func newTransitionFixture(t testing.TB, spoolOps spoolOps, backend Backend, makeOps func(*Encryptor, func() time.Time) serviceOps) *transitionFixture {
	t.Helper()
	return newTransitionFixtureWithLedgerOps(t, spoolOps, defaultLedgerOps(), backend, makeOps)
}

func newTransitionFixtureWithLedgerOps(t testing.TB, spoolOps spoolOps, ledgerOps ledgerOps, backend Backend, makeOps func(*Encryptor, func() time.Time) serviceOps) *transitionFixture {
	t.Helper()
	spool := openTestSpool(t, spoolOps)
	ledger := openTestLedger(t, ledgerOps)
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	status := NewStatusTracker(ReadinessConfig{StartedAt: now, StartupGrace: time.Hour, MaxRecoveryAge: 24 * time.Hour}, clock)
	status.SetSpoolWritable(true)
	encryptor := NewEncryptor(generateTestIdentity(t).Recipient())
	ops := serviceOps{now: clock, validateFile: recoveryarchive.ValidateFile, encryptFile: encryptor.EncryptFile}
	if makeOps != nil {
		custom := makeOps(encryptor, clock)
		if custom.now != nil {
			ops.now = custom.now
		}
		if custom.validateFile != nil {
			ops.validateFile = custom.validateFile
		}
		if custom.encryptFile != nil {
			ops.encryptFile = custom.encryptFile
		}
		if custom.crash != nil {
			ops.crash = custom.crash
		}
	}
	service := newService(spool, ledger, encryptor, backend, status, recoveryarchive.DefaultLimits(), ops)
	return &transitionFixture{spool: spool, ledger: ledger, backend: backend, status: status, service: service, now: now, clock: clock}
}

type commitBarrierBackend struct {
	mu          sync.Mutex
	objects     map[string]bool
	entered     chan struct{}
	release     chan struct{}
	enterOnce   sync.Once
	existsCalls int
}

func (b *commitBarrierBackend) Exists(_ context.Context, objectID string) (bool, error) {
	b.mu.Lock()
	b.existsCalls++
	call := b.existsCalls
	present := b.objects[objectID]
	b.mu.Unlock()
	if call == 1 {
		b.enterOnce.Do(func() { close(b.entered) })
		<-b.release
	}
	return present, nil
}

func (b *commitBarrierBackend) Create(_ context.Context, object PendingObject) error {
	b.mu.Lock()
	b.objects[object.ObjectID] = true
	b.mu.Unlock()
	return nil
}

func (*commitBarrierBackend) List(context.Context) ([]ArchiveInfo, error) { return nil, nil }

var _ Backend = (*commitBarrierBackend)(nil)

type firstExistsFailureBackend struct {
	mu      sync.Mutex
	objects map[string]bool
	calls   chan int
	count   int
}

func (b *firstExistsFailureBackend) Exists(context.Context, string) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.count++
	if b.count > 1 {
		select {
		case b.calls <- b.count:
		default:
		}
	}
	if b.count == 1 {
		return false, errors.New("first exists failure")
	}
	for _, present := range b.objects {
		if present {
			return true, nil
		}
	}
	return false, nil
}

func (b *firstExistsFailureBackend) Create(_ context.Context, object PendingObject) error {
	b.mu.Lock()
	b.objects[object.ObjectID] = true
	b.mu.Unlock()
	return nil
}

func (*firstExistsFailureBackend) List(context.Context) ([]ArchiveInfo, error) { return nil, nil }

var _ Backend = (*firstExistsFailureBackend)(nil)
