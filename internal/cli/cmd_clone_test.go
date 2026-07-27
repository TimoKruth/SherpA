package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"sherpa/internal/harness"
	"sherpa/internal/state"
)

func makeExpertRepo(t *testing.T, valid bool) string {
	d := t.TempDir()
	manifest := "name: jane-stack\nowner: \"@jane\"\nversion: 1\nharness: claude-code\nsummary: x\nforked_from: '@origin/base@v1'\n"
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
	if p.Registry != nil {
		t.Fatalf("direct URL clone inferred registry identity: %#v", p.Registry)
	}
	b, _ := os.ReadFile(filepath.Join(p.Path, "settings.json"))
	if strings.Contains(string(b), "mcpServers") && strings.Contains(string(b), "npx") {
		t.Fatal("executables not quarantined on install")
	}
}

func TestRegistryCloneRecordsIdentityAndAutoFollows(t *testing.T) {
	repo := makeExpertRepo(t, true)
	var followStatus atomic.Int32
	followStatus.Store(http.StatusOK)
	var followCalls atomic.Int32
	srv := newCloneRegistryServer(t, repo, &followStatus, &followCalls)
	defer srv.Close()
	home := setupHome(t)
	t.Setenv("SHERPA_REGISTRY_URL", srv.URL+"/")
	if err := saveRegistrySession(home, srv.URL, "user-session", "bob"); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"clone", "@jane/jane-stack", "--name", "registry-clone"}, &out, &errOut); code != 0 {
		t.Fatalf("clone failed: %s", errOut.String())
	}
	st, _ := state.Load(home)
	profile := st.Profiles["registry-clone"]
	if profile.Registry == nil || profile.Registry.RegistryURL != srv.URL || profile.Registry.Owner != "jane" || profile.Registry.Stack != "jane-stack" || profile.Registry.Version != 1 {
		t.Fatalf("registry identity = %#v", profile.Registry)
	}
	if pending := st.Registries[srv.URL].PendingFollows; len(pending) != 0 {
		t.Fatalf("pending follows = %v", pending)
	}
	if followCalls.Load() != 1 {
		t.Fatalf("follow calls = %d", followCalls.Load())
	}
}

func TestRegistryCloneQueuesFailedFollowAndStatusRecovers(t *testing.T) {
	repo := makeExpertRepo(t, true)
	var followStatus atomic.Int32
	followStatus.Store(http.StatusServiceUnavailable)
	var followCalls atomic.Int32
	srv := newCloneRegistryServer(t, repo, &followStatus, &followCalls)
	defer srv.Close()
	home := setupHome(t)
	t.Setenv("SHERPA_REGISTRY_URL", srv.URL)
	if err := saveRegistrySession(home, srv.URL, "user-session", "bob"); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"clone", "@jane/jane-stack", "--name", "queued-clone"}, &out, &errOut); code != 0 {
		t.Fatalf("clone failed: %s", errOut.String())
	}
	st, _ := state.Load(home)
	if _, ok := st.Profiles["queued-clone"]; !ok {
		t.Fatal("follow failure rolled back installed profile")
	}
	if got := st.Registries[srv.URL].PendingFollows; len(got) != 1 || got[0] != "@jane/jane-stack" {
		t.Fatalf("pending follows = %v", got)
	}
	if !strings.Contains(errOut.String(), "remains queued") {
		t.Fatalf("warning = %q", errOut.String())
	}
	followStatus.Store(http.StatusOK)
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"status"}, &out, &errOut); code != 0 {
		t.Fatalf("status failed: %s", errOut.String())
	}
	st, _ = state.Load(home)
	if got := st.Registries[srv.URL].PendingFollows; len(got) != 0 {
		t.Fatalf("pending after recovery = %v", got)
	}
	if followCalls.Load() != 2 {
		t.Fatalf("follow calls = %d", followCalls.Load())
	}
}

func newCloneRegistryServer(t *testing.T, repo string, followStatus *atomic.Int32, followCalls *atomic.Int32) *httptest.Server {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, "jane", "jane-stack.git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	// Pin the initial branch: the content below is pushed to main, and without
	// this the bare repo's HEAD follows init.defaultBranch, so a machine
	// without that configured gets HEAD -> master. A clone then resolves HEAD
	// to a branch that does not exist and checks out nothing.
	gitOut(t, root, "init", "--bare", "-b", "main", bare)
	gitOut(t, repo, "push", bare, "main:main", "--tags")
	gitOut(t, bare, "update-server-info")
	files := http.StripPrefix("/v1/stacks", http.FileServer(http.Dir(root)))
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/v1/me/follows/jane/jane-stack":
			followCalls.Add(1)
			status := int(followStatus.Load())
			if status != http.StatusOK {
				w.WriteHeader(status)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"ref": "@jane/jane-stack", "owner": "jane", "name": "jane-stack", "latest_version": 1, "followed_at": "2026-07-13T12:00:00Z"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/me/updates":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"updates":[]}`))
		default:
			files.ServeHTTP(w, r)
		}
	}))
}

func TestCloneApproveAllFlagApprovesQuarantinedCapabilities(t *testing.T) {
	home := setupHome(t)
	repo := makeExpertRepo(t, true)
	var out, errb bytes.Buffer
	if code := Run([]string{"clone", repo, "--name", "trusted", "--approve-all"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	b, _ := os.ReadFile(filepath.Join(home, "profiles", "trusted", "settings.json"))
	if !strings.Contains(string(b), "mcpServers") || !strings.Contains(string(b), "npx") {
		t.Fatalf("approve-all did not restore mcpServers: %s", b)
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

func TestCloneOverwritesTamperedGitignore(t *testing.T) {
	home := setupHome(t)
	repo := makeExpertRepo(t, true)
	malicious := "*\n!/stack.yaml\n!/.credentials.json\n"
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(malicious), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, repo, "add", "-f", ".gitignore")
	gitOut(t, repo, "-c", "user.email=j@x", "-c", "user.name=j", "commit", "-m", "malicious gitignore")

	var out, errb bytes.Buffer
	if code := Run([]string{"clone", repo, "--name", "tampered-gitignore"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	b, err := os.ReadFile(filepath.Join(home, "profiles", "tampered-gitignore", ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	st, _ := state.Load(home)
	m := st.Profiles["tampered-gitignore"]
	h, err := harness.For(m.Harness)
	if err != nil {
		t.Fatal(err)
	}
	want := h.GitignoreContent()
	if string(b) != want {
		t.Fatalf(".gitignore = %q, want canonical %q", b, want)
	}
}

func TestCloneCredentialFileStaysIgnoredAfterTamperedGitignore(t *testing.T) {
	home := setupHome(t)
	repo := makeExpertRepo(t, true)
	malicious := "*\n!/stack.yaml\n!/.credentials.json\n"
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(malicious), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, repo, "add", "-f", ".gitignore")
	gitOut(t, repo, "-c", "user.email=j@x", "-c", "user.name=j", "commit", "-m", "malicious gitignore")

	var out, errb bytes.Buffer
	if code := Run([]string{"clone", repo, "--name", "credential-guard"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	dir := filepath.Join(home, "profiles", "credential-guard")
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(`{"token":"local-only"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	gitOut(t, dir, "add", "-A")
	if status := gitOut(t, dir, "status", "--porcelain"); strings.Contains(status, ".credentials.json") {
		t.Fatalf(".credentials.json appeared in git status:\n%s", status)
	}
	gitOut(t, dir, "check-ignore", ".credentials.json")
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s", args, out)
	}
	return strings.TrimSpace(string(out))
}
