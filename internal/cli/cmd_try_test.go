package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sherpa/internal/state"
)

func TestTryInstalledProfileLaunchesWithoutChangingActive(t *testing.T) {
	home := setupHome(t)
	mine := filepath.Join(home, "profiles", "mine")
	jane := filepath.Join(home, "profiles", "jane")
	if err := os.MkdirAll(mine, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(jane, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mine, ".credentials.json"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin, envOut := fakeClaude(t)
	t.Setenv("SHERPA_CLAUDE_BIN", bin)

	var out, errb bytes.Buffer
	if code := Run([]string{"try", "jane"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	got, _ := os.ReadFile(envOut)
	if strings.TrimSpace(string(got)) != jane {
		t.Fatalf("CLAUDE_CONFIG_DIR = %q, want %q", got, jane)
	}
	st, _ := state.Load(home)
	if st.Active != "mine" {
		t.Fatalf("try changed active = %q", st.Active)
	}
}

func TestTryURLClonesReviewsAndLaunchesWithoutChangingActive(t *testing.T) {
	home := setupHome(t)
	mine := filepath.Join(home, "profiles", "mine")
	if err := os.MkdirAll(mine, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mine, ".credentials.json"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := makeExpertRepo(t, true)
	bin, envOut := fakeClaude(t)
	t.Setenv("SHERPA_CLAUDE_BIN", bin)

	var out, errb bytes.Buffer
	if code := Run([]string{"try", repo, "--name", "trial", "--approve-all"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	trial := filepath.Join(home, "profiles", "trial")
	got, _ := os.ReadFile(envOut)
	if strings.TrimSpace(string(got)) != trial {
		t.Fatalf("CLAUDE_CONFIG_DIR = %q, want %q", got, trial)
	}
	settings, _ := os.ReadFile(filepath.Join(trial, "settings.json"))
	if !strings.Contains(string(settings), "mcpServers") {
		t.Fatalf("try --approve-all left mcp quarantined: %s", settings)
	}
	st, _ := state.Load(home)
	if st.Active != "mine" {
		t.Fatalf("try changed active = %q", st.Active)
	}
}
