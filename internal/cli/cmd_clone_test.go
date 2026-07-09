package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"sherpa/internal/state"
)

func makeExpertRepo(t *testing.T, valid bool) string {
	d := t.TempDir()
	manifest := "name: jane-stack\nowner: \"@jane\"\nversion: 1\nharness: claude-code\nsummary: x\n"
	if !valid {
		manifest = strings.Replace(manifest, "claude-code", "pi", 1)
	}
	os.WriteFile(filepath.Join(d, "stack.yaml"), []byte(manifest), 0o644)
	os.WriteFile(filepath.Join(d, "CLAUDE.md"), []byte("# jane"), 0o644)
	os.WriteFile(filepath.Join(d, "settings.json"),
		[]byte(`{"mcpServers":{"gh":{"command":"npx"}}}`), 0o644)
	for _, a := range [][]string{{"init", "-b", "main"}, {"add", "-A"},
		{"-c", "user.email=j@x", "-c", "user.name=j", "commit", "-m", "v1"}, {"tag", "v1"}} {
		if out, err := exec.Command("git", append([]string{"-C", d}, a...)...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", a, out)
		}
	}
	return d
}

func TestCloneInstallsWithoutActivating(t *testing.T) {
	home := setupHome(t) // from Task 6 tests
	repo := makeExpertRepo(t, true)
	var out, errb bytes.Buffer
	if code := Run([]string{"clone", repo, "--name", "jane2"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	st, _ := state.Load(home)
	if st.Active != "mine" {
		t.Fatal("clone must not switch active profile")
	}
	p, ok := st.Profiles["jane2"]
	if !ok || p.Origin != repo {
		t.Fatalf("profile not recorded: %+v", st.Profiles)
	}
	b, _ := os.ReadFile(filepath.Join(p.Path, "settings.json"))
	if strings.Contains(string(b), "mcpServers") && strings.Contains(string(b), "npx") {
		t.Fatal("executables not quarantined on install")
	}
}

func TestCloneAbortsCleanlyOnInvalidStack(t *testing.T) {
	home := setupHome(t)
	repo := makeExpertRepo(t, false)
	var out, errb bytes.Buffer
	if code := Run([]string{"clone", repo, "--name", "bad"}, &out, &errb); code == 0 {
		t.Fatal("want failure")
	}
	if _, err := os.Stat(filepath.Join(home, "profiles", "bad")); err == nil {
		t.Fatal("partial install left behind (spec 6.2)")
	}
	st, _ := state.Load(home)
	if _, ok := st.Profiles["bad"]; ok {
		t.Fatal("state entry for failed install")
	}
}

// Expectation (e): the installed profile is on branch `local` sitting at the
// upstream head, and origin/main plus tags remain referenceable (diff/update
// in later tasks depend on this).
func TestCloneCreatesLocalBranchAtUpstreamHead(t *testing.T) {
	home := setupHome(t)
	repo := makeExpertRepo(t, true)
	var out, errb bytes.Buffer
	if code := Run([]string{"clone", repo, "--name", "jane3"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	dir := filepath.Join(home, "profiles", "jane3")

	branch := gitOut(t, dir, "rev-parse", "--abbrev-ref", "HEAD")
	if branch != "local" {
		t.Fatalf("branch = %q, want local", branch)
	}
	// local head == origin/main head (upstream head).
	if got, want := gitOut(t, dir, "rev-parse", "local"), gitOut(t, dir, "rev-parse", "origin/main"); got != want {
		t.Fatalf("local (%s) not at upstream head (%s)", got, want)
	}
	// The upstream tag survived the clone.
	if tags := gitOut(t, dir, "tag", "-l"); !strings.Contains(tags, "v1") {
		t.Fatalf("tags = %q, want v1", tags)
	}
}

// A stack that ships without the whitelist .gitignore gets one enforced on
// install so untracked runtime state can never be committed to the stack.
func TestCloneEnforcesGitignore(t *testing.T) {
	home := setupHome(t)
	repo := makeExpertRepo(t, true) // makeExpertRepo ships no .gitignore
	var out, errb bytes.Buffer
	if code := Run([]string{"clone", repo, "--name", "jane4"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	b, err := os.ReadFile(filepath.Join(home, "profiles", "jane4", ".gitignore"))
	if err != nil {
		t.Fatalf("no .gitignore enforced: %v", err)
	}
	if !strings.Contains(string(b), "!/stack.yaml") {
		t.Fatalf(".gitignore lacks whitelist form: %q", b)
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s", args, out)
	}
	return strings.TrimSpace(string(out))
}
