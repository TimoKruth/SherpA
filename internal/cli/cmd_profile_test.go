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
