package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	registry "sherpa/internal/registry"
	"sherpa/internal/registry/api"
	registryauth "sherpa/internal/registry/auth"
	"sherpa/internal/registry/content"
	registryexport "sherpa/internal/registry/export"
	"sherpa/internal/registry/store"
)

type Config = registry.Config

func LoadConfig() (Config, error) {
	return registry.LoadConfig()
}

const (
	postgresOpenAttempts = 5
	postgresRetryDelay   = 250 * time.Millisecond
	shutdownTimeout      = 30 * time.Second
)

type postgresOpener func(context.Context, string, ...int) (*store.PostgresStore, error)
type retrySleeper func(context.Context, time.Duration) error

var validateExportScheduler = registryexport.ValidateSchedulerConfig
var startExportScheduler = registryexport.StartScheduler

func sleepWithContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func openPostgresWithRetry(ctx context.Context, dsn string, maxConns int, open postgresOpener, sleep retrySleeper) (*store.PostgresStore, error) {
	delay := postgresRetryDelay
	var lastErr error
	for attempt := 1; attempt <= postgresOpenAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		st, err := open(ctx, dsn, maxConns)
		if err == nil {
			return st, nil
		}
		lastErr = err
		if attempt == postgresOpenAttempts {
			break
		}
		log.Printf("open registry database: attempt %d/%d failed; retrying", attempt, postgresOpenAttempts)
		if err := sleep(ctx, delay); err != nil {
			return nil, err
		}
		if delay < 4*time.Second {
			delay *= 2
			if delay > 4*time.Second {
				delay = 4 * time.Second
			}
		}
	}
	// pgx redacts the password in its connection errors, so wrapping the last
	// cause is secret-safe and gives ops something to debug a dead boot with.
	return nil, fmt.Errorf("open registry database: retry limit reached after %d attempts: %w", postgresOpenAttempts, lastErr)
}

func run(ctx context.Context, cfg Config) (*http.Server, func(), error) {
	exportCfg := registryexport.SchedulerConfig{
		ContentDir: cfg.ContentDir, DatabaseURL: cfg.DatabaseURL,
		ArchiveDir: cfg.ExportArchiveDir, CollectorURL: cfg.ExportURL,
		Token: cfg.ExportToken, Interval: cfg.ExportInterval,
	}
	if err := validateExportScheduler(exportCfg); err != nil {
		return nil, nil, fmt.Errorf("configure off-site export: %w", err)
	}
	st, err := openPostgresWithRetry(ctx, cfg.DatabaseURL, cfg.DBMaxConns, store.OpenPostgres, sleepWithContext)
	if err != nil {
		return nil, nil, err
	}

	var cleanupOnce sync.Once
	schedulerCtx, stopScheduler := context.WithCancel(ctx)
	var schedulerDone chan struct{}
	cleanup := func() {
		cleanupOnce.Do(func() {
			stopScheduler()
			if schedulerDone != nil {
				<-schedulerDone
			}
			if err := st.Close(); err != nil {
				log.Printf("close registry store: %v", err)
			}
		})
	}

	cs := content.NewBareGit(cfg.ContentDir)
	if err := os.MkdirAll(cfg.ContentDir, 0o755); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("prepare registry content directory: %w", err)
	}
	removed, err := cs.CleanAbandonedStages()
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("clean abandoned content stages: %w", err)
	}
	if removed > 0 {
		log.Printf("removed %d abandoned content stage directories", removed)
	}
	if cfg.ExportURL != "" {
		schedulerDone = make(chan struct{})
		go func() {
			defer close(schedulerDone)
			if err := startExportScheduler(schedulerCtx, exportCfg); err != nil && schedulerCtx.Err() == nil {
				log.Printf("off-site export scheduler stopped: %v", err)
			}
		}()
	}
	github := registryauth.NewGitHubClientWithSecret(cfg.GitHubClientID, cfg.GitHubClientSecret)
	var ready atomic.Bool
	mux := http.NewServeMux()
	mux.Handle("GET /healthz", api.HealthHandler(&ready))
	mux.Handle("/", api.NewWithOptions(st, cs, cfg.Token, github, api.Options{
		TrustProxy:       cfg.TrustProxy,
		PublicBaseURL:    cfg.PublicBaseURL,
		WebPublicBaseURL: cfg.WebPublicBaseURL,
	}))
	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       2 * time.Minute,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
	ready.Store(true)
	return srv, cleanup, nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if handled, code := dispatch(ctx, os.Args[1:], os.Stdout, os.Stderr); handled {
		os.Exit(code)
	}
	if err := serve(ctx); err != nil {
		log.Fatal(err)
	}
}

func dispatch(ctx context.Context, args []string, stdout, stderr io.Writer) (bool, int) {
	if len(args) == 0 {
		return false, 0
	}
	switch args[0] {
	case "audit":
		if len(args) != 1 {
			fmt.Fprintln(stderr, "usage: registry audit")
			return true, 2
		}
		cfg, err := LoadConfig()
		if err != nil {
			fmt.Fprintln(stderr, err)
			return true, 2
		}
		return true, runAudit(ctx, cfg, stdout, stderr)
	case "export":
		if len(args) != 2 {
			fmt.Fprintln(stderr, "usage: registry export <archive-path>")
			return true, 2
		}
		cfg, err := LoadConfig()
		if err != nil {
			fmt.Fprintln(stderr, err)
			return true, 2
		}
		return true, runExportCommand(ctx, cfg, args[1], stdout, stderr)
	default:
		return false, 0
	}
}

func serve(ctx context.Context) error {
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}

	srv, cleanup, err := run(ctx, cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	serverErr := make(chan error, 1)
	go func() {
		serverErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serverErr:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve registry: %w", err)
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown registry server: %w", err)
		}
	}
	return nil
}
