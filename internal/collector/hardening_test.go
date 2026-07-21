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

func TestSpoolReceiveOpenExactBodyWaitsForEOFOrCancellation(t *testing.T) {
	spool := openTestSpool(t, defaultSpoolOps())
	r, w := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, _, _, err := spool.Receive(ctx, r, 1); done <- err }()
	if _, err := w.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		t.Fatalf("open exact body returned without EOF: %v", err)
	case <-time.After(75 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if err == nil || err.Error() != "collector upload canceled" {
			t.Fatalf("error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("open exact body did not cancel")
	}
	_ = w.Close()
}

func TestSpoolReceiveDelayedExtraByteIsNeverAccepted(t *testing.T) {
	spool := openTestSpool(t, defaultSpoolOps())
	r := newDelayedExtraReadCloser()
	done := make(chan error, 1)
	go func() { _, _, _, err := spool.Receive(context.Background(), r, 1); done <- err }()
	<-r.probing
	select {
	case err := <-done:
		t.Fatalf("delayed overlong body accepted: %v", err)
	case <-time.After(75 * time.Millisecond):
	}
	close(r.releaseExtra)
	if err := <-done; err == nil || err.Error() != "collector upload length mismatch" {
		t.Fatalf("error = %v", err)
	}
}

func TestSpoolReceiveRejectsNonReadCloserBeforeCreatingPartial(t *testing.T) {
	spool := openTestSpool(t, defaultSpoolOps())
	_, _, _, err := spool.Receive(context.Background(), bytes.NewReader([]byte("x")), 1)
	if err == nil || err.Error() != "collector upload source not closable" {
		t.Fatalf("error = %v", err)
	}
	entries, readErr := os.ReadDir(spool.path)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("partial created: %v %v", entryNames(entries), readErr)
	}
}

func TestSpoolReceiveHandlesNEOFFraming(t *testing.T) {
	for _, tc := range []struct {
		name    string
		source  io.ReadCloser
		length  int64
		wantErr string
	}{
		{"exact", &sequenceReadCloser{steps: []readStep{{data: []byte("x"), err: io.EOF}}}, 1, ""},
		{"short", &sequenceReadCloser{steps: []readStep{{data: []byte("x"), err: io.EOF}}}, 2, "collector upload length mismatch"},
		{"overlong", &sequenceReadCloser{steps: []readStep{{data: []byte("x")}, {data: []byte("y"), err: io.EOF}}}, 1, "collector upload length mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spool := openTestSpool(t, defaultSpoolOps())
			_, _, _, err := spool.Receive(context.Background(), tc.source, tc.length)
			if tc.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if tc.wantErr != "" && (err == nil || err.Error() != tc.wantErr) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestSpoolReceiveBlockedReadIsCanceledAndJoined(t *testing.T) {
	spool := openTestSpool(t, defaultSpoolOps())
	r := newBlockingReadCloser()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, _, _, err := spool.Receive(ctx, r, 1); done <- err }()
	<-r.entered
	cancel()
	select {
	case err := <-done:
		if err == nil || err.Error() != "collector upload canceled" {
			t.Fatalf("error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked Receive was not canceled")
	}
	select {
	case <-r.exited:
	case <-time.After(time.Second):
		t.Fatal("blocked read worker was not joined")
	}
	if r.closeCount != 1 {
		t.Fatalf("source close count = %d", r.closeCount)
	}
}

func TestSpoolReceiveOwnsSourceOnEveryReturn(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		spool := openTestSpool(t, defaultSpoolOps())
		source := &trackingReadCloser{Reader: bytes.NewReader([]byte("x"))}
		if _, _, _, err := spool.Receive(context.Background(), source, 1); err != nil {
			t.Fatal(err)
		}
		if source.closeCount != 1 {
			t.Fatalf("source close count = %d", source.closeCount)
		}
	})
	t.Run("preflight failure", func(t *testing.T) {
		spool := openTestSpool(t, defaultSpoolOps())
		source := &trackingReadCloser{Reader: bytes.NewReader([]byte("x"))}
		if _, _, _, err := spool.Receive(context.Background(), source, 0); err == nil {
			t.Fatal("invalid length accepted")
		}
		if source.closeCount != 1 {
			t.Fatalf("source close count = %d", source.closeCount)
		}
	})
	t.Run("close failure clears active", func(t *testing.T) {
		spool := openTestSpool(t, defaultSpoolOps())
		source := &trackingReadCloser{Reader: bytes.NewReader([]byte("x")), closeErr: unix.EIO}
		if _, _, _, err := spool.Receive(context.Background(), source, 1); err == nil || err.Error() != "collector upload source close failed" {
			t.Fatalf("error = %v", err)
		}
		spool.transition.Lock()
		active := len(spool.active)
		spool.transition.Unlock()
		if active != 0 {
			t.Fatalf("active uploads after close failure = %d", active)
		}
	})
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
			_, _, _, err := spool.Receive(context.Background(), testReadCloser([]byte("x")), 1)
			if err == nil || err.Error() != "collector insufficient storage" {
				t.Fatalf("create error = %v", err)
			}
		})
	}
	ops := defaultSpoolOps()
	ops.fsync = func(int) error { return unix.ENOSPC }
	spool := openTestSpool(t, ops)
	_, _, _, err := spool.Receive(context.Background(), testReadCloser([]byte("x")), 1)
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
	path, _, _, err := spool.Receive(context.Background(), testReadCloser([]byte("x")), 1)
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

func TestSpoolRemovalUsesDirectUnlinkWithoutQuarantine(t *testing.T) {
	ops := defaultSpoolOps()
	renames := 0
	baseRename := ops.renameNoReplace
	ops.renameNoReplace = func(a int, b string, c int, d string) error { renames++; return baseRename(a, b, c, d) }
	spool := openTestSpool(t, ops)
	path := filepath.Join(spool.path, testDigestHex+".tar.gz.age")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := spool.RemoveEncrypted(path); err != nil {
		t.Fatal(err)
	}
	if renames != 0 {
		t.Fatalf("removal used %d quarantine renames", renames)
	}
	entries, _ := os.ReadDir(spool.path)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".sherpa-remove-") {
			t.Fatalf("quarantine residue %q", entry.Name())
		}
	}
}

func TestSpoolRemovalReportsUnlinkAndSyncFailures(t *testing.T) {
	t.Run("unlink", func(t *testing.T) {
		ops := defaultSpoolOps()
		ops.unlinkat = func(int, string, int) error { return unix.EIO }
		spool := openTestSpool(t, ops)
		path := filepath.Join(spool.path, testDigestHex+".tar.gz.age")
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := spool.RemoveEncrypted(path); err == nil {
			t.Fatal("unlink failure hidden")
		}
	})
	t.Run("sync", func(t *testing.T) {
		ops := defaultSpoolOps()
		ops.fsync = func(int) error { return unix.EIO }
		spool := openTestSpool(t, ops)
		path := filepath.Join(spool.path, testDigestHex+".tar.gz.age")
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := spool.RemoveEncrypted(path); err == nil || err.Error() != "collector spool synchronization failed" {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestSpoolLockRejectsStaticEntriesBeforeOpen(t *testing.T) {
	ops := defaultSpoolOps()
	opened := false
	baseOpen := ops.openat
	ops.openat = func(fd int, name string, flags int, mode uint32) (int, error) {
		if name == spoolLockName {
			opened = true
		}
		return baseOpen(fd, name, flags, mode)
	}
	spool := openTestSpool(t, ops)
	if err := unix.Mkfifo(filepath.Join(spool.path, spoolLockName), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := spool.AcquireLock(); err == nil {
		t.Fatal("FIFO lock accepted")
	}
	if opened {
		t.Fatal("FIFO lock operationally opened")
	}
}

func TestSpoolLockRejectsReplacementAfterFlock(t *testing.T) {
	ops := defaultSpoolOps()
	spool := openTestSpool(t, ops)
	spool.ops.flock = func(fd int, how int) error {
		if err := unix.Flock(fd, how); err != nil {
			return err
		}
		if err := os.Rename(filepath.Join(spool.path, spoolLockName), filepath.Join(spool.path, spoolLockName+".old")); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(spool.path, spoolLockName), []byte("x"), 0o600)
	}
	if _, err := spool.AcquireLock(); err == nil {
		t.Fatal("replacement lock entry accepted")
	}
}

func TestPublishedFilesRequireExactFinalMetadata(t *testing.T) {
	t.Run("spool", func(t *testing.T) {
		spool := openTestSpool(t, defaultSpoolOps())
		partial, final, _ := spool.EncryptedPaths("sha256:" + testDigestHex)
		if err := os.WriteFile(partial, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		base := spool.ops.renameNoReplace
		spool.ops.renameNoReplace = func(a int, b string, c int, d string) error {
			if err := base(a, b, c, d); err != nil {
				return err
			}
			return unix.Chmod(final, 0o640)
		}
		if err := spool.CommitEncrypted(partial, final); err == nil {
			t.Fatal("unsafe final mode accepted")
		}
	})
	t.Run("ledger", func(t *testing.T) {
		ledger := openTestLedger(t, defaultLedgerOps())
		base := ledger.ops.rename
		ledger.ops.rename = func(a int, b string, c int, d string) error {
			if err := base(a, b, c, d); err != nil {
				return err
			}
			return unix.Chmod(filepath.Join(ledger.path, d), 0o640)
		}
		if err := ledger.Put(validTestRecord(time.Now().UTC())); err == nil {
			t.Fatal("unsafe ledger mode accepted")
		}
	})
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestSpoolAcquireLockAcrossProcess")
	cmd.Env = append(os.Environ(), "SHERPA_LOCK_HELPER=1", "SHERPA_LOCK_DIR="+dir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = stdout.Close()
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
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
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	_ = stdout.Close()
	release, err := spool.AcquireLock()
	if err != nil {
		t.Fatal(err)
	}
	_ = release()
	_ = spool.Close()
}

func testReadCloser(data []byte) io.ReadCloser { return io.NopCloser(bytes.NewReader(data)) }

type trackingReadCloser struct {
	io.Reader
	closeCount int
	closeErr   error
}

func (r *trackingReadCloser) Close() error {
	r.closeCount++
	return r.closeErr
}

type readStep struct {
	data []byte
	err  error
}
type sequenceReadCloser struct {
	steps  []readStep
	closed bool
}

func (r *sequenceReadCloser) Read(p []byte) (int, error) {
	if len(r.steps) == 0 {
		return 0, io.EOF
	}
	step := r.steps[0]
	r.steps = r.steps[1:]
	n := copy(p, step.data)
	return n, step.err
}
func (r *sequenceReadCloser) Close() error { r.closed = true; return nil }

type delayedExtraReadCloser struct {
	calls        int
	probing      chan struct{}
	releaseExtra chan struct{}
	closed       chan struct{}
	once         sync.Once
}

func newDelayedExtraReadCloser() *delayedExtraReadCloser {
	return &delayedExtraReadCloser{probing: make(chan struct{}), releaseExtra: make(chan struct{}), closed: make(chan struct{})}
}
func (r *delayedExtraReadCloser) Read(p []byte) (int, error) {
	r.calls++
	if r.calls == 1 {
		p[0] = 'x'
		return 1, nil
	}
	if r.calls == 2 {
		close(r.probing)
		select {
		case <-r.releaseExtra:
			p[0] = 'y'
			return 1, io.EOF
		case <-r.closed:
			return 0, os.ErrClosed
		}
	}
	return 0, io.EOF
}
func (r *delayedExtraReadCloser) Close() error { r.once.Do(func() { close(r.closed) }); return nil }

type blockingReadCloser struct {
	entered    chan struct{}
	exited     chan struct{}
	unblock    chan struct{}
	once       sync.Once
	closeCount int
}

func newBlockingReadCloser() *blockingReadCloser {
	return &blockingReadCloser{entered: make(chan struct{}), exited: make(chan struct{}), unblock: make(chan struct{})}
}
func (r *blockingReadCloser) Read([]byte) (int, error) {
	close(r.entered)
	<-r.unblock
	close(r.exited)
	return 0, os.ErrClosed
}
func (r *blockingReadCloser) Close() error {
	r.once.Do(func() { r.closeCount++; close(r.unblock) })
	return nil
}

type zeroReader struct{}

func (zeroReader) Read([]byte) (int, error) { return 0, nil }
func (zeroReader) Close() error             { return nil }
