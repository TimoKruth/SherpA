package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"sherpa/internal/registry/store"
)

func TestRunBootsRegistryWithPostgres(t *testing.T) {
	dsn := store.StartPostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	srv, cleanup, err := run(ctx, Config{
		Port:           "0",
		DatabaseURL:    dsn,
		Token:          "test-token",
		ContentDir:     t.TempDir(),
		GitHubClientID: "test-client",
	})
	if err != nil {
		t.Fatalf("run registry: %v", err)
	}
	t.Cleanup(cleanup)

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
}

func TestLoadConfigOptionalDeploymentDefaults(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://example/sherpa")
	t.Setenv("SHERPA_PUBLIC_BASE_URL", "")
	t.Setenv("SHERPA_TRUST_PROXY", "")
	t.Setenv("SHERPA_DB_MAX_CONNS", "")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.PublicBaseURL != "" || cfg.TrustProxy || cfg.DBMaxConns != 0 {
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://example/sherpa")
			t.Setenv("SHERPA_TRUST_PROXY", "")
			t.Setenv("SHERPA_DB_MAX_CONNS", "")
			t.Setenv(tc.key, tc.value)

			_, err := LoadConfig()
			if err == nil || !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("LoadConfig error = %v, want %s error", err, tc.key)
			}
		})
	}
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
