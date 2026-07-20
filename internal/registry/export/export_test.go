package export

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"sherpa/internal/recoveryarchive"
)

func TestRunDumpsDatabaseBeforeBundlesAndBuildsVerifiableArchive(t *testing.T) {
	contentDir := t.TempDir()
	createBareRepository(t, contentDir, "alice", "one")
	createBareRepository(t, contentDir, "bob", "one")
	archivePath := filepath.Join(t.TempDir(), "backups", "registry.tar.gz")

	original := runCommand
	t.Cleanup(func() { runCommand = original })
	var calls []string
	databaseURL := "postgresql://exporter:p%40ss@db.internal:5433/sherpa?sslmode=require"
	runCommand = func(ctx context.Context, dir, name string, args, env []string) error {
		calls = append(calls, name)
		if name == "pg_dump" {
			if strings.Contains(strings.Join(args, " "), databaseURL) || strings.Contains(strings.Join(args, " "), "p@ss") {
				t.Fatal("database URL appeared in pg_dump arguments")
			}
			wantEnv := []string{
				"PGHOST=db.internal", "PGPORT=5433", "PGUSER=exporter",
				"PGPASSWORD=p@ss", "PGDATABASE=sherpa", "PGSSLMODE=require",
			}
			if !slices.Equal(env, wantEnv) {
				t.Fatalf("pg_dump environment = %q", env)
			}
			return os.WriteFile(args[2], []byte("database dump"), 0o600)
		}
		return execCommand(ctx, dir, name, args, env)
	}

	if err := Run(context.Background(), contentDir, databaseURL, archivePath); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 3 || calls[0] != "pg_dump" || calls[1] != "git" || calls[2] != "git" {
		t.Fatalf("command order = %v", calls)
	}

	files, order := readArchive(t, archivePath)
	wantPaths := []string{"manifest.json", "postgres.dump", "repos/alice/one.bundle", "repos/bob/one.bundle"}
	for _, path := range wantPaths {
		if len(files[path]) == 0 {
			t.Errorf("archive artifact %q is absent or empty", path)
		}
	}
	if order[len(order)-1] != "manifest.json" {
		t.Fatalf("final tar member = %q, want manifest.json", order[len(order)-1])
	}
	var got recoveryarchive.Manifest
	if err := json.Unmarshal(files["manifest.json"], &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Artifacts) != 3 {
		t.Fatalf("manifest artifacts = %#v", got.Artifacts)
	}
	for _, entry := range got.Artifacts {
		data, ok := files[entry.Path]
		if !ok {
			t.Errorf("manifest names missing artifact %q", entry.Path)
			continue
		}
		digest := sha256.Sum256(data)
		if got := hex.EncodeToString(digest[:]); got != entry.SHA256 {
			t.Errorf("%s hash = %s, want %s", entry.Path, entry.SHA256, got)
		}
		if int64(len(data)) != entry.Size {
			t.Errorf("%s size = %d, want %d", entry.Path, entry.Size, len(data))
		}
	}

	cloneDir := filepath.Join(t.TempDir(), "clone")
	bundlePath := filepath.Join(t.TempDir(), "one.bundle")
	if err := os.WriteFile(bundlePath, files["repos/alice/one.bundle"], 0o600); err != nil {
		t.Fatal(err)
	}
	if err := execCommand(context.Background(), t.TempDir(), "git", []string{"clone", bundlePath, cloneDir}, nil); err != nil {
		t.Fatalf("clone exported bundle: %v", err)
	}
}

func TestRunProducesArchiveAcceptedBySharedValidator(t *testing.T) {
	contentDir := t.TempDir()
	archivePath := filepath.Join(t.TempDir(), "registry.tar.gz")
	original := runCommand
	t.Cleanup(func() { runCommand = original })
	runCommand = func(_ context.Context, _ string, name string, args, _ []string) error {
		if name != "pg_dump" {
			return fmt.Errorf("unexpected command %q", name)
		}
		return os.WriteFile(args[2], []byte("database dump"), 0o600)
	}

	if err := Run(context.Background(), contentDir, "postgres://exporter@db.internal/sherpa", archivePath); err != nil {
		t.Fatal(err)
	}
	if _, err := recoveryarchive.ValidateFile(context.Background(), archivePath, recoveryarchive.DefaultLimits()); err != nil {
		t.Fatalf("shared validator rejected Run archive: %v", err)
	}
}

func TestRunDoesNotPublishWhenSharedValidationFails(t *testing.T) {
	contentDir := t.TempDir()
	archivePath := filepath.Join(t.TempDir(), "registry.tar.gz")
	previous := []byte("previous recovery archive")
	if err := os.WriteFile(archivePath, previous, 0o600); err != nil {
		t.Fatal(err)
	}

	originalCommand := runCommand
	originalValidator := validateRecoveryArchive
	t.Cleanup(func() {
		runCommand = originalCommand
		validateRecoveryArchive = originalValidator
	})
	runCommand = func(_ context.Context, _ string, name string, args, _ []string) error {
		if name != "pg_dump" {
			return fmt.Errorf("unexpected command %q", name)
		}
		return os.WriteFile(args[2], []byte("database dump"), 0o600)
	}
	var validated atomic.Bool
	validateRecoveryArchive = func(context.Context, string, recoveryarchive.Limits) (recoveryarchive.Report, error) {
		validated.Store(true)
		return recoveryarchive.Report{}, errors.New("synthetic validation failure")
	}

	err := Run(context.Background(), contentDir, "postgres://exporter@db.internal/sherpa", archivePath)
	if err == nil {
		t.Fatal("Run succeeded after shared validation failed")
	}
	if !validated.Load() {
		t.Fatal("Run did not invoke shared validator")
	}
	contents, readErr := os.ReadFile(archivePath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(contents, previous) {
		t.Fatalf("published archive contents = %q, want previous archive", contents)
	}
}

func TestRunFailureLeavesNoArchiveOrTemporaryFiles(t *testing.T) {
	parent := t.TempDir()
	archivePath := filepath.Join(parent, "registry.tar.gz")
	original := runCommand
	t.Cleanup(func() { runCommand = original })
	runCommand = func(context.Context, string, string, []string, []string) error {
		return errors.New("failed")
	}

	databaseURL := "postgres://user:hidden@db.internal/sherpa"
	err := Run(context.Background(), t.TempDir(), databaseURL, archivePath)
	if err == nil || strings.Contains(err.Error(), "hidden") || strings.Contains(err.Error(), databaseURL) {
		t.Fatalf("Run error = %q", err)
	}
	entries, readErr := os.ReadDir(parent)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("failure left files: %v", entries)
	}
}

func TestDatabaseEnvironmentDefaultsAndRejectsMalformedURLsWithoutSecrets(t *testing.T) {
	environment, err := databaseEnvironment("postgres://exporter@db.internal/sherpa")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"PGHOST=db.internal", "PGPORT=5432", "PGUSER=exporter",
		"PGPASSWORD=", "PGDATABASE=sherpa", "PGSSLMODE=prefer",
	}
	if !slices.Equal(environment, want) {
		t.Fatalf("database environment = %q, want %q", environment, want)
	}

	for _, databaseURL := range []string{
		"mysql://user:top-secret@db.internal/sherpa",
		"postgres://user:top-secret@/sherpa",
		"postgres://user:top-secret@db.internal/",
		"postgres://user:top-secret@db.internal/one/two",
		"postgres://user:top-secret@db.internal/sherpa?connect_timeout=10",
		"postgres://user:top-secret@db.internal/sherpa?sslmode=unsafe",
		"postgres://user:top-secret@db.internal/sherpa#fragment",
	} {
		_, err := databaseEnvironment(databaseURL)
		if err == nil {
			t.Errorf("databaseEnvironment(%q) succeeded", databaseURL)
			continue
		}
		if strings.Contains(err.Error(), "top-secret") || strings.Contains(err.Error(), databaseURL) {
			t.Errorf("databaseEnvironment error %q exposes credentials", err)
		}
	}
}

func TestHashArchiveDoesNotReadWithCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reader := &countingHashReader{}

	_, err := hashArchive(ctx, reader)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("hashArchive error = %v, want context canceled", err)
	}
	if reader.reads != 0 {
		t.Fatalf("underlying reads = %d, want 0", reader.reads)
	}
}

func TestHashArchiveStopsAfterContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reader := &cancellationHashReader{
		started:    make(chan struct{}),
		resume:     make(chan struct{}),
		secondRead: make(chan struct{}),
		finish:     make(chan struct{}),
	}
	done := make(chan error, 1)
	go func() {
		_, err := hashArchive(ctx, reader)
		done <- err
	}()

	<-reader.started
	cancel()
	close(reader.resume)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("hashArchive error = %v, want context canceled", err)
		}
	case <-reader.secondRead:
		close(reader.finish)
		<-done
		t.Fatal("hashArchive read again after context cancellation")
	case <-time.After(time.Second):
		close(reader.finish)
		t.Fatal("hashArchive did not stop after context cancellation")
	}
}

func TestHashArchiveReturnsCancellationFromTerminalRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reader := &terminalCancellationHashReader{cancel: cancel}

	_, err := hashArchive(ctx, reader)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("hashArchive error = %v, want context canceled", err)
	}
	if reader.reads != 1 {
		t.Fatalf("underlying reads = %d, want 1", reader.reads)
	}
}

func TestHashArchiveChecksContextAfterCopy(t *testing.T) {
	ctx := &stagedCancellationContext{cancelAt: 3}
	reader := &terminalEOFHashReader{}

	_, err := hashArchive(ctx, reader)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("hashArchive error = %v, want context canceled", err)
	}
	if ctx.errCalls != ctx.cancelAt {
		t.Fatalf("context error checks = %d, want %d", ctx.errCalls, ctx.cancelAt)
	}
	if reader.reads != 1 {
		t.Fatalf("underlying reads = %d, want 1", reader.reads)
	}
}

func TestHashArchiveHashesNormally(t *testing.T) {
	contents := []byte("complete gzip bytes")
	digest := sha256.Sum256(contents)
	want := "sha256:" + hex.EncodeToString(digest[:])

	got, err := hashArchive(context.Background(), bytes.NewReader(contents))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("hashArchive = %q, want %q", got, want)
	}
}

func TestUploadReturnsPreCanceledContextWithoutRequest(t *testing.T) {
	path, _ := writeUploadArchive(t, []byte("complete gzip bytes"))
	var requested atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requested.Store(true)
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Upload(ctx, path, server.URL, "collector-secret")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Upload error = %v, want context canceled", err)
	}
	if requested.Load() {
		t.Fatal("Upload sent a request after the context was canceled")
	}
}

func TestUploadAcceptsCreatedStoredResponse(t *testing.T) {
	path, objectID := writeUploadArchive(t, []byte("complete gzip bytes"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"object_id":%q,"status":"stored"}`, objectID)
	}))
	defer server.Close()

	result, err := Upload(context.Background(), path, server.URL, "collector-secret")
	if err != nil {
		t.Fatal(err)
	}
	if result != (UploadResult{ObjectID: objectID, Status: "stored"}) {
		t.Fatalf("Upload result = %+v", result)
	}
}

func TestUploadAcceptsOKExistingResponse(t *testing.T) {
	path, objectID := writeUploadArchive(t, []byte("complete gzip bytes"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"object_id":%q,"status":"existing"}`, objectID)
	}))
	defer server.Close()

	result, err := Upload(context.Background(), path, server.URL, "collector-secret")
	if err != nil {
		t.Fatal(err)
	}
	if result != (UploadResult{ObjectID: objectID, Status: "existing"}) {
		t.Fatalf("Upload result = %+v", result)
	}
}

func TestUploadRejectsMismatchedObjectID(t *testing.T) {
	path, _ := writeUploadArchive(t, []byte("complete gzip bytes"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"object_id":"sha256:%s","status":"stored"}`, strings.Repeat("0", sha256.Size*2))
	}))
	defer server.Close()

	if _, err := Upload(context.Background(), path, server.URL, "collector-secret"); err == nil {
		t.Fatal("Upload accepted a mismatched object ID")
	}
}

func TestUploadRejectsMalformedSuccessfulResponse(t *testing.T) {
	path, objectID := writeUploadArchive(t, []byte("complete gzip bytes"))
	for name, body := range map[string]string{
		"invalid JSON":  `{`,
		"unknown field": fmt.Sprintf(`{"object_id":%q,"status":"stored","unexpected":true}`, objectID),
		"trailing JSON": fmt.Sprintf(`{"object_id":%q,"status":"stored"}{}`, objectID),
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(w, body)
			}))
			defer server.Close()

			if _, err := Upload(context.Background(), path, server.URL, "collector-secret"); err == nil {
				t.Fatal("Upload accepted a malformed successful response")
			}
		})
	}
}

func TestUploadRejectsStatusCodeStatusBodyMismatch(t *testing.T) {
	path, objectID := writeUploadArchive(t, []byte("complete gzip bytes"))
	for name, response := range map[string]struct {
		code   int
		status string
	}{
		"created existing": {code: http.StatusCreated, status: "existing"},
		"ok stored":        {code: http.StatusOK, status: "stored"},
		"accepted stored":  {code: http.StatusAccepted, status: "stored"},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(response.code)
				fmt.Fprintf(w, `{"object_id":%q,"status":%q}`, objectID, response.status)
			}))
			defer server.Close()

			if _, err := Upload(context.Background(), path, server.URL, "collector-secret"); err == nil {
				t.Fatal("Upload accepted a status-code/body mismatch")
			}
		})
	}
}

func TestUploadAcceptsCollectorResponseAtExactSizeLimit(t *testing.T) {
	path, objectID := writeUploadArchive(t, []byte("complete gzip bytes"))
	body := []byte(fmt.Sprintf(`{"object_id":%q,"status":"stored"}`, objectID))
	body = append(body, bytes.Repeat([]byte(" "), maxCollectorResponseBodySize-len(body))...)
	if len(body) != maxCollectorResponseBodySize {
		t.Fatalf("response body size = %d, want %d", len(body), maxCollectorResponseBodySize)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(body)
	}))
	defer server.Close()

	result, err := Upload(context.Background(), path, server.URL, "collector-secret")
	if err != nil {
		t.Fatal(err)
	}
	if result != (UploadResult{ObjectID: objectID, Status: "stored"}) {
		t.Fatalf("Upload result = %+v", result)
	}
}

func TestUploadRejectsCollectorResponseAboveSizeLimit(t *testing.T) {
	path, objectID := writeUploadArchive(t, []byte("complete gzip bytes"))
	body := []byte(fmt.Sprintf(`{"object_id":%q,"status":"stored"}`, objectID))
	body = append(body, bytes.Repeat([]byte(" "), maxCollectorResponseBodySize+1-len(body))...)
	if len(body) != maxCollectorResponseBodySize+1 {
		t.Fatalf("response body size = %d, want %d", len(body), maxCollectorResponseBodySize+1)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		w.(http.Flusher).Flush()
		_, _ = w.Write(body)
	}))
	defer server.Close()

	_, err := Upload(context.Background(), path, server.URL, "collector-secret")
	if err == nil {
		t.Fatal("Upload accepted an oversized collector response")
	}
	if err.Error() != "export collector response is too large" {
		t.Fatalf("Upload error = %q, want size-limit rejection", err)
	}
}

func TestUploadErrorDoesNotExposeURLTokenOrResponseBody(t *testing.T) {
	path, _ := writeUploadArchive(t, []byte("archive"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "response-body-secret collector-secret", http.StatusBadGateway)
	}))
	defer server.Close()

	_, err := Upload(context.Background(), path, server.URL+"/upload?private=query", "collector-secret")
	if err == nil {
		t.Fatal("Upload succeeded")
	}
	for _, secret := range []string{"collector-secret", "response-body-secret", "private=query", server.URL} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("Upload error %q exposes %q", err, secret)
		}
	}
}

func TestUploadSendsArchiveWithOnlyConfiguredAuthorization(t *testing.T) {
	payload := []byte("complete gzip bytes")
	path, objectID := writeUploadArchive(t, payload)
	var received []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength != int64(len(payload)) || r.ContentLength <= 0 {
			t.Errorf("ContentLength = %d, want %d", r.ContentLength, len(payload))
		}
		if len(r.TransferEncoding) != 0 {
			t.Errorf("TransferEncoding = %q, want none", r.TransferEncoding)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer collector-secret" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Values("Authorization"); len(got) != 1 {
			t.Errorf("Authorization values = %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/gzip" {
			t.Errorf("Content-Type = %q", got)
		}
		received, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"object_id":%q,"status":"stored"}`, objectID)
	}))
	defer server.Close()

	if _, err := Upload(context.Background(), path, server.URL+"/ingest?private=query", "collector-secret"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(received, payload) {
		t.Fatalf("uploaded payload = %q", received)
	}
	if _, err := Upload(context.Background(), path, "http://example.com/upload?private=query", "collector-secret"); err == nil {
		t.Fatal("Upload accepted non-loopback HTTP collector")
	}
}

func TestUploadErrorDoesNotExposeURLOrToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.tar.gz")
	if err := os.WriteFile(path, []byte("archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "response includes collector-secret", http.StatusBadGateway)
	}))
	defer server.Close()
	_, err := Upload(context.Background(), path, server.URL+"/upload?private=query", "collector-secret")
	if err == nil {
		t.Fatal("Upload succeeded")
	}
	for _, secret := range []string{"collector-secret", "private=query", server.URL} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("Upload error %q exposes %q", err, secret)
		}
	}
}

func TestUploadDoesNotFollowRedirectsWithCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.tar.gz")
	if err := os.WriteFile(path, []byte("archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	var followed atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		followed.Store(true)
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	_, err := Upload(context.Background(), path, source.URL, "collector-secret")
	if err == nil {
		t.Fatal("redirect response accepted as a successful upload")
	}
	if followed.Load() {
		t.Fatal("upload followed redirect with collector credentials")
	}
}

func TestStartSchedulerRetriesOnePendingArchiveAndCancelsCleanly(t *testing.T) {
	originalRun, originalUpload := runExport, uploadPendingArchive
	t.Cleanup(func() { runExport, uploadPendingArchive = originalRun, originalUpload })
	dir := t.TempDir()
	var runs atomic.Int32
	var uploads atomic.Int32
	runExport = func(_ context.Context, _, _, path string) error {
		runs.Add(1)
		return os.WriteFile(path, []byte("archive"), 0o600)
	}
	uploadPendingArchive = func(context.Context, *os.File, os.FileInfo, string, string) (UploadResult, error) {
		uploads.Add(1)
		return UploadResult{}, errors.New("collector returned HTTP 503")
	}
	var logs bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- StartScheduler(ctx, SchedulerConfig{
			ContentDir: "content", DatabaseURL: "postgres://db-secret", ArchiveDir: dir,
			CollectorURL: "https://collector.example/upload?private=query", Token: "collector-secret",
			Interval: 5 * time.Millisecond, Logger: log.New(&logs, "", 0),
		})
	}()
	deadline := time.Now().Add(time.Second)
	for uploads.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if uploads.Load() < 3 {
		t.Fatalf("upload attempts = %d, want at least 3", uploads.Load())
	}
	if runs.Load() != 1 {
		t.Fatalf("export runs = %d, want 1 while retrying pending archive", runs.Load())
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("scheduler did not stop after cancellation")
	}
	for _, secret := range []string{"collector-secret", "private=query", "postgres://db-secret"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("scheduler log %q exposes %q", logs.String(), secret)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("failed upload archive was deleted")
	}
}

func TestStartSchedulerDisabledAndPartialConfiguration(t *testing.T) {
	if err := StartScheduler(context.Background(), SchedulerConfig{}); err != nil {
		t.Fatalf("disabled scheduler: %v", err)
	}
	if err := StartScheduler(context.Background(), SchedulerConfig{CollectorURL: "https://collector.example"}); err == nil {
		t.Fatal("partial scheduler configuration accepted")
	}
	root := t.TempDir()
	err := StartScheduler(context.Background(), SchedulerConfig{
		ContentDir: root, ArchiveDir: filepath.Join(root, "exports"),
		CollectorURL: "https://collector.example", Token: "secret", Interval: time.Hour,
	})
	if err == nil {
		t.Fatal("scheduler accepted an archive directory inside content")
	}
}

func TestValidateSchedulerConfigRejectsTokenOnlyAndInsecureCollector(t *testing.T) {
	if err := ValidateSchedulerConfig(SchedulerConfig{Token: "secret"}); err == nil {
		t.Fatal("token-only scheduler configuration accepted")
	}
	err := ValidateSchedulerConfig(SchedulerConfig{
		ContentDir: t.TempDir(), ArchiveDir: t.TempDir(),
		CollectorURL: "http://collector.example/upload", Token: "secret", Interval: time.Hour,
	})
	if err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("insecure collector error = %v", err)
	}
}

func TestValidateSchedulerConfigRequiresTokenWhenEnabled(t *testing.T) {
	err := ValidateSchedulerConfig(SchedulerConfig{
		ContentDir: t.TempDir(), ArchiveDir: t.TempDir(),
		CollectorURL: "https://collector.example/upload", Interval: time.Hour,
	})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "token") {
		t.Fatalf("missing token error = %v", err)
	}
}

func TestValidateSchedulerConfigRejectsArchiveSymlinkIntoContent(t *testing.T) {
	root := t.TempDir()
	contentDir := filepath.Join(root, "content")
	if err := os.Mkdir(contentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	archiveLink := filepath.Join(root, "exports")
	if err := os.Symlink(contentDir, archiveLink); err != nil {
		t.Fatal(err)
	}

	err := ValidateSchedulerConfig(SchedulerConfig{
		ContentDir: contentDir, ArchiveDir: archiveLink,
		CollectorURL: "https://collector.example/upload", Token: "secret", Interval: time.Hour,
	})
	if err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("archive symlink containment error = %v", err)
	}
}

func TestStartSchedulerRejectsMissingParentSwappedIntoContentDuringPreparation(t *testing.T) {
	root := t.TempDir()
	contentDir := filepath.Join(root, "content")
	if err := os.Mkdir(contentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	archiveParent := filepath.Join(root, "missing-parent")
	archiveDir := filepath.Join(archiveParent, "exports")
	originalHook := beforePrepareArchiveDirectory
	t.Cleanup(func() { beforePrepareArchiveDirectory = originalHook })
	beforePrepareArchiveDirectory = func(string) {
		if err := os.Symlink(contentDir, archiveParent); err != nil {
			t.Fatal(err)
		}
	}

	err := StartScheduler(context.Background(), SchedulerConfig{
		ContentDir: contentDir, ArchiveDir: archiveDir,
		CollectorURL: "https://collector.example/upload", Token: "secret", Interval: time.Hour,
		Logger: log.New(io.Discard, "", 0),
	})
	if err == nil || (!strings.Contains(err.Error(), "prepare") && !strings.Contains(err.Error(), "outside")) {
		t.Fatalf("post-preparation containment error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(contentDir, "exports")); !os.IsNotExist(err) {
		t.Fatalf("unsafe preparation touched content directory: %v", err)
	}
}

func TestStartSchedulerRejectsArchiveDirectoryFIFOReplacementWithoutBlocking(t *testing.T) {
	root := t.TempDir()
	contentDir := filepath.Join(root, "content")
	archiveDir := filepath.Join(root, "exports")
	if err := os.Mkdir(contentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(archiveDir, 0o700); err != nil {
		t.Fatal(err)
	}
	preserved := archiveDir + ".validated"
	originalHook := beforeOpenArchiveQueue
	t.Cleanup(func() { beforeOpenArchiveQueue = originalHook })
	beforeOpenArchiveQueue = func(string) {
		if err := os.Rename(archiveDir, preserved); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(archiveDir, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	done := make(chan error, 1)
	go func() {
		done <- StartScheduler(context.Background(), SchedulerConfig{
			ContentDir: contentDir, ArchiveDir: archiveDir,
			CollectorURL: "https://collector.example/upload", Token: "secret", Interval: time.Hour,
			Logger: log.New(io.Discard, "", 0),
		})
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO replacement was accepted as archive directory")
		}
	case <-time.After(time.Second):
		writer, _ := os.OpenFile(archiveDir, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if writer != nil {
			_ = writer.Close()
		}
		t.Fatal("opening FIFO replacement blocked scheduler startup")
	}
}

func TestStartSchedulerKeepsUsingValidatedDirectoryAfterPathSwap(t *testing.T) {
	root := t.TempDir()
	contentDir := filepath.Join(root, "content")
	archiveDir := filepath.Join(root, "exports")
	if err := os.Mkdir(contentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(archiveDir, 0o700); err != nil {
		t.Fatal(err)
	}
	attackerPartial := createTempPartial(t, contentDir, time.Now().Add(-48*time.Hour))
	validatedPartial := createTempPartial(t, archiveDir, time.Now().Add(-48*time.Hour))
	preservedArchiveDir := archiveDir + ".validated"
	originalHook := afterArchiveQueueValidated
	t.Cleanup(func() { afterArchiveQueueValidated = originalHook })
	afterArchiveQueueValidated = func() {
		if err := os.Rename(archiveDir, preservedArchiveDir); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(contentDir, archiveDir); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := StartScheduler(ctx, SchedulerConfig{
		ContentDir: contentDir, ArchiveDir: archiveDir,
		CollectorURL: "https://collector.example/upload", Token: "secret", Interval: time.Hour,
		Logger: log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(attackerPartial); err != nil {
		t.Fatalf("swapped-in content entry was touched: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(preservedArchiveDir, filepath.Base(validatedPartial))); !os.IsNotExist(err) {
		t.Fatalf("stale entry in validated directory was not cleaned: %v", err)
	}
	if info, err := os.Stat(preservedArchiveDir); err != nil || !info.IsDir() {
		t.Fatalf("validated archive directory was not preserved: %v", err)
	}
}

func TestStartSchedulerWhitespaceOnlyConfigurationIsDisabledWithoutFilesystemAccessOrPanic(t *testing.T) {
	t.Chdir(t.TempDir())
	var panicValue any
	var err error
	func() {
		defer func() { panicValue = recover() }()
		err = StartScheduler(context.Background(), SchedulerConfig{
			ContentDir: " \t", DatabaseURL: "\n", ArchiveDir: " \t",
			CollectorURL: "\n", Token: " \r", Interval: 0,
		})
	}()
	if panicValue != nil {
		t.Fatalf("whitespace-only scheduler panicked: %v", panicValue)
	}
	if err != nil {
		t.Fatalf("whitespace-only scheduler error = %v", err)
	}
	entries, readErr := os.ReadDir(".")
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("disabled scheduler touched filesystem: %v", entries)
	}
}

func TestStartSchedulerTrimsConfigurationBeforeValidationAndExecution(t *testing.T) {
	dir := t.TempDir()
	pending := writePendingArchive(t, dir, generatedPendingName(time.Now()), "archive", time.Now())
	originalUpload := uploadPendingArchive
	t.Cleanup(func() { uploadPendingArchive = originalUpload })
	uploaded := make(chan struct{}, 1)
	uploadPendingArchive = func(_ context.Context, file *os.File, _ os.FileInfo, collectorURL, token string) (UploadResult, error) {
		if file.Name() != pending {
			t.Errorf("opened path = %q, want %q", file.Name(), pending)
		}
		if collectorURL != "https://collector.example/upload" || token != "collector-token" {
			t.Errorf("upload configuration = URL %q token %q", collectorURL, token)
		}
		uploaded <- struct{}{}
		return validatedUploadResult("stored"), nil
	}
	cfg := schedulerTestConfig(dir, time.Hour)
	cfg.ContentDir = " \t" + cfg.ContentDir + "\n"
	cfg.DatabaseURL = " " + cfg.DatabaseURL + "\t"
	cfg.ArchiveDir = " \t" + cfg.ArchiveDir + "\n"
	cfg.CollectorURL = " " + cfg.CollectorURL + "\t"
	cfg.Token = "\n" + cfg.Token + " "

	ctx, cancel := context.WithCancel(context.Background())
	done := startTestScheduler(ctx, cfg)
	select {
	case <-uploaded:
	case <-time.After(time.Second):
		t.Fatal("trimmed scheduler configuration did not drain pending archive")
	}
	waitForPathRemoval(t, pending)
	cancel()
	waitForScheduler(t, done)
}

func TestValidateSchedulerConfigRejectsEveryNormalizedPartialConfiguration(t *testing.T) {
	root := t.TempDir()
	valid := SchedulerConfig{
		ContentDir: filepath.Join(root, "content"), ArchiveDir: filepath.Join(root, "exports"),
		CollectorURL: "https://collector.example/upload", Token: "secret", Interval: time.Hour,
	}
	for name, mutate := range map[string]func(*SchedulerConfig){
		"URL":      func(cfg *SchedulerConfig) { cfg.CollectorURL = " \t" },
		"token":    func(cfg *SchedulerConfig) { cfg.Token = "\r\n" },
		"interval": func(cfg *SchedulerConfig) { cfg.Interval = 0 },
		"archive":  func(cfg *SchedulerConfig) { cfg.ArchiveDir = " \n" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := valid
			mutate(&cfg)
			if err := ValidateSchedulerConfig(cfg); err == nil {
				t.Fatal("normalized partial scheduler configuration accepted")
			}
		})
	}
}

func TestCompletedArchiveNameMatchesOnlySchedulerGeneratedGrammar(t *testing.T) {
	created := time.Date(2026, time.July, 20, 14, 15, 16, 123456789, time.FixedZone("offset", 2*60*60))
	generated := completedArchiveBase(created)
	if !completedArchiveName(generated) {
		t.Fatalf("generated archive name %q was rejected", generated)
	}
	if generated != "sherpa-20260720T121516Z-1784549716123456789.tar.gz" {
		t.Fatalf("generated archive name = %q", generated)
	}
	for _, name := range []string{
		"sherpa-secret\nforged-log.tar.gz",
		"sherpa-secret\tcontrol.tar.gz",
		"sherpa-20260720T121516Z/1784549716123456789.tar.gz",
		"sherpa-20261320T121516Z-1784549716123456789.tar.gz",
		"sherpa-20260720T121516-1784549716123456789.tar.gz",
		"sherpa-20260720T121516Z-not-decimal.tar.gz",
		"sherpa-20260720T121516Z-01784549716123456789.tar.gz",
		"sherpa-20260720T121515Z-1784549716123456789.tar.gz",
		"sherpa-attacker-chosen.tar.gz",
		"sherpa-.tar.gz",
	} {
		if completedArchiveName(name) {
			t.Errorf("unsafe archive name %q was accepted", name)
		}
	}
}

func TestPendingArchiveBaseAcceptsOnlyCanonicalPrivateClaims(t *testing.T) {
	base := generatedPendingName(time.Now())
	claim := fmt.Sprintf(".sherpa-queue-claim-%d-%d-%s", os.Getpid(), 1, base)
	if got, ok := pendingArchiveBase(claim); !ok || got != base {
		t.Fatalf("generated private claim = %q, %v", got, ok)
	}
	for _, name := range []string{
		".sherpa-queue-claim-0-1-" + base,
		".sherpa-queue-claim-01-1-" + base,
		fmt.Sprintf(".sherpa-queue-claim-%d-01-%s", os.Getpid(), base),
		fmt.Sprintf(".sherpa-queue-claim-%d-0-%s", os.Getpid(), base),
		fmt.Sprintf(".sherpa-queue-claim-%d-1-sherpa-attacker.tar.gz", os.Getpid()),
		fmt.Sprintf(".sherpa-queue-claim-%d-1-%s\nforged", os.Getpid(), base),
	} {
		if got, ok := pendingArchiveBase(name); ok {
			t.Errorf("unsafe private claim %q accepted as %q", name, got)
		}
	}
}

func TestDiscoverPendingArchivesReturnsCompletedArchivesOldestFirst(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	newName := generatedPendingName(now)
	oldBName := generatedPendingName(now.Add(-2 * time.Nanosecond))
	oldAName := generatedPendingName(now.Add(-3 * time.Nanosecond))
	writePendingArchive(t, dir, newName, "new", now.Add(-time.Hour))
	writePendingArchive(t, dir, oldBName, "old-b", now.Add(-3*time.Hour))
	writePendingArchive(t, dir, oldAName, "old-a", now.Add(-3*time.Hour))

	archives, err := discoverPendingArchives(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{oldAName, oldBName, newName}
	if len(archives) != len(want) {
		t.Fatalf("archives = %#v, want %v", archives, want)
	}
	for index, archive := range archives {
		if archive.Base != want[index] {
			t.Fatalf("archive %d basename = %q, want %q", index, archive.Base, want[index])
		}
		if archive.Path != filepath.Join(dir, archive.Base) || archive.Size <= 0 || archive.ModTime.IsZero() {
			t.Fatalf("archive %d metadata = %#v", index, archive)
		}
	}
}

func TestDiscoverPendingArchivesIgnoresTemporaryWorkspacesAndUnsafeNames(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	completeName := generatedPendingName(now)
	complete := writePendingArchive(t, dir, completeName, "complete", now)
	for _, name := range []string{
		".sherpa-export-partial.tar.gz",
		"sherpa-.tar.gz",
		"not-sherpa.tar.gz",
		"sherpa-attacker-chosen.tar.gz",
		"sherpa-secret\nforged-log.tar.gz",
		"sherpa-20260720T121516Z-not-decimal.tar.gz",
	} {
		writePendingArchive(t, dir, name, "ignored", now)
	}
	if err := os.Mkdir(filepath.Join(dir, ".sherpa-export-work-123"), 0o700); err != nil {
		t.Fatal(err)
	}
	directoryName := generatedPendingName(now.Add(time.Nanosecond))
	if err := os.Mkdir(filepath.Join(dir, directoryName), 0o700); err != nil {
		t.Fatal(err)
	}
	linkName := generatedPendingName(now.Add(2 * time.Nanosecond))
	if err := os.Symlink(complete, filepath.Join(dir, linkName)); err != nil {
		t.Fatal(err)
	}

	archives, err := discoverPendingArchives(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(archives) != 1 || archives[0].Base != completeName {
		t.Fatalf("archives = %#v, want only generated completed regular archive", archives)
	}
}

func TestCleanupStaleExportPartialsRemovesOnlyActualTempNamesOlderThan24Hours(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().Round(time.Second)
	old := now.Add(-25 * time.Hour)
	boundary := now.Add(-24 * time.Hour)
	oldPartial := createTempPartial(t, dir, old)
	youngPartial := createTempPartial(t, dir, now.Add(-time.Hour))
	boundaryPartial := createTempPartial(t, dir, boundary)
	oldWorkspace, err := os.MkdirTemp(dir, ".sherpa-export-work-*")
	if err != nil {
		t.Fatal(err)
	}
	youngWorkspace, err := os.MkdirTemp(dir, ".sherpa-export-work-*")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{oldWorkspace} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{
		".sherpa-export-old.tar.gz",
		".sherpa-export-0123.tar.gz",
		".sherpa-export-4294967296.tar.gz",
		".sherpa-export-.tar.gz",
		".sherpa-export-123.tar.gz.extra",
		".sherpa-export-work-old",
		".sherpa-export-work-0123",
		".sherpa-export-work-4294967296",
		".sherpa-export-work-",
	} {
		path := filepath.Join(dir, name)
		if strings.Contains(name, "work") {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		} else {
			writePendingArchive(t, dir, name, "lookalike", old)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	completedName := generatedPendingName(now.Add(-48 * time.Hour))
	writePendingArchive(t, dir, completedName, "complete", old)

	removed, err := cleanupStaleExportPartials(dir, now, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}
	for _, path := range []string{oldPartial, oldWorkspace} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("stale generated %q remains: %v", filepath.Base(path), err)
		}
	}
	for _, path := range []string{youngPartial, boundaryPartial, youngWorkspace, filepath.Join(dir, completedName)} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("safe entry %q removed: %v", filepath.Base(path), err)
		}
	}
	for _, name := range []string{
		".sherpa-export-old.tar.gz", ".sherpa-export-0123.tar.gz", ".sherpa-export-4294967296.tar.gz",
		".sherpa-export-.tar.gz", ".sherpa-export-123.tar.gz.extra", ".sherpa-export-work-old",
		".sherpa-export-work-0123", ".sherpa-export-work-4294967296", ".sherpa-export-work-",
	} {
		if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("lookalike entry %q removed: %v", name, err)
		}
	}
}

func TestCleanupStaleExportPartialsRecoversPrivateClaimsAfterRestart(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().Round(time.Second)
	old := now.Add(-25 * time.Hour)
	partial := createTempPartial(t, dir, old)
	workspace, err := os.MkdirTemp(dir, ".sherpa-export-work-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(workspace, old, old); err != nil {
		t.Fatal(err)
	}
	originalCounter := queueClaimCounter.Load()
	queueClaimCounter.Store(0)
	t.Cleanup(func() { queueClaimCounter.Store(originalCounter) })
	partialClaim := filepath.Join(dir, fmt.Sprintf(".sherpa-cleanup-claim-%d-1-%s", os.Getpid(), filepath.Base(partial)))
	workspaceClaim := filepath.Join(dir, fmt.Sprintf(".sherpa-cleanup-claim-%d-2-%s", os.Getpid(), filepath.Base(workspace)))
	if err := os.Rename(partial, partialClaim); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(workspace, workspaceClaim); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(workspaceClaim, now, now); err != nil {
		t.Fatal(err)
	}

	removed, err := cleanupStaleExportPartials(dir, now, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Fatalf("removed recovered claims = %d, want 2", removed)
	}
	for _, path := range []string{partialClaim, workspaceClaim} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("recovered cleanup claim remains: %s: %v", filepath.Base(path), err)
		}
	}
}

func TestCleanupStaleExportPartialsPreservesYoungReplacementsInstalledBeforeClaim(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().Round(time.Second)
	old := now.Add(-25 * time.Hour)
	oldPartial := createTempPartial(t, dir, old)
	if err := os.WriteFile(oldPartial, []byte("old partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(oldPartial, old, old); err != nil {
		t.Fatal(err)
	}
	oldWorkspace, err := os.MkdirTemp(dir, ".sherpa-export-work-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(oldWorkspace, old, old); err != nil {
		t.Fatal(err)
	}

	originalHook := beforeClaimStaleExportEntry
	t.Cleanup(func() { beforeClaimStaleExportEntry = originalHook })
	beforeClaimStaleExportEntry = func(path string) {
		preserved := path + ".old"
		if err := os.Rename(path, preserved); err != nil {
			t.Fatal(err)
		}
		if generatedPartialArchiveName(filepath.Base(path)) {
			if err := os.WriteFile(path, []byte("young replacement"), 0o600); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(path, "sentinel"), []byte("young replacement"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}

	removed, err := cleanupStaleExportPartials(dir, now, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Fatalf("removed = %d, want 0", removed)
	}
	partialContents, err := os.ReadFile(oldPartial)
	if err != nil {
		t.Fatal(err)
	}
	if string(partialContents) != "young replacement" {
		t.Fatalf("partial replacement contents = %q", partialContents)
	}
	workspaceContents, err := os.ReadFile(filepath.Join(oldWorkspace, "sentinel"))
	if err != nil {
		t.Fatal(err)
	}
	if string(workspaceContents) != "young replacement" {
		t.Fatalf("workspace replacement contents = %q", workspaceContents)
	}
}

func TestCleanupStaleExportPartialsNeverFollowsSymlinksOrRemovesCompletedArchives(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	targetFile := filepath.Join(outside, "target-file")
	targetDir := filepath.Join(outside, "target-dir")
	if err := os.WriteFile(targetFile, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(targetDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(targetDir, "sentinel"), []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	fileLink := filepath.Join(dir, ".sherpa-export-123.tar.gz")
	dirLink := filepath.Join(dir, ".sherpa-export-work-456")
	if err := os.Symlink(targetFile, fileLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(targetDir, dirLink); err != nil {
		t.Fatal(err)
	}
	completed := writePendingArchive(t, dir, generatedPendingName(time.Now().Add(-72*time.Hour)), "complete", time.Now().Add(-72*time.Hour))

	removed, err := cleanupStaleExportPartials(dir, time.Now().Add(48*time.Hour), 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Fatalf("removed = %d, want 0", removed)
	}
	for _, path := range []string{fileLink, dirLink, targetFile, filepath.Join(targetDir, "sentinel"), completed} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("safe path %q removed: %v", path, err)
		}
	}
}

func TestStartSchedulerDrainsExistingArchiveBeforeFirstTick(t *testing.T) {
	dir := t.TempDir()
	pending := writePendingArchive(t, dir, generatedPendingName(time.Now()), "archive", time.Now())
	originalRun, originalUpload := runExport, uploadPendingArchive
	t.Cleanup(func() { runExport, uploadPendingArchive = originalRun, originalUpload })
	var runs atomic.Int32
	uploaded := make(chan string, 1)
	runExport = func(context.Context, string, string, string) error {
		runs.Add(1)
		return nil
	}
	uploadPendingArchive = func(_ context.Context, file *os.File, _ os.FileInfo, _, _ string) (UploadResult, error) {
		uploaded <- file.Name()
		return validatedUploadResult("stored"), nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := startTestScheduler(ctx, schedulerTestConfig(dir, time.Hour))
	select {
	case got := <-uploaded:
		if got != pending {
			t.Fatalf("uploaded path = %q, want %q", got, pending)
		}
	case <-time.After(time.Second):
		t.Fatal("pending archive was not drained before first tick")
	}
	waitForPathRemoval(t, pending)
	cancel()
	waitForScheduler(t, done)
	if runs.Load() != 0 {
		t.Fatalf("export runs = %d, want 0", runs.Load())
	}
}

func TestStartSchedulerDoesNotCreateAcrossTicksWhilePendingArchiveExists(t *testing.T) {
	dir := t.TempDir()
	pending := writePendingArchive(t, dir, generatedPendingName(time.Now()), "archive", time.Now())
	originalRun, originalUpload := runExport, uploadPendingArchive
	t.Cleanup(func() { runExport, uploadPendingArchive = originalRun, originalUpload })
	var runs atomic.Int32
	attempts := make(chan struct{}, 4)
	runExport = func(context.Context, string, string, string) error {
		runs.Add(1)
		return nil
	}
	uploadPendingArchive = func(context.Context, *os.File, os.FileInfo, string, string) (UploadResult, error) {
		attempts <- struct{}{}
		return UploadResult{}, errors.New("temporary collector failure")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := startTestScheduler(ctx, schedulerTestConfig(dir, 5*time.Millisecond))
	for attempt := 1; attempt <= 3; attempt++ {
		select {
		case <-attempts:
		case <-time.After(time.Second):
			t.Fatalf("pending archive attempts = %d, want at least 3 across scheduler ticks", attempt-1)
		}
	}
	cancel()
	waitForScheduler(t, done)
	if runs.Load() != 0 {
		t.Fatalf("export runs = %d, want 0 while pending archive remains", runs.Load())
	}
	if _, err := os.Stat(pending); err != nil {
		t.Fatalf("pending archive was not preserved: %v", err)
	}
}

func TestStartSchedulerDrainsMultipleArchivesOldestFirst(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	thirdName := generatedPendingName(now)
	firstName := generatedPendingName(now.Add(-2 * time.Nanosecond))
	secondName := generatedPendingName(now.Add(-time.Nanosecond))
	writePendingArchive(t, dir, thirdName, "third", now.Add(-time.Hour))
	writePendingArchive(t, dir, firstName, "first", now.Add(-3*time.Hour))
	writePendingArchive(t, dir, secondName, "second", now.Add(-2*time.Hour))
	originalRun, originalUpload := runExport, uploadPendingArchive
	t.Cleanup(func() { runExport, uploadPendingArchive = originalRun, originalUpload })
	var runs atomic.Int32
	uploaded := make(chan string, 3)
	runExport = func(context.Context, string, string, string) error {
		runs.Add(1)
		return nil
	}
	uploadPendingArchive = func(_ context.Context, file *os.File, _ os.FileInfo, _, _ string) (UploadResult, error) {
		uploaded <- filepath.Base(file.Name())
		return validatedUploadResult("stored"), nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := startTestScheduler(ctx, schedulerTestConfig(dir, time.Hour))
	var got []string
	for len(got) < 3 {
		select {
		case name := <-uploaded:
			got = append(got, name)
		case <-time.After(time.Second):
			t.Fatalf("uploaded order = %v", got)
		}
	}
	for _, name := range got {
		waitForPathRemoval(t, filepath.Join(dir, name))
	}
	cancel()
	waitForScheduler(t, done)
	want := []string{firstName, secondName, thirdName}
	if !slices.Equal(got, want) {
		t.Fatalf("uploaded order = %v, want %v", got, want)
	}
	if runs.Load() != 0 {
		t.Fatalf("export runs = %d, want 0", runs.Load())
	}
}

func TestDrainPendingArchivesRestartAfterStoredDeleteFailureResendsExistingThenDeletes(t *testing.T) {
	dir := t.TempDir()
	payload := []byte("archive committed before restart")
	pending := writePendingArchive(t, dir, generatedPendingName(time.Now()), string(payload), time.Now())
	originalUpload, originalRemove := uploadPendingArchive, removeClaimedPendingArchive
	t.Cleanup(func() {
		uploadPendingArchive = originalUpload
		removeClaimedPendingArchive = originalRemove
	})
	statuses := []string{"stored", "existing"}
	var uploads int
	uploadPendingArchive = func(_ context.Context, file *os.File, _ os.FileInfo, _, _ string) (UploadResult, error) {
		body, err := io.ReadAll(file)
		if err != nil {
			return UploadResult{}, err
		}
		if !bytes.Equal(body, payload) {
			t.Fatalf("upload %d bytes = %q", uploads+1, body)
		}
		if uploads >= len(statuses) {
			t.Fatal("archive uploaded more than twice")
		}
		status := statuses[uploads]
		uploads++
		return validatedUploadResult(status), nil
	}
	removeClaimedPendingArchive = func(*archiveQueue, string) error {
		return errors.New("deterministic local delete failure")
	}

	remaining, err := drainPendingArchives(context.Background(), schedulerTestConfig(dir, time.Hour))
	if remaining != 1 || err == nil || err.Error() != "remove uploaded export archive" {
		t.Fatalf("first drain result = remaining %d, error %v", remaining, err)
	}
	if _, err := os.Lstat(pending); !os.IsNotExist(err) {
		t.Fatalf("original pathname remains after private claim: %v", err)
	}
	queued, err := discoverPendingArchives(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 || queued[0].Base != filepath.Base(pending) {
		t.Fatalf("recoverable claimed queue = %#v", queued)
	}

	removeClaimedPendingArchive = originalRemove
	remaining, err = drainPendingArchives(context.Background(), schedulerTestConfig(dir, time.Hour))
	if err != nil || remaining != 0 {
		t.Fatalf("restart drain result = remaining %d, error %v", remaining, err)
	}
	if uploads != 2 {
		t.Fatalf("uploads = %d, want stored then existing", uploads)
	}
	queued, err = discoverPendingArchives(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 0 {
		t.Fatalf("queue after validated existing response = %#v", queued)
	}
}

func TestStartSchedulerPreservesAllArchivesAfterMidQueueFailure(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	first := writePendingArchive(t, dir, generatedPendingName(now.Add(-2*time.Nanosecond)), "first", now.Add(-3*time.Hour))
	second := writePendingArchive(t, dir, generatedPendingName(now.Add(-time.Nanosecond)), "second", now.Add(-2*time.Hour))
	third := writePendingArchive(t, dir, generatedPendingName(now), "third", now.Add(-time.Hour))
	originalRun, originalUpload := runExport, uploadPendingArchive
	t.Cleanup(func() { runExport, uploadPendingArchive = originalRun, originalUpload })
	uploaded := make(chan string, 2)
	uploadPendingArchive = func(_ context.Context, file *os.File, _ os.FileInfo, _, _ string) (UploadResult, error) {
		base := filepath.Base(file.Name())
		uploaded <- base
		if base == filepath.Base(second) {
			return UploadResult{}, errors.New("temporary collector failure")
		}
		return validatedUploadResult("stored"), nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := startTestScheduler(ctx, schedulerTestConfig(dir, time.Hour))
	for range 2 {
		select {
		case <-uploaded:
		case <-time.After(time.Second):
			t.Fatal("queue did not reach the failing archive")
		}
	}
	waitForPathRemoval(t, first)
	cancel()
	waitForScheduler(t, done)
	for _, path := range []string{second, third} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("archive after failure was not preserved: %s: %v", filepath.Base(path), err)
		}
	}
	select {
	case name := <-uploaded:
		t.Fatalf("archive after failure was uploaded: %s", name)
	default:
	}
}

func TestDrainPendingArchivesRejectsSymlinkToFIFOWithoutBlockingOrOpeningTarget(t *testing.T) {
	dir := t.TempDir()
	pending := writePendingArchive(t, dir, generatedPendingName(time.Now()), "queued archive", time.Now())
	fifo := filepath.Join(t.TempDir(), "blocking-fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	var requested atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requested.Store(true)
	}))
	defer server.Close()

	originalBeforeHook, originalAfterHook := beforeOpenPendingArchive, afterOpenPendingArchive
	t.Cleanup(func() {
		beforeOpenPendingArchive = originalBeforeHook
		afterOpenPendingArchive = originalAfterHook
	})
	var openedTarget atomic.Bool
	afterOpenPendingArchive = func(string) { openedTarget.Store(true) }
	preserved := pending + ".original"
	beforeOpenPendingArchive = func(path string) {
		if err := os.Rename(path, preserved); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(fifo, path); err != nil {
			t.Fatal(err)
		}
	}

	done := make(chan struct{})
	var remaining int
	var drainErr error
	go func() {
		remaining, drainErr = drainPendingArchives(context.Background(), schedulerTestConfigWithCollector(dir, time.Hour, server.URL))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("queue open followed a symlink to a FIFO and blocked")
	}
	if remaining != 1 || drainErr == nil || drainErr.Error() != "pending export archive changed" {
		t.Fatalf("drain result = remaining %d, error %v", remaining, drainErr)
	}
	if openedTarget.Load() {
		t.Fatal("queue open dereferenced the symlink target")
	}
	if requested.Load() {
		t.Fatal("symlink target was uploaded")
	}
	for _, path := range []string{pending, preserved, fifo} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("safe path %q missing: %v", path, err)
		}
	}
}

func TestDrainPendingArchivesDoesNotDeleteReplacementCreatedDuringSuccessfulUpload(t *testing.T) {
	dir := t.TempDir()
	pending := writePendingArchive(t, dir, generatedPendingName(time.Now()), "original archive", time.Now())
	preserved := pending + ".uploaded"
	replacement := []byte("replacement archive")
	originalUpload := uploadPendingArchive
	t.Cleanup(func() { uploadPendingArchive = originalUpload })
	uploadPendingArchive = func(_ context.Context, file *os.File, _ os.FileInfo, _, _ string) (UploadResult, error) {
		contents, err := io.ReadAll(file)
		if err != nil {
			return UploadResult{}, err
		}
		if string(contents) != "original archive" {
			t.Fatalf("uploaded bytes = %q", contents)
		}
		if err := os.Rename(pending, preserved); err != nil {
			return UploadResult{}, err
		}
		if err := os.WriteFile(pending, replacement, 0o600); err != nil {
			return UploadResult{}, err
		}
		return validatedUploadResult("stored"), nil
	}

	remaining, err := drainPendingArchives(context.Background(), schedulerTestConfig(dir, time.Hour))
	if remaining != 1 || err == nil || err.Error() != "pending export archive changed" {
		t.Fatalf("drain result = remaining %d, error %v", remaining, err)
	}
	contents, readErr := os.ReadFile(pending)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(contents, replacement) {
		t.Fatalf("replacement contents = %q", contents)
	}
	if _, err := os.Stat(preserved); err != nil {
		t.Fatalf("uploaded original was not preserved by test: %v", err)
	}
}

func TestDrainPendingArchivesClaimSetupFailureLeavesOnlyOriginalQueueEntry(t *testing.T) {
	dir := t.TempDir()
	pending := writePendingArchive(t, dir, generatedPendingName(time.Now()), "original archive", time.Now())
	originalUpload, originalHook := uploadPendingArchive, beforePendingClaimRename
	t.Cleanup(func() {
		uploadPendingArchive = originalUpload
		beforePendingClaimRename = originalHook
	})
	uploadPendingArchive = func(context.Context, *os.File, os.FileInfo, string, string) (UploadResult, error) {
		return validatedUploadResult("stored"), nil
	}
	beforePendingClaimRename = func(string) error {
		return errors.New("deterministic interruption before claim rename")
	}

	remaining, err := drainPendingArchives(context.Background(), schedulerTestConfig(dir, time.Hour))
	if remaining != 1 || err == nil || err.Error() != "pending export archive changed" {
		t.Fatalf("drain result = remaining %d, error %v", remaining, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(pending) {
		t.Fatalf("queue entries after interrupted claim = %v", entries)
	}
}

func TestDrainPendingArchivesDoesNotOverwriteCollidingPrivateClaim(t *testing.T) {
	dir := t.TempDir()
	pending := writePendingArchive(t, dir, generatedPendingName(time.Now()), "original archive", time.Now())
	originalUpload, originalHook := uploadPendingArchive, beforePendingClaimRename
	t.Cleanup(func() {
		uploadPendingArchive = originalUpload
		beforePendingClaimRename = originalHook
	})
	uploadPendingArchive = func(context.Context, *os.File, os.FileInfo, string, string) (UploadResult, error) {
		return validatedUploadResult("stored"), nil
	}
	var collisionPath string
	beforePendingClaimRename = func(path string) error {
		collisionPath = path
		return os.WriteFile(path, []byte("preserve colliding claim"), 0o600)
	}

	remaining, err := drainPendingArchives(context.Background(), schedulerTestConfig(dir, time.Hour))
	if remaining != 1 || err == nil || err.Error() != "pending export archive changed" {
		t.Fatalf("drain result = remaining %d, error %v", remaining, err)
	}
	contents, err := os.ReadFile(collisionPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "preserve colliding claim" {
		t.Fatalf("colliding claim contents = %q", contents)
	}
	if _, err := os.Stat(pending); err != nil {
		t.Fatalf("original queue entry was not preserved: %v", err)
	}
}

func TestDrainPendingArchivesDoesNotDeleteReplacementInstalledImmediatelyBeforeClaim(t *testing.T) {
	dir := t.TempDir()
	pending := writePendingArchive(t, dir, generatedPendingName(time.Now()), "original archive", time.Now())
	preserved := pending + ".uploaded"
	replacement := []byte("last-window replacement")
	originalUpload, originalHook := uploadPendingArchive, beforeClaimPendingArchive
	t.Cleanup(func() {
		uploadPendingArchive = originalUpload
		beforeClaimPendingArchive = originalHook
	})
	uploadPendingArchive = func(_ context.Context, file *os.File, _ os.FileInfo, _, _ string) (UploadResult, error) {
		contents, err := io.ReadAll(file)
		if err != nil {
			return UploadResult{}, err
		}
		if string(contents) != "original archive" {
			t.Fatalf("uploaded bytes = %q", contents)
		}
		return validatedUploadResult("stored"), nil
	}
	beforeClaimPendingArchive = func(path string) {
		if err := os.Rename(path, preserved); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, replacement, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	remaining, err := drainPendingArchives(context.Background(), schedulerTestConfig(dir, time.Hour))
	if remaining != 1 || err == nil || err.Error() != "pending export archive changed" {
		t.Fatalf("drain result = remaining %d, error %v", remaining, err)
	}
	contents, readErr := os.ReadFile(pending)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(contents, replacement) {
		t.Fatalf("replacement contents = %q", contents)
	}
	if _, err := os.Stat(preserved); err != nil {
		t.Fatalf("uploaded original was not preserved by test: %v", err)
	}
}

func TestDrainPendingArchivesDeletesUnchangedUploadedFile(t *testing.T) {
	dir := t.TempDir()
	pending := writePendingArchive(t, dir, generatedPendingName(time.Now()), "original archive", time.Now())
	originalUpload := uploadPendingArchive
	t.Cleanup(func() { uploadPendingArchive = originalUpload })
	uploadPendingArchive = func(_ context.Context, file *os.File, _ os.FileInfo, _, _ string) (UploadResult, error) {
		contents, err := io.ReadAll(file)
		if err != nil {
			return UploadResult{}, err
		}
		if string(contents) != "original archive" {
			t.Fatalf("uploaded bytes = %q", contents)
		}
		return validatedUploadResult("stored"), nil
	}

	remaining, err := drainPendingArchives(context.Background(), schedulerTestConfig(dir, time.Hour))
	if err != nil || remaining != 0 {
		t.Fatalf("drain result = remaining %d, error %v", remaining, err)
	}
	if _, err := os.Lstat(pending); !os.IsNotExist(err) {
		t.Fatalf("unchanged uploaded file remains: %v", err)
	}
}

func TestDrainPendingArchivesRealHTTPUploadRetainsFileOwnershipAndDeletes(t *testing.T) {
	dir := t.TempDir()
	payload := []byte("real queued archive")
	pending := writePendingArchive(t, dir, generatedPendingName(time.Now()), string(payload), time.Now())
	digest := sha256.Sum256(payload)
	objectID := "sha256:" + hex.EncodeToString(digest[:])
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
		}
		if !bytes.Equal(body, payload) {
			t.Errorf("uploaded body = %q", body)
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"object_id":%q,"status":"stored"}`, objectID)
	}))
	defer server.Close()
	originalUpload := uploadPendingArchive
	t.Cleanup(func() { uploadPendingArchive = originalUpload })
	uploadPendingArchive = uploadArchiveFile

	remaining, err := drainPendingArchives(context.Background(), schedulerTestConfigWithCollector(dir, time.Hour, server.URL))
	if err != nil || remaining != 0 {
		t.Fatalf("drain result = remaining %d, error %v", remaining, err)
	}
	if _, err := os.Lstat(pending); !os.IsNotExist(err) {
		t.Fatalf("successfully uploaded archive remains: %v", err)
	}
}

func TestStartSchedulerLogsOnlyGeneratedSafeBasenameAndRetryMetadata(t *testing.T) {
	dir := t.TempDir()
	safeName := generatedPendingName(time.Now().Add(-2 * time.Hour))
	writePendingArchive(t, dir, safeName, "archive", time.Now().Add(-2*time.Hour))
	maliciousName := "sherpa-attacker-secret\nforged-log.tar.gz"
	writePendingArchive(t, dir, maliciousName, "ignored", time.Now().Add(-3*time.Hour))
	originalRun, originalUpload := runExport, uploadPendingArchive
	t.Cleanup(func() { runExport, uploadPendingArchive = originalRun, originalUpload })
	attempted := make(chan struct{}, 1)
	uploadPendingArchive = func(context.Context, *os.File, os.FileInfo, string, string) (UploadResult, error) {
		attempted <- struct{}{}
		return UploadResult{}, errors.New("transport-secret from private collector")
	}
	var logs bytes.Buffer
	cfg := schedulerTestConfig(dir, time.Hour)
	cfg.DatabaseURL = "postgres://db-secret@db.internal/sherpa"
	cfg.CollectorURL = "https://collector.example/upload?private=query"
	cfg.Token = "collector-secret"
	cfg.Logger = log.New(&logs, "", 0)

	ctx, cancel := context.WithCancel(context.Background())
	done := startTestScheduler(ctx, cfg)
	select {
	case <-attempted:
	case <-time.After(time.Second):
		t.Fatal("pending archive was not attempted")
	}
	cancel()
	waitForScheduler(t, done)
	got := logs.String()
	for _, safe := range []string{"queue_count=1", "archive=" + safeName, "age=", "retry_class=upload"} {
		if !strings.Contains(got, safe) {
			t.Fatalf("scheduler log %q does not contain %q", got, safe)
		}
	}
	for _, secret := range []string{dir, maliciousName, "attacker-secret", "forged-log", "db-secret", "collector-secret", "private=query", "transport-secret", "collector.example"} {
		if strings.Contains(got, secret) {
			t.Fatalf("scheduler log %q exposes %q", got, secret)
		}
	}
}

func writePendingArchive(t *testing.T, dir, name, contents string, modTime time.Time) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatal(err)
	}
	return path
}

func generatedPendingName(created time.Time) string {
	return fmt.Sprintf("sherpa-%s-%d.tar.gz", created.UTC().Format("20060102T150405Z"), created.UnixNano())
}

func createTempPartial(t *testing.T, dir string, modTime time.Time) string {
	t.Helper()
	file, err := os.CreateTemp(dir, ".sherpa-export-*.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(file.Name(), modTime, modTime); err != nil {
		t.Fatal(err)
	}
	return file.Name()
}

func schedulerTestConfig(archiveDir string, interval time.Duration) SchedulerConfig {
	return schedulerTestConfigWithCollector(archiveDir, interval, "https://collector.example/upload")
}

func schedulerTestConfigWithCollector(archiveDir string, interval time.Duration, collectorURL string) SchedulerConfig {
	return SchedulerConfig{
		ContentDir: testrunContentDir(archiveDir), DatabaseURL: "postgres://db.internal/sherpa",
		ArchiveDir: archiveDir, CollectorURL: collectorURL,
		Token: "collector-token", Interval: interval, Logger: log.New(io.Discard, "", 0),
	}
}

func testrunContentDir(archiveDir string) string {
	return filepath.Join(filepath.Dir(archiveDir), "content")
}

func startTestScheduler(ctx context.Context, cfg SchedulerConfig) <-chan error {
	done := make(chan error, 1)
	go func() {
		done <- StartScheduler(ctx, cfg)
	}()
	return done
}

func waitForScheduler(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("scheduler did not stop")
	}
}

func waitForPathRemoval(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("path was not removed: %s", path)
}

func validatedUploadResult(status string) UploadResult {
	return UploadResult{ObjectID: "sha256:" + strings.Repeat("a", sha256.Size*2), Status: status}
}

type countingHashReader struct {
	reads int
}

func (r *countingHashReader) Read([]byte) (int, error) {
	r.reads++
	return 0, io.EOF
}

type cancellationHashReader struct {
	started    chan struct{}
	resume     chan struct{}
	secondRead chan struct{}
	finish     chan struct{}
	reads      int
}

func (r *cancellationHashReader) Read(p []byte) (int, error) {
	r.reads++
	if r.reads == 1 {
		close(r.started)
		<-r.resume
		return copy(p, "archive bytes"), nil
	}
	close(r.secondRead)
	<-r.finish
	return 0, io.EOF
}

type stagedCancellationContext struct {
	cancelAt int
	errCalls int
}

func (*stagedCancellationContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (*stagedCancellationContext) Done() <-chan struct{}       { return nil }
func (c *stagedCancellationContext) Err() error {
	c.errCalls++
	if c.errCalls >= c.cancelAt {
		return context.Canceled
	}
	return nil
}
func (*stagedCancellationContext) Value(any) any { return nil }

type terminalEOFHashReader struct {
	reads int
}

func (r *terminalEOFHashReader) Read(p []byte) (int, error) {
	r.reads++
	return copy(p, "archive bytes"), io.EOF
}

type terminalCancellationHashReader struct {
	cancel context.CancelFunc
	reads  int
}

func (r *terminalCancellationHashReader) Read(p []byte) (int, error) {
	r.reads++
	n := copy(p, "archive bytes")
	r.cancel()
	return n, io.EOF
}

func writeUploadArchive(t *testing.T, contents []byte) (string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "archive.tar.gz")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	return path, "sha256:" + hex.EncodeToString(digest[:])
}

func createBareRepository(t *testing.T, contentDir, owner, name string) {
	t.Helper()
	work := t.TempDir()
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
	} {
		if err := execCommand(context.Background(), work, "git", args, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte(owner+"/"+name), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := execCommand(context.Background(), work, "git", []string{"add", "README.md"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := execCommand(context.Background(), work, "git", []string{"commit", "-m", "initial"}, nil); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(contentDir, "profiles", owner, name+".git")
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := execCommand(context.Background(), t.TempDir(), "git", []string{"clone", "--bare", work, destination}, nil); err != nil {
		t.Fatal(err)
	}
}

func readArchive(t *testing.T, path string) (map[string][]byte, []string) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	files := make(map[string][]byte)
	var order []string
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(tarReader)
		if err != nil {
			t.Fatal(err)
		}
		files[header.Name] = data
		order = append(order, header.Name)
	}
	return files, order
}
