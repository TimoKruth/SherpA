package collector

import (
	"context"
	"errors"
	"fmt"
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

func TestWorkerQueuesStoredNoLocalPendingBeforeRetryWaitAndClearsAfterProof(t *testing.T) {
	fixture := newServiceFixture(t)
	receivedAt := fixture.now.Add(-2 * time.Hour)
	pending := createPendingFixture(t, fixture.spool, fixture.ledger, receivedAt, "stored-no-local-before-due")
	record, found, err := fixture.ledger.Get(pending.ObjectID)
	if err != nil || !found {
		t.Fatalf("record = %#v found=%v err=%v", record, found, err)
	}
	storedAt := fixture.now.Add(-10 * time.Minute)
	attemptedAt := fixture.now
	record.StoredAt = &storedAt
	record.LastAttemptAt = &attemptedAt
	record.LatestRetryClass = "backend_unavailable"
	record.RetryCount = 1
	if err := fixture.ledger.Put(record); err != nil {
		t.Fatal(err)
	}
	if err := fixture.spool.RemoveEncrypted(pending.EncryptedPath); err != nil {
		t.Fatal(err)
	}
	backend := &fakeBackend{objects: map[string]bool{pending.ObjectID: false}, existsErr: errors.New("must not call before due"), createVisible: true}
	status := NewStatusTracker(ReadinessConfig{StartedAt: fixture.now.Add(-3 * time.Hour), StartupGrace: time.Hour, MaxRecoveryAge: time.Hour}, fixture.clock)
	status.RecordSuccess(storedAt)
	service := newService(fixture.spool, fixture.ledger, fixture.service.encryptor, backend, status, recoveryarchive.DefaultLimits(), serviceOps{now: fixture.clock})
	ctx, cancel := context.WithCancel(context.Background())
	var waited time.Duration
	worker := newWorker(fixture.spool, fixture.ledger, service, status, 5*time.Minute, workerOps{
		now: fixture.clock,
		wait: func(_ context.Context, delay time.Duration) error {
			waited = delay
			assertOperationalPending(t, status, receivedAt, false)
			cancel()
			return context.Canceled
		},
	})
	if err := worker.Run(ctx); err != nil {
		t.Fatalf("Run before due: %v", err)
	}
	if waited != 5*time.Minute || backend.existsCalls != 0 || backend.createCalls != 0 {
		t.Fatalf("before due waited=%v exists=%d create=%d", waited, backend.existsCalls, backend.createCalls)
	}
	unchanged, found, err := fixture.ledger.Get(pending.ObjectID)
	if err != nil || !found || unchanged.StoredAt == nil || !unchanged.StoredAt.Equal(storedAt) || unchanged.RetryCount != 1 || !equalOptionalTime(unchanged.LastAttemptAt, record.LastAttemptAt) {
		t.Fatalf("before due record = %#v found=%v err=%v", unchanged, found, err)
	}

	backend.existsErr = nil
	backend.set(pending.ObjectID, true)
	proofNow := fixture.now.Add(5 * time.Minute)
	ctx, cancel = context.WithCancel(context.Background())
	worker = newWorker(fixture.spool, fixture.ledger, service, status, 5*time.Minute, workerOps{
		now: func() time.Time { return proofNow },
		wait: func(context.Context, time.Duration) error {
			cancel()
			return context.Canceled
		},
	})
	if err := worker.Run(ctx); err != nil {
		t.Fatalf("Run exact proof: %v", err)
	}
	proved, found, err := fixture.ledger.Get(pending.ObjectID)
	if err != nil || !found || proved.StoredAt == nil || !proved.StoredAt.Equal(storedAt) || proved.RetryCount != record.RetryCount || !equalOptionalTime(proved.LastAttemptAt, record.LastAttemptAt) {
		t.Fatalf("proved record = %#v found=%v err=%v", proved, found, err)
	}
	if snapshot := status.Snapshot(); snapshot.OldestPendingAt != nil || snapshot.TerminalLocalError || !status.Ready() {
		t.Fatalf("proved status = %#v ready=%v", snapshot, status.Ready())
	}
}

func TestWorkerStoredNoLocalFailureNeverLooksReady(t *testing.T) {
	tests := []struct {
		name       string
		existsErr  error
		wantRetry  int
		wantClass  string
		wantRunErr string
	}{
		{name: "backend unavailable", existsErr: errors.New("offline"), wantRetry: 1, wantClass: "backend_failed"},
		{name: "remote absent", wantRunErr: "collector local state failed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newServiceFixture(t)
			receivedAt := fixture.now.Add(-2 * time.Hour)
			pending := createPendingFixture(t, fixture.spool, fixture.ledger, receivedAt, "stored-no-local-"+test.name)
			record, _, err := fixture.ledger.Get(pending.ObjectID)
			if err != nil {
				t.Fatal(err)
			}
			storedAt := fixture.now.Add(-10 * time.Minute)
			record.StoredAt = &storedAt
			if err := fixture.ledger.Put(record); err != nil {
				t.Fatal(err)
			}
			if err := fixture.spool.RemoveEncrypted(pending.EncryptedPath); err != nil {
				t.Fatal(err)
			}
			backend := &fakeBackend{objects: map[string]bool{pending.ObjectID: false}, existsErr: test.existsErr, createVisible: true}
			status := NewStatusTracker(ReadinessConfig{StartedAt: fixture.now.Add(-3 * time.Hour), StartupGrace: time.Hour, MaxRecoveryAge: time.Hour}, fixture.clock)
			status.RecordSuccess(storedAt)
			service := newService(fixture.spool, fixture.ledger, fixture.service.encryptor, backend, status, recoveryarchive.DefaultLimits(), serviceOps{now: fixture.clock})
			ctx, cancel := context.WithCancel(context.Background())
			worker := newWorker(fixture.spool, fixture.ledger, service, status, 5*time.Minute, workerOps{
				now: fixture.clock,
				wait: func(context.Context, time.Duration) error {
					cancel()
					return context.Canceled
				},
			})
			runErr := worker.Run(ctx)
			if test.wantRunErr == "" {
				if runErr != nil {
					t.Fatalf("Run: %v", runErr)
				}
			} else if runErr == nil || runErr.Error() != test.wantRunErr {
				t.Fatalf("Run error = %v", runErr)
			}
			assertOperationalPendingWithTerminal(t, status, receivedAt, test.wantRunErr != "")
			failed, found, err := fixture.ledger.Get(pending.ObjectID)
			if err != nil || !found || failed.StoredAt == nil || !failed.StoredAt.Equal(storedAt) || failed.RetryCount != test.wantRetry || failed.LatestRetryClass != test.wantClass {
				t.Fatalf("failure record = %#v found=%v err=%v", failed, found, err)
			}
		})
	}
}

func TestWorkerDeduplicatesAbandonedUploadsUsingEarliestReceipt(t *testing.T) {
	for _, count := range []int{2, 3} {
		t.Run(fmt.Sprintf("%d uploads", count), func(t *testing.T) {
			backend := &fakeBackend{objects: make(map[string]bool), createVisible: true}
			fixture := newTransitionFixture(t, defaultSpoolOps(), backend, nil)
			archive := validRecoveryArchive(t, fmt.Sprintf("duplicate-upload-%d", count))
			objectID := "sha256:" + digestHex(archive)
			receipts := []time.Time{fixture.now.Add(-time.Hour), fixture.now.Add(-3 * time.Hour), fixture.now.Add(-2 * time.Hour)}
			for index := 0; index < count; index++ {
				crashCompleteUploadAt(t, fixture, archive, receipts[index])
			}
			assertPlaintextNamespace(t, fixture.spool.path, count, 0)

			restartedSpool, restartedLedger := reopenRecoveryFixture(t, fixture.spool, fixture.ledger)
			status := NewStatusTracker(ReadinessConfig{StartedAt: fixture.now.Add(-4 * time.Hour), StartupGrace: time.Hour, MaxRecoveryAge: 24 * time.Hour}, fixture.clock)
			service := newService(restartedSpool, restartedLedger, fixture.service.encryptor, backend, status, recoveryarchive.DefaultLimits(), serviceOps{now: fixture.clock})
			runWorkerOnce(t, restartedSpool, restartedLedger, service, status, fixture.clock)
			assertStoredRecoveryComplete(t, restartedSpool, restartedLedger, status, objectID, int64(len(archive)))
			record, found, err := restartedLedger.Get(objectID)
			if err != nil || !found || !record.ReceivedAt.Equal(receipts[1]) || record.StoredAt == nil || !record.StoredAt.Equal(fixture.now) {
				t.Fatalf("deduplicated record = %#v found=%v err=%v", record, found, err)
			}
			if backend.createCalls != 1 {
				t.Fatalf("Create calls = %d", backend.createCalls)
			}
			assertNoActiveSpoolReservations(t, restartedSpool)
		})
	}
}

func TestWorkerDeduplicatesBoundAndDifferentInodeUpload(t *testing.T) {
	backend := &fakeBackend{objects: make(map[string]bool), createVisible: true}
	fixture := newTransitionFixture(t, defaultSpoolOps(), backend, nil)
	archive := validRecoveryArchive(t, "bound-different-inode-duplicate")
	objectID := "sha256:" + digestHex(archive)
	earliest := fixture.now.Add(-3 * time.Hour)
	crashBoundPlaintextAt(t, fixture, archive, earliest)
	crashCompleteUploadAt(t, fixture, archive, fixture.now.Add(-time.Hour))
	assertPlaintextNamespace(t, fixture.spool.path, 1, 1)
	assertDifferentPlaintextInodes(t, fixture.spool.path)

	restartedSpool, restartedLedger := reopenRecoveryFixture(t, fixture.spool, fixture.ledger)
	status := NewStatusTracker(ReadinessConfig{StartedAt: fixture.now.Add(-4 * time.Hour), StartupGrace: time.Hour, MaxRecoveryAge: 24 * time.Hour}, fixture.clock)
	service := newService(restartedSpool, restartedLedger, fixture.service.encryptor, backend, status, recoveryarchive.DefaultLimits(), serviceOps{now: fixture.clock})
	runWorkerOnce(t, restartedSpool, restartedLedger, service, status, fixture.clock)
	assertStoredRecoveryComplete(t, restartedSpool, restartedLedger, status, objectID, int64(len(archive)))
	record, found, err := restartedLedger.Get(objectID)
	if err != nil || !found || !record.ReceivedAt.Equal(earliest) {
		t.Fatalf("deduplicated record = %#v found=%v err=%v", record, found, err)
	}
	assertNoActiveSpoolReservations(t, restartedSpool)
}

func TestWorkerDuplicateCleanupFailurePreservesCanonicalRecovery(t *testing.T) {
	tests := []struct {
		name       string
		configure  func(*spoolOps, *atomic.Bool, string)
		wantUpload int
	}{
		{
			name: "unlink failure",
			configure: func(ops *spoolOps, seam *atomic.Bool, duplicateName string) {
				unlink := ops.unlinkat
				ops.unlinkat = func(dirFD int, name string, flags int) error {
					if name == duplicateName {
						seam.Store(true)
						return unix.EIO
					}
					return unlink(dirFD, name, flags)
				}
			},
			wantUpload: 1,
		},
		{
			name: "directory fsync failure",
			configure: func(ops *spoolOps, seam *atomic.Bool, duplicateName string) {
				unlink := ops.unlinkat
				fsync := ops.fsync
				var removed atomic.Bool
				ops.unlinkat = func(dirFD int, name string, flags int) error {
					err := unlink(dirFD, name, flags)
					if err == nil && name == duplicateName {
						removed.Store(true)
					}
					return err
				}
				ops.fsync = func(fd int) error {
					if removed.Load() {
						seam.Store(true)
						return unix.EIO
					}
					return fsync(fd)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend := &fakeBackend{objects: make(map[string]bool), createVisible: true}
			fixture := newTransitionFixture(t, defaultSpoolOps(), backend, nil)
			archive := validRecoveryArchive(t, "duplicate-cleanup-"+test.name)
			objectID := "sha256:" + digestHex(archive)
			crashBoundPlaintextAt(t, fixture, archive, fixture.now.Add(-2*time.Hour))
			crashCompleteUploadAt(t, fixture, archive, fixture.now.Add(-time.Hour))
			duplicateName := ""
			for _, entry := range spoolEntryNames(t, fixture.spool.path) {
				if uploadPartialPattern.MatchString(entry) {
					duplicateName = entry
				}
			}
			if duplicateName == "" {
				t.Fatal("duplicate upload missing")
			}
			spoolPath, ledgerPath := fixture.spool.path, fixture.ledger.path
			if err := fixture.spool.Close(); err != nil {
				t.Fatal(err)
			}
			if err := fixture.ledger.Close(); err != nil {
				t.Fatal(err)
			}
			ops := defaultSpoolOps()
			var seam atomic.Bool
			test.configure(&ops, &seam, duplicateName)
			spool, err := openSpool(spoolPath, time.Hour, ops)
			if err != nil {
				t.Fatal(err)
			}
			ledger, err := OpenLedger(ledgerPath)
			if err != nil {
				t.Fatal(err)
			}
			status := NewStatusTracker(ReadinessConfig{StartedAt: fixture.now.Add(-3 * time.Hour), StartupGrace: time.Hour, MaxRecoveryAge: 24 * time.Hour}, fixture.clock)
			service := newService(spool, ledger, fixture.service.encryptor, backend, status, recoveryarchive.DefaultLimits(), serviceOps{now: fixture.clock})
			runErr := NewWorker(spool, ledger, service, status, time.Minute).Run(context.Background())
			if runErr == nil || runErr.Error() != "collector local state failed" || !seam.Load() {
				t.Fatalf("Run error=%v seam=%v", runErr, seam.Load())
			}
			assertPlaintextNamespace(t, spool.path, test.wantUpload, 1)
			assertNoActiveSpoolReservations(t, spool)
			if backend.existsCalls != 0 || backend.createCalls != 0 {
				t.Fatalf("backend called before duplicate cleanup: exists=%d create=%d", backend.existsCalls, backend.createCalls)
			}

			finalSpool, finalLedger := reopenRecoveryFixture(t, spool, ledger)
			finalStatus := NewStatusTracker(ReadinessConfig{StartedAt: fixture.now.Add(-3 * time.Hour), StartupGrace: time.Hour, MaxRecoveryAge: 24 * time.Hour}, fixture.clock)
			finalService := newService(finalSpool, finalLedger, fixture.service.encryptor, backend, finalStatus, recoveryarchive.DefaultLimits(), serviceOps{now: fixture.clock})
			runWorkerOnce(t, finalSpool, finalLedger, finalService, finalStatus, fixture.clock)
			assertStoredRecoveryComplete(t, finalSpool, finalLedger, finalStatus, objectID, int64(len(archive)))
		})
	}
}

func crashCompleteUploadAt(t testing.TB, fixture *transitionFixture, archive []byte, receivedAt time.Time) {
	t.Helper()
	fixture.service.ops.now = func() time.Time { return receivedAt }
	fixture.service.ops.crash = crashAt(crashAfterUploadPartial)
	if _, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive))); err == nil || err.Error() != "collector operation interrupted" {
		t.Fatalf("upload crash error = %v", err)
	}
}

func crashBoundPlaintextAt(t testing.TB, fixture *transitionFixture, archive []byte, receivedAt time.Time) {
	t.Helper()
	fixture.service.ops.now = func() time.Time { return receivedAt }
	fixture.service.ops.crash = crashAt(crashAfterPlaintextBind)
	if _, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive))); err == nil || err.Error() != "collector operation interrupted" {
		t.Fatalf("bound crash error = %v", err)
	}
}

func assertDifferentPlaintextInodes(t testing.TB, dir string) {
	t.Helper()
	var uploadPath, boundPath string
	for _, entry := range spoolEntryNames(t, dir) {
		switch {
		case uploadPartialPattern.MatchString(entry):
			uploadPath = filepath.Join(dir, entry)
		case plainPendingPattern.MatchString(entry):
			boundPath = filepath.Join(dir, entry)
		}
	}
	uploadInfo, uploadErr := os.Stat(uploadPath)
	boundInfo, boundErr := os.Stat(boundPath)
	if uploadErr != nil || boundErr != nil || os.SameFile(uploadInfo, boundInfo) {
		t.Fatalf("plaintext inodes upload=%v bound=%v uploadErr=%v boundErr=%v", uploadInfo, boundInfo, uploadErr, boundErr)
	}
}

func assertOperationalPendingWithTerminal(t testing.TB, status *StatusTracker, want time.Time, terminal bool) {
	t.Helper()
	snapshot := status.Snapshot()
	if snapshot.OldestPendingAt == nil || !snapshot.OldestPendingAt.Equal(want) || snapshot.TerminalLocalError != terminal || status.Ready() {
		t.Fatalf("operational pending snapshot=%#v ready=%v, want oldest=%v terminal=%v", snapshot, status.Ready(), want, terminal)
	}
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
