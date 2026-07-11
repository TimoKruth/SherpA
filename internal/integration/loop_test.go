package integration

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"sherpa/internal/cli"
	"sherpa/internal/state"
)

func TestFullPhase1Loop(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "sherpa-home")
	claudeDir := filepath.Join(root, "real-claude")
	t.Setenv("SHERPA_HOME", home)
	t.Setenv("SHERPA_CLAUDE_DIR", claudeDir)
	t.Setenv("SHERPA_SECURITY_BIN", "/usr/bin/false")

	if err := writeFile(filepath.Join(claudeDir, "CLAUDE.md"), "mine instructions\n", 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(claudeDir, "settings.json"), `{"theme":"mine","mcpServers":{}}`+"\n", 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(claudeDir, "skills", "mine", "SKILL.md"), "mine skill\n", 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(claudeDir, ".credentials.json"), "credential-copy\n", 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(claudeDir+".json", `{"hasCompletedOnboarding":true,"theme":"dark","projects":{"/tmp/project":{"trust":true}}}`+"\n", 0o600); err != nil {
		t.Fatal(err)
	}

	repo := makeExpertRepo(t, root)
	fakeClaude, marker := makeFakeClaude(t, root)
	t.Setenv("SHERPA_CLAUDE_BIN", fakeClaude)
	t.Setenv("SHERPA_FAKE_CLAUDE_MARKER", marker)

	profileName := "expert-loop"
	expertDir := filepath.Join(home, "profiles", profileName)
	var mineSnapshot treeSnapshot

	t.Run("init", func(t *testing.T) {
		out, errb, code := runCLI(t, nil, "init")
		if code != 0 {
			t.Fatalf("init failed: %s\nstdout:\n%s", errb, out)
		}
		st := loadState(t, home)
		if st.Active != "mine" {
			t.Fatalf("active = %q, want mine", st.Active)
		}
		mineSnapshot = snapshotTree(t, filepath.Join(home, "profiles", "mine"))
	})

	t.Run("clone expert v1 with approve all", func(t *testing.T) {
		out, errb, code := runCLI(t, nil, "clone", repo, "--name", profileName, "--review=approve-all")
		if code != 0 {
			t.Fatalf("clone failed: %s\nstdout:\n%s", errb, out)
		}
		settings := readFile(t, filepath.Join(expertDir, "settings.json"))
		if !strings.Contains(settings, `"mcpServers"`) || !strings.Contains(settings, `"hooks"`) {
			t.Fatalf("approve-all did not restore live capabilities:\n%s", settings)
		}
		if st := loadState(t, home); st.Active != "mine" {
			t.Fatalf("clone changed active profile to %q", st.Active)
		}
	})

	t.Run("try launches fake claude under expert config dir", func(t *testing.T) {
		out, errb, code := runCLI(t, nil, "try", profileName)
		if code != 0 {
			t.Fatalf("try failed: %s\nstdout:\n%s", errb, out)
		}
		got := strings.TrimSpace(readFile(t, marker))
		if got != expertDir {
			t.Fatalf("CLAUDE_CONFIG_DIR = %q, want %q", got, expertDir)
		}
		if st := loadState(t, home); st.Active != "mine" {
			t.Fatalf("try changed active profile to %q", st.Active)
		}
		assertCuratedSetupSeed(t, filepath.Join(expertDir, ".claude.json"))

		secondName := "expert-loop-two"
		secondDir := filepath.Join(home, "profiles", secondName)
		out, errb, code = runCLI(t, nil, "clone", repo, "--name", secondName, "--review=approve-all")
		if code != 0 {
			t.Fatalf("clone second profile failed: %s\nstdout:\n%s", errb, out)
		}
		out, errb, code = runCLI(t, nil, "try", secondName)
		if code != 0 {
			t.Fatalf("try second profile failed: %s\nstdout:\n%s", errb, out)
		}
		assertCuratedSetupSeed(t, filepath.Join(expertDir, ".claude.json"))
		assertCuratedSetupSeed(t, filepath.Join(secondDir, ".claude.json"))
		if st := loadState(t, home); st.Active != "mine" {
			t.Fatalf("second try changed active profile to %q", st.Active)
		}
		if err := os.Remove(filepath.Join(expertDir, ".claude.json")); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("modify installed profile and save", func(t *testing.T) {
		out, errb, code := runCLI(t, nil, "use", profileName)
		if code != 0 {
			t.Fatalf("use failed: %s\nstdout:\n%s", errb, out)
		}
		if err := writeFile(filepath.Join(expertDir, "skills", "review", "SKILL.md"), "expert review skill\nlocal addition\n", 0o644); err != nil {
			t.Fatal(err)
		}
		out, errb, code = runCLI(t, nil, "save", "-m", "local review customization")
		if code != 0 {
			t.Fatalf("save failed: %s\nstdout:\n%s", errb, out)
		}
		assertCleanGit(t, expertDir)
	})

	t.Run("update clean merge keeps local edits", func(t *testing.T) {
		publishUpstream(t, repo, "v2", "expert v2", map[string]string{
			"CHANGELOG.md":             "# Changelog\n\n## v2\n\n- adds upstream skill\n\n## v1\n\n- initial\n",
			"skills/upstream/SKILL.md": "upstream v2 skill\n",
		})
		out, errb, code := runCLI(t, "yes\n", "update", profileName)
		if code != 0 {
			t.Fatalf("update v2 failed: %s\nstdout:\n%s", errb, out)
		}
		if !strings.Contains(out, "adds upstream skill") {
			t.Fatalf("update output did not include v2 changelog:\n%s", out)
		}
		if got := readFile(t, filepath.Join(expertDir, "skills", "upstream", "SKILL.md")); got != "upstream v2 skill\n" {
			t.Fatalf("merged upstream skill = %q", got)
		}
		if got := readFile(t, filepath.Join(expertDir, "skills", "review", "SKILL.md")); !strings.Contains(got, "local addition") {
			t.Fatalf("local profile edit was not preserved:\n%s", got)
		}
		assertCleanGit(t, expertDir)
	})

	t.Run("conflicting update reports and preserves local sha", func(t *testing.T) {
		if err := writeFile(filepath.Join(expertDir, "CLAUDE.md"), "local conflict edit\n", 0o644); err != nil {
			t.Fatal(err)
		}
		out, errb, code := runCLI(t, nil, "save", "-m", "local claude conflict edit")
		if code != 0 {
			t.Fatalf("save conflict setup failed: %s\nstdout:\n%s", errb, out)
		}
		publishUpstream(t, repo, "v3", "expert v3", map[string]string{
			"CLAUDE.md":    "upstream conflict edit\n",
			"CHANGELOG.md": "# Changelog\n\n## v3\n\n- edits CLAUDE.md\n\n## v2\n\n- adds upstream skill\n",
		})
		before := git(t, expertDir, "rev-parse", "local")
		out, errb, code = runCLI(t, "yes\n", "update", profileName)
		if code == 0 {
			t.Fatalf("conflicting update unexpectedly succeeded:\n%s", out)
		}
		if !strings.Contains(out, "both you and upstream changed: CLAUDE.md") || !strings.Contains(out, "untouched") {
			t.Fatalf("conflict output missing expected explanation:\nstdout:\n%s\nstderr:\n%s", out, errb)
		}
		if after := git(t, expertDir, "rev-parse", "local"); after != before {
			t.Fatalf("local moved after aborted update: %s -> %s", before, after)
		}
		assertCleanGit(t, expertDir)
	})

	t.Run("back restores mine pointer and mine bytes", func(t *testing.T) {
		out, errb, code := runCLI(t, nil, "back")
		if code != 0 {
			t.Fatalf("back failed: %s\nstdout:\n%s", errb, out)
		}
		st := loadState(t, home)
		if st.Active != "mine" {
			t.Fatalf("active = %q, want mine", st.Active)
		}
		compareSnapshots(t, mineSnapshot, snapshotTree(t, filepath.Join(home, "profiles", "mine")))
	})

	t.Run("publish fork preserves lineage", func(t *testing.T) {
		out, errb, code := runCLI(t, nil, "use", profileName)
		if code != 0 {
			t.Fatalf("use before publish failed: %s\nstdout:\n%s", errb, out)
		}
		remote := filepath.Join(root, "fork.git")
		gitRaw(t, "init", "--bare", remote)
		out, errb, code = runCLI(t, "yes\nyes\n", "publish", "--remote", remote)
		if code != 0 {
			t.Fatalf("publish failed: %s\nstdout:\n%s", errb, out)
		}
		tags := strings.Fields(gitRaw(t, "--git-dir", remote, "tag", "-l", "v*"))
		if len(tags) != 1 {
			t.Fatalf("remote tags = %v, want exactly one published fork tag", tags)
		}
		stackYAML := gitRaw(t, "--git-dir", remote, "show", "refs/heads/main:stack.yaml")
		version := stackVersion(t, stackYAML)
		tagVersion, err := strconv.Atoi(strings.TrimPrefix(tags[0], "v"))
		if err != nil || version != tagVersion || version <= 3 {
			t.Fatalf("published tag/version mismatch: tags=%v stack version=%d", tags, version)
		}
		if !strings.Contains(stackYAML, `forked_from: "@mentor/base@v7"`) {
			t.Fatalf("remote stack.yaml lost fork lineage:\n%s", stackYAML)
		}
	})
}

func TestCodexHarnessRoundTrip(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "sherpa-home")
	claudeDir := filepath.Join(root, "real-claude")
	codexDir := filepath.Join(root, "real-codex")
	t.Setenv("SHERPA_HOME", home)
	t.Setenv("SHERPA_CLAUDE_DIR", claudeDir)
	t.Setenv("SHERPA_CODEX_DIR", codexDir)
	t.Setenv("SHERPA_SECURITY_BIN", "/usr/bin/false")

	if err := writeFile(filepath.Join(claudeDir, "CLAUDE.md"), "claude baseline\n", 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(claudeDir, ".credentials.json"), "claude-credential\n", 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(claudeDir+".json", `{"hasCompletedOnboarding":true}`+"\n", 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(codexDir, "AGENTS.md"), "codex baseline\n", 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(codexDir, "config.toml"), "model = \"gpt-5-codex\"\n", 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(codexDir, "auth.json"), `{"tokens":{"access_token":"baseline-token"}}`+"\n", 0o600); err != nil {
		t.Fatal(err)
	}

	fakeCodex, codexMarker := makeFakeCodex(t, root)
	t.Setenv("SHERPA_CODEX_BIN", fakeCodex)
	t.Setenv("SHERPA_FAKE_CODEX_MARKER", codexMarker)

	if out, errb, code := runCLI(t, nil, "init"); code != 0 {
		t.Fatalf("claude init failed: %s\nstdout:\n%s", errb, out)
	}
	if out, errb, code := runCLI(t, nil, "init", "--harness", "codex"); code != 0 {
		t.Fatalf("codex init failed: %s\nstdout:\n%s", errb, out)
	}

	st := loadState(t, home)
	if _, ok := st.Profiles["mine"]; ok {
		t.Fatalf("bare mine should have been renamed after codex init: %#v", st.Profiles)
	}
	if st.Active != "mine-claude" {
		t.Fatalf("Active = %q, want mine-claude", st.Active)
	}
	if got := st.Baselines["claude-code"]; got != "mine-claude" {
		t.Fatalf("claude baseline = %q, want mine-claude (all baselines: %#v)", got, st.Baselines)
	}
	if got := st.Baselines["codex"]; got != "mine-codex" {
		t.Fatalf("codex baseline = %q, want mine-codex (all baselines: %#v)", got, st.Baselines)
	}

	repo := makeCodexExpertRepo(t, root)
	profileName := "codex-loop"
	profileDir := filepath.Join(home, "profiles", profileName)
	if out, errb, code := runCLI(t, nil, "clone", repo, "--name", profileName, "--review=approve-all"); code != 0 {
		t.Fatalf("codex clone failed: %s\nstdout:\n%s", errb, out)
	}
	if st := loadState(t, home); st.Active != "mine-claude" {
		t.Fatalf("clone changed active profile to %q", st.Active)
	}

	if out, errb, code := runCLI(t, nil, "try", profileName); code != 0 {
		t.Fatalf("codex try failed: %s\nstdout:\n%s", errb, out)
	}
	if got := strings.TrimSpace(readFile(t, codexMarker)); got != profileDir {
		t.Fatalf("CODEX_HOME = %q, want %q", got, profileDir)
	}
	if got := readFile(t, filepath.Join(profileDir, "auth.json")); !strings.Contains(got, "baseline-token") {
		t.Fatalf("codex credentials were not linked from baseline:\n%s", got)
	}

	if out, errb, code := runCLI(t, nil, "use", profileName); code != 0 {
		t.Fatalf("use codex profile failed: %s\nstdout:\n%s", errb, out)
	}
	if out, errb, code := runCLI(t, nil, "back"); code != 0 {
		t.Fatalf("back from codex profile failed: %s\nstdout:\n%s", errb, out)
	}
	if st := loadState(t, home); st.Active != "mine-codex" {
		t.Fatalf("back active = %q, want mine-codex", st.Active)
	}

	if out, errb, code := runCLI(t, nil, "use", profileName); code != 0 {
		t.Fatalf("use codex profile for publish barrier failed: %s\nstdout:\n%s", errb, out)
	}
	if err := writeFile(filepath.Join(profileDir, "auth.json"), `{"tokens":{"access_token":"must-not-publish"}}`+"\n", 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, profileDir, "add", "-f", "auth.json")
	if out, errb, code := runCLI(t, nil, "save", "-m", "track codex auth fixture"); code != 0 {
		t.Fatalf("save tracked auth fixture failed: %s\nstdout:\n%s", errb, out)
	}
	remote := filepath.Join(root, "codex-fork.git")
	gitRaw(t, "init", "--bare", remote)
	out, errb, code := runCLI(t, "yes\nyes\n", "publish", "--remote", remote)
	if code == 0 {
		t.Fatalf("publish with tracked codex auth.json unexpectedly succeeded:\n%s", out)
	}
	if !strings.Contains(errb, "setup-state") && !strings.Contains(errb, "login") {
		t.Fatalf("publish error did not mention setup-state/login barrier:\n%s", errb)
	}
	if refs := gitRaw(t, "--git-dir", remote, "for-each-ref", "--format=%(refname)"); refs != "" {
		t.Fatalf("blocked codex publish pushed refs:\n%s", refs)
	}
}

type snapshotEntry struct {
	mode    os.FileMode
	content []byte
	isDir   bool
}

type treeSnapshot map[string]snapshotEntry

func snapshotTree(t *testing.T, root string) treeSnapshot {
	t.Helper()
	snap := treeSnapshot{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if entry.IsDir() && entry.Name() == ".git" {
			return filepath.SkipDir
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		item := snapshotEntry{mode: info.Mode().Perm(), isDir: entry.IsDir()}
		if !entry.IsDir() {
			item.content, err = os.ReadFile(path)
			if err != nil {
				return err
			}
		}
		snap[filepath.ToSlash(rel)] = item
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func compareSnapshots(t *testing.T, want, got treeSnapshot) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("mine tree entry count changed: got %d, want %d\nwant: %v\ngot: %v", len(got), len(want), keys(want), keys(got))
	}
	for rel, wantEntry := range want {
		gotEntry, ok := got[rel]
		if !ok {
			t.Fatalf("mine tree missing %s", rel)
		}
		if gotEntry.isDir != wantEntry.isDir || gotEntry.mode != wantEntry.mode || !bytes.Equal(gotEntry.content, wantEntry.content) {
			t.Fatalf("mine tree changed at %s", rel)
		}
	}
}

func makeExpertRepo(t *testing.T, root string) string {
	t.Helper()
	repo := filepath.Join(root, "expert")
	files := map[string]string{
		"stack.yaml": `name: expert-loop
owner: "@expert"
version: 1
harness: claude-code
summary: Phase 1 integration fixture
forked_from: "@mentor/base@v7"
executes:
  hooks:
    - path: hooks/check.sh
      event: PreToolUse
      purpose: "Checks writes"
  mcp_servers:
    - name: docs
      transport: stdio
      command: "npx @modelcontextprotocol/server-filesystem"
      purpose: "Reads docs"
`,
		"README.md":              "expert stack\n",
		"CHANGELOG.md":           "# Changelog\n\n## v1\n\n- initial\n",
		"CLAUDE.md":              "expert v1 instructions\n",
		"skills/review/SKILL.md": "expert review skill\n",
		"hooks/check.sh":         "#!/bin/sh\nexit 0\n",
		"settings.json":          `{"theme":"expert","hooks":{"PreToolUse":[{"matcher":"Write","hooks":[{"type":"command","command":"hooks/check.sh"}]}]},"mcpServers":{"docs":{"command":"npx","args":["-y","@modelcontextprotocol/server-filesystem"]}}}` + "\n",
	}
	for name, content := range files {
		mode := os.FileMode(0o644)
		if strings.HasPrefix(name, "hooks/") {
			mode = 0o755
		}
		if err := writeFile(filepath.Join(repo, name), content, mode); err != nil {
			t.Fatal(err)
		}
	}
	git(t, repo, "init", "-b", "main")
	git(t, repo, "add", "-A")
	git(t, repo, "-c", "user.email=expert.invalid", "-c", "user.name=expert", "-c", "commit.gpgsign=false", "commit", "-m", "v1")
	git(t, repo, "tag", "v1")
	return repo
}

func makeCodexExpertRepo(t *testing.T, root string) string {
	t.Helper()
	repo := filepath.Join(root, "codex-expert")
	files := map[string]string{
		"stack.yaml": `name: codex-loop
owner: "@codex-expert"
version: 1
harness: codex
summary: Codex integration fixture
`,
		"README.md":              "codex expert stack\n",
		"CHANGELOG.md":           "# Changelog\n\n## v1\n\n- initial\n",
		"AGENTS.md":              "codex expert instructions\n",
		"config.toml":            "model = \"gpt-5-codex\"\n",
		"rules/style.md":         "prefer direct answers\n",
		"skills/review/SKILL.md": "codex review skill\n",
	}
	for name, content := range files {
		if err := writeFile(filepath.Join(repo, name), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, repo, "init", "-b", "main")
	git(t, repo, "add", "-A")
	git(t, repo, "-c", "user.email=codex.invalid", "-c", "user.name=codex", "-c", "commit.gpgsign=false", "commit", "-m", "v1")
	git(t, repo, "tag", "v1")
	return repo
}

func publishUpstream(t *testing.T, repo, tag, msg string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		if err := writeFile(filepath.Join(repo, name), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, repo, "add", "-A")
	git(t, repo, "-c", "user.email=expert.invalid", "-c", "user.name=expert", "-c", "commit.gpgsign=false", "commit", "-m", msg)
	git(t, repo, "tag", tag)
}

func makeFakeClaude(t *testing.T, root string) (bin, marker string) {
	t.Helper()
	dir := filepath.Join(root, "fake-bin")
	bin = filepath.Join(dir, "claude")
	marker = filepath.Join(dir, "claude-config-dir.txt")
	script := "#!/bin/sh\nprintf '%s\\n' \"$CLAUDE_CONFIG_DIR\" > \"$SHERPA_FAKE_CLAUDE_MARKER\"\n"
	if err := writeFile(bin, script, 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, marker
}

func makeFakeCodex(t *testing.T, root string) (bin, marker string) {
	t.Helper()
	dir := filepath.Join(root, "fake-bin")
	bin = filepath.Join(dir, "codex")
	marker = filepath.Join(dir, "codex-home.txt")
	script := "#!/bin/sh\nprintf '%s\\n' \"$CODEX_HOME\" > \"$SHERPA_FAKE_CODEX_MARKER\"\n"
	if err := writeFile(bin, script, 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, marker
}

func runCLI(t *testing.T, stdin any, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errb bytes.Buffer
	if stdin != nil {
		f := stdinFile(t, fmt.Sprint(stdin))
		old := os.Stdin
		os.Stdin = f
		defer func() {
			os.Stdin = old
			_ = f.Close()
		}()
	}
	code = cli.Run(args, &out, &errb)
	return out.String(), errb.String(), code
}

func stdinFile(t *testing.T, content string) *os.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func loadState(t *testing.T, home string) *state.State {
	t.Helper()
	st, err := state.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func assertCleanGit(t *testing.T, dir string) {
	t.Helper()
	if status := git(t, dir, "status", "--porcelain"); status != "" {
		t.Fatalf("git tree dirty in %s:\n%s", dir, status)
	}
}

func assertCuratedSetupSeed(t *testing.T, path string) {
	t.Helper()
	setup := readFile(t, path)
	if !strings.Contains(setup, "hasCompletedOnboarding") {
		t.Fatalf("setup seed missing onboarding marker:\n%s", setup)
	}
	if strings.Contains(setup, "projects") {
		t.Fatalf("setup seed leaked projects:\n%s", setup)
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	all := append([]string{"-C", dir}, args...)
	return gitRaw(t, all...)
}

func gitRaw(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(path, content string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), mode)
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func stackVersion(t *testing.T, manifest string) int {
	t.Helper()
	for _, line := range strings.Split(manifest, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(key) != "version" {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			t.Fatalf("invalid stack version line %q", line)
		}
		return n
	}
	t.Fatalf("stack.yaml missing version:\n%s", manifest)
	return 0
}

func keys(m treeSnapshot) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
