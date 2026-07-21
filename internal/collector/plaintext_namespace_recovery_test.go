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

	"golang.org/x/sys/unix"

	"sherpa/internal/recoveryarchive"
)

func TestWorkerRecoversEveryDurableBindNamespaceOutcome(t *testing.T) {
	tests := []struct {
		name       string
		configure  func(*spoolOps)
		wantUpload int
		wantBound  int
	}{
		{
			name: "link failure leaves original upload",
			configure: func(ops *spoolOps) {
				ops.linkat = func(int, string, int, string, int) error { return unix.EIO }
			},
			wantUpload: 1,
		},
		{
			name: "first directory fsync failure leaves both names",
			configure: func(ops *spoolOps) {
				link := ops.linkat
				fsync := ops.fsync
				var linked atomic.Bool
				ops.linkat = func(oldDirFD int, oldName string, newDirFD int, newName string, flags int) error {
					err := link(oldDirFD, oldName, newDirFD, newName, flags)
					if err == nil && plainPendingPattern.MatchString(newName) {
						linked.Store(true)
					}
					return err
				}
				ops.fsync = func(fd int) error {
					if linked.Load() {
						return unix.EIO
					}
					return fsync(fd)
				}
			},
			wantUpload: 1,
			wantBound:  1,
		},
		{
			name: "source unlink failure leaves both names",
			configure: func(ops *spoolOps) {
				unlink := ops.unlinkat
				ops.unlinkat = func(dirFD int, name string, flags int) error {
					if uploadPartialPattern.MatchString(name) {
						return unix.EIO
					}
					return unlink(dirFD, name, flags)
				}
			},
			wantUpload: 1,
			wantBound:  1,
		},
		{
			name: "second directory fsync failure leaves bound name",
			configure: func(ops *spoolOps) {
				unlink := ops.unlinkat
				fsync := ops.fsync
				var sourceUnlinked atomic.Bool
				ops.unlinkat = func(dirFD int, name string, flags int) error {
					err := unlink(dirFD, name, flags)
					if err == nil && uploadPartialPattern.MatchString(name) {
						sourceUnlinked.Store(true)
					}
					return err
				}
				ops.fsync = func(fd int) error {
					if sourceUnlinked.Load() {
						return unix.EIO
					}
					return fsync(fd)
				}
			},
			wantBound: 1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ops := defaultSpoolOps()
			test.configure(&ops)
			backend := &fakeBackend{objects: make(map[string]bool), createVisible: true}
			fixture := newTransitionFixture(t, ops, backend, nil)
			archive := validRecoveryArchive(t, "namespace-"+test.name)
			objectID := "sha256:" + digestHex(archive)

			if _, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive))); err == nil || err.Error() != "collector local state failed" {
				t.Fatalf("Ingest error = %v", err)
			}
			assertPlaintextNamespace(t, fixture.spool.path, test.wantUpload, test.wantBound)
			assertNoActiveSpoolReservations(t, fixture.spool)
			if !fixture.status.Snapshot().TerminalLocalError {
				t.Fatal("uncertain bind did not fail readiness closed")
			}

			restartedSpool, restartedLedger := reopenRecoveryFixture(t, fixture.spool, fixture.ledger)
			status := NewStatusTracker(ReadinessConfig{StartedAt: fixture.now, StartupGrace: time.Hour, MaxRecoveryAge: 24 * time.Hour}, fixture.clock)
			service := newService(restartedSpool, restartedLedger, fixture.service.encryptor, backend, status, recoveryarchive.DefaultLimits(), serviceOps{now: fixture.clock})
			runWorkerOnce(t, restartedSpool, restartedLedger, service, status, fixture.clock)
			if !backend.has(objectID) {
				t.Fatal("restart did not create exact remote object")
			}
			assertStoredRecoveryComplete(t, restartedSpool, restartedLedger, status, objectID, int64(len(archive)))
			assertNoActiveSpoolReservations(t, restartedSpool)
		})
	}
}

func TestWorkerBindConflictPreservesDifferentInodes(t *testing.T) {
	backend := &fakeBackend{objects: make(map[string]bool), createVisible: true}
	fixture := newTransitionFixture(t, defaultSpoolOps(), backend, nil)
	archive := validRecoveryArchive(t, "namespace-conflict-upload")
	objectID := "sha256:" + digestHex(archive)
	digest, _ := canonicalDigest(objectID)
	boundPath := filepath.Join(fixture.spool.path, digest+".tar.gz.plain.pending")
	if err := os.WriteFile(boundPath, validRecoveryArchive(t, "different-bound-content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := unix.Fsync(fixture.spool.dirFD); err != nil {
		t.Fatal(err)
	}

	if _, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive))); err == nil || err.Error() != "collector local state failed" {
		t.Fatalf("Ingest error = %v", err)
	}
	assertPlaintextNamespace(t, fixture.spool.path, 1, 1)
	entries := spoolEntryNames(t, fixture.spool.path)
	var uploadPath string
	for _, entry := range entries {
		if uploadPartialPattern.MatchString(entry) {
			uploadPath = filepath.Join(fixture.spool.path, entry)
		}
	}
	uploadInfo, err := os.Stat(uploadPath)
	if err != nil {
		t.Fatal(err)
	}
	boundInfo, err := os.Stat(boundPath)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(uploadInfo, boundInfo) {
		t.Fatal("conflicting namespaces unexpectedly share an inode")
	}
	assertNoActiveSpoolReservations(t, fixture.spool)

	restartedSpool, restartedLedger := reopenRecoveryFixture(t, fixture.spool, fixture.ledger)
	status := NewStatusTracker(ReadinessConfig{StartedAt: fixture.now, StartupGrace: time.Hour, MaxRecoveryAge: 24 * time.Hour}, fixture.clock)
	service := newService(restartedSpool, restartedLedger, fixture.service.encryptor, backend, status, recoveryarchive.DefaultLimits(), serviceOps{now: fixture.clock})
	err = NewWorker(restartedSpool, restartedLedger, service, status, time.Minute).Run(context.Background())
	if err == nil || err.Error() != "collector local state failed" {
		t.Fatalf("Run error = %v", err)
	}
	assertPlaintextNamespace(t, restartedSpool.path, 1, 1)
	assertNoActiveSpoolReservations(t, restartedSpool)
	if backend.existsCalls != 0 || backend.createCalls != 0 {
		t.Fatalf("backend called for conflicting namespaces: exists=%d create=%d", backend.existsCalls, backend.createCalls)
	}
}

func TestStoredRepairFailureTracksOperationalPendingAcrossRestart(t *testing.T) {
	fixture := newServiceFixture(t)
	archive := validRecoveryArchive(t, "stored-repair-operational-pending")
	objectID := "sha256:" + digestHex(archive)
	if _, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive))); err != nil {
		t.Fatalf("initial Ingest: %v", err)
	}
	record, found, err := fixture.ledger.Get(objectID)
	if err != nil || !found || record.StoredAt == nil {
		t.Fatalf("initial record = %#v found=%v err=%v", record, found, err)
	}
	originalStoredAt := *record.StoredAt
	repairReceivedAt := fixture.now.Add(-2 * time.Hour)
	record.ReceivedAt = repairReceivedAt
	if err := fixture.ledger.Put(record); err != nil {
		t.Fatal(err)
	}
	backend := fixture.backend
	backend.set(objectID, false)
	backend.existsErr = errors.New("offline")
	status := NewStatusTracker(ReadinessConfig{StartedAt: fixture.now.Add(-3 * time.Hour), StartupGrace: time.Hour, MaxRecoveryAge: time.Hour}, fixture.clock)
	status.SetSpoolWritable(true)
	status.RecordSuccess(fixture.now.Add(-10 * time.Minute))
	fixture.status = status
	fixture.service = newService(fixture.spool, fixture.ledger, fixture.service.encryptor, backend, status, recoveryarchive.DefaultLimits(), serviceOps{now: fixture.clock})

	if _, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive))); err == nil || err.Error() != "collector backend unavailable" {
		t.Fatalf("repair Ingest error = %v", err)
	}
	assertOperationalPending(t, status, repairReceivedAt, false)
	failedRecord, found, err := fixture.ledger.Get(objectID)
	if err != nil || !found || failedRecord.StoredAt == nil || !failedRecord.StoredAt.Equal(originalStoredAt) || failedRecord.RetryCount != 1 || failedRecord.LastAttemptAt == nil {
		t.Fatalf("failed repair record = %#v found=%v err=%v", failedRecord, found, err)
	}

	restartedSpool, restartedLedger := reopenRecoveryFixture(t, fixture.spool, fixture.ledger)
	restartedBackend := &fakeBackend{objects: map[string]bool{objectID: false}, existsErr: errors.New("must not call before due"), createVisible: true}
	restartedStatus := NewStatusTracker(ReadinessConfig{StartedAt: fixture.now.Add(-3 * time.Hour), StartupGrace: time.Hour, MaxRecoveryAge: time.Hour}, fixture.clock)
	restartedStatus.RecordSuccess(fixture.now.Add(-10 * time.Minute))
	restartedService := newService(restartedSpool, restartedLedger, fixture.service.encryptor, restartedBackend, restartedStatus, recoveryarchive.DefaultLimits(), serviceOps{now: fixture.clock})
	ctx, cancel := context.WithCancel(context.Background())
	var waited time.Duration
	worker := newWorker(restartedSpool, restartedLedger, restartedService, restartedStatus, 5*time.Minute, workerOps{
		now: fixture.clock,
		wait: func(_ context.Context, delay time.Duration) error {
			waited = delay
			cancel()
			return context.Canceled
		},
	})
	if err := worker.Run(ctx); err != nil {
		t.Fatalf("Run before retry due: %v", err)
	}
	if waited != 5*time.Minute || restartedBackend.existsCalls != 0 || restartedBackend.createCalls != 0 {
		t.Fatalf("retry gate waited=%v exists=%d create=%d", waited, restartedBackend.existsCalls, restartedBackend.createCalls)
	}
	assertOperationalPending(t, restartedStatus, repairReceivedAt, false)

	restartedBackend.existsErr = nil
	restartedBackend.set(objectID, true)
	objects, err := restartedSpool.Discover()
	if err != nil || len(objects) != 1 {
		t.Fatalf("Discover = %#v err=%v", objects, err)
	}
	objects[0].ReceivedAt = failedRecord.ReceivedAt
	existing, err := restartedService.CommitPending(context.Background(), objects[0])
	if err != nil || !existing {
		t.Fatalf("CommitPending existing=%v err=%v", existing, err)
	}
	stored, found, err := restartedLedger.Get(objectID)
	if err != nil || !found || stored.StoredAt == nil || !stored.StoredAt.Equal(originalStoredAt) || stored.RetryCount != failedRecord.RetryCount || !equalOptionalTime(stored.LastAttemptAt, failedRecord.LastAttemptAt) {
		t.Fatalf("proved repair record = %#v found=%v err=%v", stored, found, err)
	}
	if snapshot := restartedStatus.Snapshot(); snapshot.OldestPendingAt != nil || snapshot.TerminalLocalError || !restartedStatus.Ready() {
		t.Fatalf("proved repair readiness = %#v ready=%v", snapshot, restartedStatus.Ready())
	}
}

func TestWorkerCancellationDuringBoundEncryptionIsNonterminalAndRecoverable(t *testing.T) {
	fixture := newServiceFixture(t)
	archive := validRecoveryArchive(t, "cancel-bound-recovery")
	objectID := "sha256:" + digestHex(archive)
	fixture.service.ops.crash = crashAt(crashAfterPlaintextBind)
	if _, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive))); err == nil {
		t.Fatal("Ingest succeeded across plaintext bind crash")
	}
	restartedSpool, restartedLedger := reopenRecoveryFixture(t, fixture.spool, fixture.ledger)
	status := NewStatusTracker(ReadinessConfig{StartedAt: fixture.now, StartupGrace: time.Hour, MaxRecoveryAge: 24 * time.Hour}, fixture.clock)
	started := make(chan struct{})
	var once sync.Once
	service := newService(restartedSpool, restartedLedger, fixture.service.encryptor, fixture.backend, status, recoveryarchive.DefaultLimits(), serviceOps{
		now: fixture.clock,
		encryptFile: func(ctx context.Context, _, _ string) (int64, error) {
			once.Do(func() { close(started) })
			<-ctx.Done()
			return 0, ctx.Err()
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- NewWorker(restartedSpool, restartedLedger, service, status, time.Minute).Run(ctx) }()
	<-started
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run cancellation error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return promptly after encryption cancellation")
	}
	assertPlaintextNamespace(t, restartedSpool.path, 0, 1)
	assertNoActiveSpoolReservations(t, restartedSpool)
	if records, err := restartedLedger.List(); err != nil || len(records) != 0 {
		t.Fatalf("canceled recovery ledger = %#v err=%v", records, err)
	}
	if snapshot := status.Snapshot(); snapshot.TerminalLocalError {
		t.Fatalf("canceled recovery readiness = %#v", snapshot)
	}
	if fixture.backend.existsCalls != 0 || fixture.backend.createCalls != 0 {
		t.Fatalf("backend called during canceled recovery: exists=%d create=%d", fixture.backend.existsCalls, fixture.backend.createCalls)
	}

	finalSpool, finalLedger := reopenRecoveryFixture(t, restartedSpool, restartedLedger)
	finalStatus := NewStatusTracker(ReadinessConfig{StartedAt: fixture.now, StartupGrace: time.Hour, MaxRecoveryAge: 24 * time.Hour}, fixture.clock)
	finalService := newService(finalSpool, finalLedger, fixture.service.encryptor, fixture.backend, finalStatus, recoveryarchive.DefaultLimits(), serviceOps{now: fixture.clock})
	runWorkerOnce(t, finalSpool, finalLedger, finalService, finalStatus, fixture.clock)
	assertStoredRecoveryComplete(t, finalSpool, finalLedger, finalStatus, objectID, int64(len(archive)))
}

func TestWorkerEncryptionFailureWinsCancellationRace(t *testing.T) {
	fixture := newServiceFixture(t)
	archive := validRecoveryArchive(t, "cancel-local-failure")
	fixture.service.ops.crash = crashAt(crashAfterPlaintextBind)
	if _, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive))); err == nil {
		t.Fatal("Ingest succeeded across plaintext bind crash")
	}
	restartedSpool, restartedLedger := reopenRecoveryFixture(t, fixture.spool, fixture.ledger)
	status := NewStatusTracker(ReadinessConfig{StartedAt: fixture.now, StartupGrace: time.Hour, MaxRecoveryAge: 24 * time.Hour}, fixture.clock)
	started := make(chan struct{})
	service := newService(restartedSpool, restartedLedger, fixture.service.encryptor, fixture.backend, status, recoveryarchive.DefaultLimits(), serviceOps{
		now: fixture.clock,
		encryptFile: func(ctx context.Context, _, _ string) (int64, error) {
			close(started)
			<-ctx.Done()
			return 0, errors.New("local encryption failure")
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- NewWorker(restartedSpool, restartedLedger, service, status, time.Minute).Run(ctx) }()
	<-started
	cancel()
	err := <-done
	if err == nil || err.Error() != "collector local state failed" {
		t.Fatalf("Run error = %v", err)
	}
	if !status.Snapshot().TerminalLocalError {
		t.Fatal("genuine encryption failure was masked by cancellation")
	}
	assertPlaintextNamespace(t, restartedSpool.path, 0, 1)
	assertNoActiveSpoolReservations(t, restartedSpool)
}

func assertPlaintextNamespace(t testing.TB, dir string, wantUpload, wantBound int) {
	t.Helper()
	entries := spoolEntryNames(t, dir)
	uploads, bounds := 0, 0
	for _, entry := range entries {
		if uploadPartialPattern.MatchString(entry) {
			uploads++
		}
		if plainPendingPattern.MatchString(entry) {
			bounds++
		}
	}
	if uploads != wantUpload || bounds != wantBound || len(entries) != wantUpload+wantBound {
		t.Fatalf("plaintext namespace entries=%v uploads=%d bounds=%d, want uploads=%d bounds=%d", entries, uploads, bounds, wantUpload, wantBound)
	}
}

func assertOperationalPending(t testing.TB, status *StatusTracker, want time.Time, ready bool) {
	t.Helper()
	snapshot := status.Snapshot()
	if snapshot.OldestPendingAt == nil || !snapshot.OldestPendingAt.Equal(want) || snapshot.TerminalLocalError || status.Ready() != ready {
		t.Fatalf("operational pending snapshot=%#v ready=%v, want oldest=%v ready=%v", snapshot, status.Ready(), want, ready)
	}
}
