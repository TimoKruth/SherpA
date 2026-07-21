package collector

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const testDigestHex = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestSpoolAcquireLockRejectsSecondProcess(t *testing.T) {
	dir := newPrivateDir(t)
	first, err := OpenSpool(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := OpenSpool(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	release, err := first.AcquireLock()
	if err != nil {
		t.Fatalf("first AcquireLock: %v", err)
	}
	if _, err := second.AcquireLock(); err == nil || err.Error() != "collector spool already in use" {
		t.Fatalf("second AcquireLock error = %v", err)
	}
	if err := release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := release(); err != nil {
		t.Fatalf("idempotent release: %v", err)
	}
	releaseAgain, err := second.AcquireLock()
	if err != nil {
		t.Fatalf("AcquireLock after release: %v", err)
	}
	if err := releaseAgain(); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(filepath.Join(dir, spoolLockName))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("lock mode = %04o", got)
	}
}

func TestSpoolReceiveHashesExactCompressedBytes(t *testing.T) {
	spool := openTestSpool(t, defaultSpoolOps())
	body := bytes.Repeat([]byte("compressed-archive-canary\n"), 4096)
	path, objectID, written, err := spool.Receive(context.Background(), bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if written != int64(len(body)) {
		t.Fatalf("written = %d", written)
	}
	wantDigest := fmt.Sprintf("sha256:%x", sha256.Sum256(body))
	if objectID != wantDigest {
		t.Fatalf("object ID = %q, want %q", objectID, wantDigest)
	}
	if filepath.Dir(path) != spool.path || !validUploadPartialName(filepath.Base(path)) {
		t.Fatalf("unsafe returned path %q", path)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatal("received bytes differ")
	}
	var stat unix.Stat_t
	if err := unix.Stat(path, &stat); err != nil {
		t.Fatal(err)
	}
	if gotMode := stat.Mode & 0o7777; gotMode != 0o600 {
		t.Fatalf("raw mode = %04o", gotMode)
	}
}

func TestSpoolReceiveRejectsShortAndOverlongBodies(t *testing.T) {
	for _, tc := range []struct {
		name     string
		body     []byte
		declared int64
		want     string
	}{
		{"short", []byte("short"), 6, "collector upload length mismatch"},
		{"overlong", []byte("overlong"), 7, "collector upload length mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spool := openTestSpool(t, defaultSpoolOps())
			path, digest, written, err := spool.Receive(context.Background(), bytes.NewReader(tc.body), tc.declared)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("Receive error = %v", err)
			}
			if path != "" || digest != "" || written != 0 {
				t.Fatalf("unsafe failure result = %q %q %d", path, digest, written)
			}
			entries, readErr := os.ReadDir(spool.path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(entries) != 1 || !validUploadPartialName(entries[0].Name()) {
				t.Fatalf("failure entries = %v", entryNames(entries))
			}
		})
	}
}

func TestSpoolReceiveRejectsOverlongBodyAfterTransientEmptyRead(t *testing.T) {
	spool := openTestSpool(t, defaultSpoolOps())
	reader := &transientEmptyReader{}
	if _, _, _, err := spool.Receive(context.Background(), reader, 1); err == nil || err.Error() != "collector upload length mismatch" {
		t.Fatalf("Receive error = %v", err)
	}
}

func TestSpoolAdmitRequiresSpaceForPlaintextEncryptedCopyAndReserve(t *testing.T) {
	const length = int64(4096)
	for _, tc := range []struct {
		name      string
		available uint64
		ok        bool
	}{
		{"one byte short", uint64(spoolSafetyReserve + 2*length - 1), false},
		{"exactly enough", uint64(spoolSafetyReserve + 2*length), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ops := defaultSpoolOps()
			ops.statfs = func(_ int, stat *unix.Statfs_t) error { stat.Bsize = 1; stat.Bavail = tc.available; return nil }
			spool := openTestSpool(t, ops)
			err := spool.Admit(length)
			if tc.ok && err != nil {
				t.Fatalf("Admit: %v", err)
			}
			if !tc.ok && (err == nil || err.Error() != "collector insufficient storage") {
				t.Fatalf("Admit error = %v", err)
			}
		})
	}
	spool := openTestSpool(t, defaultSpoolOps())
	for _, length := range []int64{0, -1, math.MaxInt64} {
		if err := spool.Admit(length); err == nil {
			t.Fatalf("Admit(%d) succeeded", length)
		}
	}
}

func TestSpoolCommitEncryptedSyncsFileRenamesAndDirectory(t *testing.T) {
	var events []string
	ops := defaultSpoolOps()
	ops.fsync = func(fd int) error {
		if fd == -1 {
			return errors.New("invalid")
		}
		events = append(events, "sync")
		return unix.Fsync(fd)
	}
	ops.renameNoReplace = func(oldFD int, oldName string, newFD int, newName string) error {
		events = append(events, "rename")
		return renameAtNoReplace(oldFD, oldName, newFD, newName)
	}
	spool := openTestSpool(t, ops)
	partial, final, err := spool.EncryptedPaths("sha256:" + testDigestHex)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(partial, []byte("encrypted"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := spool.CommitEncrypted(partial, final); err != nil {
		t.Fatal(err)
	}
	if want := []string{"sync", "rename", "sync"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	if _, err := os.Stat(partial); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial remains: %v", err)
	}
	if got, err := os.ReadFile(final); err != nil || string(got) != "encrypted" {
		t.Fatalf("final = %q, %v", got, err)
	}

	if err := os.WriteFile(partial, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := spool.CommitEncrypted(partial, final); err == nil {
		t.Fatal("existing final was replaced")
	}
	if got, _ := os.ReadFile(final); string(got) != "encrypted" {
		t.Fatal("existing final changed")
	}
}

func TestSpoolCommitEncryptedRejectsPartialReplacementRace(t *testing.T) {
	ops := defaultSpoolOps()
	spool := openTestSpool(t, ops)
	partial, final, _ := spool.EncryptedPaths("sha256:" + testDigestHex)
	if err := os.WriteFile(partial, []byte("validated"), 0o600); err != nil {
		t.Fatal(err)
	}
	original := partial + ".original"
	spool.ops.fsync = func(fd int) error {
		if err := unix.Fsync(fd); err != nil {
			return err
		}
		if err := os.Rename(partial, original); err != nil {
			return err
		}
		return os.WriteFile(partial, []byte("replacement"), 0o600)
	}
	if err := spool.CommitEncrypted(partial, final); err == nil {
		t.Fatal("replaced partial was committed")
	}
	if _, err := os.Stat(final); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsafe final exists: %v", err)
	}
}

func TestSpoolCommitEncryptedPostRenameSyncFailureIsTerminal(t *testing.T) {
	ops := defaultSpoolOps()
	calls := 0
	ops.fsync = func(fd int) error {
		calls++
		if calls == 2 {
			return errors.New("raw-private-sync")
		}
		return unix.Fsync(fd)
	}
	spool := openTestSpool(t, ops)
	partial, final, _ := spool.EncryptedPaths("sha256:" + testDigestHex)
	if err := os.WriteFile(partial, []byte("encrypted"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := spool.CommitEncrypted(partial, final)
	if err == nil || err.Error() != "collector spool synchronization failed" || strings.Contains(err.Error(), "raw-private") {
		t.Fatalf("error = %v", err)
	}
	if _, statErr := os.Stat(final); statErr != nil {
		t.Fatalf("rename was incorrectly rolled back: %v", statErr)
	}
}

func TestSpoolNeverDiscoversPartialFiles(t *testing.T) {
	spool := openTestSpool(t, defaultSpoolOps())
	for _, name := range []string{
		"." + testDigestHex + ".tar.gz.age", testDigestHex + ".tar.gz.age.partial",
		"g" + testDigestHex[1:] + ".tar.gz.age", testDigestHex + ".tar.gz.age.extra",
		".sherpa-upload-deadbeef.upload.partial",
	} {
		if err := os.WriteFile(filepath.Join(spool.path, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := unix.Mkfifo(filepath.Join(spool.path, testDigestHex+".tar.gz.age"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(newPrivateDir(t), "target")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(spool.path, strings.Repeat("a", 64)+".tar.gz.age")); err != nil {
		t.Fatal(err)
	}

	objects, err := spool.Discover()
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(objects) != 0 {
		t.Fatalf("objects = %+v", objects)
	}
}

func TestSpoolDiscoversCompleteAgeFilesOldestFirst(t *testing.T) {
	spool := openTestSpool(t, defaultSpoolOps())
	now := time.Now().Add(-time.Hour).Truncate(time.Second)
	digests := []string{strings.Repeat("b", 64), strings.Repeat("a", 64), strings.Repeat("c", 64)}
	mtimes := []time.Time{now, now, now.Add(time.Minute)}
	for i, digest := range digests {
		path := filepath.Join(spool.path, digest+".tar.gz.age")
		if err := os.WriteFile(path, bytes.Repeat([]byte{byte(i)}, i+1), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, mtimes[i], mtimes[i]); err != nil {
			t.Fatal(err)
		}
	}
	objects, err := spool.Discover()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"sha256:" + strings.Repeat("a", 64), "sha256:" + strings.Repeat("b", 64), "sha256:" + strings.Repeat("c", 64)}
	var got []string
	for _, object := range objects {
		got = append(got, object.ObjectID)
		if object.ArchiveName != "sherpa-"+object.DigestHex || filepath.Dir(object.EncryptedPath) != spool.path || object.EncryptedSize <= 0 {
			t.Fatalf("bad object: %+v", object)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestSpoolCleanupRemovesOnlyStalePartials(t *testing.T) {
	spool := openTestSpoolWithAge(t, defaultSpoolOps(), time.Hour)
	now := time.Now()
	stale := []string{".sherpa-upload-0123456789abcdef0123456789abcdef.upload.partial", testDigestHex + ".tar.gz.age.partial"}
	keep := []string{".sherpa-upload-deadbeef.upload.partial.extra", testDigestHex + ".tar.gz.age", "g" + testDigestHex[1:] + ".tar.gz.age.partial"}
	for _, name := range append(append([]string{}, stale...), keep...) {
		path := filepath.Join(spool.path, name)
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	boundary := filepath.Join(spool.path, ".sherpa-upload-boundary.upload.partial")
	if err := os.WriteFile(boundary, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(boundary, now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(spool.path, ".sherpa-upload-symlink.upload.partial")
	outside := filepath.Join(newPrivateDir(t), "outside")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	removed, err := spool.cleanupStalePartialsAt(now)
	if err != nil {
		t.Fatal(err)
	}
	if removed != len(stale) {
		t.Fatalf("removed = %d", removed)
	}
	for _, name := range stale {
		if _, err := os.Stat(filepath.Join(spool.path, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stale %q remains", name)
		}
	}
	for _, name := range append(keep, filepath.Base(boundary), filepath.Base(link)) {
		if _, err := os.Lstat(filepath.Join(spool.path, name)); err != nil {
			t.Fatalf("kept %q missing: %v", name, err)
		}
	}
}

func TestSpoolCheckWritableUsesMode0600Probe(t *testing.T) {
	ops := defaultSpoolOps()
	var sawProbe bool
	ops.fsync = func(fd int) error {
		var stat unix.Stat_t
		if err := unix.Fstat(fd, &stat); err != nil {
			return err
		}
		if stat.Mode&unix.S_IFMT == unix.S_IFREG {
			sawProbe = true
			if got := stat.Mode & 0o7777; got != 0o600 {
				return fmt.Errorf("probe mode %04o", got)
			}
		}
		return unix.Fsync(fd)
	}
	spool := openTestSpool(t, ops)
	oldUmask := unix.Umask(0o777)
	defer unix.Umask(oldUmask)
	if err := spool.CheckWritable(); err != nil {
		t.Fatal(err)
	}
	if !sawProbe {
		t.Fatal("probe file was not synced")
	}
	entries, err := os.ReadDir(spool.path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("probe residue = %v", entryNames(entries))
	}
}

func TestSpoolDiskFullClassifiesAsInsufficientStorage(t *testing.T) {
	ops := defaultSpoolOps()
	ops.statfs = func(_ int, _ *unix.Statfs_t) error { return errors.New("raw-path-and-device-canary") }
	spool := openTestSpool(t, ops)
	err := spool.Admit(1)
	if err == nil || err.Error() != "collector insufficient storage" || strings.Contains(err.Error(), "canary") {
		t.Fatalf("error = %v", err)
	}
}

func TestSpoolRejectsUnsafeRootsAndArguments(t *testing.T) {
	parent := newPrivateDir(t)
	outside := newPrivateDir(t)
	link := filepath.Join(parent, "spool-link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSpool(link, time.Hour); err == nil || err.Error() != "collector spool directory unsafe" {
		t.Fatalf("symlink root error = %v", err)
	}
	ancestorTarget := newPrivateDir(t)
	if err := os.Mkdir(filepath.Join(ancestorTarget, "spool"), 0o700); err != nil {
		t.Fatal(err)
	}
	ancestorLink := filepath.Join(parent, "ancestor-link")
	if err := os.Symlink(ancestorTarget, ancestorLink); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSpool(filepath.Join(ancestorLink, "spool"), time.Hour); err == nil || err.Error() != "collector spool directory unsafe" {
		t.Fatalf("symlink ancestor error = %v", err)
	}
	unsafe := filepath.Join(parent, "unsafe")
	if err := os.Mkdir(unsafe, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSpool(unsafe, time.Hour); err == nil {
		t.Fatal("unsafe mode accepted")
	}
	if err := unix.Chmod(unsafe, 0o1700); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSpool(unsafe, time.Hour); err == nil {
		t.Fatal("special mode bits accepted")
	}
	spool := openTestSpool(t, defaultSpoolOps())
	for _, objectID := range []string{"", testDigestHex, "sha256:" + strings.ToUpper(testDigestHex), "sha256:../" + testDigestHex[:61]} {
		if _, _, err := spool.EncryptedPaths(objectID); err == nil {
			t.Fatalf("invalid object ID accepted: %q", objectID)
		}
	}
	outsidePath := filepath.Join(parent, ".sherpa-upload-deadbeef.upload.partial")
	if err := os.WriteFile(outsidePath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := spool.RemovePlaintext(outsidePath); err == nil {
		t.Fatal("outside plaintext accepted")
	}
	if err := spool.RemoveEncrypted(filepath.Join(parent, testDigestHex+".tar.gz.age")); err == nil {
		t.Fatal("outside encrypted path accepted")
	}
}

func TestSpoolReceiveHonorsContextCancellation(t *testing.T) {
	spool := openTestSpool(t, defaultSpoolOps())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	path, digest, written, err := spool.Receive(ctx, bytes.NewReader([]byte("x")), 1)
	if err == nil || err.Error() != "collector upload canceled" {
		t.Fatalf("error = %v", err)
	}
	if path != "" || digest != "" || written != 0 {
		t.Fatalf("unsafe result = %q %q %d", path, digest, written)
	}
}

func TestSpoolRemoveValidatesCanonicalNamesAndSyncs(t *testing.T) {
	spool := openTestSpool(t, defaultSpoolOps())
	plain := filepath.Join(spool.path, ".sherpa-upload-0123456789abcdef0123456789abcdef.upload.partial")
	encrypted := filepath.Join(spool.path, testDigestHex+".tar.gz.age")
	for _, path := range []string{plain, encrypted} {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := spool.RemovePlaintext(plain); err != nil {
		t.Fatal(err)
	}
	if err := spool.RemoveEncrypted(encrypted); err != nil {
		t.Fatal(err)
	}
}

func openTestSpool(t testing.TB, ops spoolOps) *Spool { return openTestSpoolWithAge(t, ops, time.Hour) }
func openTestSpoolWithAge(t testing.TB, ops spoolOps, age time.Duration) *Spool {
	t.Helper()
	spool, err := openSpool(newPrivateDir(t), age, ops)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = spool.Close() })
	return spool
}
func newPrivateDir(t testing.TB) string {
	t.Helper()
	root := safeTempDir(t)
	dir := filepath.Join(root, "private")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}
func entryNames(entries []os.DirEntry) []string {
	names := make([]string, len(entries))
	for i := range entries {
		names[i] = entries[i].Name()
	}
	return names
}

type transientEmptyReader struct{ calls int }

func (r *transientEmptyReader) Read(buffer []byte) (int, error) {
	r.calls++
	switch r.calls {
	case 1:
		buffer[0] = 'x'
		return 1, nil
	case 2:
		return 0, nil
	default:
		buffer[0] = 'y'
		return 1, nil
	}
}
