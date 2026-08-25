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
	t.Setenv("SHERPA_CODEX_DIR", filepath.Join(t.TempDir(), "missing-codex"))

	var out, errb bytes.Buffer
	if code := Run([]string{"init", "--primary-harness", "claude-code"}, &out, &errb); code == 0 {
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
	t.Setenv("SHERPA_CODEX_DIR", filepath.Join(t.TempDir(), "missing-codex"))
	os.MkdirAll(filepath.Join(src, ".claude"), 0o755)
	os.WriteFile(filepath.Join(src, ".claude", "CLAUDE.md"), []byte("# me"), 0o644)
	// setup-state sibling file
	os.WriteFile(filepath.Join(src, ".claude.json"), []byte(`{"hasCompletedOnboarding":true,"projects":{}}`), 0o600)

	var out, errb bytes.Buffer
	if code := Run([]string{"init", "--primary-harness", "claude-code"}, &out, &errb); code != 0 {
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
	t.Setenv("SHERPA_CODEX_DIR", filepath.Join(t.TempDir(), "missing-codex"))
	os.WriteFile(claudeDir+".json", []byte(`{"hasCompletedOnboarding":true}`), 0o600)

	out, errb, err := runInitWithInput(home, "yes\n")
	if err != nil {
		t.Fatalf("init: %v\nstdout:\n%s\nstderr:\n%s", err, out, errb)
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

func TestInitRefusesExistingUntrackedBaselineDirWithoutDeletingIt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SHERPA_HOME", home)
	claudeDir := fixtureConfigDir(t, "claude", map[string]string{
		"CLAUDE.md": "# claude\n",
	})
	t.Setenv("SHERPA_CLAUDE_DIR", claudeDir)
	t.Setenv("SHERPA_CODEX_DIR", filepath.Join(t.TempDir(), "missing-codex"))

	mineDir := filepath.Join(home, "profiles", "mine")
	if err := os.MkdirAll(mineDir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(mineDir, "marker.txt")
	if err := os.WriteFile(marker, []byte("pre-existing user data\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := Run([]string{"init", "--primary-harness", "claude-code"}, &out, &errb); code == 0 {
		t.Fatal("init must fail when profiles/mine already exists outside state")
	}
	if !strings.Contains(errb.String(), "profile directory already exists") ||
		!strings.Contains(errb.String(), mineDir) ||
		!strings.Contains(errb.String(), "refusing to overwrite") {
		t.Fatalf("init error did not explain existing baseline dir: %q", errb.String())
	}
	if _, err := os.Stat(mineDir); err != nil {
		t.Fatalf("existing profile dir was removed: %v", err)
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "pre-existing user data\n" {
		t.Fatalf("marker was not preserved: got %q err %v", got, err)
	}
}

func TestInitExplicitPrimaryImportsEveryDetectedHarnessWithoutRenamingMine(t *testing.T) {
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
	if code := Run([]string{"init", "--primary-harness", "claude-code"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}

	st := loadTestState(t, home)
	if st.Active != "mine" {
		t.Fatalf("Active = %q, want mine", st.Active)
	}
	if got := st.Baselines["claude-code"]; got != "mine" {
		t.Fatalf("Baselines[claude-code] = %q, want mine (all baselines: %#v)", got, st.Baselines)
	}
	if got := st.Baselines["codex"]; got != "mine-codex" {
		t.Fatalf("Baselines[codex] = %q, want mine-codex (all baselines: %#v)", got, st.Baselines)
	}
	if p := st.Profiles["mine"]; p.Harness != "claude-code" {
		t.Fatalf("mine harness = %q, want claude-code", p.Harness)
	}
	if p := st.Profiles["mine-codex"]; p.Harness != "codex" {
		t.Fatalf("mine-codex harness = %q, want codex", p.Harness)
	}
	if _, err := os.Stat(filepath.Join(home, "profiles", "mine", "CLAUDE.md")); err != nil {
		t.Fatalf("primary claude baseline content missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "profiles", "mine-codex", "AGENTS.md")); err != nil {
		t.Fatalf("codex baseline content missing: %v", err)
	}
}

func TestInitRollbackKeepsEstablishedMineAfterLaterHarnessSaveFails(t *testing.T) {
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
	t.Setenv("SHERPA_CODEX_DIR", filepath.Join(t.TempDir(), "missing-codex"))
	os.WriteFile(claudeDir+".json", []byte(`{"hasCompletedOnboarding":true}`), 0o600)

	var out, errb bytes.Buffer
	if code := Run([]string{"init", "--primary-harness", "claude-code"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	t.Setenv("SHERPA_CODEX_DIR", codexDir)
	if err := os.MkdirAll(filepath.Join(home, "state.json.tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errb.Reset()
	if code := Run([]string{"init", "--harness", "codex"}, &out, &errb); code == 0 {
		t.Fatal("second-harness init must fail when final state save fails")
	}
	if _, err := os.Stat(filepath.Join(home, "profiles", "mine", "CLAUDE.md")); err != nil {
		t.Fatalf("rollback did not restore profiles/mine: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "profiles", "mine-codex")); !os.IsNotExist(err) {
		t.Fatalf("rollback left profiles/mine-codex behind: %v", err)
	}

	if err := os.RemoveAll(filepath.Join(home, "state.json.tmp")); err != nil {
		t.Fatal(err)
	}
	st := loadTestState(t, home)
	if st.Active != "mine" {
		t.Fatalf("Active = %q, want mine", st.Active)
	}
	if got := st.Baselines["claude-code"]; got != "mine" {
		t.Fatalf("Baselines[claude-code] = %q, want mine (all baselines: %#v)", got, st.Baselines)
	}
	if _, ok := st.Baselines["codex"]; ok {
		t.Fatalf("codex baseline recorded despite rollback: %#v", st.Baselines)
	}
	if _, ok := st.Profiles["mine"]; !ok {
		t.Fatalf("state lost mine profile: %#v", st.Profiles)
	}
	if _, ok := st.Profiles["mine-codex"]; ok {
		t.Fatalf("state recorded mine-codex despite rollback: %#v", st.Profiles)
	}
}

func TestInitExistingCodexBaselineIsIdempotentAndMentionsRefresh(t *testing.T) {
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
	t.Setenv("SHERPA_CODEX_DIR", filepath.Join(t.TempDir(), "missing-codex"))
	os.WriteFile(claudeDir+".json", []byte(`{"hasCompletedOnboarding":true}`), 0o600)

	var out, errb bytes.Buffer
	if code := Run([]string{"init", "--primary-harness", "claude-code"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	t.Setenv("SHERPA_CODEX_DIR", codexDir)
	if code := Run([]string{"init", "--harness", "codex"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	errb.Reset()
	if code := Run([]string{"init", "--harness", "codex"}, &out, &errb); code != 0 {
		t.Fatalf("idempotent codex init failed: %s", errb.String())
	}
	if !strings.Contains(out.String(), "already initialized as profile \"mine-codex\"") || !strings.Contains(out.String(), "--refresh") {
		t.Fatalf("unexpected output: %q", out.String())
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
	t.Setenv("SHERPA_CODEX_DIR", filepath.Join(t.TempDir(), "missing-codex"))
	os.WriteFile(filepath.Join(src, ".claude.json"), []byte(`{"theme":"light"}`), 0o600)

	var out, errb bytes.Buffer
	if code := Run([]string{"init", "--primary-harness", "claude-code"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
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

func TestInitMultipleDetectedRequiresPrimaryChoiceAndRerunIsIdempotent(t *testing.T) {
	home := t.TempDir()
	claudeDir := fixtureConfigDir(t, "claude", map[string]string{"CLAUDE.md": "# claude\n"})
	codexDir := fixtureConfigDir(t, "codex", map[string]string{"AGENTS.md": "# codex\n"})
	t.Setenv("SHERPA_CLAUDE_DIR", claudeDir)
	t.Setenv("SHERPA_CODEX_DIR", codexDir)

	out, _, err := runInitWithInput(home, "codex\nyes\n")
	if err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	if !strings.Contains(out, "claude-code: "+claudeDir) || !strings.Contains(out, "codex: "+codexDir) {
		t.Fatalf("detected setups were not shown with paths:\n%s", out)
	}
	st := loadTestState(t, home)
	if st.Active != "mine" || st.Baselines["codex"] != "mine" || st.Baselines["claude-code"] != "mine-claude" {
		t.Fatalf("unexpected primary/baselines: active=%q baselines=%#v", st.Active, st.Baselines)
	}

	out, _, err = runInitWithInput(home, "")
	if err != nil {
		t.Fatalf("idempotent rerun: %v", err)
	}
	if !strings.Contains(out, "already initialized") {
		t.Fatalf("idempotent rerun output = %q", out)
	}
	st = loadTestState(t, home)
	if len(st.Profiles) != 2 || len(st.Baselines) != 2 {
		t.Fatalf("rerun duplicated state: profiles=%#v baselines=%#v", st.Profiles, st.Baselines)
	}
}

func TestInitDeclinedConfirmationLeavesSourceAndSherpaStateUntouched(t *testing.T) {
	home := t.TempDir()
	claudeDir := fixtureConfigDir(t, "claude", map[string]string{"CLAUDE.md": "original\n"})
	t.Setenv("SHERPA_CLAUDE_DIR", claudeDir)
	t.Setenv("SHERPA_CODEX_DIR", filepath.Join(t.TempDir(), "missing-codex"))

	if _, _, err := runInitWithInput(home, "no\n"); err == nil || !strings.Contains(err.Error(), "confirmation declined") {
		t.Fatalf("declined init error = %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(claudeDir, "CLAUDE.md")); err != nil || string(got) != "original\n" {
		t.Fatalf("source changed: content=%q err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(home, "state.json")); !os.IsNotExist(err) {
		t.Fatalf("declined init created state: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "profiles")); !os.IsNotExist(err) {
		t.Fatalf("declined init created profiles: %v", err)
	}
}

func TestInitLaterDiscoveryAddsTaggedBaselineWithoutRenamingMine(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SHERPA_HOME", home)
	claudeDir := fixtureConfigDir(t, "claude", map[string]string{"CLAUDE.md": "# claude\n"})
	codexDir := fixtureConfigDir(t, "codex", map[string]string{"AGENTS.md": "# codex\n"})
	t.Setenv("SHERPA_CLAUDE_DIR", claudeDir)
	t.Setenv("SHERPA_CODEX_DIR", filepath.Join(t.TempDir(), "missing-codex"))
	var out, errb bytes.Buffer
	if code := Run([]string{"init", "--primary-harness", "claude-code"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}

	t.Setenv("SHERPA_CODEX_DIR", codexDir)
	if _, _, err := runInitWithInput(home, "yes\n"); err != nil {
		t.Fatal(err)
	}
	st := loadTestState(t, home)
	if st.Active != "mine" || st.Baselines["claude-code"] != "mine" || st.Baselines["codex"] != "mine-codex" {
		t.Fatalf("later discovery renamed primary: active=%q baselines=%#v", st.Active, st.Baselines)
	}
	if _, err := os.Stat(filepath.Join(home, "profiles", "mine", "CLAUDE.md")); err != nil {
		t.Fatalf("canonical mine was not preserved: %v", err)
	}
}

func TestInitBatchImportRollsBackEveryCreatedProfile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod-based unreadable file test is Unix-specific")
	}
	home := t.TempDir()
	t.Setenv("SHERPA_HOME", home)
	claudeDir := fixtureConfigDir(t, "claude", map[string]string{"CLAUDE.md": "# claude\n"})
	codexDir := fixtureConfigDir(t, "codex", map[string]string{
		"AGENTS.md": "# codex\n",
		"blocked":   "cannot copy\n",
	})
	blocked := filepath.Join(codexDir, "blocked")
	if err := os.Chmod(blocked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o600) })
	t.Setenv("SHERPA_CLAUDE_DIR", claudeDir)
	t.Setenv("SHERPA_CODEX_DIR", codexDir)

	var out, errb bytes.Buffer
	if code := Run([]string{"init", "--primary-harness", "claude-code"}, &out, &errb); code == 0 {
		t.Fatal("batch import unexpectedly succeeded")
	}
	for _, name := range []string{"mine", "mine-codex"} {
		if _, err := os.Stat(filepath.Join(home, "profiles", name)); !os.IsNotExist(err) {
			t.Fatalf("rollback left %s behind: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(home, "state.json")); !os.IsNotExist(err) {
		t.Fatalf("failed batch import wrote state: %v", err)
	}
}

func runInitWithInput(home, input string, args ...string) (stdout, stderr string, err error) {
	var out, errOut bytes.Buffer
	ctx := &Ctx{Home: home, Stdout: &out, Stderr: &errOut, Stdin: strings.NewReader(input)}
	err = cmdInit(ctx, args)
	return out.String(), errOut.String(), err
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
