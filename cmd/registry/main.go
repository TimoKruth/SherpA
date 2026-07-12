package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	registry "sherpa/internal/registry"
	"sherpa/internal/registry/api"
	registryauth "sherpa/internal/registry/auth"
	"sherpa/internal/registry/content"
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
	for attempt := 1; attempt <= postgresOpenAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		st, err := open(ctx, dsn, maxConns)
		if err == nil {
			return st, nil
		}
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
	return nil, errors.New("open registry database: retry limit reached")
}

func run(ctx context.Context, cfg Config) (*http.Server, func(), error) {
	st, err := openPostgresWithRetry(ctx, cfg.DatabaseURL, cfg.DBMaxConns, store.OpenPostgres, sleepWithContext)
	if err != nil {
		return nil, nil, err
	}

	cleanup := func() {
		if err := st.Close(); err != nil {
			log.Printf("close registry store: %v", err)
		}
	}

	cs := content.NewBareGit(cfg.ContentDir)
	github := registryauth.NewGitHubClient(cfg.GitHubClientID)
	var ready atomic.Bool
	mux := http.NewServeMux()
	mux.Handle("GET /healthz", api.HealthHandler(&ready))
	mux.Handle("/", api.New(st, cs, cfg.Token, github))
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
	if len(os.Args) > 1 && os.Args[1] == "audit" {
		cfg, err := LoadConfig()
		if err != nil {
			log.Print(err)
			os.Exit(2)
		}
		os.Exit(runAudit(ctx, cfg, os.Stdout, os.Stderr))
	}
	if err := serve(ctx); err != nil {
		log.Fatal(err)
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
