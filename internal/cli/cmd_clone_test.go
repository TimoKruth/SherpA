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
	if entries, _ := os.ReadDir(filepath.Join(home, "staging")); len(entries) != 0 {
		t.Fatalf("staging tree left behind after abort: %v", entries)
	}
	st, _ := state.Load(home)
	if _, ok := st.Profiles["bad"]; ok {
		t.Fatal("state entry for failed install")
	}
}

// Expectation (e): the installed profile is on branch `local` carrying exactly
// one sherpa quarantine commit on top of the upstream head, and origin/main plus
// tags remain referenceable (diff/update in later tasks depend on this, and a
// `git reset --hard local` must NOT resurrect the stripped capabilities).
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
	// The quarantine commit sits on top of the untouched upstream head: local's
	// PARENT is origin/main.
	if got, want := gitOut(t, dir, "rev-parse", "local^"), gitOut(t, dir, "rev-parse", "origin/main"); got != want {
		t.Fatalf("local parent (%s) not at upstream head (%s)", got, want)
	}
	// The stripping is committed, so a hard reset to local keeps it quarantined.
	if b := gitOut(t, dir, "show", "local:settings.json"); strings.Contains(b, "mcpServers") {
		t.Fatalf("committed settings.json still carries live capabilities: %s", b)
	}
	// The upstream tag survived the clone.
	if tags := gitOut(t, dir, "tag", "-l"); !strings.Contains(tags, "v1") {
		t.Fatalf("tags = %q, want v1", tags)
	}
}

// Cloning onto a name that already exists must fail without disturbing the
// installed profile (its tree and state entry).
func TestCloneOntoExistingNameFailsAndPreserves(t *testing.T) {
	home := setupHome(t)
	repo := makeExpertRepo(t, true)
	var out, errb bytes.Buffer
	if code := Run([]string{"clone", repo, "--name", "dup"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	before, _ := os.ReadFile(filepath.Join(home, "profiles", "dup", "settings.json"))

	out.Reset()
	errb.Reset()
	if code := Run([]string{"clone", repo, "--name", "dup"}, &out, &errb); code == 0 {
		t.Fatal("cloning onto an existing profile name must fail")
	}
	after, _ := os.ReadFile(filepath.Join(home, "profiles", "dup", "settings.json"))
	if string(before) != string(after) {
		t.Fatal("existing profile modified by a failed clone")
	}
	st, _ := state.Load(home)
	if _, ok := st.Profiles["dup"]; !ok {
		t.Fatal("existing state entry removed by a failed clone")
	}
}

// If state.Save fails after the atomic rename, the freshly installed tree is
// rolled back so the clone leaves no trace (spec §6.2). state.json.tmp as a
// directory forces the atomic write to fail while Load still reads the real
// state.json.
func TestCloneRollsBackWhenStateSaveFails(t *testing.T) {
	home := setupHome(t)
	if err := os.MkdirAll(filepath.Join(home, "state.json.tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	repo := makeExpertRepo(t, true)
	var out, errb bytes.Buffer
	if code := Run([]string{"clone", repo, "--name", "rb"}, &out, &errb); code == 0 {
		t.Fatal("clone must fail when state.Save fails")
	}
	if _, err := os.Stat(filepath.Join(home, "profiles", "rb")); err == nil {
		t.Fatal("post-rename Save failure left the installed tree behind")
	}
	os.RemoveAll(filepath.Join(home, "state.json.tmp")) // let Load read the untouched state
	st, _ := state.Load(home)
	if _, ok := st.Profiles["rb"]; ok {
		t.Fatal("state entry recorded despite Save failure")
	}
	if _, ok := st.Profiles["mine"]; !ok {
		t.Fatal("existing state clobbered by the failed clone")
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
