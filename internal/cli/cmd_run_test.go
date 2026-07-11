package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sherpa/internal/state"
)

// setupRunHome builds a home with real on-disk profile dirs for `mine` and an
// active profile `jane`, so run can link/launch against them.
func setupRunHome(t *testing.T) (home, mineDir, janeDir string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("SHERPA_HOME", home)
	mineDir = filepath.Join(home, "profiles", "mine")
	janeDir = filepath.Join(home, "profiles", "jane")
	if err := os.MkdirAll(mineDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(janeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	st, _ := state.Load(home)
	st.Active = "jane"
	st.Profiles["mine"] = state.Profile{Name: "mine", Path: mineDir, Harness: "claude-code"}
	st.Profiles["jane"] = state.Profile{Name: "jane", Path: janeDir, Origin: "https://x/jane.git", Harness: "claude-code"}
	st.Save(home)
	return home, mineDir, janeDir
}

// fakeClaude installs a fake `claude` that records $CLAUDE_CONFIG_DIR so the test
// can prove a launch occurred and under which profile.
func fakeClaude(t *testing.T) (bin, marker string) {
	t.Helper()
	dir := t.TempDir()
	marker = filepath.Join(dir, "launched.txt")
	bin = filepath.Join(dir, "claude")
	script := "#!/bin/sh\necho \"$CLAUDE_CONFIG_DIR\" > " + marker + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, marker
}

func fakeCodex(t *testing.T) (bin, marker string) {
	t.Helper()
	dir := t.TempDir()
	marker = filepath.Join(dir, "codex-launched.txt")
	bin = filepath.Join(dir, "codex")
	script := "#!/bin/sh\necho \"$CODEX_HOME\" > " + marker + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, marker
}

func TestRunLaunchesUnderActiveProfileWithLinkedCreds(t *testing.T) {
	_, mineDir, janeDir := setupRunHome(t)
	os.WriteFile(filepath.Join(mineDir, ".credentials.json"), []byte("secret"), 0o600)
	bin, marker := fakeClaude(t)
	t.Setenv("SHERPA_CLAUDE_BIN", bin)

	var out, errb bytes.Buffer
	if code := Run([]string{"run"}, &out, &errb); code != 0 {
		t.Fatalf("run failed: %s", errb.String())
	}
	got, _ := os.ReadFile(marker)
	if strings.TrimSpace(string(got)) != janeDir {
		t.Fatalf("launched under %q, want active profile %q", got, janeDir)
	}
	cred, err := os.ReadFile(filepath.Join(janeDir, ".credentials.json"))
	if err != nil || string(cred) != "secret" {
		t.Fatal("credentials not linked into active profile")
	}
}

func TestRunRequiresActiveHarnessBaselineWithoutFallingBackToMine(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SHERPA_HOME", home)
	mineDir := filepath.Join(home, "profiles", "mine")
	codexDir := filepath.Join(home, "profiles", "codex-stack")
	if err := os.MkdirAll(mineDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	st, _ := state.Load(home)
	st.Active = "codex-stack"
	st.Profiles["mine"] = state.Profile{Name: "mine", Path: mineDir, Harness: "claude-code"}
	st.Profiles["codex-stack"] = state.Profile{Name: "codex-stack", Path: codexDir, Harness: "codex"}
	st.Baselines["claude-code"] = "mine"
	if err := st.Save(home); err != nil {
		t.Fatal(err)
	}
	bin, marker := fakeCodex(t)
	t.Setenv("SHERPA_CODEX_BIN", bin)

	var out, errb bytes.Buffer
	if code := Run([]string{"run"}, &out, &errb); code == 0 {
		t.Fatal("run must fail when the active harness has no baseline")
	}
	if !strings.Contains(errb.String(), `no baseline for harness "codex"`) {
		t.Fatalf("stderr = %q", errb.String())
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("codex launched despite missing baseline; marker err=%v", err)
	}
}

func TestRunStillLaunchesWhenCredentialPrepFails(t *testing.T) {
	// mine has no .credentials.json and the keychain export fails; run must warn
	// and launch anyway.
	setupRunHome(t)
	failSec := filepath.Join(t.TempDir(), "security")
	os.WriteFile(failSec, []byte("#!/bin/sh\nexit 1\n"), 0o755)
	t.Setenv("SHERPA_SECURITY_BIN", failSec)
	bin, marker := fakeClaude(t)
	t.Setenv("SHERPA_CLAUDE_BIN", bin)

	var out, errb bytes.Buffer
	if code := Run([]string{"run"}, &out, &errb); code != 0 {
		t.Fatalf("run should not be fatal on credential failure: %s", errb.String())
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("claude was not launched despite credential prep failure")
	}
	if !strings.Contains(errb.String(), "warning") {
		t.Fatalf("expected a warning on stderr, got %q", errb.String())
	}
}

func TestRunSeedsSetupUnlessFreshSetup(t *testing.T) {
	_, mineDir, janeDir := setupRunHome(t)
	os.WriteFile(filepath.Join(mineDir, ".credentials.json"), []byte("secret"), 0o600)
	os.WriteFile(filepath.Join(mineDir, ".sherpa-setup.json"),
		[]byte(`{"hasCompletedOnboarding":true,"projects":{"x":1}}`), 0o600)
	bin, marker := fakeClaude(t)
	t.Setenv("SHERPA_CLAUDE_BIN", bin)

	var out, errb bytes.Buffer
	if code := Run([]string{"run"}, &out, &errb); code != 0 {
		t.Fatalf("run failed: %s", errb.String())
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("claude was not launched")
	}
	seed, err := os.ReadFile(filepath.Join(janeDir, ".claude.json"))
	if err != nil {
		t.Fatal("setup seed not written")
	}
	if !strings.Contains(string(seed), "hasCompletedOnboarding") || strings.Contains(string(seed), "projects") {
		t.Fatalf("seed not curated: %s", seed)
	}

	_, freshMine, freshJane := setupRunHome(t)
	os.WriteFile(filepath.Join(freshMine, ".credentials.json"), []byte("secret"), 0o600)
	os.WriteFile(filepath.Join(freshMine, ".sherpa-setup.json"), []byte(`{"theme":"dark"}`), 0o600)
	freshBin, freshMarker := fakeClaude(t)
	t.Setenv("SHERPA_CLAUDE_BIN", freshBin)

	out.Reset()
	errb.Reset()
	if code := Run([]string{"run", "--fresh-setup"}, &out, &errb); code != 0 {
		t.Fatalf("run --fresh-setup failed: %s", errb.String())
	}
	if _, err := os.Stat(freshMarker); err != nil {
		t.Fatal("claude was not launched with --fresh-setup")
	}
	if _, err := os.Stat(filepath.Join(freshJane, ".claude.json")); err == nil {
		t.Fatal("run --fresh-setup wrote setup seed")
	}
}
