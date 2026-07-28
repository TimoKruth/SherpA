package cli

import (
	"bytes"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSearchAcceptsRegisteredHarnessStacks(t *testing.T) {
	t.Setenv("SHERPA_HOME", t.TempDir())
	t.Setenv("SHERPA_INDEX_URL", searchIndexURL(t, map[string]any{
		"stacks": []map[string]any{
			{
				"ref":         "@jane/rust-reviewer",
				"name":        "rust-reviewer",
				"owner":       "@jane",
				"harness":     "claude-code",
				"summary":     "Rust code review and refactoring setup",
				"tags":        []string{"rust", "review"},
				"repo_url":    "https://example.invalid/jane/rust-reviewer.git",
				"version":     14,
				"forked_from": nil,
			},
			{
				"ref":         "@casey/rust-codex",
				"name":        "rust-codex",
				"owner":       "@casey",
				"harness":     "codex",
				"summary":     "Rust workflows for Codex",
				"tags":        []string{"rust"},
				"repo_url":    "https://example.invalid/casey/rust-codex.git",
				"version":     2,
				"forked_from": nil,
			},
		},
	}))

	var out, errb bytes.Buffer
	if code := Run([]string{"search", "rust"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, errb.String())
	}
	stdout := out.String()
	if !strings.Contains(stdout, "@jane/rust-reviewer  Rust code review and refactoring setup  (claude-code)") {
		t.Fatalf("stdout missing claude-code row:\n%s", stdout)
	}
	if !strings.Contains(stdout, "try: sherpa try https://example.invalid/jane/rust-reviewer.git") {
		t.Fatalf("stdout missing try hint:\n%s", stdout)
	}
	if !strings.Contains(stdout, "@casey/rust-codex  Rust workflows for Codex  (codex)") {
		t.Fatalf("stdout missing codex row:\n%s", stdout)
	}
}

func TestSearchEmptyResultExitsZero(t *testing.T) {
	t.Setenv("SHERPA_HOME", t.TempDir())
	t.Setenv("SHERPA_INDEX_URL", searchIndexURL(t, map[string]any{
		"stacks": []map[string]any{
			{
				"ref":         "@jane/rust-reviewer",
				"name":        "rust-reviewer",
				"owner":       "@jane",
				"harness":     "claude-code",
				"summary":     "Rust code review and refactoring setup",
				"tags":        []string{"rust", "review"},
				"repo_url":    "https://example.invalid/jane/rust-reviewer.git",
				"version":     14,
				"forked_from": nil,
			},
		},
	}))

	var out, errb bytes.Buffer
	if code := Run([]string{"search", "python"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, errb.String())
	}
	if got := strings.TrimSpace(out.String()); got != "no stacks found" {
		t.Fatalf("stdout = %q", out.String())
	}
}

func TestSearchSourcePrecedence(t *testing.T) {
	// An unconfigured install must still be able to search, so the build
	// default applies; an explicitly configured registry or Phase 1 index
	// still wins over it.
	for _, tc := range []struct {
		name         string
		registryEnv  string
		indexEnv     string
		wantRegistry string
		wantIndex    string
	}{
		{name: "nothing configured uses the build default", wantRegistry: DefaultRegistryURL},
		{name: "registry wins", registryEnv: "https://r.example", wantRegistry: "https://r.example"},
		{name: "registry wins over index", registryEnv: "https://r.example", indexEnv: "https://i.example", wantRegistry: "https://r.example"},
		{name: "index still reachable", indexEnv: "https://i.example", wantIndex: "https://i.example"},
		{name: "whitespace counts as unset", registryEnv: "  ", indexEnv: " ", wantRegistry: DefaultRegistryURL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registryURL, indexURL := searchSource(tc.registryEnv, tc.indexEnv)
			if registryURL != tc.wantRegistry || indexURL != tc.wantIndex {
				t.Fatalf("searchSource(%q, %q) = (%q, %q), want (%q, %q)",
					tc.registryEnv, tc.indexEnv, registryURL, indexURL, tc.wantRegistry, tc.wantIndex)
			}
		})
	}
}

func searchIndexURL(t *testing.T, body map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "index.json")
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	return (&url.URL{Scheme: "file", Path: path}).String()
}
