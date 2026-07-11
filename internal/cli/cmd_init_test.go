package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"sherpa/internal/state"
)

func TestInitCleansUpDestinationWhenImportFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod-based unreadable file test is Unix-specific")
	}
	home := t.TempDir()
	t.Setenv("SHERPA_HOME", home)
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "CLAUDE.md"), []byte("# mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	unreadable := filepath.Join(source, "unreadable.txt")
	if err := os.WriteFile(unreadable, []byte("cannot copy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unreadable, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(unreadable, 0o600)
	})
	t.Setenv("SHERPA_CLAUDE_DIR", source)

	var out, errb bytes.Buffer
	if code := Run([]string{"init"}, &out, &errb); code == 0 {
		t.Fatal("init must fail when the source contains an unreadable file")
	}
	if _, err := os.Stat(filepath.Join(home, "profiles", "mine")); !os.IsNotExist(err) {
		t.Fatalf("failed init left destination behind: %v", err)
	}
	if !strings.Contains(errb.String(), "permission") && !strings.Contains(errb.String(), "denied") {
		t.Fatalf("init error did not describe unreadable source: %q", errb.String())
	}
}

func TestInitCapturesSetupState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SHERPA_HOME", home)
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "CLAUDE.md"), []byte("# me"), 0o644)
	t.Setenv("SHERPA_CLAUDE_DIR", filepath.Join(src, ".claude"))
	os.MkdirAll(filepath.Join(src, ".claude"), 0o755)
	os.WriteFile(filepath.Join(src, ".claude", "CLAUDE.md"), []byte("# me"), 0o644)
	// setup-state sibling file
	os.WriteFile(filepath.Join(src, ".claude.json"), []byte(`{"hasCompletedOnboarding":true,"projects":{}}`), 0o600)

	var out, errb bytes.Buffer
	if code := Run([]string{"init"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	cap := filepath.Join(home, "profiles", "mine", ".sherpa-setup.json")
	b, err := os.ReadFile(cap)
	if err != nil {
		t.Fatalf("setup state not captured: %v", err)
	}
	if !strings.Contains(string(b), "hasCompletedOnboarding") {
		t.Fatalf("captured blob wrong: %s", b)
	}
	fi, _ := os.Stat(cap)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("captured perms = %v, want 0600", fi.Mode().Perm())
	}
}

func TestInitDefaultCreatesBareMineBaseline(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SHERPA_HOME", home)
	claudeDir := fixtureConfigDir(t, "claude", map[string]string{
		"CLAUDE.md": "# claude\n",
	})
	t.Setenv("SHERPA_CLAUDE_DIR", claudeDir)
	os.WriteFile(claudeDir+".json", []byte(`{"hasCompletedOnboarding":true}`), 0o600)

	var out, errb bytes.Buffer
	if code := Run([]string{"init"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}

	st := loadTestState(t, home)
	if st.Active != "mine" {
		t.Fatalf("Active = %q, want mine", st.Active)
	}
	if got := st.Baselines["claude-code"]; got != "mine" {
		t.Fatalf("Baselines[claude-code] = %q, want mine (all baselines: %#v)", got, st.Baselines)
	}
	p, ok := st.Profiles["mine"]
	if !ok {
		t.Fatalf("profiles = %#v, want mine", st.Profiles)
	}
	if p.Harness != "claude-code" {
		t.Fatalf("mine harness = %q, want claude-code", p.Harness)
	}
	if _, err := os.Stat(filepath.Join(home, "profiles", "mine", "CLAUDE.md")); err != nil {
		t.Fatalf("profile content missing: %v", err)
	}
}

func TestInitCodexRenamesLoneClaudeMineAndCreatesTaggedBaseline(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SHERPA_HOME", home)
	claudeDir := fixtureConfigDir(t, "claude", map[string]string{
		"CLAUDE.md": "# claude\n",
	})
	codexDir := fixtureConfigDir(t, "codex", map[string]string{
		"AGENTS.md":   "# codex\n",
		"auth.json":   `{"tokens":{"access_token":"fixture"}}`,
		"config.toml": "model = \"gpt-5-codex\"\n",
	})
	t.Setenv("SHERPA_CLAUDE_DIR", claudeDir)
	t.Setenv("SHERPA_CODEX_DIR", codexDir)
	os.WriteFile(claudeDir+".json", []byte(`{"hasCompletedOnboarding":true}`), 0o600)

	var out, errb bytes.Buffer
	if code := Run([]string{"init"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	if code := Run([]string{"init", "--harness", "codex"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}

	st := loadTestState(t, home)
	if _, ok := st.Profiles["mine"]; ok {
		t.Fatalf("bare mine should be renamed, profiles = %#v", st.Profiles)
	}
	if st.Active != "mine-claude" {
		t.Fatalf("Active = %q, want mine-claude", st.Active)
	}
	if got := st.Baselines["claude-code"]; got != "mine-claude" {
		t.Fatalf("Baselines[claude-code] = %q, want mine-claude (all baselines: %#v)", got, st.Baselines)
	}
	if got := st.Baselines["codex"]; got != "mine-codex" {
		t.Fatalf("Baselines[codex] = %q, want mine-codex (all baselines: %#v)", got, st.Baselines)
	}
	if p := st.Profiles["mine-claude"]; p.Harness != "claude-code" {
		t.Fatalf("mine-claude harness = %q, want claude-code", p.Harness)
	}
	if p := st.Profiles["mine-codex"]; p.Harness != "codex" {
		t.Fatalf("mine-codex harness = %q, want codex", p.Harness)
	}
	if _, err := os.Stat(filepath.Join(home, "profiles", "mine-claude", "CLAUDE.md")); err != nil {
		t.Fatalf("renamed claude baseline content missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "profiles", "mine-codex", "AGENTS.md")); err != nil {
		t.Fatalf("codex baseline content missing: %v", err)
	}
}

func TestInitExistingCodexBaselineRequiresRefresh(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SHERPA_HOME", home)
	claudeDir := fixtureConfigDir(t, "claude", map[string]string{
		"CLAUDE.md": "# claude\n",
	})
	codexDir := fixtureConfigDir(t, "codex", map[string]string{
		"AGENTS.md": "# codex\n",
		"auth.json": `{"tokens":{"access_token":"fixture"}}`,
	})
	t.Setenv("SHERPA_CLAUDE_DIR", claudeDir)
	t.Setenv("SHERPA_CODEX_DIR", codexDir)
	os.WriteFile(claudeDir+".json", []byte(`{"hasCompletedOnboarding":true}`), 0o600)

	var out, errb bytes.Buffer
	if code := Run([]string{"init"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	if code := Run([]string{"init", "--harness", "codex"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	errb.Reset()
	if code := Run([]string{"init", "--harness", "codex"}, &out, &errb); code == 0 {
		t.Fatal("second codex init must fail without --refresh")
	}
	if !strings.Contains(errb.String(), "already initialized for codex") || !strings.Contains(errb.String(), "--refresh") {
		t.Fatalf("unexpected error: %q", errb.String())
	}
}

func TestInitRefreshRecaptures(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SHERPA_HOME", home)
	src := t.TempDir()
	cdir := filepath.Join(src, ".claude")
	os.MkdirAll(cdir, 0o755)
	os.WriteFile(filepath.Join(cdir, "CLAUDE.md"), []byte("# me"), 0o644)
	t.Setenv("SHERPA_CLAUDE_DIR", cdir)
	os.WriteFile(filepath.Join(src, ".claude.json"), []byte(`{"theme":"light"}`), 0o600)

	var out, errb bytes.Buffer
	Run([]string{"init"}, &out, &errb)
	// user logs in / changes theme afterward
	os.WriteFile(filepath.Join(src, ".claude.json"), []byte(`{"theme":"dark","hasCompletedOnboarding":true}`), 0o600)
	if code := Run([]string{"init", "--refresh"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	b, _ := os.ReadFile(filepath.Join(home, "profiles", "mine", ".sherpa-setup.json"))
	if !strings.Contains(string(b), "dark") {
		t.Fatalf("refresh did not re-capture: %s", b)
	}
}

func fixtureConfigDir(t *testing.T, name string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "."+name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for rel, content := range files {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func loadTestState(t *testing.T, home string) *state.State {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var st state.State
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	return &st
}
