package collector

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"sherpa/internal/recoveryarchive"
)

func TestIngestStoredLedgerMissingLocalRecreatesRemoteFromFreshUpload(t *testing.T) {
	fixture := newServiceFixture(t)
	archive := validRecoveryArchive(t, "stored-ledger-remote-repair")
	objectID := "sha256:" + digestHex(archive)
	if _, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive))); err != nil {
		t.Fatalf("initial Ingest: %v", err)
	}
	original, found, err := fixture.ledger.Get(objectID)
	if err != nil || !found || original.StoredAt == nil {
		t.Fatalf("original record = %#v found=%v err=%v", original, found, err)
	}
	fixture.backend.set(objectID, false)

	result, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive)))
	if err != nil {
		t.Fatalf("repair Ingest: %v", err)
	}
	if result.Status != ResultStored || !fixture.backend.has(objectID) || fixture.backend.createCalls != 2 {
		t.Fatalf("repair result=%#v remote=%v createCalls=%d", result, fixture.backend.has(objectID), fixture.backend.createCalls)
	}
	updated, found, err := fixture.ledger.Get(objectID)
	if err != nil || !found || updated.StoredAt == nil || !updated.StoredAt.Equal(*original.StoredAt) ||
		!updated.ReceivedAt.Equal(original.ReceivedAt) || updated.RetryCount != original.RetryCount ||
		updated.LatestRetryClass != original.LatestRetryClass || !equalOptionalTime(updated.LastAttemptAt, original.LastAttemptAt) {
		t.Fatalf("stored metadata regressed: original=%#v updated=%#v found=%v err=%v", original, updated, found, err)
	}
	if entries := spoolEntryNames(t, fixture.spool.path); len(entries) != 0 {
		t.Fatalf("repair local entries = %v", entries)
	}
	if snapshot := fixture.status.Snapshot(); snapshot.TerminalLocalError || !snapshot.SpoolWritable || snapshot.OldestPendingAt != nil || snapshot.NewestSuccessfulAt == nil {
		t.Fatalf("repair readiness = %#v", snapshot)
	}
}

func TestIngestStoredLedgerMissingLocalRemotePresentCleansFreshPublication(t *testing.T) {
	fixture := newServiceFixture(t)
	archive := validRecoveryArchive(t, "stored-ledger-remote-present")
	objectID := "sha256:" + digestHex(archive)
	if _, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive))); err != nil {
		t.Fatalf("initial Ingest: %v", err)
	}
	original, _, err := fixture.ledger.Get(objectID)
	if err != nil {
		t.Fatal(err)
	}

	result, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive)))
	if err != nil {
		t.Fatalf("idempotent Ingest: %v", err)
	}
	if result.Status != ResultExisting || fixture.backend.createCalls != 1 {
		t.Fatalf("result=%#v createCalls=%d", result, fixture.backend.createCalls)
	}
	updated, found, err := fixture.ledger.Get(objectID)
	if err != nil || !found || updated.StoredAt == nil || !updated.StoredAt.Equal(*original.StoredAt) ||
		!updated.ReceivedAt.Equal(original.ReceivedAt) || updated.EncryptedSize != original.EncryptedSize {
		t.Fatalf("stored record changed: original=%#v updated=%#v found=%v err=%v", original, updated, found, err)
	}
	if entries := spoolEntryNames(t, fixture.spool.path); len(entries) != 0 {
		t.Fatalf("idempotent local entries = %v", entries)
	}
	if fixture.status.Snapshot().TerminalLocalError {
		t.Fatal("idempotent stored repair set terminal readiness")
	}
}

func TestIngestStoredMetadataMismatchPreservesBoundPlaintext(t *testing.T) {
	fixture := newServiceFixture(t)
	archive := validRecoveryArchive(t, "stored-ledger-metadata-mismatch")
	objectID := "sha256:" + digestHex(archive)
	if _, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive))); err != nil {
		t.Fatalf("initial Ingest: %v", err)
	}
	record, _, err := fixture.ledger.Get(objectID)
	if err != nil {
		t.Fatal(err)
	}
	record.EncryptedSize++
	if err := fixture.ledger.Put(record); err != nil {
		t.Fatal(err)
	}

	_, err = fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive)))
	if err == nil || err.Error() != "collector local state failed" {
		t.Fatalf("mismatched Ingest error = %v", err)
	}
	entries := spoolEntryNames(t, fixture.spool.path)
	if len(entries) != 1 || !plainPendingPattern.MatchString(entries[0]) {
		t.Fatalf("recoverable mismatch entries = %v", entries)
	}
	preserved, found, getErr := fixture.ledger.Get(objectID)
	if getErr != nil || !found || preserved.EncryptedSize != record.EncryptedSize || preserved.StoredAt == nil {
		t.Fatalf("canonical record overwritten: %#v found=%v err=%v", preserved, found, getErr)
	}
	assertNoActiveSpoolReservations(t, fixture.spool)
	if !fixture.status.Snapshot().TerminalLocalError {
		t.Fatal("metadata mismatch did not fail readiness closed")
	}
}

func TestIngestPersistentPostRenameFsyncRetainsBoundPlaintext(t *testing.T) {
	fixture, archive, objectID, stopFailing := persistentPlaintextRecoveryFixture(t, "persistent-bound-plaintext")
	_, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive)))
	if err == nil || err.Error() != "collector local state failed" {
		t.Fatalf("Ingest error = %v", err)
	}
	assertBoundRecoveryState(t, fixture.spool, fixture.ledger, objectID, int64(len(archive)))
	assertNoActiveSpoolReservations(t, fixture.spool)
	snapshot := fixture.status.Snapshot()
	if !snapshot.SpoolWritable || snapshot.OldestPendingAt != nil || snapshot.NewestSuccessfulAt != nil || !snapshot.TerminalLocalError {
		t.Fatalf("persistent fsync readiness = %#v", snapshot)
	}

	stopFailing()
	result, retryErr := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive)))
	if retryErr != nil {
		t.Fatalf("same-process retry: %v", retryErr)
	}
	if result.Status != ResultStored || !fixture.backend.(*fakeBackend).has(objectID) {
		t.Fatalf("retry result=%#v remote=%v", result, fixture.backend.(*fakeBackend).has(objectID))
	}
	if entries := spoolEntryNames(t, fixture.spool.path); len(entries) != 0 {
		t.Fatalf("retry entries = %v", entries)
	}
	assertNoActiveSpoolReservations(t, fixture.spool)
}

func TestWorkerRecoversBoundPlaintextWhenUnsyncedFinalDisappears(t *testing.T) {
	fixture, archive, objectID, _ := persistentPlaintextRecoveryFixture(t, "lost-unsynced-final")
	if _, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive))); err == nil {
		t.Fatal("Ingest succeeded during persistent fsync failure")
	}
	finalPath := filepath.Join(fixture.spool.path, digestHex(archive)+".tar.gz.age")
	if err := os.Remove(finalPath); err != nil {
		t.Fatalf("simulate lost final: %v", err)
	}
	restartedSpool, restartedLedger := reopenRecoveryFixture(t, fixture.spool, fixture.ledger)
	status := NewStatusTracker(ReadinessConfig{StartedAt: fixture.now, StartupGrace: time.Hour, MaxRecoveryAge: 24 * time.Hour}, fixture.clock)
	service := newService(restartedSpool, restartedLedger, fixture.service.encryptor, fixture.backend, status, recoveryarchive.DefaultLimits(), serviceOps{now: fixture.clock})
	runWorkerOnce(t, restartedSpool, restartedLedger, service, status, fixture.clock)

	if !fixture.backend.(*fakeBackend).has(objectID) {
		t.Fatal("restart did not recreate exact remote object")
	}
	assertStoredRecoveryComplete(t, restartedSpool, restartedLedger, status, objectID, int64(len(archive)))
}

func TestWorkerRecoversBoundPlaintextWithSurvivingFinal(t *testing.T) {
	fixture, archive, objectID, _ := persistentPlaintextRecoveryFixture(t, "surviving-unsynced-final")
	if _, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive))); err == nil {
		t.Fatal("Ingest succeeded during persistent fsync failure")
	}
	restartedSpool, restartedLedger := reopenRecoveryFixture(t, fixture.spool, fixture.ledger)
	status := NewStatusTracker(ReadinessConfig{StartedAt: fixture.now, StartupGrace: time.Hour, MaxRecoveryAge: 24 * time.Hour}, fixture.clock)
	service := newService(restartedSpool, restartedLedger, fixture.service.encryptor, fixture.backend, status, recoveryarchive.DefaultLimits(), serviceOps{now: fixture.clock})
	runWorkerOnce(t, restartedSpool, restartedLedger, service, status, fixture.clock)

	if !fixture.backend.(*fakeBackend).has(objectID) {
		t.Fatal("restart did not commit surviving final")
	}
	assertStoredRecoveryComplete(t, restartedSpool, restartedLedger, status, objectID, int64(len(archive)))
}

func TestWorkerRecoversCrashAfterDurablePlaintextBinding(t *testing.T) {
	fixture := newServiceFixture(t)
	archive := validRecoveryArchive(t, "crash-after-plaintext-bind")
	objectID := "sha256:" + digestHex(archive)
	fixture.service.ops.crash = crashAt(crashAfterPlaintextBind)
	_, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive)))
	if err == nil || err.Error() != "collector operation interrupted" {
		t.Fatalf("Ingest error = %v", err)
	}
	entries := spoolEntryNames(t, fixture.spool.path)
	if len(entries) != 1 || !plainPendingPattern.MatchString(entries[0]) {
		t.Fatalf("bound crash entries = %v", entries)
	}
	if records, listErr := fixture.ledger.List(); listErr != nil || len(records) != 0 {
		t.Fatalf("ledger before recovery = %#v err=%v", records, listErr)
	}
	restartedSpool, restartedLedger := reopenRecoveryFixture(t, fixture.spool, fixture.ledger)
	status := NewStatusTracker(ReadinessConfig{StartedAt: fixture.now, StartupGrace: time.Hour, MaxRecoveryAge: 24 * time.Hour}, fixture.clock)
	service := newService(restartedSpool, restartedLedger, fixture.service.encryptor, fixture.backend, status, recoveryarchive.DefaultLimits(), serviceOps{now: fixture.clock})
	runWorkerOnce(t, restartedSpool, restartedLedger, service, status, fixture.clock)
	assertStoredRecoveryComplete(t, restartedSpool, restartedLedger, status, objectID, int64(len(archive)))
}

func TestSpoolStaleCleanupNeverDeletesBoundPlaintext(t *testing.T) {
	spool := openTestSpool(t, defaultSpoolOps())
	archive := validRecoveryArchive(t, "bound-not-stale")
	upload, objectID, _, err := spool.Receive(context.Background(), testReadCloser(archive), int64(len(archive)))
	if err != nil {
		t.Fatal(err)
	}
	bound, _, err := spool.BindPlaintext(upload, objectID)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Unix(1, 0)
	if err := os.Chtimes(bound, old, old); err != nil {
		t.Fatal(err)
	}
	if removed, err := spool.cleanupStalePartialsAt(time.Now().Add(48 * time.Hour)); err != nil || removed != 0 {
		t.Fatalf("cleanup removed=%d err=%v", removed, err)
	}
	if _, err := os.Stat(bound); err != nil {
		t.Fatalf("bound plaintext removed: %v", err)
	}
}

func TestSpoolBindPlaintextRenameFailurePreservesUploadReservation(t *testing.T) {
	ops := defaultSpoolOps()
	ops.renameNoReplace = func(int, string, int, string) error { return unix.EIO }
	spool := openTestSpool(t, ops)
	archive := validRecoveryArchive(t, "bind-rename-failure")
	upload, objectID, _, err := spool.Receive(context.Background(), testReadCloser(archive), int64(len(archive)))
	if err != nil {
		t.Fatal(err)
	}

	if bound, reused, err := spool.BindPlaintext(upload, objectID); err == nil || bound != "" || reused {
		t.Fatalf("BindPlaintext bound=%q reused=%v err=%v", bound, reused, err)
	}
	if _, err := os.Stat(upload); err != nil {
		t.Fatalf("upload lost after failed bind: %v", err)
	}
	spool.transition.Lock()
	_, active := spool.active[filepath.Base(upload)]
	spool.transition.Unlock()
	if !active {
		t.Fatal("failed bind released the upload reservation")
	}
}

func TestSpoolBindPlaintextDirectoryFsyncFailureRetainsBoundReservation(t *testing.T) {
	ops := defaultSpoolOps()
	rename := ops.renameNoReplace
	fsync := ops.fsync
	var renamed atomic.Bool
	ops.renameNoReplace = func(oldDirFD int, oldName string, newDirFD int, newName string) error {
		err := rename(oldDirFD, oldName, newDirFD, newName)
		if err == nil && plainPendingPattern.MatchString(newName) {
			renamed.Store(true)
		}
		return err
	}
	ops.fsync = func(fd int) error {
		if renamed.Load() {
			return unix.EIO
		}
		return fsync(fd)
	}
	spool := openTestSpool(t, ops)
	archive := validRecoveryArchive(t, "bind-fsync-failure")
	upload, objectID, _, err := spool.Receive(context.Background(), testReadCloser(archive), int64(len(archive)))
	if err != nil {
		t.Fatal(err)
	}

	bound, reused, err := spool.BindPlaintext(upload, objectID)
	if err == nil || reused || !plainPendingPattern.MatchString(filepath.Base(bound)) {
		t.Fatalf("BindPlaintext bound=%q reused=%v err=%v", bound, reused, err)
	}
	if _, err := os.Stat(upload); !os.IsNotExist(err) {
		t.Fatalf("renamed upload remains: %v", err)
	}
	if _, err := os.Stat(bound); err != nil {
		t.Fatalf("uncertain bound plaintext lost: %v", err)
	}
	spool.transition.Lock()
	_, active := spool.active[filepath.Base(bound)]
	_, oldActive := spool.active[filepath.Base(upload)]
	spool.transition.Unlock()
	if !active || oldActive {
		t.Fatalf("active reservation not moved: bound=%v upload=%v", active, oldActive)
	}
}

func TestSpoolBindPlaintextNoReplaceReusesExistingBound(t *testing.T) {
	spool := openTestSpool(t, defaultSpoolOps())
	archive := validRecoveryArchive(t, "bind-no-replace")
	firstUpload, objectID, _, err := spool.Receive(context.Background(), testReadCloser(archive), int64(len(archive)))
	if err != nil {
		t.Fatal(err)
	}
	bound, reused, err := spool.BindPlaintext(firstUpload, objectID)
	if err != nil || reused {
		t.Fatalf("first BindPlaintext reused=%v err=%v", reused, err)
	}
	before, err := os.Stat(bound)
	if err != nil {
		t.Fatal(err)
	}
	if err := spool.ReleasePlaintext(bound); err != nil {
		t.Fatal(err)
	}
	secondUpload, secondID, _, err := spool.Receive(context.Background(), testReadCloser(archive), int64(len(archive)))
	if err != nil {
		t.Fatal(err)
	}

	reusedPath, reused, err := spool.BindPlaintext(secondUpload, secondID)
	if err != nil || !reused || reusedPath != bound {
		t.Fatalf("second BindPlaintext path=%q reused=%v err=%v", reusedPath, reused, err)
	}
	after, err := os.Stat(reusedPath)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("bound plaintext overwritten: before=%v after=%v err=%v", before, after, err)
	}
	if digest, size, err := spool.HashPlaintext(reusedPath); err != nil || digest != objectID || size != int64(len(archive)) {
		t.Fatalf("reused plaintext digest=%q size=%d err=%v", digest, size, err)
	}
	if _, err := os.Stat(secondUpload); err != nil {
		t.Fatalf("duplicate upload removed before caller validation: %v", err)
	}
}

func TestIngestPlaintextBindDirectoryFsyncFailureRetainsRecoverableBound(t *testing.T) {
	ops := defaultSpoolOps()
	rename := ops.renameNoReplace
	fsync := ops.fsync
	var renamed atomic.Bool
	var fail atomic.Bool
	fail.Store(true)
	ops.renameNoReplace = func(oldDirFD int, oldName string, newDirFD int, newName string) error {
		err := rename(oldDirFD, oldName, newDirFD, newName)
		if err == nil && plainPendingPattern.MatchString(newName) {
			renamed.Store(true)
		}
		return err
	}
	ops.fsync = func(fd int) error {
		if renamed.Load() && fail.Load() {
			return unix.EIO
		}
		return fsync(fd)
	}
	fixture := newTransitionFixture(t, ops, &fakeBackend{objects: make(map[string]bool), createVisible: true}, nil)
	archive := validRecoveryArchive(t, "service-bind-fsync-failure")
	objectID := "sha256:" + digestHex(archive)

	if _, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive))); err == nil || err.Error() != "collector local state failed" {
		t.Fatalf("Ingest error = %v", err)
	}
	entries := spoolEntryNames(t, fixture.spool.path)
	if len(entries) != 1 || !plainPendingPattern.MatchString(entries[0]) {
		t.Fatalf("bind failure entries = %v", entries)
	}
	if records, err := fixture.ledger.List(); err != nil || len(records) != 0 {
		t.Fatalf("bind failure ledger = %#v err=%v", records, err)
	}
	assertNoActiveSpoolReservations(t, fixture.spool)
	if !fixture.status.Snapshot().TerminalLocalError {
		t.Fatal("bind fsync failure did not fail readiness closed")
	}

	fail.Store(false)
	result, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive)))
	if err != nil || result.Status != ResultStored || result.ObjectID != objectID {
		t.Fatalf("same-process retry result=%#v err=%v", result, err)
	}
	if entries := spoolEntryNames(t, fixture.spool.path); len(entries) != 0 {
		t.Fatalf("same-process retry entries = %v", entries)
	}
	assertNoActiveSpoolReservations(t, fixture.spool)
}

func TestIngestCorruptBoundPlaintextPreservesFreshRecoveryCopy(t *testing.T) {
	fixture := newServiceFixture(t)
	archive := validRecoveryArchive(t, "corrupt-bound-with-fresh-upload")
	upload, objectID, _, err := fixture.spool.Receive(context.Background(), testReadCloser(archive), int64(len(archive)))
	if err != nil {
		t.Fatal(err)
	}
	bound, _, err := fixture.spool.BindPlaintext(upload, objectID)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.spool.ReleasePlaintext(bound); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bound, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive))); err == nil || err.Error() != "collector local state failed" {
		t.Fatalf("Ingest error = %v", err)
	}
	entries := spoolEntryNames(t, fixture.spool.path)
	boundCount, uploadCount := 0, 0
	for _, entry := range entries {
		if plainPendingPattern.MatchString(entry) {
			boundCount++
		}
		if uploadPartialPattern.MatchString(entry) {
			uploadCount++
		}
	}
	if len(entries) != 2 || boundCount != 1 || uploadCount != 1 {
		t.Fatalf("preserved recovery entries = %v", entries)
	}
	assertNoActiveSpoolReservations(t, fixture.spool)
}

func TestWorkerCorruptBoundPlaintextFailsClosedWithoutDeletingRecoveryCopy(t *testing.T) {
	backend := &fakeBackend{objects: make(map[string]bool), createVisible: true}
	fixture := newTransitionFixture(t, defaultSpoolOps(), backend, nil)
	archive := validRecoveryArchive(t, "corrupt-bound")
	upload, objectID, _, err := fixture.spool.Receive(context.Background(), testReadCloser(archive), int64(len(archive)))
	if err != nil {
		t.Fatal(err)
	}
	bound, _, err := fixture.spool.BindPlaintext(upload, objectID)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.spool.ReleasePlaintext(bound); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bound, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}

	err = NewWorker(fixture.spool, fixture.ledger, fixture.service, fixture.status, time.Minute).Run(context.Background())
	if err == nil || err.Error() != "collector local state failed" {
		t.Fatalf("Run error = %v", err)
	}
	if _, err := os.Stat(bound); err != nil {
		t.Fatalf("corrupt recovery copy deleted: %v", err)
	}
	if backend.existsCalls != 0 || backend.createCalls != 0 {
		t.Fatalf("backend called for corrupt bound state: exists=%d create=%d", backend.existsCalls, backend.createCalls)
	}
	assertNoActiveSpoolReservations(t, fixture.spool)
	if snapshot := fixture.status.Snapshot(); !snapshot.SpoolWritable || !snapshot.TerminalLocalError {
		t.Fatalf("corrupt bound readiness = %#v", snapshot)
	}
}

func TestWorkerUsesDurablePlaintextReceiptTimeWhenLedgerIsMissing(t *testing.T) {
	fixture := newServiceFixture(t)
	archive := validRecoveryArchive(t, "durable-receipt-time")
	objectID := "sha256:" + digestHex(archive)
	fixture.service.ops.crash = crashAt(crashAfterPlaintextBind)
	if _, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive))); err == nil {
		t.Fatal("Ingest succeeded across plaintext-bind crash")
	}
	entries := spoolEntryNames(t, fixture.spool.path)
	if len(entries) != 1 || !plainPendingPattern.MatchString(entries[0]) {
		t.Fatalf("bound entries = %v", entries)
	}
	receivedAt := fixture.now.Add(-2 * time.Hour)
	bound := filepath.Join(fixture.spool.path, entries[0])
	if err := os.Chtimes(bound, receivedAt, receivedAt); err != nil {
		t.Fatal(err)
	}
	restartedSpool, restartedLedger := reopenRecoveryFixture(t, fixture.spool, fixture.ledger)
	status := NewStatusTracker(ReadinessConfig{StartedAt: fixture.now, StartupGrace: time.Hour, MaxRecoveryAge: 24 * time.Hour}, fixture.clock)
	service := newService(restartedSpool, restartedLedger, fixture.service.encryptor, fixture.backend, status, recoveryarchive.DefaultLimits(), serviceOps{now: fixture.clock})
	runWorkerOnce(t, restartedSpool, restartedLedger, service, status, fixture.clock)

	record, found, err := restartedLedger.Get(objectID)
	if err != nil || !found || !record.ReceivedAt.Equal(receivedAt) {
		t.Fatalf("recovered receipt = %#v found=%v err=%v, want %v", record, found, err, receivedAt)
	}
}

func persistentPlaintextRecoveryFixture(t testing.TB, label string) (*transitionFixture, []byte, string, func()) {
	t.Helper()
	ops := defaultSpoolOps()
	rename := ops.renameNoReplace
	fsync := ops.fsync
	var ageRenamed atomic.Bool
	var fail atomic.Bool
	fail.Store(true)
	ops.renameNoReplace = func(oldDirFD int, oldName string, newDirFD int, newName string) error {
		err := rename(oldDirFD, oldName, newDirFD, newName)
		if err == nil && completeAgePattern.MatchString(newName) {
			ageRenamed.Store(true)
		}
		return err
	}
	ops.fsync = func(fd int) error {
		if ageRenamed.Load() && fail.Load() {
			return unix.EIO
		}
		return fsync(fd)
	}
	fixture := newTransitionFixture(t, ops, &fakeBackend{objects: make(map[string]bool), createVisible: true}, nil)
	archive := validRecoveryArchive(t, label)
	return fixture, archive, "sha256:" + digestHex(archive), func() { fail.Store(false) }
}

func assertBoundRecoveryState(t testing.TB, spool *Spool, ledger *Ledger, objectID string, compressedSize int64) {
	t.Helper()
	entries := spoolEntryNames(t, spool.path)
	bound, final := 0, 0
	for _, entry := range entries {
		if plainPendingPattern.MatchString(entry) {
			bound++
		}
		if completeAgePattern.MatchString(entry) {
			final++
		}
	}
	if bound != 1 || final != 1 || len(entries) != 2 {
		t.Fatalf("recovery entries = %v", entries)
	}
	record, found, err := ledger.Get(objectID)
	if err != nil || !found || record.CompressedSize != compressedSize || record.EncryptedSize <= 0 || record.StoredAt != nil ||
		record.RetryCount != 0 || record.LastAttemptAt != nil || record.LatestRetryClass != "" {
		t.Fatalf("recovery record = %#v found=%v err=%v", record, found, err)
	}
}

func reopenRecoveryFixture(t testing.TB, currentSpool *Spool, currentLedger *Ledger) (*Spool, *Ledger) {
	t.Helper()
	spoolPath, ledgerPath := currentSpool.path, currentLedger.path
	if err := currentSpool.Close(); err != nil {
		t.Fatal(err)
	}
	if err := currentLedger.Close(); err != nil {
		t.Fatal(err)
	}
	spool, err := OpenSpool(spoolPath, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = spool.Close() })
	ledger, err := OpenLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ledger.Close() })
	return spool, ledger
}

func runWorkerOnce(t testing.TB, spool *Spool, ledger *Ledger, service *Service, status *StatusTracker, now func() time.Time) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	worker := newWorker(spool, ledger, service, status, time.Minute, workerOps{
		now: now,
		wait: func(context.Context, time.Duration) error {
			cancel()
			return context.Canceled
		},
	})
	if err := worker.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func assertStoredRecoveryComplete(t testing.TB, spool *Spool, ledger *Ledger, status *StatusTracker, objectID string, compressedSize int64) {
	t.Helper()
	if entries := spoolEntryNames(t, spool.path); len(entries) != 0 {
		t.Fatalf("recovered entries = %v", entries)
	}
	record, found, err := ledger.Get(objectID)
	if err != nil || !found || record.StoredAt == nil || record.CompressedSize != compressedSize || record.EncryptedSize <= 0 ||
		record.RetryCount != 0 || record.LastAttemptAt != nil || record.LatestRetryClass != "" {
		t.Fatalf("stored recovery record = %#v found=%v err=%v", record, found, err)
	}
	snapshot := status.Snapshot()
	if !snapshot.SpoolWritable || snapshot.OldestPendingAt != nil || snapshot.NewestSuccessfulAt == nil ||
		!snapshot.NewestSuccessfulAt.Equal(*record.StoredAt) || snapshot.TerminalLocalError {
		t.Fatalf("stored recovery readiness = %#v", snapshot)
	}
}

func equalOptionalTime(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}
