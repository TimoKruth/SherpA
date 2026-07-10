package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sherpa/internal/state"
)

func TestProfileSetupRunsFresh(t *testing.T) {
	home := setupHome(t) // existing helper; installs mine + jane
	// seed a stale .claude.json into jane so we can prove it is removed
	os.WriteFile(filepath.Join(home, "profiles", "jane", ".claude.json"), []byte(`{"stale":true}`), 0o600)
	bin, envOut := fakeClaude(t) // existing helper
	t.Setenv("SHERPA_CLAUDE_BIN", bin)

	var out, errb bytes.Buffer
	if code := Run([]string{"profile", "setup", "jane"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	// launched under jane
	got, _ := os.ReadFile(envOut)
	if strings.TrimSpace(string(got)) != filepath.Join(home, "profiles", "jane") {
		t.Fatalf("did not launch under jane: %q", got)
	}
	// stale seed was removed before launch (fresh onboarding)
	if b, _ := os.ReadFile(filepath.Join(home, "profiles", "jane", ".claude.json")); strings.Contains(string(b), "stale") {
		t.Fatal("stale .claude.json not cleared for fresh setup")
	}
	st, _ := state.Load(home)
	if st.Active != "mine" {
		t.Fatalf("profile setup changed active = %q", st.Active)
	}
}

func TestProfileSetupWithoutMineDoesNotExportCredentialsToCwd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SHERPA_HOME", home)
	janeDir := filepath.Join(home, "profiles", "jane")
	if err := os.MkdirAll(janeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	st, _ := state.Load(home)
	st.Active = "jane"
	st.Profiles["jane"] = state.Profile{Name: "jane", Path: janeDir, Origin: "https://x/jane.git", Harness: "claude-code"}
	if err := st.Save(home); err != nil {
		t.Fatal(err)
	}

	cwd := t.TempDir()
	t.Chdir(cwd)
	marker := filepath.Join(t.TempDir(), "security-called")
	securityBin := filepath.Join(t.TempDir(), "security")
	script := "#!/bin/sh\nprintf called > " + marker + "\nprintf '{\"accessToken\":\"real-token\"}'\n"
	if err := os.WriteFile(securityBin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHERPA_SECURITY_BIN", securityBin)
	claudeBin, _ := fakeClaude(t)
	t.Setenv("SHERPA_CLAUDE_BIN", claudeBin)

	var out, errb bytes.Buffer
	if code := Run([]string{"profile", "setup", "jane"}, &out, &errb); code == 0 {
		t.Fatal("profile setup without mine must fail")
	}
	if !strings.Contains(errb.String(), "no `mine` profile") {
		t.Fatalf("stderr = %q", errb.String())
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("security export was reached; marker err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(cwd, ".credentials.json")); !os.IsNotExist(err) {
		t.Fatalf("cwd .credentials.json exists; err=%v", err)
	}
	err := filepath.WalkDir(home, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() == ".credentials.json" {
			t.Fatalf("unexpected credential file under home: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
