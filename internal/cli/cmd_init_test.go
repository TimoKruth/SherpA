package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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
