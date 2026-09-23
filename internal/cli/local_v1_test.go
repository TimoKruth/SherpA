package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sherpa/internal/state"
	"strings"
	"testing"
)

func TestBaselineLaunchIsDisposableAndBaselineCannotBeSaved(t *testing.T) {
	home, mine, _ := setupRunHome(t)
	st, _ := state.Load(home)
	st.Active = "mine"
	st.Save(home)
	os.WriteFile(filepath.Join(mine, "CLAUDE.md"), []byte("protected"), 0600)
	os.WriteFile(filepath.Join(mine, ".credentials.json"), []byte("protected-token"), 0600)
	marker := filepath.Join(t.TempDir(), "path")
	bin := filepath.Join(t.TempDir(), "agent")
	script := "#!/bin/sh\nprintf '%s' \"$CLAUDE_CONFIG_DIR\" > '" + marker + "'\nprintf changed > \"$CLAUDE_CONFIG_DIR/CLAUDE.md\"\nprintf refreshed > \"$CLAUDE_CONFIG_DIR/.credentials.json\"\n"
	os.WriteFile(bin, []byte(script), 0700)
	t.Setenv("SHERPA_CLAUDE_BIN", bin)
	var out, errout bytes.Buffer
	for _, args := range [][]string{{"run"}, {"try", "mine"}} {
		if Run(args, &out, &errout) != 0 {
			t.Fatal(errout.String())
		}
		b, _ := os.ReadFile(marker)
		if string(b) == mine {
			t.Fatal("launched inside protected baseline")
		}
		if _, err := os.Stat(string(b)); !os.IsNotExist(err) {
			t.Fatal("disposable config retained")
		}
	}
	b, _ := os.ReadFile(filepath.Join(mine, "CLAUDE.md"))
	if string(b) != "protected" {
		t.Fatal("baseline instructions changed")
	}
	b, _ = os.ReadFile(filepath.Join(mine, ".credentials.json"))
	if string(b) != "protected-token" {
		t.Fatal("baseline credentials changed")
	}
	if Run([]string{"save"}, &out, &errout) == 0 {
		t.Fatal("saved baseline")
	}
	if Run([]string{"profile", "setup", "mine"}, &out, &errout) == 0 {
		t.Fatal("reset baseline")
	}
}
func TestLocalCreateImportAndSafeNames(t *testing.T) {
	home, mine, _ := setupRunHome(t)
	os.WriteFile(filepath.Join(mine, "CLAUDE.md"), []byte("protected"), 0600)
	var out, errout bytes.Buffer
	if Run([]string{"profile", "create", "careful", "--from", "mine", "--instructions", "Explain first."}, &out, &errout) != 0 {
		t.Fatal(errout.String())
	}
	dir := filepath.Join(home, "profiles", "careful")
	b, _ := os.ReadFile(filepath.Join(dir, "CLAUDE.md"))
	if !strings.Contains(string(b), "protected") || !strings.Contains(string(b), "Explain first.") {
		t.Fatal(string(b))
	}
	st, _ := state.Load(home)
	if st.Active != "jane" {
		t.Fatal("create switched profile")
	}
	for _, name := range []string{"../escape", "mine-other", "mine", "a/b", ""} {
		if Run([]string{"profile", "create", name, "--from", "mine"}, &out, &errout) == 0 {
			t.Fatalf("accepted name %q", name)
		}
	}
	source := t.TempDir()
	os.WriteFile(filepath.Join(source, "settings.json"), []byte(`{"hooks":{"SessionStart":[]}}`), 0600)
	args := []string{"profile", "import", "external", "--path", source, "--harness", "claude-code"}
	if Run(args, &out, &errout) == 0 {
		t.Fatal("import did not require trust review")
	}
	if _, err := os.Stat(filepath.Join(home, "profiles", "external")); !os.IsNotExist(err) {
		t.Fatal("untrusted import created files")
	}
	if Run(append(args, "--trusted"), &out, &errout) != 0 {
		t.Fatal(errout.String())
	}
	// Local save/diff works independently of any remote repository.
	if Run([]string{"use", "careful"}, &out, &errout) != 0 {
		t.Fatal(errout.String())
	}
	os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("new rules"), 0600)
	out.Reset()
	if Run([]string{"diff"}, &out, &errout) != 0 || !strings.Contains(out.String(), "+new rules") {
		t.Fatal(errout.String(), out.String())
	}
	if Run([]string{"save", "-m", "local changes"}, &out, &errout) != 0 {
		t.Fatal(errout.String())
	}
}
func TestStateKeepsLegacyPrivateJournalWithoutUsingRegistry(t *testing.T) {
	home := t.TempDir()
	raw := `{"active":"mine","profiles":{"mine":{"name":"mine","harness":"codex","registry":{"registry_url":"http://127.0.0.1:1"}}},"registries":{"http://127.0.0.1:1":{"pending_follows":["@a/b"]}},"trials":[{"notes":"keep private"}]}`
	os.WriteFile(filepath.Join(home, "state.json"), []byte(raw), 0600)
	st, err := state.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Save(home); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(home, "state.json"))
	var got map[string]json.RawMessage
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(got["trials"], []byte("keep private")) || !bytes.Contains(got["registries"], []byte("@a/b")) {
		t.Fatal("legacy data discarded")
	}
	var out, errout bytes.Buffer
	ctx := &Ctx{Home: home, Stdout: &out, Stderr: &errout}
	if err := cmdStatus(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if errout.Len() != 0 {
		t.Fatalf("status tried a registry: %s", errout.String())
	}
}
