package main

import (
	"context"
	"encoding/json"
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

	handler, cleanup, err := run(ctx, Config{
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
	handler.ServeHTTP(rr, req)

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
}

func TestLoadConfigDefaultsAndEnv(t *testing.T) {
	t.Setenv("PORT", "")
	t.Setenv("DATABASE_URL", "postgres://example/sherpa")
	t.Setenv("SHERPA_REGISTRY_TOKEN", "secret")
	t.Setenv("SHERPA_CONTENT_DIR", "")
	t.Setenv("SHERPA_GITHUB_CLIENT_ID", "github-client")

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
