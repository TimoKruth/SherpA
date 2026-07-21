package collector

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

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
	select {
	case err := <-secondDone:
		if err == nil || err.Error() != "collector backend cancelled" {
			t.Fatalf("canceled CommitPending error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled commit waiter remained blocked behind holder")
	}
	select {
	case err := <-firstDone:
		t.Fatalf("commit holder returned before release: %v", err)
	default:
	}
	close(backend.release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first CommitPending: %v", err)
	}
	backend.mu.Lock()
	existsCalls := backend.existsCalls
	backend.mu.Unlock()
	if existsCalls != 2 {
		t.Fatalf("Exists calls = %d, canceled waiter reached backend", existsCalls)
	}
}

func TestIngestPublicationWaiterCancellationReturnsBeforeHolderRelease(t *testing.T) {
	partialReady := make(chan struct{})
	releaseHolder := make(chan struct{})
	var encryptCalls atomic.Int32
	backend := &fakeBackend{objects: make(map[string]bool), createVisible: true}
	fixture := newTransitionFixture(t, defaultSpoolOps(), backend, func(encryptor *Encryptor, now func() time.Time) serviceOps {
		return serviceOps{
			now: now,
			encryptFile: func(ctx context.Context, sourcePath, partialPath string) (int64, error) {
				size, err := encryptor.EncryptFile(ctx, sourcePath, partialPath)
				if err == nil && encryptCalls.Add(1) == 1 {
					close(partialReady)
					<-releaseHolder
				}
				return size, err
			},
		}
	})
	archive := validRecoveryArchive(t, "cancel-publication-waiter")
	holderDone := make(chan error, 1)
	go func() {
		_, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive)))
		holderDone <- err
	}()
	<-partialReady

	ctx, cancel := context.WithCancel(context.Background())
	waiterDone := make(chan error, 1)
	go func() {
		_, err := fixture.service.Ingest(ctx, testReadCloser(archive), int64(len(archive)))
		waiterDone <- err
	}()
	select {
	case err := <-waiterDone:
		t.Fatalf("publication waiter returned before cancellation: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-waiterDone:
		if err == nil || err.Error() != "collector ingestion canceled" {
			t.Fatalf("publication waiter error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled publication waiter remained blocked behind holder")
	}
	select {
	case err := <-holderDone:
		t.Fatalf("publication holder returned before release: %v", err)
	default:
	}
	close(releaseHolder)
	if err := <-holderDone; err != nil {
		t.Fatalf("publication holder: %v", err)
	}
	assertNoActiveSpoolReservations(t, fixture.spool)
	if fixture.status.Snapshot().TerminalLocalError {
		t.Fatal("publication gate cancellation set terminal readiness")
	}
}

func TestIngestConcurrentDuplicateWaitsForOwnedPartialAndReusesWinner(t *testing.T) {
	partialReady := make(chan struct{})
	releaseFirst := make(chan struct{})
	var encryptCalls atomic.Int32
	var renameCalls atomic.Int32
	spoolOps := defaultSpoolOps()
	rename := spoolOps.renameNoReplace
	spoolOps.renameNoReplace = func(oldDirFD int, oldName string, newDirFD int, newName string) error {
		err := rename(oldDirFD, oldName, newDirFD, newName)
		if err == nil && completeAgePattern.MatchString(newName) {
			renameCalls.Add(1)
		}
		return err
	}
	backend := &fakeBackend{objects: make(map[string]bool), createVisible: true}
	fixture := newTransitionFixture(t, spoolOps, backend, func(encryptor *Encryptor, now func() time.Time) serviceOps {
		return serviceOps{
			now: now,
			encryptFile: func(ctx context.Context, sourcePath, partialPath string) (int64, error) {
				size, err := encryptor.EncryptFile(ctx, sourcePath, partialPath)
				if err == nil && encryptCalls.Add(1) == 1 {
					close(partialReady)
					<-releaseFirst
				}
				return size, err
			},
		}
	})
	archive := validRecoveryArchive(t, "duplicate-owned-partial")
	results := make(chan Result, 2)
	errs := make(chan error, 2)
	go func() {
		result, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive)))
		results <- result
		errs <- err
	}()
	<-partialReady
	go func() {
		result, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive)))
		results <- result
		errs <- err
	}()
	select {
	case err := <-errs:
		t.Fatalf("duplicate returned while winner owned partial: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
	close(releaseFirst)
	statuses := make(map[ResultStatus]int)
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("Ingest caller %d: %v", i, err)
		}
		statuses[(<-results).Status]++
	}
	if renameCalls.Load() != 1 {
		t.Fatalf("canonical publication count = %d", renameCalls.Load())
	}
	if statuses[ResultStored]+statuses[ResultExisting] != 2 {
		t.Fatalf("statuses = %#v", statuses)
	}
	if entries := spoolEntryNames(t, fixture.spool.path); len(entries) != 0 {
		t.Fatalf("duplicate publication entries = %v", entries)
	}
	assertNoActiveSpoolReservations(t, fixture.spool)
	objectID := "sha256:" + digestHex(archive)
	record, found, err := fixture.ledger.Get(objectID)
	if err != nil || !found || record.StoredAt == nil {
		t.Fatalf("stored ledger = %#v found=%v err=%v", record, found, err)
	}
	if fixture.status.Snapshot().TerminalLocalError {
		t.Fatal("normal duplicate publication set terminal readiness")
	}
}

func TestIngestConcurrentDuplicateWaitsUntilWinnerFinishesPostRename(t *testing.T) {
	renamed := make(chan struct{})
	releaseWinner := make(chan struct{})
	var once sync.Once
	var renameCalls atomic.Int32
	spoolOps := defaultSpoolOps()
	rename := spoolOps.renameNoReplace
	spoolOps.renameNoReplace = func(oldDirFD int, oldName string, newDirFD int, newName string) error {
		err := rename(oldDirFD, oldName, newDirFD, newName)
		if err == nil && completeAgePattern.MatchString(newName) {
			renameCalls.Add(1)
		}
		return err
	}
	fixture := newTransitionFixture(t, spoolOps, &fakeBackend{objects: make(map[string]bool), createVisible: true}, func(encryptor *Encryptor, now func() time.Time) serviceOps {
		return serviceOps{
			now:         now,
			encryptFile: encryptor.EncryptFile,
			crash: func(point serviceCrashPoint) error {
				if point == crashAfterAgeRename {
					once.Do(func() { close(renamed) })
					<-releaseWinner
				}
				return nil
			},
		}
	})
	archive := validRecoveryArchive(t, "duplicate-after-rename")
	done := make(chan error, 2)
	go func() {
		_, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive)))
		done <- err
	}()
	<-renamed
	go func() {
		_, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive)))
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("duplicate escaped publication transaction after rename: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
	close(releaseWinner)
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatalf("Ingest caller %d: %v", i, err)
		}
	}
	if renameCalls.Load() != 1 {
		t.Fatalf("canonical publication count = %d", renameCalls.Load())
	}
	if entries := spoolEntryNames(t, fixture.spool.path); len(entries) != 0 {
		t.Fatalf("post-rename duplicate entries = %v", entries)
	}
	assertNoActiveSpoolReservations(t, fixture.spool)
	if fixture.status.Snapshot().TerminalLocalError {
		t.Fatal("post-rename duplicate set terminal readiness")
	}
}

func TestIngestDuplicateJoinsWinnerCommitCompletion(t *testing.T) {
	tests := []struct {
		name             string
		beforeWaiter     func(*fakeBackend, string)
		wantEncryptions  int32
		wantRenames      int32
		wantCreates      int
		wantExists       int
		wantWaiterStatus ResultStatus
		wantWaiterError  string
		wantBound        int
		wantPending      bool
	}{
		{
			name:             "remote remains present",
			wantEncryptions:  1,
			wantRenames:      1,
			wantCreates:      1,
			wantExists:       3,
			wantWaiterStatus: ResultExisting,
		},
		{
			name: "remote removed before waiter proof",
			beforeWaiter: func(backend *fakeBackend, objectID string) {
				backend.set(objectID, false)
			},
			wantEncryptions:  2,
			wantRenames:      2,
			wantCreates:      2,
			wantExists:       5,
			wantWaiterStatus: ResultStored,
		},
		{
			name: "backend unavailable during waiter proof",
			beforeWaiter: func(backend *fakeBackend, _ string) {
				backend.existsErr = errors.New("offline")
			},
			wantEncryptions: 1,
			wantRenames:     1,
			wantCreates:     1,
			wantExists:      3,
			wantWaiterError: "collector backend unavailable",
			wantBound:       1,
			wantPending:     true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			winnerAtCleanup := make(chan struct{})
			releaseWinner := make(chan struct{})
			waiterWaiting := make(chan struct{})
			secondPublished := make(chan struct{})
			var cleanupOnce sync.Once
			var waitOnce sync.Once
			var publishOnce sync.Once
			var encryptions atomic.Int32
			var renames atomic.Int32
			spoolOps := defaultSpoolOps()
			rename := spoolOps.renameNoReplace
			spoolOps.renameNoReplace = func(oldDirFD int, oldName string, newDirFD int, newName string) error {
				err := rename(oldDirFD, oldName, newDirFD, newName)
				if err == nil && completeAgePattern.MatchString(newName) {
					if renames.Add(1) == 2 {
						publishOnce.Do(func() { close(secondPublished) })
					}
				}
				return err
			}
			backend := &fakeBackend{objects: make(map[string]bool), createVisible: true}
			fixture := newTransitionFixture(t, spoolOps, backend, func(encryptor *Encryptor, now func() time.Time) serviceOps {
				return serviceOps{
					now: now,
					encryptFile: func(ctx context.Context, sourcePath, partialPath string) (int64, error) {
						encryptions.Add(1)
						return encryptor.EncryptFile(ctx, sourcePath, partialPath)
					},
					publishWait: func() {
						waitOnce.Do(func() { close(waiterWaiting) })
					},
					crash: func(point serviceCrashPoint) error {
						if point == crashAfterAgeRemoval {
							cleanupOnce.Do(func() {
								close(winnerAtCleanup)
								<-releaseWinner
							})
						}
						return nil
					},
				}
			})
			archive := validRecoveryArchive(t, "join-commit-"+test.name)
			objectID := "sha256:" + digestHex(archive)
			type outcome struct {
				result Result
				err    error
			}
			winnerDone := make(chan outcome, 1)
			waiterDone := make(chan outcome, 1)
			go func() {
				result, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive)))
				winnerDone <- outcome{result: result, err: err}
			}()
			<-winnerAtCleanup
			storedBefore, found, err := fixture.ledger.Get(objectID)
			if err != nil || !found || storedBefore.StoredAt == nil {
				t.Fatalf("winner stored record = %#v found=%v err=%v", storedBefore, found, err)
			}
			go func() {
				result, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive)))
				waiterDone <- outcome{result: result, err: err}
			}()
			select {
			case <-waiterWaiting:
				if test.beforeWaiter != nil {
					test.beforeWaiter(backend, objectID)
				}
				close(releaseWinner)
			case <-secondPublished:
				close(releaseWinner)
				winner := <-winnerDone
				waiter := <-waiterDone
				t.Fatalf("waiter republished before winner completion: winner=%#v waiter=%#v renames=%d", winner, waiter, renames.Load())
			}
			winner := <-winnerDone
			waiter := <-waiterDone
			if winner.err != nil || winner.result.ObjectID != objectID || winner.result.Status != ResultStored {
				t.Fatalf("winner outcome = %#v err=%v", winner.result, winner.err)
			}
			if test.wantWaiterError == "" {
				if waiter.err != nil || waiter.result.ObjectID != objectID || waiter.result.Status != test.wantWaiterStatus {
					t.Fatalf("waiter outcome = %#v err=%v", waiter.result, waiter.err)
				}
			} else if waiter.err == nil || waiter.err.Error() != test.wantWaiterError {
				t.Fatalf("waiter error = %v", waiter.err)
			}
			if encryptions.Load() != test.wantEncryptions || renames.Load() != test.wantRenames {
				t.Fatalf("publication counts encryptions=%d renames=%d, want %d/%d", encryptions.Load(), renames.Load(), test.wantEncryptions, test.wantRenames)
			}
			if backend.createCalls != test.wantCreates || backend.existsCalls != test.wantExists {
				t.Fatalf("backend calls create=%d exists=%d, want %d/%d", backend.createCalls, backend.existsCalls, test.wantCreates, test.wantExists)
			}
			storedAfter, found, err := fixture.ledger.Get(objectID)
			if err != nil || !found || storedAfter.StoredAt == nil || !storedAfter.StoredAt.Equal(*storedBefore.StoredAt) || !storedAfter.ReceivedAt.Equal(storedBefore.ReceivedAt) {
				t.Fatalf("final stored record = %#v found=%v err=%v", storedAfter, found, err)
			}
			if test.wantPending {
				snapshot := fixture.status.Snapshot()
				if snapshot.OldestPendingAt == nil || !snapshot.OldestPendingAt.Equal(storedBefore.ReceivedAt) || snapshot.TerminalLocalError {
					t.Fatalf("pending readiness = %#v", snapshot)
				}
			} else if snapshot := fixture.status.Snapshot(); snapshot.OldestPendingAt != nil || snapshot.TerminalLocalError || !fixture.status.Ready() {
				t.Fatalf("final readiness = %#v ready=%v", snapshot, fixture.status.Ready())
			}
			assertPlaintextNamespace(t, fixture.spool.path, 0, test.wantBound)
			if entries := spoolEntryNames(t, fixture.spool.path); len(entries) != test.wantBound {
				t.Fatalf("final spool entries = %v", entries)
			}
			assertNoActiveSpoolReservations(t, fixture.spool)
		})
	}
}

func TestIngestCancelledJoinedRepairRetainsBoundForWorker(t *testing.T) {
	winnerAtCleanup := make(chan struct{})
	releaseWinner := make(chan struct{})
	waiterWaiting := make(chan struct{})
	repairEncryptStarted := make(chan struct{})
	var cleanupOnce sync.Once
	var waitOnce sync.Once
	var repairOnce sync.Once
	var encryptions atomic.Int32
	var renames atomic.Int32
	spoolOps := defaultSpoolOps()
	rename := spoolOps.renameNoReplace
	spoolOps.renameNoReplace = func(oldDirFD int, oldName string, newDirFD int, newName string) error {
		err := rename(oldDirFD, oldName, newDirFD, newName)
		if err == nil && completeAgePattern.MatchString(newName) {
			renames.Add(1)
		}
		return err
	}
	backend := &fakeBackend{objects: make(map[string]bool), createVisible: true}
	fixture := newTransitionFixture(t, spoolOps, backend, func(encryptor *Encryptor, now func() time.Time) serviceOps {
		return serviceOps{
			now: now,
			encryptFile: func(ctx context.Context, sourcePath, partialPath string) (int64, error) {
				if encryptions.Add(1) == 2 {
					repairOnce.Do(func() { close(repairEncryptStarted) })
					<-ctx.Done()
					return 0, ctx.Err()
				}
				return encryptor.EncryptFile(ctx, sourcePath, partialPath)
			},
			publishWait: func() {
				waitOnce.Do(func() { close(waiterWaiting) })
			},
			crash: func(point serviceCrashPoint) error {
				if point == crashAfterAgeRemoval {
					cleanupOnce.Do(func() {
						close(winnerAtCleanup)
						<-releaseWinner
					})
				}
				return nil
			},
		}
	})
	archive := validRecoveryArchive(t, "cancel-joined-stored-repair")
	objectID := "sha256:" + digestHex(archive)
	type outcome struct {
		result Result
		err    error
	}
	winnerDone := make(chan outcome, 1)
	go func() {
		result, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive)))
		winnerDone <- outcome{result: result, err: err}
	}()
	<-winnerAtCleanup
	storedBefore, found, err := fixture.ledger.Get(objectID)
	if err != nil || !found || storedBefore.StoredAt == nil {
		t.Fatalf("winner stored record = %#v found=%v err=%v", storedBefore, found, err)
	}
	waiterCtx, cancelWaiter := context.WithCancel(context.Background())
	defer cancelWaiter()
	waiterDone := make(chan outcome, 1)
	go func() {
		result, err := fixture.service.Ingest(waiterCtx, testReadCloser(archive), int64(len(archive)))
		waiterDone <- outcome{result: result, err: err}
	}()
	<-waiterWaiting
	backend.set(objectID, false)
	close(releaseWinner)
	winner := <-winnerDone
	if winner.err != nil || winner.result.ObjectID != objectID || winner.result.Status != ResultStored {
		t.Fatalf("winner outcome = %#v err=%v", winner.result, winner.err)
	}
	<-repairEncryptStarted
	cancelWaiter()
	select {
	case waiter := <-waiterDone:
		if waiter.err == nil || waiter.err.Error() != "collector ingestion canceled" {
			t.Fatalf("waiter outcome = %#v err=%v", waiter.result, waiter.err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled repair waiter did not return promptly")
	}
	if encryptions.Load() != 2 || renames.Load() != 1 || backend.createCalls != 1 || backend.existsCalls != 3 {
		t.Fatalf("cancelled repair calls encrypt=%d rename=%d create=%d exists=%d", encryptions.Load(), renames.Load(), backend.createCalls, backend.existsCalls)
	}
	assertRetainedStoredRepair(t, fixture, archive, storedBefore, false)

	restartedSpool, restartedLedger := reopenRecoveryFixture(t, fixture.spool, fixture.ledger)
	restartedStatus := NewStatusTracker(ReadinessConfig{StartedAt: fixture.now, StartupGrace: time.Hour, MaxRecoveryAge: 24 * time.Hour}, fixture.clock)
	restartedService := newService(restartedSpool, restartedLedger, fixture.service.encryptor, backend, restartedStatus, recoveryarchive.DefaultLimits(), serviceOps{now: fixture.clock})
	runWorkerOnce(t, restartedSpool, restartedLedger, restartedService, restartedStatus, fixture.clock)
	if !backend.has(objectID) || backend.createCalls != 2 || backend.existsCalls != 5 {
		t.Fatalf("worker repair remote=%v create=%d exists=%d", backend.has(objectID), backend.createCalls, backend.existsCalls)
	}
	storedAfter, found, err := restartedLedger.Get(objectID)
	if err != nil || !found || storedAfter.StoredAt == nil || !storedAfter.StoredAt.Equal(*storedBefore.StoredAt) ||
		!storedAfter.ReceivedAt.Equal(storedBefore.ReceivedAt) || storedAfter.RetryCount != storedBefore.RetryCount ||
		!equalOptionalTime(storedAfter.LastAttemptAt, storedBefore.LastAttemptAt) || storedAfter.LatestRetryClass != storedBefore.LatestRetryClass {
		t.Fatalf("worker repair record = %#v found=%v err=%v", storedAfter, found, err)
	}
	assertStoredRecoveryComplete(t, restartedSpool, restartedLedger, restartedStatus, objectID, int64(len(archive)))
}

func TestIngestStoredRepairFinalizationFailureRetainsBoundForRestart(t *testing.T) {
	backend := &fakeBackend{objects: make(map[string]bool), createVisible: true}
	fixture := newTransitionFixture(t, defaultSpoolOps(), backend, nil)
	archive := validRecoveryArchive(t, "stored-repair-finalization-failure")
	objectID := "sha256:" + digestHex(archive)
	if _, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive))); err != nil {
		t.Fatalf("initial Ingest: %v", err)
	}
	storedBefore, found, err := fixture.ledger.Get(objectID)
	if err != nil || !found || storedBefore.StoredAt == nil {
		t.Fatalf("initial stored record = %#v found=%v err=%v", storedBefore, found, err)
	}
	backend.set(objectID, false)
	encryptor := fixture.service.encryptor
	fixture.service = newService(fixture.spool, fixture.ledger, encryptor, backend, fixture.status, recoveryarchive.DefaultLimits(), serviceOps{
		now: fixture.clock,
		encryptFile: func(ctx context.Context, sourcePath, partialPath string) (int64, error) {
			return encryptor.encryptFile(ctx, sourcePath, partialPath, encryptionFileFinalizer{
				ageClose: func(writer io.Closer) error {
					if err := writer.Close(); err != nil {
						return err
					}
					return errors.New("forced finalization failure")
				},
			})
		},
	})
	_, err = fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive)))
	if err == nil || err.Error() != "collector encryption failed" {
		t.Fatalf("repair Ingest error = %v", err)
	}
	if backend.createCalls != 1 || backend.existsCalls != 3 {
		t.Fatalf("failed repair backend create=%d exists=%d", backend.createCalls, backend.existsCalls)
	}
	assertRetainedStoredRepair(t, fixture, archive, storedBefore, false)

	restartedSpool, restartedLedger := reopenRecoveryFixture(t, fixture.spool, fixture.ledger)
	restartedStatus := NewStatusTracker(ReadinessConfig{StartedAt: fixture.now, StartupGrace: time.Hour, MaxRecoveryAge: 24 * time.Hour}, fixture.clock)
	restartedService := newService(restartedSpool, restartedLedger, fixture.service.encryptor, backend, restartedStatus, recoveryarchive.DefaultLimits(), serviceOps{now: fixture.clock})
	runWorkerOnce(t, restartedSpool, restartedLedger, restartedService, restartedStatus, fixture.clock)
	if !backend.has(objectID) || backend.createCalls != 2 || backend.existsCalls != 5 {
		t.Fatalf("restarted repair remote=%v create=%d exists=%d", backend.has(objectID), backend.createCalls, backend.existsCalls)
	}
	storedAfter, found, err := restartedLedger.Get(objectID)
	if err != nil || !found || storedAfter.StoredAt == nil || !storedAfter.StoredAt.Equal(*storedBefore.StoredAt) ||
		!storedAfter.ReceivedAt.Equal(storedBefore.ReceivedAt) || storedAfter.RetryCount != storedBefore.RetryCount ||
		!equalOptionalTime(storedAfter.LastAttemptAt, storedBefore.LastAttemptAt) || storedAfter.LatestRetryClass != storedBefore.LatestRetryClass {
		t.Fatalf("restarted repair record = %#v found=%v err=%v", storedAfter, found, err)
	}
	assertStoredRecoveryComplete(t, restartedSpool, restartedLedger, restartedStatus, objectID, int64(len(archive)))
}

func TestIngestStoredRepairPreRenameFailuresRetainCanonicalBound(t *testing.T) {
	tests := []string{
		"partial fsync",
		"partial close",
		"identity recheck",
		"rename no replace",
	}
	for _, failurePoint := range tests {
		t.Run(failurePoint, func(t *testing.T) {
			archive := validRecoveryArchive(t, "stored-repair-"+failurePoint)
			objectID := "sha256:" + digestHex(archive)
			partialName := digestHex(archive) + ".tar.gz.age.partial"
			var dirFD int
			var armed atomic.Bool
			var fired atomic.Bool
			spoolOps := preRenameFailureOps(failurePoint, partialName, &dirFD, &armed, &fired)
			backend := &fakeBackend{objects: make(map[string]bool), createVisible: true}
			fixture := newTransitionFixture(t, spoolOps, backend, nil)
			dirFD = fixture.spool.dirFD

			if _, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive))); err != nil {
				t.Fatalf("initial Ingest: %v", err)
			}
			storedBefore, found, err := fixture.ledger.Get(objectID)
			if err != nil || !found || storedBefore.StoredAt == nil {
				t.Fatalf("initial stored record = %#v found=%v err=%v", storedBefore, found, err)
			}
			backend.set(objectID, false)
			armed.Store(true)
			_, err = fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive)))
			if err == nil || err.Error() != "collector local state failed" {
				t.Fatalf("repair Ingest error = %v", err)
			}
			if !fired.Load() {
				t.Fatal("pre-rename failure seam did not fire")
			}
			if backend.createCalls != 1 {
				t.Fatalf("failed repair Create calls = %d", backend.createCalls)
			}
			assertRetainedStoredRepair(t, fixture, archive, storedBefore, true)

			armed.Store(false)
			restartedSpool, restartedLedger := reopenRecoveryFixture(t, fixture.spool, fixture.ledger)
			restartedStatus := NewStatusTracker(ReadinessConfig{StartedAt: fixture.now, StartupGrace: time.Hour, MaxRecoveryAge: 24 * time.Hour}, fixture.clock)
			restartedStatus.SetSpoolWritable(true)
			restartedService := newService(restartedSpool, restartedLedger, fixture.service.encryptor, backend, restartedStatus, recoveryarchive.DefaultLimits(), serviceOps{now: fixture.clock})
			runWorkerOnce(t, restartedSpool, restartedLedger, restartedService, restartedStatus, fixture.clock)
			if !backend.has(objectID) || backend.createCalls != 2 {
				t.Fatalf("restarted repair remote=%v create=%d", backend.has(objectID), backend.createCalls)
			}
			storedAfter, found, err := restartedLedger.Get(objectID)
			if err != nil || !found || storedAfter.StoredAt == nil || !storedAfter.StoredAt.Equal(*storedBefore.StoredAt) ||
				!storedAfter.ReceivedAt.Equal(storedBefore.ReceivedAt) || storedAfter.RetryCount != storedBefore.RetryCount ||
				!equalOptionalTime(storedAfter.LastAttemptAt, storedBefore.LastAttemptAt) || storedAfter.LatestRetryClass != storedBefore.LatestRetryClass {
				t.Fatalf("restarted repair record = %#v found=%v err=%v", storedAfter, found, err)
			}
			assertStoredRecoveryComplete(t, restartedSpool, restartedLedger, restartedStatus, objectID, int64(len(archive)))
		})
	}
}

func TestIngestPreExistingBoundCancellationRetainsCanonicalRecovery(t *testing.T) {
	tests := []string{"immediate retry", "restart worker"}
	for _, recovery := range tests {
		t.Run(recovery, func(t *testing.T) {
			archive := validRecoveryArchive(t, "preexisting-cancel-"+recovery)
			backend := &fakeBackend{objects: make(map[string]bool), createVisible: true}
			fixture, encryptor := interruptedBoundFixture(t, archive, backend)
			encryptionStarted := make(chan struct{})
			var calls atomic.Int32
			fixture.service = newService(fixture.spool, fixture.ledger, encryptor, backend, fixture.status, recoveryarchive.DefaultLimits(), serviceOps{
				now: fixture.clock,
				encryptFile: func(ctx context.Context, sourcePath, partialPath string) (int64, error) {
					if calls.Add(1) == 1 {
						close(encryptionStarted)
						<-ctx.Done()
						return 0, ctx.Err()
					}
					return encryptor.EncryptFile(ctx, sourcePath, partialPath)
				},
			})
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() {
				_, err := fixture.service.Ingest(ctx, testReadCloser(archive), int64(len(archive)))
				done <- err
			}()
			<-encryptionStarted
			cancel()
			if err := <-done; err == nil || err.Error() != "collector ingestion canceled" {
				t.Fatalf("cancelled Ingest error = %v", err)
			}
			assertRetainedUnledgeredBound(t, fixture, archive, false)
			if backend.createCalls != 0 {
				t.Fatalf("Create calls after cancellation = %d", backend.createCalls)
			}

			objectID := "sha256:" + digestHex(archive)
			switch recovery {
			case "immediate retry":
				result, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive)))
				if err != nil || result.Status != ResultStored || result.ObjectID != objectID {
					t.Fatalf("immediate retry result = %#v err=%v", result, err)
				}
				assertStoredRecoveryComplete(t, fixture.spool, fixture.ledger, fixture.status, objectID, int64(len(archive)))
			case "restart worker":
				restartedSpool, restartedLedger := reopenRecoveryFixture(t, fixture.spool, fixture.ledger)
				restartedStatus := NewStatusTracker(ReadinessConfig{StartedAt: fixture.now, StartupGrace: time.Hour, MaxRecoveryAge: 24 * time.Hour}, fixture.clock)
				restartedStatus.SetSpoolWritable(true)
				restartedService := newService(restartedSpool, restartedLedger, encryptor, backend, restartedStatus, recoveryarchive.DefaultLimits(), serviceOps{now: fixture.clock})
				runWorkerOnce(t, restartedSpool, restartedLedger, restartedService, restartedStatus, fixture.clock)
				assertStoredRecoveryComplete(t, restartedSpool, restartedLedger, restartedStatus, objectID, int64(len(archive)))
			default:
				t.Fatalf("unknown recovery path %q", recovery)
			}
			if backend.createCalls != 1 {
				t.Fatalf("recovery Create calls = %d", backend.createCalls)
			}
		})
	}
}

func TestIngestPreExistingBoundFinalizationFailureRetainsCanonicalRecovery(t *testing.T) {
	archive := validRecoveryArchive(t, "preexisting-finalization-failure")
	objectID := "sha256:" + digestHex(archive)
	backend := &fakeBackend{objects: make(map[string]bool), createVisible: true}
	fixture, encryptor := interruptedBoundFixture(t, archive, backend)
	fixture.service = newService(fixture.spool, fixture.ledger, encryptor, backend, fixture.status, recoveryarchive.DefaultLimits(), serviceOps{
		now: fixture.clock,
		encryptFile: func(ctx context.Context, sourcePath, partialPath string) (int64, error) {
			return encryptor.encryptFile(ctx, sourcePath, partialPath, encryptionFileFinalizer{
				ageClose: func(writer io.Closer) error {
					if err := writer.Close(); err != nil {
						return err
					}
					return errors.New("forced finalization failure")
				},
			})
		},
	})
	_, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive)))
	if err == nil || err.Error() != "collector encryption failed" {
		t.Fatalf("Ingest error = %v", err)
	}
	assertRetainedUnledgeredBound(t, fixture, archive, false)
	if backend.createCalls != 0 {
		t.Fatalf("Create calls after finalization failure = %d", backend.createCalls)
	}

	restartedSpool, restartedLedger := reopenRecoveryFixture(t, fixture.spool, fixture.ledger)
	restartedStatus := NewStatusTracker(ReadinessConfig{StartedAt: fixture.now, StartupGrace: time.Hour, MaxRecoveryAge: 24 * time.Hour}, fixture.clock)
	restartedStatus.SetSpoolWritable(true)
	restartedService := newService(restartedSpool, restartedLedger, encryptor, backend, restartedStatus, recoveryarchive.DefaultLimits(), serviceOps{now: fixture.clock})
	runWorkerOnce(t, restartedSpool, restartedLedger, restartedService, restartedStatus, fixture.clock)
	if backend.createCalls != 1 {
		t.Fatalf("restarted Create calls = %d", backend.createCalls)
	}
	assertStoredRecoveryComplete(t, restartedSpool, restartedLedger, restartedStatus, objectID, int64(len(archive)))
}

func interruptedBoundFixture(t testing.TB, archive []byte, backend Backend) (*transitionFixture, *Encryptor) {
	t.Helper()
	initial := newTransitionFixture(t, defaultSpoolOps(), backend, nil)
	initial.service.ops.crash = crashAt(crashAfterPlaintextBind)
	_, err := initial.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive)))
	if err == nil || err.Error() != "collector operation interrupted" {
		t.Fatalf("interrupted Ingest error = %v", err)
	}
	if entries := spoolEntryNames(t, initial.spool.path); len(entries) != 1 || !plainPendingPattern.MatchString(entries[0]) {
		t.Fatalf("interrupted bound entries = %v", entries)
	}
	encryptor := initial.service.encryptor
	spool, ledger := reopenRecoveryFixture(t, initial.spool, initial.ledger)
	status := NewStatusTracker(ReadinessConfig{StartedAt: initial.now, StartupGrace: time.Hour, MaxRecoveryAge: 24 * time.Hour}, initial.clock)
	status.SetSpoolWritable(true)
	return &transitionFixture{
		spool: spool, ledger: ledger, backend: backend, status: status,
		now: initial.now, clock: initial.clock,
	}, encryptor
}

func assertRetainedUnledgeredBound(t testing.TB, fixture *transitionFixture, archive []byte, terminal bool) {
	t.Helper()
	objects, err := fixture.spool.DiscoverPlaintexts()
	if err != nil || len(objects) != 1 || objects[0].ObjectID != "sha256:"+digestHex(archive) ||
		objects[0].CompressedSize != int64(len(archive)) || !objects[0].ReceivedAt.Equal(fixture.now) {
		t.Fatalf("retained plaintexts = %#v err=%v", objects, err)
	}
	contents, err := os.ReadFile(objects[0].Path)
	if err != nil || !bytes.Equal(contents, archive) {
		t.Fatalf("retained plaintext content mismatch err=%v", err)
	}
	var metadata unix.Stat_t
	if err := unix.Lstat(objects[0].Path, &metadata); err != nil || metadata.Mode&unix.S_IFMT != unix.S_IFREG || metadata.Mode&0o7777 != 0o600 ||
		metadata.Uid != uint32(os.Geteuid()) || metadata.Gid != uint32(os.Getegid()) {
		t.Fatalf("retained plaintext metadata mode=%#o uid=%d gid=%d err=%v", metadata.Mode, metadata.Uid, metadata.Gid, err)
	}
	if entries := spoolEntryNames(t, fixture.spool.path); len(entries) != 1 || !plainPendingPattern.MatchString(entries[0]) {
		t.Fatalf("retained bound entries = %v", entries)
	}
	if records, err := fixture.ledger.List(); err != nil || len(records) != 0 {
		t.Fatalf("unexpected ledger records = %#v err=%v", records, err)
	}
	snapshot := fixture.status.Snapshot()
	if snapshot.OldestPendingAt == nil || !snapshot.OldestPendingAt.Equal(fixture.now) || snapshot.TerminalLocalError != terminal {
		t.Fatalf("retained bound readiness = %#v", snapshot)
	}
	assertNoActiveSpoolReservations(t, fixture.spool)
}

func preRenameFailureOps(failurePoint, partialName string, dirFD *int, armed, fired *atomic.Bool) spoolOps {
	ops := defaultSpoolOps()
	switch failurePoint {
	case "partial fsync":
		fsync := ops.fsync
		ops.fsync = func(fd int) error {
			if armed.Load() && !fired.Load() && descriptorMatchesEntry(fd, *dirFD, partialName) && fired.CompareAndSwap(false, true) {
				return unix.EIO
			}
			return fsync(fd)
		}
	case "partial close":
		closeFD := ops.close
		ops.close = func(fd int) error {
			matches := armed.Load() && !fired.Load() && descriptorMatchesEntry(fd, *dirFD, partialName)
			err := closeFD(fd)
			if matches && fired.CompareAndSwap(false, true) {
				if err != nil {
					return err
				}
				return unix.EIO
			}
			return err
		}
	case "identity recheck":
		fstatat := ops.fstatat
		var checks atomic.Int32
		ops.fstatat = func(fd int, name string, stat *unix.Stat_t, flags int) error {
			if armed.Load() && !fired.Load() && name == partialName && checks.Add(1) == 2 && fired.CompareAndSwap(false, true) {
				return unix.EIO
			}
			return fstatat(fd, name, stat, flags)
		}
	case "rename no replace":
		rename := ops.renameNoReplace
		ops.renameNoReplace = func(oldDirFD int, oldName string, newDirFD int, newName string) error {
			if armed.Load() && oldName == partialName && completeAgePattern.MatchString(newName) && fired.CompareAndSwap(false, true) {
				return unix.EIO
			}
			return rename(oldDirFD, oldName, newDirFD, newName)
		}
	default:
		panic("unknown pre-rename failure point")
	}
	return ops
}

func descriptorMatchesEntry(fd, dirFD int, name string) bool {
	if fd < 0 || dirFD < 0 || name == "" {
		return false
	}
	var descriptor, entry unix.Stat_t
	return unix.Fstat(fd, &descriptor) == nil &&
		unix.Fstatat(dirFD, name, &entry, unix.AT_SYMLINK_NOFOLLOW) == nil &&
		sameInode(&descriptor, &entry)
}

func assertRetainedStoredRepair(t testing.TB, fixture *transitionFixture, archive []byte, record ObjectRecord, terminal bool) {
	t.Helper()
	objects, err := fixture.spool.DiscoverPlaintexts()
	if err != nil || len(objects) != 1 || objects[0].ObjectID != record.ObjectID || objects[0].CompressedSize != int64(len(archive)) || !objects[0].ReceivedAt.Equal(record.ReceivedAt) {
		t.Fatalf("retained plaintexts = %#v err=%v", objects, err)
	}
	contents, err := os.ReadFile(objects[0].Path)
	if err != nil || !bytes.Equal(contents, archive) {
		t.Fatalf("retained plaintext content mismatch err=%v", err)
	}
	var metadata unix.Stat_t
	if err := unix.Lstat(objects[0].Path, &metadata); err != nil || metadata.Mode&unix.S_IFMT != unix.S_IFREG || metadata.Mode&0o7777 != 0o600 || metadata.Uid != uint32(os.Geteuid()) || metadata.Gid != uint32(os.Getegid()) {
		t.Fatalf("retained plaintext metadata mode=%#o uid=%d gid=%d err=%v", metadata.Mode, metadata.Uid, metadata.Gid, err)
	}
	if entries := spoolEntryNames(t, fixture.spool.path); len(entries) != 1 || !plainPendingPattern.MatchString(entries[0]) {
		t.Fatalf("retained repair entries = %v", entries)
	}
	preserved, found, err := fixture.ledger.Get(record.ObjectID)
	if err != nil || !found || preserved.StoredAt == nil || record.StoredAt == nil || !preserved.StoredAt.Equal(*record.StoredAt) ||
		!preserved.ReceivedAt.Equal(record.ReceivedAt) || preserved.CompressedSize != record.CompressedSize || preserved.EncryptedSize != record.EncryptedSize ||
		preserved.RetryCount != record.RetryCount || !equalOptionalTime(preserved.LastAttemptAt, record.LastAttemptAt) || preserved.LatestRetryClass != record.LatestRetryClass {
		t.Fatalf("retained repair ledger = %#v found=%v err=%v", preserved, found, err)
	}
	snapshot := fixture.status.Snapshot()
	if snapshot.OldestPendingAt == nil || !snapshot.OldestPendingAt.Equal(record.ReceivedAt) || snapshot.TerminalLocalError != terminal {
		t.Fatalf("retained repair readiness = %#v", snapshot)
	}
	assertNoActiveSpoolReservations(t, fixture.spool)
}

func TestIngestReconcilesTransientPostRenameFailures(t *testing.T) {
	tests := []struct {
		name string
		ops  func(*atomic.Bool) spoolOps
	}{
		{
			name: "final identity check",
			ops: func(renamed *atomic.Bool) spoolOps {
				ops := defaultSpoolOps()
				rename := ops.renameNoReplace
				fstatat := ops.fstatat
				var failed atomic.Bool
				ops.renameNoReplace = func(oldDirFD int, oldName string, newDirFD int, newName string) error {
					err := rename(oldDirFD, oldName, newDirFD, newName)
					if err == nil && completeAgePattern.MatchString(newName) {
						renamed.Store(true)
					}
					return err
				}
				ops.fstatat = func(dirFD int, name string, stat *unix.Stat_t, flags int) error {
					if renamed.Load() && completeAgePattern.MatchString(name) && failed.CompareAndSwap(false, true) {
						return unix.EIO
					}
					return fstatat(dirFD, name, stat, flags)
				}
				return ops
			},
		},
		{
			name: "directory fsync",
			ops: func(renamed *atomic.Bool) spoolOps {
				ops := defaultSpoolOps()
				rename := ops.renameNoReplace
				fsync := ops.fsync
				var failed atomic.Bool
				ops.renameNoReplace = func(oldDirFD int, oldName string, newDirFD int, newName string) error {
					err := rename(oldDirFD, oldName, newDirFD, newName)
					if err == nil && completeAgePattern.MatchString(newName) {
						renamed.Store(true)
					}
					return err
				}
				ops.fsync = func(fd int) error {
					if renamed.Load() && failed.CompareAndSwap(false, true) {
						return unix.EIO
					}
					return fsync(fd)
				}
				return ops
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var renamed atomic.Bool
			fixture := newTransitionFixture(t, test.ops(&renamed), &fakeBackend{objects: make(map[string]bool), createVisible: true}, nil)
			archive := validRecoveryArchive(t, "post-rename-"+test.name)
			result, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive)))
			if err != nil {
				t.Fatalf("Ingest: %v", err)
			}
			if result.Status != ResultStored {
				t.Fatalf("status = %q", result.Status)
			}
			if entries := spoolEntryNames(t, fixture.spool.path); len(entries) != 0 {
				t.Fatalf("post-rename entries = %v", entries)
			}
			assertNoActiveSpoolReservations(t, fixture.spool)
			if fixture.status.Snapshot().TerminalLocalError {
				t.Fatal("reconciled post-rename failure set terminal readiness")
			}
		})
	}
}

func TestIngestPersistentPostRenameFsyncFailureRetainsRecoverableCanonicalState(t *testing.T) {
	ops := defaultSpoolOps()
	rename := ops.renameNoReplace
	fsync := ops.fsync
	var renamed atomic.Bool
	var failSync atomic.Bool
	failSync.Store(true)
	ops.renameNoReplace = func(oldDirFD int, oldName string, newDirFD int, newName string) error {
		err := rename(oldDirFD, oldName, newDirFD, newName)
		if err == nil && completeAgePattern.MatchString(newName) {
			renamed.Store(true)
		}
		return err
	}
	ops.fsync = func(fd int) error {
		if renamed.Load() && failSync.Load() {
			return unix.EIO
		}
		return fsync(fd)
	}
	fixture := newTransitionFixture(t, ops, &fakeBackend{objects: make(map[string]bool), createVisible: true}, nil)
	archive := validRecoveryArchive(t, "persistent-post-rename-fsync")
	_, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive)))
	if err == nil || err.Error() != "collector local state failed" {
		t.Fatalf("Ingest error = %v", err)
	}
	entries := spoolEntryNames(t, fixture.spool.path)
	bound, final := 0, 0
	for _, entry := range entries {
		if plainPendingPattern.MatchString(entry) {
			bound++
		}
		if completeAgePattern.MatchString(entry) {
			final++
		}
	}
	if len(entries) != 2 || bound != 1 || final != 1 {
		t.Fatalf("recoverable canonical entries = %v", entries)
	}
	assertNoActiveSpoolReservations(t, fixture.spool)
	objectID := "sha256:" + digestHex(archive)
	record, found, getErr := fixture.ledger.Get(objectID)
	if getErr != nil || !found || record.StoredAt != nil {
		t.Fatalf("recoverable pending ledger = %#v found=%v err=%v", record, found, getErr)
	}
	failSync.Store(false)
	result, retryErr := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive)))
	if retryErr != nil {
		t.Fatalf("same-process retry: %v", retryErr)
	}
	if result.Status != ResultStored {
		t.Fatalf("retry status = %q", result.Status)
	}
	if entries := spoolEntryNames(t, fixture.spool.path); len(entries) != 0 {
		t.Fatalf("retry entries = %v", entries)
	}
	assertNoActiveSpoolReservations(t, fixture.spool)
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
	assertNoActiveSpoolReservations(t, fixture.spool)
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
	if records, err := fixture.ledger.List(); err != nil || len(records) != 0 {
		t.Fatalf("encryption failure ledger = %#v err=%v", records, err)
	}
	if snapshot := fixture.status.Snapshot(); snapshot.OldestPendingAt != nil || snapshot.TerminalLocalError {
		t.Fatalf("encryption failure readiness = %#v", snapshot)
	}
	assertNoActiveSpoolReservations(t, fixture.spool)
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
	unlink := spoolOps.unlinkat
	fsync := spoolOps.fsync
	var partialRemoved atomic.Bool
	spoolOps.unlinkat = func(dirFD int, name string, flags int) error {
		err := unlink(dirFD, name, flags)
		if err == nil && partialAgePattern.MatchString(name) {
			partialRemoved.Store(true)
		}
		return err
	}
	spoolOps.fsync = func(fd int) error {
		if partialRemoved.Load() {
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
	entries := spoolEntryNames(t, fixture.spool.path)
	if len(entries) != 1 || !plainPendingPattern.MatchString(entries[0]) {
		t.Fatalf("pending sync failure entries = %v", entries)
	}
	if records, err := fixture.ledger.List(); err != nil || len(records) != 0 {
		t.Fatalf("published pending record remains = %#v err=%v", records, err)
	}
	assertNoActiveSpoolReservations(t, fixture.spool)
	if !fixture.status.Snapshot().TerminalLocalError {
		t.Fatal("pending sync failure did not fail readiness closed")
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

func TestIngestPendingLedgerRenameFailureRetainsCanonicalBound(t *testing.T) {
	spoolOps := defaultSpoolOps()
	var failRename atomic.Bool
	failRename.Store(true)
	rename := spoolOps.renameNoReplace
	spoolOps.renameNoReplace = func(oldDirFD int, oldName string, newDirFD int, newName string) error {
		if failRename.Load() && partialAgePattern.MatchString(oldName) && completeAgePattern.MatchString(newName) {
			return errors.New("rename failure")
		}
		return rename(oldDirFD, oldName, newDirFD, newName)
	}
	backend := &fakeBackend{objects: make(map[string]bool), createVisible: true}
	fixture := newTransitionFixture(t, spoolOps, backend, nil)
	archive := validRecoveryArchive(t, "rename-retains-pending")
	objectID := "sha256:" + digestHex(archive)
	_, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive)))
	if err == nil || err.Error() != "collector local state failed" {
		t.Fatalf("Ingest error = %v", err)
	}
	objects, discoverErr := fixture.spool.DiscoverPlaintexts()
	if discoverErr != nil || len(objects) != 1 || objects[0].ObjectID != objectID || objects[0].CompressedSize != int64(len(archive)) || !objects[0].ReceivedAt.Equal(fixture.now) {
		t.Fatalf("retained pending plaintexts = %#v err=%v", objects, discoverErr)
	}
	if entries := spoolEntryNames(t, fixture.spool.path); len(entries) != 1 || !plainPendingPattern.MatchString(entries[0]) {
		t.Fatalf("rename failure entries = %v", entries)
	}
	record, found, getErr := fixture.ledger.Get(objectID)
	if getErr != nil || !found || record.StoredAt != nil || record.CompressedSize != int64(len(archive)) || record.EncryptedSize <= 0 || !record.ReceivedAt.Equal(fixture.now) {
		t.Fatalf("retained pending ledger = %#v found=%v err=%v", record, found, getErr)
	}
	snapshot := fixture.status.Snapshot()
	if snapshot.OldestPendingAt == nil || !snapshot.OldestPendingAt.Equal(record.ReceivedAt) || !snapshot.TerminalLocalError {
		t.Fatalf("retained pending readiness = %#v", snapshot)
	}
	assertNoActiveSpoolReservations(t, fixture.spool)
	if backend.createCalls != 0 {
		t.Fatalf("Create calls before durable age = %d", backend.createCalls)
	}

	failRename.Store(false)
	restartedSpool, restartedLedger := reopenRecoveryFixture(t, fixture.spool, fixture.ledger)
	restartedStatus := NewStatusTracker(ReadinessConfig{StartedAt: fixture.now, StartupGrace: time.Hour, MaxRecoveryAge: 24 * time.Hour}, fixture.clock)
	restartedStatus.SetSpoolWritable(true)
	restartedService := newService(restartedSpool, restartedLedger, fixture.service.encryptor, backend, restartedStatus, recoveryarchive.DefaultLimits(), serviceOps{now: fixture.clock})
	runWorkerOnce(t, restartedSpool, restartedLedger, restartedService, restartedStatus, fixture.clock)
	if backend.createCalls != 1 {
		t.Fatalf("restarted Create calls = %d", backend.createCalls)
	}
	assertStoredRecoveryComplete(t, restartedSpool, restartedLedger, restartedStatus, objectID, int64(len(archive)))
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

func TestServiceTenCrashBoundariesRestartToExactState(t *testing.T) {
	points := []serviceCrashPoint{
		crashAfterUploadPartial,
		crashAfterPlaintextBind,
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
			snapshot := status.Snapshot()
			if getErr != nil || !found || record.StoredAt == nil {
				t.Fatalf("recovered record = %#v found=%v err=%v", record, found, getErr)
			}
			if record.CompressedSize != int64(len(archive)) || record.EncryptedSize <= 0 ||
				record.RetryCount != 0 || record.LastAttemptAt != nil || record.LatestRetryClass != "" ||
				!record.ReceivedAt.Equal(fixture.now) || !record.StoredAt.Equal(fixture.now) {
				t.Fatalf("recovered exact record = %#v", record)
			}
			if !snapshot.SpoolWritable || snapshot.OldestPendingAt != nil || snapshot.NewestSuccessfulAt == nil ||
				!snapshot.NewestSuccessfulAt.Equal(*record.StoredAt) || snapshot.TerminalLocalError {
				t.Fatalf("recovered readiness = %#v", snapshot)
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

func assertNoActiveSpoolReservations(t testing.TB, spool *Spool) {
	t.Helper()
	spool.transition.Lock()
	defer spool.transition.Unlock()
	if len(spool.active) != 0 {
		t.Fatalf("active spool reservations = %#v", spool.active)
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
		if custom.publishWait != nil {
			ops.publishWait = custom.publishWait
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
