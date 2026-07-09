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

func TestSearchFiltersToClaudeCodeStacks(t *testing.T) {
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
	if strings.Contains(stdout, "@casey/rust-codex") || strings.Contains(stdout, "(codex)") {
		t.Fatalf("stdout included codex stack:\n%s", stdout)
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

func TestSearchRequiresIndexURL(t *testing.T) {
	t.Setenv("SHERPA_HOME", t.TempDir())
	t.Setenv("SHERPA_INDEX_URL", "")

	var out, errb bytes.Buffer
	if code := Run([]string{"search", "rust"}, &out, &errb); code == 0 {
		t.Fatal("want nonzero exit when SHERPA_INDEX_URL is unset")
	}
	if !strings.Contains(errb.String(), "SHERPA_INDEX_URL") {
		t.Fatalf("stderr = %q", errb.String())
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
