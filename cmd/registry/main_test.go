package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	registryexport "sherpa/internal/registry/export"
	"sherpa/internal/registry/store"
)

type exportSchedulerFunc func(context.Context) error

func (run exportSchedulerFunc) Run(ctx context.Context) error {
	return run(ctx)
}

func (exportSchedulerFunc) Close() error {
	return nil
}

type recordingExportScheduler struct {
	run        func(context.Context) error
	close      func() error
	runCalls   atomic.Int32
	closeCalls atomic.Int32
}

func (scheduler *recordingExportScheduler) Run(ctx context.Context) error {
	scheduler.runCalls.Add(1)
	if scheduler.run == nil {
		return nil
	}
	return scheduler.run(ctx)
}

func (scheduler *recordingExportScheduler) Close() error {
	scheduler.closeCalls.Add(1)
	if scheduler.close == nil {
		return nil
	}
	return scheduler.close()
}

func TestRunBootsRegistryWithPostgres(t *testing.T) {
	dsn := store.StartPostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	contentDir := t.TempDir()
	abandonedStage := filepath.Join(contentDir, ".stage-abandoned")
	if err := os.MkdirAll(abandonedStage, 0o755); err != nil {
		t.Fatal(err)
	}
	stageFile := filepath.Join(contentDir, ".stage-file")
	if err := os.WriteFile(stageFile, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}

	srv, cleanup, err := run(ctx, Config{
		Port:           "0",
		DatabaseURL:    dsn,
		Token:          "test-token",
		ContentDir:     contentDir,
		GitHubClientID: "test-client",
	})
	if err != nil {
		t.Fatalf("run registry: %v", err)
	}
	t.Cleanup(cleanup)
	if _, err := os.Stat(abandonedStage); !os.IsNotExist(err) {
		t.Fatalf("abandoned stage stat error = %v, want not exist", err)
	}
	if _, err := os.Stat(stageFile); err != nil {
		t.Fatalf("stage-shaped file was removed: %v", err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://registry.test/v1/search", nil)
	srv.Handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}

	var body struct {
		Stacks []json.RawMessage `json:"stacks"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v; body %s", err, rr.Body.String())
	}
	if len(body.Stacks) != 0 {
		t.Fatalf("stacks len = %d, want 0: %#v", len(body.Stacks), body.Stacks)
	}

	health := httptest.NewRecorder()
	srv.Handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "http://registry.test/healthz", nil))
	if health.Code != http.StatusOK || health.Body.String() != "ok" {
		t.Fatalf("health status = %d, body = %q", health.Code, health.Body.String())
	}
	if srv.ReadHeaderTimeout == 0 || srv.ReadTimeout == 0 || srv.WriteTimeout == 0 || srv.IdleTimeout == 0 {
		t.Fatalf("server timeouts must all be non-zero: %#v", srv)
	}

	invalidContentDir := filepath.Join(t.TempDir(), "content-file")
	if err := os.WriteFile(invalidContentDir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	badServer, badCleanup, err := run(ctx, Config{
		Port:        "0",
		DatabaseURL: dsn,
		ContentDir:  invalidContentDir,
	})
	if err == nil || !strings.Contains(err.Error(), "prepare registry content directory") {
		t.Fatalf("invalid content run error = %v, want preparation failure", err)
	}
	if badServer != nil || badCleanup != nil {
		t.Fatal("invalid content run returned a server or cleanup function")
	}
}

func TestLoadConfigDefaultsAndEnv(t *testing.T) {
	t.Setenv("PORT", "")
	t.Setenv("DATABASE_URL", "postgres://example/sherpa")
	t.Setenv("SHERPA_REGISTRY_TOKEN", "secret")
	t.Setenv("SHERPA_CONTENT_DIR", "")
	t.Setenv("SHERPA_GITHUB_CLIENT_ID", "github-client")
	t.Setenv("SHERPA_PUBLIC_BASE_URL", "https://registry.example")
	t.Setenv("SHERPA_TRUST_PROXY", "true")
	t.Setenv("SHERPA_DB_MAX_CONNS", "12")
	t.Setenv("SHERPA_EXPORT_URL", "https://backups.example/upload")
	t.Setenv("SHERPA_EXPORT_TOKEN", "backup-secret")
	t.Setenv("SHERPA_EXPORT_INTERVAL", "6h")
	t.Setenv("SHERPA_EXPORT_ARCHIVE_DIR", "/tmp/sherpa-exports")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Port != "8080" {
		t.Fatalf("Port = %q, want 8080", cfg.Port)
	}
	if cfg.DatabaseURL != "postgres://example/sherpa" {
		t.Fatalf("DatabaseURL = %q", cfg.DatabaseURL)
	}
	if cfg.Token != "secret" {
		t.Fatalf("Token = %q", cfg.Token)
	}
	if cfg.ContentDir != "./registry-content" {
		t.Fatalf("ContentDir = %q, want ./registry-content", cfg.ContentDir)
	}
	if cfg.GitHubClientID != "github-client" {
		t.Fatalf("GitHubClientID = %q", cfg.GitHubClientID)
	}
	if cfg.PublicBaseURL != "https://registry.example" {
		t.Fatalf("PublicBaseURL = %q", cfg.PublicBaseURL)
	}
	if !cfg.TrustProxy {
		t.Fatal("TrustProxy = false, want true")
	}
	if cfg.DBMaxConns != 12 {
		t.Fatalf("DBMaxConns = %d, want 12", cfg.DBMaxConns)
	}
	if cfg.ExportURL != "https://backups.example/upload" || cfg.ExportToken != "backup-secret" || cfg.ExportInterval != 6*time.Hour || cfg.ExportArchiveDir != "/tmp/sherpa-exports" {
		t.Fatalf("export config = %#v", cfg)
	}
}

func TestLoadConfigOptionalDeploymentDefaults(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://example/sherpa")
	t.Setenv("SHERPA_PUBLIC_BASE_URL", "")
	t.Setenv("SHERPA_TRUST_PROXY", "")
	t.Setenv("SHERPA_DB_MAX_CONNS", "")
	t.Setenv("SHERPA_EXPORT_URL", "")
	t.Setenv("SHERPA_EXPORT_TOKEN", "")
	t.Setenv("SHERPA_EXPORT_INTERVAL", "")
	t.Setenv("SHERPA_EXPORT_ARCHIVE_DIR", "")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.PublicBaseURL != "" || cfg.TrustProxy || cfg.DBMaxConns != 0 || cfg.ExportURL != "" || cfg.ExportToken != "" || cfg.ExportInterval != 0 || cfg.ExportArchiveDir != "" {
		t.Fatalf("deployment defaults = %#v", cfg)
	}
}

func TestLoadConfigRejectsMalformedDeploymentValues(t *testing.T) {
	for _, tc := range []struct {
		name  string
		key   string
		value string
	}{
		{name: "trust proxy", key: "SHERPA_TRUST_PROXY", value: "sometimes"},
		{name: "max connections text", key: "SHERPA_DB_MAX_CONNS", value: "many"},
		{name: "max connections negative", key: "SHERPA_DB_MAX_CONNS", value: "-1"},
		{name: "export interval text", key: "SHERPA_EXPORT_INTERVAL", value: "daily"},
		{name: "export interval zero", key: "SHERPA_EXPORT_INTERVAL", value: "0s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://example/sherpa")
			t.Setenv("SHERPA_TRUST_PROXY", "")
			t.Setenv("SHERPA_DB_MAX_CONNS", "")
			t.Setenv("SHERPA_EXPORT_URL", "")
			t.Setenv("SHERPA_EXPORT_TOKEN", "")
			t.Setenv("SHERPA_EXPORT_INTERVAL", "")
			t.Setenv("SHERPA_EXPORT_ARCHIVE_DIR", "")
			t.Setenv(tc.key, tc.value)

			_, err := LoadConfig()
			if err == nil || !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("LoadConfig error = %v, want %s error", err, tc.key)
			}
		})
	}
}

func TestLoadConfigRejectsPartialExportConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, url, token, interval, archiveDir string
	}{
		{name: "URL only", url: "https://backups.example/upload"},
		{name: "interval only", interval: "1h"},
		{name: "token only", token: "secret"},
		{name: "archive directory only", archiveDir: t.TempDir()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://example/sherpa")
			t.Setenv("SHERPA_EXPORT_URL", tc.url)
			t.Setenv("SHERPA_EXPORT_TOKEN", tc.token)
			t.Setenv("SHERPA_EXPORT_INTERVAL", tc.interval)
			t.Setenv("SHERPA_EXPORT_ARCHIVE_DIR", tc.archiveDir)
			if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "SHERPA_EXPORT_URL") {
				t.Fatalf("LoadConfig error = %v, want partial export configuration error", err)
			}
		})
	}
}

func TestDispatchExportCreatesArchive(t *testing.T) {
	binDir := t.TempDir()
	pgDump := filepath.Join(binDir, "pg_dump")
	script := "#!/bin/sh\nwhile [ \"$#\" -gt 0 ]; do\n  if [ \"$1\" = \"--file\" ]; then shift; printf dump > \"$1\"; exit 0; fi\n  shift\ndone\nexit 1\n"
	if err := os.WriteFile(pgDump, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DATABASE_URL", "postgres://exporter:dispatch-secret@db.internal/sherpa")
	t.Setenv("SHERPA_CONTENT_DIR", t.TempDir())
	t.Setenv("SHERPA_EXPORT_URL", "")
	t.Setenv("SHERPA_EXPORT_TOKEN", "")
	t.Setenv("SHERPA_EXPORT_INTERVAL", "")
	t.Setenv("SHERPA_EXPORT_ARCHIVE_DIR", "")
	archivePath := filepath.Join(t.TempDir(), "manual.tar.gz")
	var stdout, stderr strings.Builder
	handled, code := dispatch(context.Background(), []string{"export", archivePath}, &stdout, &stderr)
	if !handled || code != 0 {
		t.Fatalf("dispatch handled=%v code=%d stdout=%q stderr=%q", handled, code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(archivePath); err != nil {
		t.Fatalf("export archive: %v", err)
	}
	if !strings.Contains(stdout.String(), archivePath) || stderr.Len() != 0 {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestDispatchExportUsageAndUnknownCommand(t *testing.T) {
	var stdout, stderr strings.Builder
	handled, code := dispatch(context.Background(), []string{"export"}, &stdout, &stderr)
	if !handled || code != 2 || !strings.Contains(stderr.String(), "usage: registry export") {
		t.Fatalf("dispatch handled=%v code=%d stderr=%q", handled, code, stderr.String())
	}
	handled, code = dispatch(context.Background(), []string{"unknown"}, io.Discard, io.Discard)
	if handled || code != 0 {
		t.Fatalf("unknown dispatch handled=%v code=%d", handled, code)
	}
}

func TestRunWiresPersistentExportArchiveDirectory(t *testing.T) {
	t.Parallel()

	stop := errors.New("stop before opening database")
	var got registryexport.SchedulerConfig
	ops := defaultRegistryRunOps()
	ops.validateExportScheduler = func(cfg registryexport.SchedulerConfig) error {
		got = cfg
		return stop
	}

	srv, cleanup, err := runWithOps(context.Background(), Config{
		DatabaseURL: "postgres://db.internal/sherpa", ContentDir: "/data/git",
		ExportURL: "https://collector.example/upload", ExportToken: "collector-token",
		ExportInterval: time.Hour, ExportArchiveDir: "/data/exports",
	}, ops)
	if !errors.Is(err, stop) {
		t.Fatalf("run error = %v, want validation sentinel", err)
	}
	if srv != nil || cleanup != nil {
		t.Fatal("validation failure returned server or cleanup")
	}
	if got.ArchiveDir != "/data/exports" || got.ContentDir != "/data/git" {
		t.Fatalf("scheduler directories = content %q archive %q", got.ContentDir, got.ArchiveDir)
	}
	if got.CollectorURL != "https://collector.example/upload" || got.Token != "collector-token" || got.Interval != time.Hour {
		t.Fatalf("scheduler config = %#v", got)
	}
}

func TestRunRejectsUnsafeExportQueueBeforeReadiness(t *testing.T) {
	dsn := store.StartPostgres(t)
	archiveDir := t.TempDir()
	if err := os.Chmod(archiveDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv, cleanup, err := run(ctx, Config{
		DatabaseURL: dsn, ContentDir: t.TempDir(), ExportURL: "https://backups.example/upload",
		ExportToken: "collector-secret", ExportInterval: time.Hour, ExportArchiveDir: archiveDir,
	})
	if cleanup != nil {
		defer cleanup()
	}
	if err == nil || !strings.Contains(err.Error(), "mode 0700") {
		t.Fatalf("unsafe queue run error = %v, want synchronous mode rejection", err)
	}
	if srv != nil || cleanup != nil {
		t.Fatal("unsafe queue run returned a ready server or cleanup function")
	}
}

func TestRunAcquiresExportQueueLockBeforeReadinessAndReleasesDuringCleanup(t *testing.T) {
	dsn := store.StartPostgres(t)
	archiveDir := t.TempDir()
	if err := os.Chmod(archiveDir, 0o700); err != nil {
		t.Fatal(err)
	}
	exportCfg := registryexport.SchedulerConfig{
		ContentDir: t.TempDir(), DatabaseURL: dsn, ArchiveDir: archiveDir,
		CollectorURL: "https://backups.example/upload", Token: "collector-secret", Interval: time.Hour,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	srv, cleanup, err := run(ctx, Config{
		DatabaseURL: dsn, ContentDir: exportCfg.ContentDir, ExportURL: exportCfg.CollectorURL,
		ExportToken: exportCfg.Token, ExportInterval: exportCfg.Interval, ExportArchiveDir: archiveDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if srv == nil || cleanup == nil {
		t.Fatal("run returned no ready server or cleanup function")
	}
	if contender, err := registryexport.PrepareScheduler(exportCfg); err == nil || contender != nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("contending scheduler = %v, error = %v, want in-use rejection", contender, err)
	}

	cleanup()
	fresh, err := registryexport.PrepareScheduler(exportCfg)
	if err != nil {
		t.Fatalf("prepare scheduler after cleanup: %v", err)
	}
	stopped, stop := context.WithCancel(context.Background())
	stop()
	if err := fresh.Run(stopped); err != nil {
		t.Fatalf("run scheduler after cleanup: %v", err)
	}
	if err := fresh.Close(); err != nil {
		t.Fatalf("close scheduler after cleanup test: %v", err)
	}
}

func TestRunLockContenderDoesNotCleanActiveContentStage(t *testing.T) {
	dsn := store.StartPostgres(t)
	contentDir := t.TempDir()
	archiveDir := t.TempDir()
	if err := os.Chmod(archiveDir, 0o700); err != nil {
		t.Fatal(err)
	}
	exportCfg := registryexport.SchedulerConfig{
		ContentDir: contentDir, DatabaseURL: dsn, ArchiveDir: archiveDir,
		CollectorURL: "https://backups.example/upload", Token: "collector-secret", Interval: time.Hour,
	}
	primary, err := registryexport.PrepareScheduler(exportCfg)
	if err != nil {
		t.Fatalf("prepare primary scheduler: %v", err)
	}
	defer func() {
		if err := primary.Close(); err != nil {
			t.Errorf("release primary scheduler: %v", err)
		}
	}()

	activeStage := filepath.Join(contentDir, ".stage-active")
	if err := os.Mkdir(activeStage, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(activeStage, "sentinel")
	if err := os.WriteFile(sentinel, []byte("active publish"), 0o600); err != nil {
		t.Fatal(err)
	}

	srv, cleanup, err := run(context.Background(), Config{
		DatabaseURL: dsn, ContentDir: contentDir, ExportURL: exportCfg.CollectorURL,
		ExportToken: exportCfg.Token, ExportInterval: exportCfg.Interval, ExportArchiveDir: archiveDir,
	})
	if err == nil || !strings.Contains(err.Error(), "export archive directory is already in use") {
		t.Fatalf("contending run error = %v, want queue lock contention", err)
	}
	if srv != nil || cleanup != nil {
		t.Fatal("contending run returned a server or cleanup function")
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "active publish" {
		t.Fatalf("active stage sentinel after contention = %q, %v; want preserved", data, err)
	}
}

func TestRegistrySchedulerFatalExitDropsReadinessAndKeepsLockUntilCleanup(t *testing.T) {
	dsn := store.StartPostgres(t)
	contentDir := t.TempDir()
	archiveDir := t.TempDir()
	if err := os.Chmod(archiveDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		DatabaseURL: dsn, ContentDir: contentDir, ExportURL: "https://backups.example/upload",
		ExportToken: "collector-secret", ExportInterval: time.Hour, ExportArchiveDir: archiveDir,
	}

	runStarted := make(chan struct{})
	allowFailure := make(chan struct{})
	terminal := errors.New("terminal scheduler failure")
	var scheduler *recordingExportScheduler
	ops := defaultRegistryRunOps()
	ops.prepareExportScheduler = func(exportCfg registryexport.SchedulerConfig) (exportScheduler, error) {
		prepared, err := registryexport.PrepareScheduler(exportCfg)
		if err != nil {
			return nil, err
		}
		scheduler = &recordingExportScheduler{
			run: func(context.Context) error {
				close(runStarted)
				<-allowFailure
				return terminal
			},
			close: prepared.Close,
		}
		return scheduler, nil
	}

	runtime, err := startRegistryWithOps(context.Background(), cfg, ops)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.cleanup()
	select {
	case <-runStarted:
	case <-time.After(time.Second):
		t.Fatal("scheduler did not start")
	}

	health := httptest.NewRecorder()
	runtime.server.Handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "http://registry.test/healthz", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("health before scheduler failure = %d, want 200", health.Code)
	}
	activeStage := filepath.Join(contentDir, ".stage-active-after-scheduler-failure")
	if err := os.Mkdir(activeStage, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(activeStage, "sentinel")
	if err := os.WriteFile(sentinel, []byte("active publish"), 0o600); err != nil {
		t.Fatal(err)
	}

	close(allowFailure)
	select {
	case err := <-runtime.schedulerFatal:
		if !errors.Is(err, terminal) {
			t.Fatalf("scheduler fatal error = %v, want terminal failure", err)
		}
	case <-time.After(time.Second):
		t.Fatal("scheduler fatal error was not delivered")
	}
	health = httptest.NewRecorder()
	runtime.server.Handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "http://registry.test/healthz", nil))
	if health.Code != http.StatusServiceUnavailable {
		t.Fatalf("health after scheduler failure = %d, want 503", health.Code)
	}

	contenderServer, contenderCleanup, err := run(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "export archive directory is already in use") {
		t.Fatalf("overlapping contender error = %v, want queue lock contention", err)
	}
	if contenderServer != nil || contenderCleanup != nil {
		t.Fatal("overlapping contender returned server or cleanup")
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "active publish" {
		t.Fatalf("active stage after terminal scheduler failure = %q, %v; want preserved", data, err)
	}

	runtime.cleanup()
	if scheduler.closeCalls.Load() != 1 {
		t.Fatalf("scheduler close calls = %d, want 1", scheduler.closeCalls.Load())
	}
	fresh, err := registryexport.PrepareScheduler(registryexport.SchedulerConfig{
		ContentDir: contentDir, DatabaseURL: dsn, ArchiveDir: archiveDir,
		CollectorURL: cfg.ExportURL, Token: cfg.ExportToken, Interval: cfg.ExportInterval,
	})
	if err != nil {
		t.Fatalf("prepare scheduler after registry cleanup: %v", err)
	}
	if err := fresh.Close(); err != nil {
		t.Fatalf("close fresh scheduler: %v", err)
	}
}

func TestRegistryStartupFailuresClosePreparedSchedulerWithoutRunningIt(t *testing.T) {
	dsn := store.StartPostgres(t)
	startupFailure := errors.New("deterministic startup failure")

	for _, tc := range []struct {
		name      string
		configure func(*registryRunOps, string)
		wantError string
	}{
		{
			name: "database open",
			configure: func(ops *registryRunOps, _ string) {
				ops.openPostgres = func(context.Context, string, ...int) (*store.PostgresStore, error) {
					return nil, startupFailure
				}
				ops.sleep = func(context.Context, time.Duration) error { return nil }
			},
			wantError: "open registry database",
		},
		{
			name: "content directory creation",
			configure: func(ops *registryRunOps, contentDir string) {
				ops.mkdirAll = func(path string, mode os.FileMode) error {
					if path == contentDir {
						return startupFailure
					}
					return os.MkdirAll(path, mode)
				}
			},
			wantError: "prepare registry content directory",
		},
		{
			name: "abandoned stage cleanup",
			configure: func(ops *registryRunOps, _ string) {
				ops.cleanAbandonedStages = func(string) (int, error) { return 0, startupFailure }
			},
			wantError: "clean abandoned content stages",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			contentDir := t.TempDir()
			archiveDir := t.TempDir()
			if err := os.Chmod(archiveDir, 0o700); err != nil {
				t.Fatal(err)
			}
			cfg := Config{
				DatabaseURL: dsn, ContentDir: contentDir, ExportURL: "https://backups.example/upload",
				ExportToken: "collector-secret", ExportInterval: time.Hour, ExportArchiveDir: archiveDir,
			}
			var scheduler *recordingExportScheduler
			ops := defaultRegistryRunOps()
			ops.prepareExportScheduler = func(exportCfg registryexport.SchedulerConfig) (exportScheduler, error) {
				prepared, err := registryexport.PrepareScheduler(exportCfg)
				if err != nil {
					return nil, err
				}
				scheduler = &recordingExportScheduler{close: prepared.Close}
				return scheduler, nil
			}
			tc.configure(&ops, contentDir)

			runtime, err := startRegistryWithOps(context.Background(), cfg, ops)
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("startup error = %v, want %q", err, tc.wantError)
			}
			if runtime != nil {
				t.Fatal("failed startup returned runtime")
			}
			if scheduler == nil {
				t.Fatal("startup failed before scheduler preparation")
			}
			if scheduler.runCalls.Load() != 0 || scheduler.closeCalls.Load() != 1 {
				t.Fatalf("scheduler lifecycle calls = run %d close %d, want 0 and 1", scheduler.runCalls.Load(), scheduler.closeCalls.Load())
			}
			fresh, err := registryexport.PrepareScheduler(registryexport.SchedulerConfig{
				ContentDir: contentDir, DatabaseURL: dsn, ArchiveDir: archiveDir,
				CollectorURL: cfg.ExportURL, Token: cfg.ExportToken, Interval: cfg.ExportInterval,
			})
			if err != nil {
				t.Fatalf("prepare after failed startup: %v", err)
			}
			if err := fresh.Close(); err != nil {
				t.Fatalf("close fresh scheduler: %v", err)
			}
		})
	}
}

func TestRunCleanupStopsExportSchedulerBeforeReturning(t *testing.T) {
	t.Parallel()

	dsn := store.StartPostgres(t)
	started := make(chan struct{})
	stopped := make(chan struct{})
	ops := defaultRegistryRunOps()
	ops.prepareExportScheduler = func(registryexport.SchedulerConfig) (exportScheduler, error) {
		return exportSchedulerFunc(func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			time.Sleep(10 * time.Millisecond)
			close(stopped)
			return nil
		}), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	srv, cleanup, err := runWithOps(ctx, Config{
		DatabaseURL: dsn, ContentDir: t.TempDir(), ExportURL: "https://backups.example/upload",
		ExportToken: "collector-secret", ExportInterval: time.Hour, ExportArchiveDir: t.TempDir(),
	}, ops)
	if err != nil {
		t.Fatal(err)
	}
	if srv == nil {
		t.Fatal("run returned no server")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("export scheduler did not start")
	}
	cleanup()
	select {
	case <-stopped:
	default:
		t.Fatal("cleanup returned before export scheduler stopped")
	}
	cleanup() // cleanup is intentionally idempotent.
}

func TestOpenPostgresWithRetry(t *testing.T) {
	var attempts int
	var delays []time.Duration
	wantStore := &store.PostgresStore{}
	open := func(_ context.Context, dsn string, maxConns ...int) (*store.PostgresStore, error) {
		attempts++
		if dsn != "test-dsn" || len(maxConns) != 1 || maxConns[0] != 7 {
			t.Fatalf("open args = %q, %v", dsn, maxConns)
		}
		if attempts < 3 {
			return nil, errors.New("temporarily unavailable")
		}
		return wantStore, nil
	}
	sleep := func(_ context.Context, delay time.Duration) error {
		delays = append(delays, delay)
		return nil
	}

	got, err := openPostgresWithRetry(context.Background(), "test-dsn", 7, open, sleep)
	if err != nil {
		t.Fatalf("openPostgresWithRetry: %v", err)
	}
	if got != wantStore || attempts != 3 {
		t.Fatalf("store = %p, attempts = %d", got, attempts)
	}
	wantDelays := []time.Duration{250 * time.Millisecond, 500 * time.Millisecond}
	if len(delays) != len(wantDelays) || delays[0] != wantDelays[0] || delays[1] != wantDelays[1] {
		t.Fatalf("delays = %v, want %v", delays, wantDelays)
	}
}

func TestRunRejectsUnsafeExportConfigurationBeforeOpeningDatabase(t *testing.T) {
	srv, cleanup, err := run(context.Background(), Config{
		DatabaseURL: "postgres://must-not-be-opened", ContentDir: t.TempDir(),
		ExportURL: "http://collector.example/upload", ExportToken: "collector-secret",
		ExportInterval: time.Hour, ExportArchiveDir: t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("run error = %v, want HTTPS collector error", err)
	}
	if srv != nil || cleanup != nil {
		t.Fatal("invalid export configuration returned server or cleanup")
	}
}

func TestOpenPostgresWithRetryStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	open := func(_ context.Context, _ string, _ ...int) (*store.PostgresStore, error) {
		attempts++
		return nil, errors.New("temporarily unavailable")
	}
	sleep := func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	}

	_, err := openPostgresWithRetry(ctx, "test-dsn", 0, open, sleep)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context canceled", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}

func TestLoadConfigRequiresDatabaseURL(t *testing.T) {
	t.Setenv("PORT", "")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("SHERPA_REGISTRY_TOKEN", "secret")
	t.Setenv("SHERPA_CONTENT_DIR", "")

	_, err := LoadConfig()
	if err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("LoadConfig error = %v, want DATABASE_URL error", err)
	}
}

func TestLoadConfigAllowsDisabledAdminToken(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://example/sherpa")
	t.Setenv("SHERPA_REGISTRY_TOKEN", "")
	t.Setenv("SHERPA_GITHUB_CLIENT_ID", "github-client")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Token != "" {
		t.Fatalf("Token = %q", cfg.Token)
	}
	if cfg.GitHubClientID != "github-client" {
		t.Fatalf("GitHubClientID = %q", cfg.GitHubClientID)
	}
}
