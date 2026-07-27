package collector

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"golang.org/x/sys/unix"
)

func TestEncryptFileRoundTripsWithGeneratedX25519Identity(t *testing.T) {
	identity := generateTestIdentity(t)
	dir := safeTempDir(t)
	sourcePath := filepath.Join(dir, "plaintext-archive-canary.tar.gz")
	partialPath := filepath.Join(dir, "encrypted-object-canary.age.partial")
	plaintext := bytes.Repeat([]byte("streaming-age-round-trip-canary\n"), 32*1024)
	if err := os.WriteFile(sourcePath, plaintext, 0o600); err != nil {
		t.Fatal(err)
	}

	gotBytes, err := NewEncryptor(identity.Recipient()).EncryptFile(context.Background(), sourcePath, partialPath)
	if err != nil {
		t.Fatalf("EncryptFile: %v", err)
	}
	info, err := os.Stat(partialPath)
	if err != nil {
		t.Fatal(err)
	}
	if gotBytes != info.Size() {
		t.Fatalf("encrypted byte count = %d, file size = %d", gotBytes, info.Size())
	}
	if gotBytes <= 0 || gotBytes == int64(len(plaintext)) {
		t.Fatalf("encrypted byte count = %d, want nonzero encrypted output size", gotBytes)
	}

	encrypted, err := os.Open(partialPath)
	if err != nil {
		t.Fatal(err)
	}
	decrypted, err := age.Decrypt(encrypted, identity)
	if err != nil {
		_ = encrypted.Close()
		t.Fatalf("age.Decrypt: %v", err)
	}
	gotPlaintext, err := io.ReadAll(decrypted)
	if err != nil {
		_ = encrypted.Close()
		t.Fatal(err)
	}
	if err := encrypted.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotPlaintext, plaintext) {
		t.Fatal("decrypted content does not match source")
	}
	preserved, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(preserved, plaintext) {
		t.Fatal("source was modified")
	}
}

func TestEncryptedSizeMatchesX25519Output(t *testing.T) {
	identity := generateTestIdentity(t)
	encryptor := NewEncryptor(identity.Recipient())
	for _, plaintextSize := range []int{0, 1, 64 * 1024, 64*1024 + 1} {
		t.Run(fmt.Sprintf("plaintext-%d", plaintextSize), func(t *testing.T) {
			dir := safeTempDir(t)
			sourcePath := filepath.Join(dir, "size-source")
			partialPath := filepath.Join(dir, "size-output.age.partial")
			if err := os.WriteFile(sourcePath, make([]byte, plaintextSize), 0o600); err != nil {
				t.Fatal(err)
			}
			want, err := encryptor.encryptedSize(int64(plaintextSize))
			if err != nil {
				t.Fatalf("EncryptedSize: %v", err)
			}
			got, err := encryptor.EncryptFile(context.Background(), sourcePath, partialPath)
			if err != nil {
				t.Fatalf("EncryptFile: %v", err)
			}
			if got != want {
				t.Fatalf("encryptedSize(%d) = %d, actual output = %d", plaintextSize, want, got)
			}
		})
	}
}

func TestEncryptFileWritesMode0600AndSyncsBeforeSuccess(t *testing.T) {
	identity := generateTestIdentity(t)
	dir := safeTempDir(t)
	sourcePath := filepath.Join(dir, "mode-source-canary")
	if err := os.WriteFile(sourcePath, []byte("mode and sync canary"), 0o600); err != nil {
		t.Fatal(err)
	}
	encryptor := NewEncryptor(identity.Recipient())

	t.Run("mode is independent of umask", func(t *testing.T) {
		partialPath := filepath.Join(dir, "mode-output-canary.age.partial")
		oldUmask := unix.Umask(0o777)
		defer unix.Umask(oldUmask)

		if _, err := encryptor.EncryptFile(context.Background(), sourcePath, partialPath); err != nil {
			t.Fatalf("EncryptFile: %v", err)
		}
		var stat unix.Stat_t
		if err := unix.Stat(partialPath, &stat); err != nil {
			t.Fatal(err)
		}
		if got := stat.Mode & 0o7777; got != 0o600 {
			t.Fatalf("destination raw mode = %04o, want 0600", got)
		}
	})

	t.Run("rejects wrong or special final mode", func(t *testing.T) {
		for _, rawMode := range []uint32{0o640, 0o4600} {
			rawMode := rawMode
			t.Run(fmt.Sprintf("%04o", rawMode), func(t *testing.T) {
				partialPath := filepath.Join(dir, fmt.Sprintf("unsafe-mode-%04o.age.partial", rawMode))
				ops := encryptionFileFinalizer{
					chmod: func(fd int, _ uint32) error {
						return unix.Fchmod(fd, rawMode)
					},
					sync:  func(file *os.File) error { return file.Sync() },
					close: func(file *os.File) error { return file.Close() },
				}

				gotBytes, err := encryptor.encryptFile(context.Background(), sourcePath, partialPath, ops)
				if err == nil || err.Error() != "collector encryption destination unsafe" {
					t.Fatalf("unsafe mode error = %v, want fixed safe classification", err)
				}
				if gotBytes != 0 {
					t.Fatalf("encrypted bytes for unsafe mode = %d, want 0", gotBytes)
				}
				if _, statErr := os.Stat(partialPath); statErr != nil {
					t.Fatalf("caller-owned partial missing after mode rejection: %v", statErr)
				}
			})
		}
	})

	t.Run("age close precedes sync destination close and success", func(t *testing.T) {
		partialPath := filepath.Join(dir, "ordered-output-canary.age.partial")
		var events []string
		ops := encryptionFileFinalizer{
			ageClose: func(writer io.Closer) error {
				events = append(events, "age-close")
				return writer.Close()
			},
			sync: func(file *os.File) error {
				events = append(events, "sync")
				return file.Sync()
			},
			close: func(file *os.File) error {
				events = append(events, "destination-close")
				return file.Close()
			},
		}

		if _, err := encryptor.encryptFile(context.Background(), sourcePath, partialPath, ops); err != nil {
			t.Fatalf("encryptFile: %v", err)
		}
		if want := []string{"age-close", "sync", "destination-close"}; !reflect.DeepEqual(events, want) {
			t.Fatalf("finalization events = %v, want %v", events, want)
		}
	})

	t.Run("age close failure skips sync and still closes destination", func(t *testing.T) {
		failureDir := safeTempDir(t)
		failureSource := filepath.Join(failureDir, "age-close-source-canary")
		partialPath := filepath.Join(failureDir, "age-close-failure-output-canary.age.partial")
		if err := os.WriteFile(failureSource, []byte("age close failure canary"), 0o600); err != nil {
			t.Fatal(err)
		}
		const rawAgeCloseCanary = "raw-age-close-failure-canary"
		var events []string
		ops := encryptionFileFinalizer{
			ageClose: func(writer io.Closer) error {
				events = append(events, "age-close")
				if err := writer.Close(); err != nil {
					return err
				}
				return errors.New(rawAgeCloseCanary)
			},
			sync: func(file *os.File) error {
				events = append(events, "sync")
				return file.Sync()
			},
			close: func(file *os.File) error {
				events = append(events, "destination-close")
				return file.Close()
			},
		}

		gotBytes, err := encryptor.encryptFile(context.Background(), failureSource, partialPath, ops)
		if err == nil || err.Error() != "collector encryption finalization failed" {
			t.Fatalf("age close error = %v, want fixed safe classification", err)
		}
		if gotBytes != 0 {
			t.Fatalf("encrypted byte count on age close failure = %d, want 0", gotBytes)
		}
		if strings.Contains(err.Error(), rawAgeCloseCanary) {
			t.Fatalf("age close error exposed raw failure: %q", err)
		}
		if want := []string{"age-close", "destination-close"}; !reflect.DeepEqual(events, want) {
			t.Fatalf("age close failure events = %v, want %v", events, want)
		}
		if _, statErr := os.Stat(partialPath); statErr != nil {
			t.Fatalf("caller-owned partial missing after age close failure: %v", statErr)
		}
		entries, readErr := os.ReadDir(failureDir)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if len(entries) != 2 {
			t.Fatalf("age close failure directory entries = %d, want source and partial only", len(entries))
		}
	})

	t.Run("sync failure prevents success and still closes", func(t *testing.T) {
		partialPath := filepath.Join(dir, "sync-failure-output-canary.age.partial")
		const rawSyncCanary = "raw-sync-failure-canary"
		var events []string
		ops := encryptionFileFinalizer{
			ageClose: func(writer io.Closer) error {
				events = append(events, "age-close")
				return writer.Close()
			},
			sync: func(*os.File) error {
				events = append(events, "sync")
				return errors.New(rawSyncCanary)
			},
			close: func(file *os.File) error {
				events = append(events, "destination-close")
				return file.Close()
			},
		}

		gotBytes, err := encryptor.encryptFile(context.Background(), sourcePath, partialPath, ops)
		if err == nil || err.Error() != "collector encryption synchronization failed" {
			t.Fatalf("sync error = %v, want fixed safe classification", err)
		}
		if gotBytes != 0 {
			t.Fatalf("encrypted byte count on sync failure = %d, want 0", gotBytes)
		}
		if strings.Contains(err.Error(), rawSyncCanary) {
			t.Fatalf("sync error exposed raw failure: %q", err)
		}
		if want := []string{"age-close", "sync", "destination-close"}; !reflect.DeepEqual(events, want) {
			t.Fatalf("failure events = %v, want %v", events, want)
		}
	})

	t.Run("close failure prevents success", func(t *testing.T) {
		partialPath := filepath.Join(dir, "close-failure-output-canary.age.partial")
		const rawCloseCanary = "raw-close-failure-canary"
		ops := encryptionFileFinalizer{
			sync: func(file *os.File) error { return file.Sync() },
			close: func(file *os.File) error {
				if err := file.Close(); err != nil {
					return err
				}
				return errors.New(rawCloseCanary)
			},
		}

		gotBytes, err := encryptor.encryptFile(context.Background(), sourcePath, partialPath, ops)
		if err == nil || err.Error() != "collector encryption destination close failed" {
			t.Fatalf("close error = %v, want fixed safe classification", err)
		}
		if gotBytes != 0 {
			t.Fatalf("encrypted byte count on close failure = %d, want 0", gotBytes)
		}
		if strings.Contains(err.Error(), rawCloseCanary) {
			t.Fatalf("close error exposed raw failure: %q", err)
		}
	})
}

func TestEncryptFileCancellationLeavesOnlyPartialOutput(t *testing.T) {
	identity := generateTestIdentity(t)
	encryptor := NewEncryptor(identity.Recipient())

	t.Run("already canceled creates nothing", func(t *testing.T) {
		dir := safeTempDir(t)
		sourcePath := filepath.Join(dir, "pre-cancel-source-canary")
		partialPath := filepath.Join(dir, "pre-cancel-output-canary.age.partial")
		if err := os.WriteFile(sourcePath, []byte("preserve me"), 0o600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		gotBytes, err := encryptor.EncryptFile(ctx, sourcePath, partialPath)
		if err == nil || err.Error() != "collector encryption canceled" {
			t.Fatalf("cancellation error = %v, want fixed safe classification", err)
		}
		if gotBytes != 0 {
			t.Fatalf("encrypted byte count on cancellation = %d, want 0", gotBytes)
		}
		if _, err := os.Stat(partialPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("pre-canceled destination exists or stat failed: %v", err)
		}
	})

	t.Run("cancellation during copy leaves partial and preserves source", func(t *testing.T) {
		dir := safeTempDir(t)
		sourcePath := filepath.Join(dir, "large-sparse-source-canary")
		partialPath := filepath.Join(dir, "canceled-output-canary.age.partial")
		source, err := os.OpenFile(sourcePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		const sourceSize = int64(1 << 30)
		if err := source.Truncate(sourceSize); err != nil {
			_ = source.Close()
			t.Fatal(err)
		}
		if err := source.Close(); err != nil {
			t.Fatal(err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		type result struct {
			bytes int64
			err   error
		}
		resultCh := make(chan result, 1)
		go func() {
			gotBytes, err := encryptor.EncryptFile(ctx, sourcePath, partialPath)
			resultCh <- result{bytes: gotBytes, err: err}
		}()

		deadline := time.Now().Add(5 * time.Second)
		for {
			info, statErr := os.Stat(partialPath)
			if statErr == nil && info.Size() >= 128*1024 {
				break
			}
			if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
				t.Fatal(statErr)
			}
			if time.Now().After(deadline) {
				t.Fatal("encryption did not begin streaming before deadline")
			}
			time.Sleep(time.Millisecond)
		}
		cancel()

		select {
		case got := <-resultCh:
			if got.err == nil || got.err.Error() != "collector encryption canceled" {
				t.Fatalf("cancellation error = %v, want fixed safe classification", got.err)
			}
			if got.bytes != 0 {
				t.Fatalf("encrypted byte count on cancellation = %d, want 0", got.bytes)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("EncryptFile did not stop promptly after cancellation")
		}

		info, err := os.Stat(sourcePath)
		if err != nil {
			t.Fatalf("source was not preserved: %v", err)
		}
		if info.Size() != sourceSize {
			t.Fatalf("source size = %d, want %d", info.Size(), sourceSize)
		}
		if _, err := os.Stat(partialPath); err != nil {
			t.Fatalf("partial output was not left for caller cleanup: %v", err)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 2 {
			t.Fatalf("directory entries after cancellation = %d, want source and partial only", len(entries))
		}
	})
}

func TestEncryptFileRejectsDestinationSymlinkAncestorsAndTraversal(t *testing.T) {
	identity := generateTestIdentity(t)
	encryptor := NewEncryptor(identity.Recipient())
	root := safeTempDir(t)
	sourcePath := filepath.Join(root, "safe-source-canary")
	if err := os.WriteFile(sourcePath, []byte("safe source"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("symlink ancestor", func(t *testing.T) {
		outsideDir := safeTempDir(t)
		linkPath := filepath.Join(root, "linked-parent-canary")
		if err := os.Symlink(outsideDir, linkPath); err != nil {
			t.Fatal(err)
		}
		partialPath := filepath.Join(linkPath, "escaped-output-canary.age.partial")

		gotBytes, err := encryptor.EncryptFile(context.Background(), sourcePath, partialPath)
		if err == nil || err.Error() != "collector encryption destination unavailable" {
			t.Fatalf("symlink ancestor error = %v, want fixed safe classification", err)
		}
		if gotBytes != 0 {
			t.Fatalf("encrypted bytes through symlink ancestor = %d, want 0", gotBytes)
		}
		outsidePath := filepath.Join(outsideDir, filepath.Base(partialPath))
		if _, statErr := os.Stat(outsidePath); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("destination escaped through symlink ancestor: %v", statErr)
		}
	})

	t.Run("parent traversal", func(t *testing.T) {
		safeParent := filepath.Join(root, "safe-parent-canary")
		if err := os.Mkdir(safeParent, 0o700); err != nil {
			t.Fatal(err)
		}
		partialPath := safeParent + string(os.PathSeparator) + ".." + string(os.PathSeparator) + "traversal-output-canary.age.partial"

		gotBytes, err := encryptor.EncryptFile(context.Background(), sourcePath, partialPath)
		if err == nil || err.Error() != "collector encryption destination unavailable" {
			t.Fatalf("traversal error = %v, want fixed safe classification", err)
		}
		if gotBytes != 0 {
			t.Fatalf("encrypted bytes through traversal = %d, want 0", gotBytes)
		}
		if _, statErr := os.Stat(filepath.Join(root, "traversal-output-canary.age.partial")); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("destination created through traversal: %v", statErr)
		}
	})
}

func TestEncryptFileRejectsUnsafeSourceTypesWithoutCreatingPartial(t *testing.T) {
	identity := generateTestIdentity(t)
	encryptor := NewEncryptor(identity.Recipient())

	assertRejected := func(t *testing.T, sourcePath, partialPath string, resultErr error, gotBytes int64) {
		t.Helper()
		if resultErr == nil || resultErr.Error() != "collector encryption source unavailable" {
			t.Fatalf("unsafe source error = %v, want fixed safe classification", resultErr)
		}
		if gotBytes != 0 {
			t.Fatalf("encrypted bytes for unsafe source = %d, want 0", gotBytes)
		}
		if _, statErr := os.Stat(partialPath); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("partial created for unsafe source: %v", statErr)
		}
	}

	t.Run("directory", func(t *testing.T) {
		dir := safeTempDir(t)
		partialPath := filepath.Join(safeTempDir(t), "directory-output-canary.age.partial")
		gotBytes, err := encryptor.EncryptFile(context.Background(), dir, partialPath)
		assertRejected(t, dir, partialPath, err, gotBytes)
	})

	t.Run("final symlink", func(t *testing.T) {
		dir := safeTempDir(t)
		targetPath := filepath.Join(dir, "source-target-canary")
		linkPath := filepath.Join(dir, "source-link-canary")
		partialPath := filepath.Join(dir, "symlink-source-output-canary.age.partial")
		if err := os.WriteFile(targetPath, []byte("symlink target"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(targetPath, linkPath); err != nil {
			t.Fatal(err)
		}
		gotBytes, err := encryptor.EncryptFile(context.Background(), linkPath, partialPath)
		assertRejected(t, linkPath, partialPath, err, gotBytes)
	})

	t.Run("symlink ancestor", func(t *testing.T) {
		root := safeTempDir(t)
		outsideDir := safeTempDir(t)
		targetPath := filepath.Join(outsideDir, "ancestor-target-canary")
		if err := os.WriteFile(targetPath, []byte("ancestor target"), 0o600); err != nil {
			t.Fatal(err)
		}
		linkPath := filepath.Join(root, "source-parent-link-canary")
		if err := os.Symlink(outsideDir, linkPath); err != nil {
			t.Fatal(err)
		}
		sourcePath := filepath.Join(linkPath, filepath.Base(targetPath))
		partialPath := filepath.Join(root, "ancestor-source-output-canary.age.partial")
		gotBytes, err := encryptor.EncryptFile(context.Background(), sourcePath, partialPath)
		assertRejected(t, sourcePath, partialPath, err, gotBytes)
	})

	t.Run("fifo does not block", func(t *testing.T) {
		dir := safeTempDir(t)
		fifoPath := filepath.Join(dir, "source-fifo-canary")
		partialPath := filepath.Join(dir, "fifo-output-canary.age.partial")
		if err := unix.Mkfifo(fifoPath, 0o600); err != nil {
			t.Fatal(err)
		}
		holderFD, err := unix.Open(fifoPath, unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err != nil {
			t.Fatal(err)
		}

		type result struct {
			bytes int64
			err   error
		}
		resultCh := make(chan result, 1)
		go func() {
			gotBytes, err := encryptor.EncryptFile(context.Background(), fifoPath, partialPath)
			resultCh <- result{bytes: gotBytes, err: err}
		}()

		var got result
		select {
		case got = <-resultCh:
			if err := unix.Close(holderFD); err != nil {
				t.Fatal(err)
			}
		case <-time.After(100 * time.Millisecond):
			if err := unix.Close(holderFD); err != nil {
				t.Fatal(err)
			}
			select {
			case got = <-resultCh:
			case <-time.After(2 * time.Second):
				t.Fatal("EncryptFile blocked on FIFO source")
			}
		}
		assertRejected(t, fifoPath, partialPath, got.err, got.bytes)
	})
}

func TestEncryptFileFailureDoesNotExposeRecipientOrPaths(t *testing.T) {
	dir := safeTempDir(t)
	sourcePath := filepath.Join(dir, "private-source-path-canary-43d1")
	partialPath := filepath.Join(dir, "private-destination-path-canary-9f7a.age.partial")
	if err := os.WriteFile(sourcePath, []byte("redaction source"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity := generateTestIdentity(t)
	privateIdentityCanary := identity.String()
	const recipientCanary = "recipient-public-value-canary-age1qqqq"
	rawFailure := strings.Join([]string{sourcePath, partialPath, recipientCanary, privateIdentityCanary}, " | ")
	encryptor := NewEncryptor(failingAgeRecipient{rawFailure: rawFailure})

	gotBytes, err := encryptor.EncryptFile(context.Background(), sourcePath, partialPath)
	if err == nil || err.Error() != "collector encryption setup failed" {
		t.Fatalf("EncryptFile error = %v, want fixed safe classification", err)
	}
	if gotBytes != 0 {
		t.Fatalf("encrypted byte count on setup failure = %d, want 0", gotBytes)
	}
	for _, canary := range []string{sourcePath, partialPath, recipientCanary, privateIdentityCanary, "AGE-SECRET-KEY"} {
		if strings.Contains(err.Error(), canary) {
			t.Fatalf("error exposed sensitive canary %q: %q", canary, err)
		}
	}
	if _, statErr := os.Stat(sourcePath); statErr != nil {
		t.Fatalf("source was not preserved: %v", statErr)
	}
	if _, statErr := os.Stat(partialPath); statErr != nil {
		t.Fatalf("failed partial was not left for caller cleanup: %v", statErr)
	}

	missingSource := filepath.Join(dir, "missing-source-path-canary-b831")
	missingPartial := filepath.Join(dir, "missing-output-path-canary-277c.age.partial")
	_, err = NewEncryptor(identity.Recipient()).EncryptFile(context.Background(), missingSource, missingPartial)
	if err == nil || err.Error() != "collector encryption source unavailable" {
		t.Fatalf("missing source error = %v, want fixed safe classification", err)
	}
	if strings.Contains(err.Error(), missingSource) || strings.Contains(err.Error(), missingPartial) {
		t.Fatalf("missing source error exposed a path: %q", err)
	}
}

func TestEncryptFileRejectsExistingDestination(t *testing.T) {
	identity := generateTestIdentity(t)
	dir := safeTempDir(t)
	sourcePath := filepath.Join(dir, "collision-source-canary")
	if err := os.WriteFile(sourcePath, []byte("source must survive collision"), 0o600); err != nil {
		t.Fatal(err)
	}
	encryptor := NewEncryptor(identity.Recipient())

	t.Run("regular file is not replaced or truncated", func(t *testing.T) {
		partialPath := filepath.Join(dir, "existing-output-canary.age.partial")
		original := []byte("existing destination content canary")
		if err := os.WriteFile(partialPath, original, 0o640); err != nil {
			t.Fatal(err)
		}

		gotBytes, err := encryptor.EncryptFile(context.Background(), sourcePath, partialPath)
		if err == nil || err.Error() != "collector encryption destination unavailable" {
			t.Fatalf("collision error = %v, want fixed safe classification", err)
		}
		if gotBytes != 0 {
			t.Fatalf("encrypted byte count on collision = %d, want 0", gotBytes)
		}
		got, readErr := os.ReadFile(partialPath)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if !bytes.Equal(got, original) {
			t.Fatalf("existing destination changed: %q", got)
		}
	})

	t.Run("symlink is rejected without touching target", func(t *testing.T) {
		targetPath := filepath.Join(dir, "symlink-target-canary")
		partialPath := filepath.Join(dir, "symlink-output-canary.age.partial")
		original := []byte("symlink target content canary")
		if err := os.WriteFile(targetPath, original, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(targetPath, partialPath); err != nil {
			t.Fatal(err)
		}

		_, err := encryptor.EncryptFile(context.Background(), sourcePath, partialPath)
		if err == nil || err.Error() != "collector encryption destination unavailable" {
			t.Fatalf("symlink error = %v, want fixed safe classification", err)
		}
		got, readErr := os.ReadFile(targetPath)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if !bytes.Equal(got, original) {
			t.Fatalf("symlink target changed: %q", got)
		}
	})
}

func safeTempDir(t testing.TB) string {
	t.Helper()
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func generateTestIdentity(t testing.TB) *age.X25519Identity {
	t.Helper()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

type failingAgeRecipient struct {
	rawFailure string
}

func (r failingAgeRecipient) Wrap([]byte) ([]*age.Stanza, error) {
	return nil, errors.New(r.rawFailure)
}
