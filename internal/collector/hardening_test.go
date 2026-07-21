package collector

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestSpoolReceiveExactBodyDoesNotWaitForOpenSource(t *testing.T) {
	spool := openTestSpool(t, defaultSpoolOps())
	r, w := io.Pipe()
	defer w.Close()
	go func() { _, _ = w.Write([]byte("x")) }()
	done := make(chan error, 1)
	go func() { _, _, _, err := spool.Receive(context.Background(), r, 1); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Receive: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Receive waited forever for open exact-length source")
	}
}

func TestSpoolReceiveBlockedReadIsCanceled(t *testing.T) {
	spool := openTestSpool(t, defaultSpoolOps())
	r, w := io.Pipe()
	defer w.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, _, _, err := spool.Receive(ctx, r, 1); done <- err }()
	cancel()
	select {
	case err := <-done:
		if err == nil || err.Error() != "collector upload canceled" {
			t.Fatalf("error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked Receive was not canceled")
	}
}

func TestSpoolReceiveBoundsRepeatedZeroProgress(t *testing.T) {
	spool := openTestSpool(t, defaultSpoolOps())
	_, _, _, err := spool.Receive(context.Background(), zeroReader{}, 1)
	if err == nil || err.Error() != "collector upload read failed" {
		t.Fatalf("error = %v", err)
	}
}

func TestSpoolReceiveOverlongProbeCanBeCanceled(t *testing.T) {
	spool := openTestSpool(t, defaultSpoolOps())
	r, w := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, _, _, err := spool.Receive(ctx, r, 1); done <- err }()
	if _, err := w.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil || err.Error() != "collector upload canceled" {
			t.Fatalf("error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("overlong probe was not canceled")
	}
	_ = w.Close()
}

func TestSpoolStorageErrorsClassifyInsufficientStorage(t *testing.T) {
	for _, errno := range []error{unix.ENOSPC, unix.EDQUOT} {
		t.Run(errno.Error(), func(t *testing.T) {
			ops := defaultSpoolOps()
			ops.openat = func(int, string, int, uint32) (int, error) { return -1, errno }
			spool := openTestSpool(t, ops)
			_, _, _, err := spool.Receive(context.Background(), bytes.NewReader([]byte("x")), 1)
			if err == nil || err.Error() != "collector insufficient storage" {
				t.Fatalf("create error = %v", err)
			}
		})
	}
	ops := defaultSpoolOps()
	ops.fsync = func(int) error { return unix.ENOSPC }
	spool := openTestSpool(t, ops)
	_, _, _, err := spool.Receive(context.Background(), bytes.NewReader([]byte("x")), 1)
	if err == nil || err.Error() != "collector insufficient storage" {
		t.Fatalf("fsync error = %v", err)
	}
}

func TestSpoolUploadPartialGrammarRequiresExactRandomLength(t *testing.T) {
	if !validUploadPartialName(".sherpa-upload-0123456789abcdef0123456789abcdef.upload.partial") {
		t.Fatal("exact grammar rejected")
	}
	for _, n := range []int{0, 1, 31, 33, 64} {
		name := ".sherpa-upload-" + strings.Repeat("a", n) + ".upload.partial"
		if validUploadPartialName(name) {
			t.Fatalf("accepted %d hex characters", n)
		}
	}
}

func TestSpoolCleanupPreservesActivePartials(t *testing.T) {
	spool := openTestSpoolWithAge(t, defaultSpoolOps(), time.Millisecond)
	path, _, _, err := spool.Receive(context.Background(), bytes.NewReader([]byte("x")), 1)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	removed, err := spool.cleanupStalePartialsAt(time.Now())
	if err != nil || removed != 0 {
		t.Fatalf("active plaintext removed=%d err=%v", removed, err)
	}
	if err := spool.RemovePlaintext(path); err != nil {
		t.Fatal(err)
	}

	partial, _, err := spool.EncryptedPaths("sha256:" + testDigestHex)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(partial, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(partial, old, old); err != nil {
		t.Fatal(err)
	}
	removed, err = spool.cleanupStalePartialsAt(time.Now())
	if err != nil || removed != 0 {
		t.Fatalf("active age partial removed=%d err=%v", removed, err)
	}
	if err := spool.RemoveEncrypted(partial); err != nil {
		t.Fatal(err)
	}
}

func TestSpoolOpenCanonicalizesRelativeRoot(t *testing.T) {
	root := newPrivateDir(t)
	child := filepath.Join(root, "spool")
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatal(err)
	}
	old, _ := os.Getwd()
	defer os.Chdir(old)
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	spool, err := OpenSpool("spool", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	if !filepath.IsAbs(spool.path) {
		t.Fatalf("stored path is relative: %q", spool.path)
	}
}

func TestSpoolMetadataRejectsFIFOWithoutOperationalOpen(t *testing.T) {
	ops := defaultSpoolOps()
	openedCanonical := false
	baseOpen := ops.openat
	ops.openat = func(fd int, name string, flags int, mode uint32) (int, error) {
		if name == testDigestHex+".tar.gz.age" {
			openedCanonical = true
		}
		return baseOpen(fd, name, flags, mode)
	}
	spool := openTestSpool(t, ops)
	if err := unix.Mkfifo(filepath.Join(spool.path, testDigestHex+".tar.gz.age"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := spool.Discover(); err != nil {
		t.Fatal(err)
	}
	if openedCanonical {
		t.Fatal("FIFO was operationally opened")
	}
}

func TestSpoolCloseWaitsForInFlightOperation(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	ops := defaultSpoolOps()
	ops.statfs = func(fd int, st *unix.Statfs_t) error { close(entered); <-release; return unix.Fstatfs(fd, st) }
	spool := openTestSpool(t, ops)
	done := make(chan error, 1)
	go func() { done <- spool.Admit(1) }()
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- spool.Close() }()
	select {
	case <-closed:
		t.Fatal("Close did not wait")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if err := spool.Admit(1); err == nil {
		t.Fatal("operation started after Close")
	}
}

func TestLedgerRecordInvariantMatrix(t *testing.T) {
	now := time.Now().UTC()
	later := now.Add(time.Minute)
	validRetry := validTestRecord(now)
	validRetry.RetryCount = 1
	validRetry.LastAttemptAt = &later
	validRetry.LatestRetryClass = "backend_timeout"
	allowed := []string{"", "temporary", "backend_unavailable", "backend_timeout", "backend_failed", "remote_verification_failed", "local_state_failed", "cancelled"}
	for _, class := range allowed {
		r := validRetry
		if class == "" {
			r.RetryCount = 0
			r.LastAttemptAt = nil
		}
		r.LatestRetryClass = class
		if _, ok := validateObjectRecord(r); !ok {
			t.Fatalf("allowed class rejected: %q", class)
		}
	}
	before := now.Add(-time.Second)
	invalid := []ObjectRecord{
		func() ObjectRecord { r := validTestRecord(now); r.CompressedSize = 0; return r }(),
		func() ObjectRecord { r := validTestRecord(now); r.EncryptedSize = 0; return r }(),
		func() ObjectRecord { r := validTestRecord(now); r.RetryCount = 1; return r }(),
		func() ObjectRecord { r := validTestRecord(now); r.LatestRetryClass = "temporary"; return r }(),
		func() ObjectRecord { r := validRetry; r.LatestRetryClass = "other"; return r }(),
		func() ObjectRecord { r := validTestRecord(now); r.StoredAt = &before; return r }(),
		func() ObjectRecord { r := validRetry; r.LastAttemptAt = &before; return r }(),
	}
	for i, r := range invalid {
		if _, ok := validateObjectRecord(r); ok {
			t.Fatalf("invalid record %d accepted", i)
		}
	}
}

func TestLedgerPutRejectsFinalInodeReplacement(t *testing.T) {
	ops := defaultLedgerOps()
	ledger := openTestLedger(t, ops)
	baseRename := ledger.ops.rename
	ledger.ops.rename = func(oldfd int, old string, newfd int, new string) error {
		if err := baseRename(oldfd, old, newfd, new); err != nil {
			return err
		}
		if err := unix.Unlinkat(newfd, new, 0); err != nil {
			return err
		}
		fd, err := unix.Openat(newfd, new, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY, 0o600)
		if err == nil {
			_ = unix.Close(fd)
		}
		return err
	}
	if err := ledger.Put(validTestRecord(time.Now().UTC())); err == nil {
		t.Fatal("replacement inode accepted")
	}
}

func TestLedgerCloseWaitsForPut(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	ops := defaultLedgerOps()
	var once sync.Once
	ops.fsync = func(fd int) error { once.Do(func() { close(entered); <-release }); return unix.Fsync(fd) }
	ledger := openTestLedger(t, ops)
	done := make(chan error, 1)
	go func() { done <- ledger.Put(validTestRecord(time.Now().UTC())) }()
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- ledger.Close() }()
	select {
	case <-closed:
		t.Fatal("Close did not wait")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	_ = <-done
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if _, _, err := ledger.Get("sha256:" + testDigestHex); err == nil {
		t.Fatal("Get started after Close")
	}
}

func TestSpoolAcquireLockAcrossProcess(t *testing.T) {
	if os.Getenv("SHERPA_LOCK_HELPER") == "1" {
		dir := os.Getenv("SHERPA_LOCK_DIR")
		s, err := OpenSpool(dir, time.Hour)
		if err != nil {
			os.Exit(2)
		}
		release, err := s.AcquireLock()
		if err != nil {
			os.Exit(3)
		}
		_, _ = os.Stdout.WriteString("ready\n")
		_, _ = io.Copy(io.Discard, os.Stdin)
		_ = release()
		_ = s.Close()
		os.Exit(0)
	}
	dir := newPrivateDir(t)
	cmd := exec.Command(os.Args[0], "-test.run=TestSpoolAcquireLockAcrossProcess")
	cmd.Env = append(os.Environ(), "SHERPA_LOCK_HELPER=1", "SHERPA_LOCK_DIR="+dir)
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 6)
	if _, err := io.ReadFull(stdout, buf); err != nil {
		t.Fatal(err)
	}
	spool, err := OpenSpool(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := spool.AcquireLock(); err == nil || err.Error() != "collector spool already in use" {
		t.Fatalf("contention = %v", err)
	}
	_ = stdin.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	release, err := spool.AcquireLock()
	if err != nil {
		t.Fatal(err)
	}
	_ = release()
	_ = spool.Close()
}

type zeroReader struct{}

func (zeroReader) Read([]byte) (int, error) { return 0, nil }
