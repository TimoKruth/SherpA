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
