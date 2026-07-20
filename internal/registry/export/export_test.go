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
	originalRun, originalUpload := runExport, uploadArchive
	t.Cleanup(func() { runExport, uploadArchive = originalRun, originalUpload })
	dir := t.TempDir()
	var runs atomic.Int32
	var uploads atomic.Int32
	runExport = func(_ context.Context, _, _, path string) error {
		runs.Add(1)
		return os.WriteFile(path, []byte("archive"), 0o600)
	}
	uploadArchive = func(context.Context, string, string, string) (UploadResult, error) {
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

func TestDiscoverPendingArchivesReturnsCompletedArchivesOldestFirst(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writePendingArchive(t, dir, "sherpa-new.tar.gz", "new", now.Add(-time.Hour))
	writePendingArchive(t, dir, "sherpa-old-b.tar.gz", "old-b", now.Add(-3*time.Hour))
	writePendingArchive(t, dir, "sherpa-old-a.tar.gz", "old-a", now.Add(-3*time.Hour))

	archives, err := discoverPendingArchives(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"sherpa-old-a.tar.gz", "sherpa-old-b.tar.gz", "sherpa-new.tar.gz"}
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

func TestDiscoverPendingArchivesIgnoresTemporaryWorkspacesAndPartialArchives(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writePendingArchive(t, dir, "sherpa-complete.tar.gz", "complete", now)
	writePendingArchive(t, dir, ".sherpa-export-partial.tar.gz", "partial", now)
	writePendingArchive(t, dir, "sherpa-.tar.gz", "empty-name", now)
	writePendingArchive(t, dir, "not-sherpa.tar.gz", "other", now)
	if err := os.Mkdir(filepath.Join(dir, ".sherpa-export-work-123"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sherpa-directory.tar.gz"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "sherpa-complete.tar.gz"), filepath.Join(dir, "sherpa-link.tar.gz")); err != nil {
		t.Fatal(err)
	}

	archives, err := discoverPendingArchives(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(archives) != 1 || archives[0].Base != "sherpa-complete.tar.gz" {
		t.Fatalf("archives = %#v, want only completed regular archive", archives)
	}
}

func TestCleanupStaleExportPartialsRemovesOnlyGeneratedNamesOlderThan24Hours(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().Round(time.Second)
	old := now.Add(-25 * time.Hour)
	boundary := now.Add(-24 * time.Hour)
	writePendingArchive(t, dir, ".sherpa-export-old.tar.gz", "partial", old)
	writePendingArchive(t, dir, ".sherpa-export-young.tar.gz", "partial", now.Add(-time.Hour))
	writePendingArchive(t, dir, ".sherpa-export-boundary.tar.gz", "partial", boundary)
	writePendingArchive(t, dir, ".sherpa-export-.tar.gz", "lookalike", old)
	writePendingArchive(t, dir, ".sherpa-export-old.tar.gz.extra", "lookalike", old)
	writePendingArchive(t, dir, "sherpa-completed.tar.gz", "complete", old)
	oldWorkspace := filepath.Join(dir, ".sherpa-export-work-old")
	youngWorkspace := filepath.Join(dir, ".sherpa-export-work-young")
	lookalikeWorkspace := filepath.Join(dir, ".sherpa-export-work-")
	for _, path := range []string{oldWorkspace, youngWorkspace, lookalikeWorkspace} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{oldWorkspace, lookalikeWorkspace} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}

	removed, err := cleanupStaleExportPartials(dir, now, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}
	for _, name := range []string{".sherpa-export-old.tar.gz", ".sherpa-export-work-old"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("stale generated %q remains: %v", name, err)
		}
	}
	for _, name := range []string{
		".sherpa-export-young.tar.gz", ".sherpa-export-boundary.tar.gz",
		".sherpa-export-.tar.gz", ".sherpa-export-old.tar.gz.extra",
		"sherpa-completed.tar.gz", ".sherpa-export-work-young", ".sherpa-export-work-",
	} {
		if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("safe entry %q removed: %v", name, err)
		}
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
	fileLink := filepath.Join(dir, ".sherpa-export-linked.tar.gz")
	dirLink := filepath.Join(dir, ".sherpa-export-work-linked")
	if err := os.Symlink(targetFile, fileLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(targetDir, dirLink); err != nil {
		t.Fatal(err)
	}
	completed := writePendingArchive(t, dir, "sherpa-old.tar.gz", "complete", time.Now().Add(-72*time.Hour))

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
	pending := writePendingArchive(t, dir, "sherpa-existing.tar.gz", "archive", time.Now())
	originalRun, originalUpload := runExport, uploadArchive
	t.Cleanup(func() { runExport, uploadArchive = originalRun, originalUpload })
	var runs atomic.Int32
	uploaded := make(chan string, 1)
	runExport = func(context.Context, string, string, string) error {
		runs.Add(1)
		return nil
	}
	uploadArchive = func(_ context.Context, path, _, _ string) (UploadResult, error) {
		uploaded <- path
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

func TestStartSchedulerDoesNotCreateWhilePendingArchiveExists(t *testing.T) {
	dir := t.TempDir()
	pending := writePendingArchive(t, dir, "sherpa-pending.tar.gz", "archive", time.Now())
	originalRun, originalUpload := runExport, uploadArchive
	t.Cleanup(func() { runExport, uploadArchive = originalRun, originalUpload })
	var runs atomic.Int32
	uploaded := make(chan struct{}, 1)
	runExport = func(context.Context, string, string, string) error {
		runs.Add(1)
		return nil
	}
	uploadArchive = func(context.Context, string, string, string) (UploadResult, error) {
		uploaded <- struct{}{}
		return UploadResult{}, errors.New("temporary collector failure")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := startTestScheduler(ctx, schedulerTestConfig(dir, time.Hour))
	select {
	case <-uploaded:
	case <-time.After(time.Second):
		t.Fatal("pending archive was not attempted")
	}
	cancel()
	waitForScheduler(t, done)
	if runs.Load() != 0 {
		t.Fatalf("export runs = %d, want 0", runs.Load())
	}
	if _, err := os.Stat(pending); err != nil {
		t.Fatalf("pending archive was not preserved: %v", err)
	}
}

func TestStartSchedulerDrainsMultipleArchivesOldestFirst(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writePendingArchive(t, dir, "sherpa-third.tar.gz", "third", now.Add(-time.Hour))
	writePendingArchive(t, dir, "sherpa-first.tar.gz", "first", now.Add(-3*time.Hour))
	writePendingArchive(t, dir, "sherpa-second.tar.gz", "second", now.Add(-2*time.Hour))
	originalRun, originalUpload := runExport, uploadArchive
	t.Cleanup(func() { runExport, uploadArchive = originalRun, originalUpload })
	var runs atomic.Int32
	uploaded := make(chan string, 3)
	runExport = func(context.Context, string, string, string) error {
		runs.Add(1)
		return nil
	}
	uploadArchive = func(_ context.Context, path, _, _ string) (UploadResult, error) {
		uploaded <- filepath.Base(path)
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
	want := []string{"sherpa-first.tar.gz", "sherpa-second.tar.gz", "sherpa-third.tar.gz"}
	if !slices.Equal(got, want) {
		t.Fatalf("uploaded order = %v, want %v", got, want)
	}
	if runs.Load() != 0 {
		t.Fatalf("export runs = %d, want 0", runs.Load())
	}
}

func TestStartSchedulerRestartAfterRemoteCommitResendsThenDeletes(t *testing.T) {
	dir := t.TempDir()
	pending := writePendingArchive(t, dir, "sherpa-committed.tar.gz", "archive", time.Now())
	originalRun, originalUpload := runExport, uploadArchive
	t.Cleanup(func() { runExport, uploadArchive = originalRun, originalUpload })
	var runs atomic.Int32
	resent := make(chan string, 1)
	runExport = func(context.Context, string, string, string) error {
		runs.Add(1)
		return nil
	}
	uploadArchive = func(_ context.Context, path, _, _ string) (UploadResult, error) {
		resent <- path
		return validatedUploadResult("existing"), nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := startTestScheduler(ctx, schedulerTestConfig(dir, time.Hour))
	select {
	case got := <-resent:
		if got != pending {
			t.Fatalf("resent path = %q, want %q", got, pending)
		}
	case <-time.After(time.Second):
		t.Fatal("restart did not resend committed archive")
	}
	waitForPathRemoval(t, pending)
	cancel()
	waitForScheduler(t, done)
	if runs.Load() != 0 {
		t.Fatalf("export runs = %d, want 0", runs.Load())
	}
}

func TestStartSchedulerPreservesAllArchivesAfterMidQueueFailure(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	first := writePendingArchive(t, dir, "sherpa-first.tar.gz", "first", now.Add(-3*time.Hour))
	second := writePendingArchive(t, dir, "sherpa-second.tar.gz", "second", now.Add(-2*time.Hour))
	third := writePendingArchive(t, dir, "sherpa-third.tar.gz", "third", now.Add(-time.Hour))
	originalRun, originalUpload := runExport, uploadArchive
	t.Cleanup(func() { runExport, uploadArchive = originalRun, originalUpload })
	uploaded := make(chan string, 2)
	uploadArchive = func(_ context.Context, path, _, _ string) (UploadResult, error) {
		uploaded <- filepath.Base(path)
		if filepath.Base(path) == filepath.Base(second) {
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

func TestStartSchedulerLogsQueueCountSafeBasenameAgeAndRetryClassOnly(t *testing.T) {
	dir := t.TempDir()
	writePendingArchive(t, dir, "sherpa-safe.tar.gz", "archive", time.Now().Add(-2*time.Hour))
	originalRun, originalUpload := runExport, uploadArchive
	t.Cleanup(func() { runExport, uploadArchive = originalRun, originalUpload })
	attempted := make(chan struct{}, 1)
	uploadArchive = func(context.Context, string, string, string) (UploadResult, error) {
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
	for _, safe := range []string{"queue_count=1", "archive=sherpa-safe.tar.gz", "age=", "retry_class=upload"} {
		if !strings.Contains(got, safe) {
			t.Fatalf("scheduler log %q does not contain %q", got, safe)
		}
	}
	for _, secret := range []string{dir, "db-secret", "collector-secret", "private=query", "transport-secret", "collector.example"} {
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

func schedulerTestConfig(archiveDir string, interval time.Duration) SchedulerConfig {
	return SchedulerConfig{
		ContentDir: testrunContentDir(archiveDir), DatabaseURL: "postgres://db.internal/sherpa",
		ArchiveDir: archiveDir, CollectorURL: "https://collector.example/upload",
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
