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
	var got manifest
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

func TestUploadSendsArchiveWithOnlyConfiguredAuthorization(t *testing.T) {
	payload := []byte("complete gzip bytes")
	path := filepath.Join(t.TempDir(), "archive.tar.gz")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	var received []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	}))
	defer server.Close()

	if err := Upload(context.Background(), path, server.URL+"/ingest?private=query", "collector-secret"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(received, payload) {
		t.Fatalf("uploaded payload = %q", received)
	}
	if err := Upload(context.Background(), path, "http://example.com/upload?private=query", "collector-secret"); err == nil {
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
	err := Upload(context.Background(), path, server.URL+"/upload?private=query", "collector-secret")
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

	err := Upload(context.Background(), path, source.URL, "collector-secret")
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
	uploadArchive = func(context.Context, string, string, string) error {
		uploads.Add(1)
		return errors.New("collector returned HTTP 503")
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
		CollectorURL: "https://collector.example", Interval: time.Hour,
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
		ContentDir: t.TempDir(), CollectorURL: "http://collector.example/upload", Interval: time.Hour,
	})
	if err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("insecure collector error = %v", err)
	}
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
