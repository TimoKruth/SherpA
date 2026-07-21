package collector

import (
	"bytes"
	"context"
	"errors"
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
	dir := t.TempDir()
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

func TestEncryptFileWritesMode0600AndSyncsBeforeSuccess(t *testing.T) {
	identity := generateTestIdentity(t)
	dir := t.TempDir()
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
		info, err := os.Stat(partialPath)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("destination mode = %04o, want 0600", got)
		}
	})

	t.Run("sync precedes close and success", func(t *testing.T) {
		partialPath := filepath.Join(dir, "ordered-output-canary.age.partial")
		var events []string
		ops := encryptionFileFinalizer{
			sync: func(file *os.File) error {
				events = append(events, "sync")
				return file.Sync()
			},
			close: func(file *os.File) error {
				events = append(events, "close")
				return file.Close()
			},
		}

		if _, err := encryptor.encryptFile(context.Background(), sourcePath, partialPath, ops); err != nil {
			t.Fatalf("encryptFile: %v", err)
		}
		if want := []string{"sync", "close"}; !reflect.DeepEqual(events, want) {
			t.Fatalf("finalization events = %v, want %v", events, want)
		}
	})

	t.Run("sync failure prevents success and still closes", func(t *testing.T) {
		partialPath := filepath.Join(dir, "sync-failure-output-canary.age.partial")
		const rawSyncCanary = "raw-sync-failure-canary"
		var events []string
		ops := encryptionFileFinalizer{
			sync: func(*os.File) error {
				events = append(events, "sync")
				return errors.New(rawSyncCanary)
			},
			close: func(file *os.File) error {
				events = append(events, "close")
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
		if want := []string{"sync", "close"}; !reflect.DeepEqual(events, want) {
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
		dir := t.TempDir()
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
		dir := t.TempDir()
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

func TestEncryptFileFailureDoesNotExposeRecipientOrPaths(t *testing.T) {
	dir := t.TempDir()
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
	dir := t.TempDir()
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
