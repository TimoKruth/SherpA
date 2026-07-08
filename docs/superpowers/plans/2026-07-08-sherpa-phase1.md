# SherpA Phase 1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship the `sherpa` CLI proving the Phase 1 loop — profile isolation, publish sanitization, install review gate, reversible switching — plus a static index site, per spec `docs/superpowers/specs/2026-07-08-follow-the-expert-design.md`.

**Architecture:** A single Go binary (`sherpa`) manages profiles under `~/.sherpa/` (env-overridable), launches Claude Code as a subprocess with `CLAUDE_CONFIG_DIR`, and treats every installed stack as a git clone (upstream = expert repo, `local` = user branch). Executables (hooks/MCP) are structurally quarantined out of live settings until approved. Registry in Phase 1 is just git URLs + a static `index.json`.

**Tech Stack:** Go ≥1.22 (stdlib only + `gopkg.in/yaml.v3`), system `git` via subprocess, vanilla HTML/JS for the static index site. No cobra, no go-git.

## Global Constraints

- Spec: `docs/superpowers/specs/2026-07-08-follow-the-expert-design.md` — re-read §3 and §6 before starting.
- **`mine` is sacred**: no code path may write into the user's real `~/.claude` (spec §6.1). All tests must use temp dirs; never touch the developer's real home.
- **Safety invariants** spec §6.1–6.5 each get an explicit test (Tasks 4, 9, 12, 13).
- All sherpa state lives under `$SHERPA_HOME` (default `~/.sherpa`); Claude config dir override env var is `CLAUDE_CONFIG_DIR`.
- Harness in Phase 1 is exactly `claude-code`; manifest validation rejects others (spec §10.5).
- Quarantine is structural: hook/MCP entries are *absent* from live `settings.json` until approved (spec §3.5).
- Publish is fail-closed: any secret-scan hit blocks publish, no override flag (spec §6.5).
- Fork provenance: `forked_from` is preserved verbatim through clone/save/publish (spec §10.8).
- Go: `gofmt`-clean, stdlib `testing`, table-driven tests where natural. Module name: `sherpa`.
- Commit after every task (at minimum); messages `feat:/fix:/test:/docs:/chore:`.

## Codex Delegation

Tasks marked **[CODEX]** are mechanical enough to delegate to GPT 5.5 via
`codex exec --sandbox workspace-write "<task text verbatim + paths + 'run the listed commands and make the tests pass'>"` from the repo root.
Rules for the executor (Claude):
1. Paste the *entire task section* into the codex prompt, plus the Global Constraints block.
2. After codex finishes, Claude reviews the diff (`git diff`), runs the task's test commands itself, and fixes or rejects before committing.
3. Tasks not marked [CODEX] involve design judgment or safety invariants — do them yourself.

---

### Task 1: Spike — verify `CLAUDE_CONFIG_DIR` coverage (day-1 gate)

The whole design rests on this (spec §3.2). Exploratory — no TDD. **Not delegable** (needs judgment about what the findings mean for the design).

**Files:**
- Create: `docs/superpowers/spikes/2026-07-XX-claude-config-dir.md`
- Create: `spike/claude-config-dir.sh`

**Interfaces:**
- Produces: the confirmed **allowlist** of stack-tracked paths and the **credential-link list**, consumed verbatim by Task 5 (`internal/stack/allowlist.go`).

- [ ] **Step 1: Write the probe script**

```bash
#!/usr/bin/env bash
# spike/claude-config-dir.sh — probe what CLAUDE_CONFIG_DIR isolates.
set -euo pipefail
DIR="$(mktemp -d)/sherpa-spike"
mkdir -p "$DIR"
echo "## Test marker: respond with the word SPIKE_MARKER_OK" > "$DIR/CLAUDE.md"
cat > "$DIR/settings.json" <<'EOF'
{ "env": { "SPIKE_SENTINEL": "1" } }
EOF
echo "--- before ---"; find "$DIR" -type f | sort
CLAUDE_CONFIG_DIR="$DIR" claude -p "What does your global CLAUDE.md instruct? Answer in one line." || true
echo "--- after ---"; find "$DIR" -type f | sort
echo "config dir: $DIR (inspect manually, esp. auth/state files)"
```

- [ ] **Step 2: Run it and record findings**

Run: `bash spike/claude-config-dir.sh`
Then inspect: does the reply reference SPIKE_MARKER_OK (CLAUDE.md read from override dir)? Which files did Claude create in `$DIR` (state, history, `.credentials.json`, `.claude.json`)? Did it require login (→ which file carries auth)? Repeat with a `skills/` dir and a `settings.json` hook to confirm skills+hooks load from the override dir.

- [ ] **Step 3: Write the findings doc**

`docs/superpowers/spikes/2026-07-XX-claude-config-dir.md` must answer, with observed evidence: (a) which of CLAUDE.md / settings.json / skills / agents / hooks / plugins load from `CLAUDE_CONFIG_DIR`; (b) which files hold credentials/OAuth (→ credential-link list); (c) which files are runtime noise (→ ignore list); (d) any gaps (things still read from `~/.claude` or `~/.claude.json`) and their impact on spec §3.2. **If isolation is materially broken, STOP and revise the spec before Task 2.**

- [ ] **Step 4: Commit**

```bash
git add spike/ docs/superpowers/spikes/
git commit -m "docs: CLAUDE_CONFIG_DIR isolation spike findings"
```

---

### Task 2: Project scaffold + command dispatcher **[CODEX]**

**Files:**
- Create: `go.mod`, `cmd/sherpa/main.go`, `internal/cli/cli.go`
- Test: `internal/cli/cli_test.go`

**Interfaces:**
- Produces: `cli.Run(args []string, stdout, stderr io.Writer) int` — every later command task registers in the `commands` map in `internal/cli/cli.go` with signature `func(ctx *cli.Ctx, args []string) error` where `Ctx` is `struct { Home string; Stdout, Stderr io.Writer; Stdin io.Reader }` (`Home` = resolved `$SHERPA_HOME`).

- [ ] **Step 1: Write the failing test**

```go
package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunUnknownCommand(t *testing.T) {
	var out, errb bytes.Buffer
	code := Run([]string{"nope"}, &out, &errb)
	if code == 0 {
		t.Fatal("want nonzero exit for unknown command")
	}
	if !strings.Contains(errb.String(), "unknown command") {
		t.Fatalf("stderr = %q", errb.String())
	}
}

func TestRunVersion(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run([]string{"version"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "sherpa") {
		t.Fatalf("stdout = %q", out.String())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go mod init sherpa && go test ./internal/cli/`
Expected: FAIL (Run undefined)

- [ ] **Step 3: Minimal implementation**

```go
// internal/cli/cli.go
package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const Version = "0.1.0-dev"

type Ctx struct {
	Home   string
	Stdout io.Writer
	Stderr io.Writer
	Stdin  io.Reader
}

type command func(ctx *Ctx, args []string) error

var commands = map[string]command{}

func register(name string, fn command) { commands[name] = fn }

func homeDir() string {
	if h := os.Getenv("SHERPA_HOME"); h != "" {
		return h
	}
	u, _ := os.UserHomeDir()
	return filepath.Join(u, ".sherpa")
}

func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: sherpa <command> [args]")
		return 2
	}
	if args[0] == "version" {
		fmt.Fprintf(stdout, "sherpa %s\n", Version)
		return 0
	}
	cmd, ok := commands[args[0]]
	if !ok {
		fmt.Fprintf(stderr, "sherpa: unknown command %q\n", args[0])
		return 2
	}
	ctx := &Ctx{Home: homeDir(), Stdout: stdout, Stderr: stderr, Stdin: os.Stdin}
	if err := cmd(ctx, args[1:]); err != nil {
		fmt.Fprintf(stderr, "sherpa %s: %v\n", args[0], err)
		return 1
	}
	return 0
}
```

```go
// cmd/sherpa/main.go
package main

import (
	"os"
	"sherpa/internal/cli"
)

func main() { os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr)) }
```

- [ ] **Step 4: Verify pass** — Run: `go test ./... && go build ./cmd/sherpa` → PASS, binary builds.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: sherpa CLI scaffold with command dispatcher"`

---

### Task 3: State store (`~/.sherpa/state.json`)

**Files:**
- Create: `internal/state/state.go`
- Test: `internal/state/state_test.go`

**Interfaces:**
- Produces (consumed by Tasks 4, 6, 7, 9, 12, 13):

```go
type Profile struct {
	Name    string `json:"name"`
	Path    string `json:"path"`     // absolute profile dir
	Origin  string `json:"origin"`   // git URL; "" for mine
	Harness string `json:"harness"`  // "claude-code"
}
type State struct {
	Active   string             `json:"active"` // profile name; "" until init
	Profiles map[string]Profile `json:"profiles"`
}
func Load(home string) (*State, error)   // missing file → empty State, no error
func (s *State) Save(home string) error  // atomic: temp file + rename
```

- [ ] **Step 1: Write the failing test**

```go
package state

import (
	"path/filepath"
	"testing"
)

func TestLoadMissingGivesEmpty(t *testing.T) {
	s, err := Load(t.TempDir())
	if err != nil || s.Active != "" || len(s.Profiles) != 0 {
		t.Fatalf("got %+v, %v", s, err)
	}
}

func TestSaveLoadRoundtrip(t *testing.T) {
	home := t.TempDir()
	s, _ := Load(home)
	s.Active = "mine"
	s.Profiles["mine"] = Profile{Name: "mine", Path: filepath.Join(home, "profiles", "mine"), Harness: "claude-code"}
	if err := s.Save(home); err != nil {
		t.Fatal(err)
	}
	s2, err := Load(home)
	if err != nil || s2.Active != "mine" || s2.Profiles["mine"].Harness != "claude-code" {
		t.Fatalf("roundtrip: %+v, %v", s2, err)
	}
}
```

- [ ] **Step 2: Verify fail** — `go test ./internal/state/` → FAIL.
- [ ] **Step 3: Implement**

```go
// internal/state/state.go
package state

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

type Profile struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Origin  string `json:"origin"`
	Harness string `json:"harness"`
}

type State struct {
	Active   string             `json:"active"`
	Profiles map[string]Profile `json:"profiles"`
}

func file(home string) string { return filepath.Join(home, "state.json") }

func Load(home string) (*State, error) {
	s := &State{Profiles: map[string]Profile{}}
	b, err := os.ReadFile(file(home))
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, s); err != nil {
		return nil, err
	}
	if s.Profiles == nil {
		s.Profiles = map[string]Profile{}
	}
	return s, nil
}

func (s *State) Save(home string) error {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := file(home) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, file(home))
}
```

- [ ] **Step 4: Verify pass** — `go test ./internal/state/` → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: atomic sherpa state store"`

---

### Task 4: `sherpa init` — import `~/.claude` as sacred `mine`

**Not delegable** (implements spec §6.1). Source dir comes from `$SHERPA_CLAUDE_DIR` (default `~/.claude`) so tests never touch the real one.

**Files:**
- Create: `internal/profile/importer.go`, `internal/cli/cmd_init.go`
- Test: `internal/profile/importer_test.go`

**Interfaces:**
- Consumes: `state` (Task 3), allowlist gitignore content (inline here, replaced by Task 5's constant later is NOT needed — importer takes it as a parameter).
- Produces: `profile.Import(src, dest, gitignore string) error` — recursive copy (files+dirs only, skip symlinks/sockets), write `.gitignore`, `git init` + initial commit on branch `local`.

- [ ] **Step 1: Write the failing test**

```go
package profile

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestImportCopiesAndInitsGit(t *testing.T) {
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "CLAUDE.md"), []byte("# rules"), 0o644)
	os.MkdirAll(filepath.Join(src, "skills", "x"), 0o755)
	os.WriteFile(filepath.Join(src, "skills", "x", "SKILL.md"), []byte("s"), 0o644)
	os.WriteFile(filepath.Join(src, "history.jsonl"), []byte("private"), 0o644)

	dest := filepath.Join(t.TempDir(), "mine")
	gi := "*\n!/.gitignore\n!/CLAUDE.md\n!/skills/\n!/skills/**\n"
	if err := Import(src, dest, gi); err != nil {
		t.Fatal(err)
	}
	// source untouched (sacred): still exactly 3 files
	// copied content present
	if _, err := os.Stat(filepath.Join(dest, "skills", "x", "SKILL.md")); err != nil {
		t.Fatal("skill not copied")
	}
	// git repo on branch local, tracked files exclude history.jsonl
	out, err := exec.Command("git", "-C", dest, "ls-files").Output()
	if err != nil {
		t.Fatal(err)
	}
	tracked := string(out)
	if strings.Contains(tracked, "history.jsonl") {
		t.Fatal("runtime noise got tracked")
	}
	if !strings.Contains(tracked, "CLAUDE.md") {
		t.Fatal("CLAUDE.md not tracked")
	}
	br, _ := exec.Command("git", "-C", dest, "branch", "--show-current").Output()
	if strings.TrimSpace(string(br)) != "local" {
		t.Fatalf("branch = %q", br)
	}
}

func TestImportRefusesExistingDest(t *testing.T) {
	dest := t.TempDir() // exists
	if err := Import(t.TempDir(), dest, "*\n"); err == nil {
		t.Fatal("want error on existing dest")
	}
}
```

- [ ] **Step 2: Verify fail** — `go test ./internal/profile/` → FAIL.
- [ ] **Step 3: Implement**

```go
// internal/profile/importer.go
package profile

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

func git(dir string, args ...string) error {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %v: %s: %w", args, out, err)
	}
	return nil
}

// Import copies src into a fresh dest, writes gitignore, inits git on branch "local".
// It never writes into src (spec §6.1: mine is sacred).
func Import(src, dest, gitignore string) error {
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("destination %s already exists", dest)
	}
	if err := copyTree(src, dest); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dest, ".gitignore"), []byte(gitignore), 0o644); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"init", "-b", "local"}, {"add", "-A"},
		{"-c", "user.email=sherpa@local", "-c", "user.name=sherpa", "commit", "-m", "sherpa: import"},
	} {
		if err := git(dest, args...); err != nil {
			return err
		}
	}
	return nil
}

func copyTree(src, dest string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dest, rel)
		switch {
		case info.IsDir():
			return os.MkdirAll(target, 0o755)
		case !info.Mode().IsRegular():
			return nil // skip symlinks/sockets
		default:
			in, err := os.Open(p)
			if err != nil {
				return err
			}
			defer in.Close()
			out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
			if err != nil {
				return err
			}
			defer out.Close()
			_, err = io.Copy(out, in)
			return err
		}
	})
}
```

```go
// internal/cli/cmd_init.go
package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sherpa/internal/profile"
	"sherpa/internal/state"
)

// gitignoreContent is temporary; Task 5 replaces it with stack.GitignoreContent
// (do not import internal/stack here yet — it doesn't exist until Task 5).
const gitignoreContent = `*
!/.gitignore
!/stack.yaml
!/README.md
!/CHANGELOG.md
!/CLAUDE.md
!/settings.json
!/keybindings.json
!/quarantine.json
!/skills/
!/skills/**
!/agents/
!/agents/**
!/hooks/
!/hooks/**
`

func init() { register("init", cmdInit) }

func claudeDir() string {
	if d := os.Getenv("SHERPA_CLAUDE_DIR"); d != "" {
		return d
	}
	u, _ := os.UserHomeDir()
	return filepath.Join(u, ".claude")
}

func cmdInit(ctx *Ctx, args []string) error {
	st, err := state.Load(ctx.Home)
	if err != nil {
		return err
	}
	if _, ok := st.Profiles["mine"]; ok {
		return fmt.Errorf("already initialized (profile 'mine' exists)")
	}
	dest := filepath.Join(ctx.Home, "profiles", "mine")
	if err := profile.Import(claudeDir(), dest, gitignoreContent); err != nil {
		return err
	}
	st.Profiles["mine"] = state.Profile{Name: "mine", Path: dest, Harness: "claude-code"}
	st.Active = "mine"
	if err := st.Save(ctx.Home); err != nil {
		return err
	}
	fmt.Fprintf(ctx.Stdout, "imported %s as profile 'mine' (your original config is untouched)\n", claudeDir())
	return nil
}
```

Note: Task 5 deletes the local `gitignoreContent` constant and switches this call to `stack.GitignoreContent`.

- [ ] **Step 4: Verify pass** — `go test ./...` → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: sherpa init imports ~/.claude as sacred mine profile"`

---

### Task 5: Manifest, allowlist, validation (spec §3.1)

**Files:**
- Create: `internal/stack/manifest.go`, `internal/stack/allowlist.go`
- Test: `internal/stack/manifest_test.go`

**Interfaces:**
- Consumes: allowlist findings from Task 1 spike (adjust the constants below to the spike's evidence).
- Produces (consumed by Tasks 8, 9, 11, 12):

```go
type Manifest struct { Name, Owner, Summary, Harness, ForkedFrom string; Version int; Tags []string; Executes Executes; Parameters []Parameter }
type Executes struct { Hooks []HookDecl; MCPServers []MCPDecl }
type HookDecl struct { Path, Event, Purpose string }
type MCPDecl struct { Name, Transport, Command, Purpose string }
func Parse(b []byte) (*Manifest, error)                 // YAML
func (m *Manifest) Validate(dir string) []string        // returns violations; empty = ok
const GitignoreContent string                            // whitelist-style ignore
```

- [ ] **Step 1: Failing tests** — valid manifest parses; `harness: pi` rejected ("phase 1 supports claude-code only"); undeclared hook file under `hooks/` rejected; hook declared but file missing rejected; non-empty `hooks`/`mcpServers` keys in `settings.json` rejected (must live in quarantine, see Task 8).

```go
package stack

import (
	"os"
	"path/filepath"
	"testing"
)

const goodYAML = `
name: rust-reviewer
owner: "@jane"
version: 14
harness: claude-code
summary: "Rust review setup"
tags: [rust]
executes:
  hooks:
    - path: hooks/check.sh
      event: PreToolUse
      purpose: "guard"
  mcp_servers: []
parameters: []
`

func writeStack(t *testing.T, withHookFile bool) string {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "stack.yaml"), []byte(goodYAML), 0o644)
	os.WriteFile(filepath.Join(d, "settings.json"), []byte(`{}`), 0o644)
	if withHookFile {
		os.MkdirAll(filepath.Join(d, "hooks"), 0o755)
		os.WriteFile(filepath.Join(d, "hooks", "check.sh"), []byte("#!/bin/sh"), 0o755)
	}
	return d
}

func TestParseAndValidateOK(t *testing.T) {
	m, err := Parse([]byte(goodYAML))
	if err != nil || m.Name != "rust-reviewer" || m.Version != 14 {
		t.Fatalf("%+v %v", m, err)
	}
	if v := m.Validate(writeStack(t, true)); len(v) != 0 {
		t.Fatalf("violations: %v", v)
	}
}

func TestValidateRejectsWrongHarness(t *testing.T) {
	m, _ := Parse([]byte(goodYAML))
	m.Harness = "pi"
	if v := m.Validate(writeStack(t, true)); len(v) == 0 {
		t.Fatal("want harness violation")
	}
}

func TestValidateRejectsUndeclaredHook(t *testing.T) {
	d := writeStack(t, true)
	os.WriteFile(filepath.Join(d, "hooks", "sneaky.sh"), []byte("#!/bin/sh"), 0o755)
	m, _ := Parse([]byte(goodYAML))
	if v := m.Validate(d); len(v) == 0 {
		t.Fatal("want undeclared-hook violation")
	}
}

func TestValidateRejectsMissingDeclaredHook(t *testing.T) {
	m, _ := Parse([]byte(goodYAML))
	if v := m.Validate(writeStack(t, false)); len(v) == 0 {
		t.Fatal("want missing-hook violation")
	}
}

func TestValidateRejectsLiveExecutablesInSettings(t *testing.T) {
	d := writeStack(t, true)
	os.WriteFile(filepath.Join(d, "settings.json"),
		[]byte(`{"hooks":{"PreToolUse":[{"command":"evil"}]}}`), 0o644)
	m, _ := Parse([]byte(goodYAML))
	if v := m.Validate(d); len(v) == 0 {
		t.Fatal("want live-executable violation")
	}
}
```

- [ ] **Step 2: Verify fail** — `go get gopkg.in/yaml.v3 && go test ./internal/stack/` → FAIL.
- [ ] **Step 3: Implement**

```go
// internal/stack/manifest.go
package stack

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type HookDecl struct {
	Path, Event, Purpose string
}
type MCPDecl struct {
	Name, Transport, Command, Purpose string
}
type Parameter struct {
	Name, Description string
	Required          bool
}
type Executes struct {
	Hooks      []HookDecl `yaml:"hooks"`
	MCPServers []MCPDecl  `yaml:"mcp_servers"`
}
type Manifest struct {
	Name       string    `yaml:"name"`
	Owner      string    `yaml:"owner"`
	Version    int       `yaml:"version"`
	Harness    string    `yaml:"harness"`
	Summary    string    `yaml:"summary"`
	ForkedFrom string    `yaml:"forked_from"`
	Tags       []string  `yaml:"tags"`
	Executes   Executes  `yaml:"executes"`
	Parameters []Parameter `yaml:"parameters"`
}

func Parse(b []byte) (*Manifest, error) {
	var m Manifest
	if err := yaml.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	if m.Name == "" || m.Version < 1 {
		return nil, fmt.Errorf("manifest needs name and version >= 1")
	}
	return &m, nil
}

// Validate checks the manifest against the stack directory contents.
func (m *Manifest) Validate(dir string) (violations []string) {
	if m.Harness != "claude-code" {
		violations = append(violations, fmt.Sprintf("harness %q not supported in phase 1 (claude-code only)", m.Harness))
	}
	declared := map[string]bool{}
	for _, h := range m.Executes.Hooks {
		declared[filepath.Clean(h.Path)] = true
		if _, err := os.Stat(filepath.Join(dir, h.Path)); err != nil {
			violations = append(violations, "declared hook missing: "+h.Path)
		}
	}
	filepath.Walk(filepath.Join(dir, "hooks"), func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		if !declared[filepath.Clean(rel)] {
			violations = append(violations, "undeclared executable: "+rel)
		}
		return nil
	})
	if b, err := os.ReadFile(filepath.Join(dir, "settings.json")); err == nil {
		var s map[string]json.RawMessage
		if json.Unmarshal(b, &s) == nil {
			for _, key := range []string{"hooks", "mcpServers"} {
				if v, ok := s[key]; ok && string(v) != "{}" && string(v) != "[]" && string(v) != "null" {
					violations = append(violations, "settings.json contains live "+key+" (must be quarantined)")
				}
			}
		}
	}
	return violations
}
```

```go
// internal/stack/allowlist.go
package stack

// Tracked stack content, pinned by the Task 1 spike. Everything else in a
// profile is runtime state and stays untracked (spec §3.2).
var AllowedPaths = []string{
	"stack.yaml", "README.md", "CHANGELOG.md", "CLAUDE.md",
	"settings.json", "keybindings.json", "quarantine.json",
	"skills/", "agents/", "hooks/",
}

const GitignoreContent = `*
!/.gitignore
!/stack.yaml
!/README.md
!/CHANGELOG.md
!/CLAUDE.md
!/settings.json
!/keybindings.json
!/quarantine.json
!/skills/
!/skills/**
!/agents/
!/agents/**
!/hooks/
!/hooks/**
`
```

Adjust both to the spike findings; move the temporary constant out of `cmd_init.go`.

- [ ] **Step 4: Verify pass** — `go test ./...` → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: stack manifest parsing, validation, tracked-path allowlist"`

---

### Task 6: Profile switching — `use`, `back`, `status` **[CODEX]**

**Files:**
- Create: `internal/cli/cmd_switch.go`
- Test: `internal/cli/cmd_switch_test.go`

**Interfaces:**
- Consumes: `state` (Task 3). Commands run via `cli.Run` with `SHERPA_HOME` set by tests.
- Produces: `use <name>` sets `state.Active`; `back` = `use mine`; `status` prints active profile + one line per installed profile. `back`/`use` are pure state writes — **no network, no file mutation beyond state.json** (spec §6.1, §6.4: must work offline).

- [ ] **Step 1: Failing tests**

```go
package cli

import (
	"bytes"
	"strings"
	"testing"

	"sherpa/internal/state"
)

func setupHome(t *testing.T) string {
	home := t.TempDir()
	t.Setenv("SHERPA_HOME", home)
	st, _ := state.Load(home)
	st.Active = "mine"
	st.Profiles["mine"] = state.Profile{Name: "mine", Path: home + "/profiles/mine", Harness: "claude-code"}
	st.Profiles["jane"] = state.Profile{Name: "jane", Path: home + "/profiles/jane", Origin: "https://x/jane.git", Harness: "claude-code"}
	st.Save(home)
	return home
}

func TestUseAndBack(t *testing.T) {
	home := setupHome(t)
	var out, errb bytes.Buffer
	if code := Run([]string{"use", "jane"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	st, _ := state.Load(home)
	if st.Active != "jane" {
		t.Fatalf("active = %q", st.Active)
	}
	Run([]string{"back"}, &out, &errb)
	st, _ = state.Load(home)
	if st.Active != "mine" {
		t.Fatalf("back: active = %q", st.Active)
	}
}

func TestUseUnknownProfileFails(t *testing.T) {
	setupHome(t)
	var out, errb bytes.Buffer
	if code := Run([]string{"use", "ghost"}, &out, &errb); code == 0 {
		t.Fatal("want failure")
	}
}

func TestStatusListsProfiles(t *testing.T) {
	setupHome(t)
	var out, errb bytes.Buffer
	Run([]string{"status"}, &out, &errb)
	s := out.String()
	if !strings.Contains(s, "mine") || !strings.Contains(s, "jane") || !strings.Contains(s, "active") {
		t.Fatalf("status output: %q", s)
	}
}
```

- [ ] **Step 2: Verify fail.** — `go test ./internal/cli/` → FAIL.
- [ ] **Step 3: Implement** `cmd_switch.go`: three `register(...)` calls; `use` validates the profile exists in state, sets `Active`, saves; `back` delegates to `use mine`; `status` prints `active: <name>` then `  <name>  <origin or "(yours)">` per profile, sorted.
- [ ] **Step 4: Verify pass** — `go test ./...` → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: profile switching (use/back/status)"`

---

### Task 7: Launcher — run Claude under a profile

**Not delegable** (credential-link list is safety-sensitive, spec §3.2).

**Files:**
- Create: `internal/launch/launch.go`, `internal/cli/cmd_run.go`
- Test: `internal/launch/launch_test.go`

**Interfaces:**
- Consumes: `state` (active profile path).
- Produces: `launch.Claude(profileDir, mineDir string, credFiles []string, args []string, stdio launch.Stdio) error` — hardlinks/copies each credential file from `mineDir` into `profileDir` if absent (never overwrites, never tracked — `.gitignore` from Task 5 ignores them), then execs `claude` with `CLAUDE_CONFIG_DIR=profileDir`. Binary name overridable via `SHERPA_CLAUDE_BIN` (tests use a fake). `credFiles` comes from a package-level `var CredentialFiles = []string{".credentials.json"}` — **pin to Task 1 spike findings**.
- Produces: `sherpa run [args…]` — launches under the *active* profile.

- [ ] **Step 1: Failing test** — uses a fake `claude` shell script that dumps `$CLAUDE_CONFIG_DIR` to a file:

```go
package launch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClaudeLaunchesWithConfigDirAndLinksCreds(t *testing.T) {
	profile, mine, outDir := t.TempDir(), t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(mine, ".credentials.json"), []byte("secret"), 0o600)
	fake := filepath.Join(outDir, "claude")
	os.WriteFile(fake, []byte("#!/bin/sh\necho \"$CLAUDE_CONFIG_DIR\" > "+outDir+"/env.txt\n"), 0o755)
	t.Setenv("SHERPA_CLAUDE_BIN", fake)

	err := Claude(profile, mine, []string{".credentials.json"}, nil, Stdio{})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(outDir, "env.txt"))
	if strings.TrimSpace(string(got)) != profile {
		t.Fatalf("CLAUDE_CONFIG_DIR = %q, want %q", got, profile)
	}
	cred, err := os.ReadFile(filepath.Join(profile, ".credentials.json"))
	if err != nil || string(cred) != "secret" {
		t.Fatal("credentials not linked into profile")
	}
}

func TestClaudeNeverOverwritesExistingCred(t *testing.T) {
	profile, mine := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(mine, ".credentials.json"), []byte("new"), 0o600)
	os.WriteFile(filepath.Join(profile, ".credentials.json"), []byte("keep"), 0o600)
	t.Setenv("SHERPA_CLAUDE_BIN", "/usr/bin/true")
	Claude(profile, mine, []string{".credentials.json"}, nil, Stdio{})
	b, _ := os.ReadFile(filepath.Join(profile, ".credentials.json"))
	if string(b) != "keep" {
		t.Fatal("overwrote existing credential file")
	}
}
```

- [ ] **Step 2: Verify fail.**
- [ ] **Step 3: Implement**

```go
// internal/launch/launch.go
package launch

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// CredentialFiles: pin to Task 1 spike findings.
var CredentialFiles = []string{".credentials.json"}

type Stdio struct{ In io.Reader; Out, Err io.Writer }

func bin() string {
	if b := os.Getenv("SHERPA_CLAUDE_BIN"); b != "" {
		return b
	}
	return "claude"
}

func Claude(profileDir, mineDir string, credFiles []string, args []string, stdio Stdio) error {
	for _, f := range credFiles {
		dst := filepath.Join(profileDir, f)
		if _, err := os.Stat(dst); err == nil {
			continue // never overwrite
		}
		src := filepath.Join(mineDir, f)
		if b, err := os.ReadFile(src); err == nil {
			os.WriteFile(dst, b, 0o600)
		}
	}
	cmd := exec.Command(bin(), args...)
	cmd.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+profileDir)
	// Default each stream independently so tests can override any subset.
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdio.In, stdio.Out, stdio.Err
	if cmd.Stdin == nil {
		cmd.Stdin = os.Stdin
	}
	if cmd.Stdout == nil {
		cmd.Stdout = os.Stdout
	}
	if cmd.Stderr == nil {
		cmd.Stderr = os.Stderr
	}
	return cmd.Run()
}
```

`cmd_run.go`: `register("run", …)` — load state, resolve active profile and `mine` paths, call `launch.Claude(active.Path, mine.Path, launch.CredentialFiles, args, …)`.

- [ ] **Step 4: Verify pass** — `go test ./...` → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: launch claude under active profile with credential linking"`

---

### Task 8: Quarantine — structural stripping of executables (spec §3.5)

**Not delegable** (core safety mechanism).

**Files:**
- Create: `internal/quarantine/quarantine.go`
- Test: `internal/quarantine/quarantine_test.go`

**Interfaces:**
- Produces (consumed by Tasks 9, 10):

```go
// Strip moves "hooks" and "mcpServers" out of dir/settings.json into dir/quarantine.json.
// Idempotent. Missing settings.json is fine.
func Strip(dir string) error
// Pending lists quarantined capability IDs: "hook:<event>:<n>" / "mcp:<name>".
func Pending(dir string) ([]string, error)
// Approve moves the identified entry back into settings.json. "all" approves everything.
func Approve(dir string, id string) error
```

- [ ] **Step 1: Failing tests** — Strip removes both keys from settings.json and preserves them byte-comparably in quarantine.json; Pending lists them; Approve("all") restores settings.json to original semantics (JSON-equal); Approve of a single mcp id moves only that entry; settings without executables → Strip is a no-op and Pending is empty.

```go
package quarantine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

const settings = `{
  "env": {"FOO": "1"},
  "hooks": {"PreToolUse": [{"matcher": "*", "hooks": [{"type": "command", "command": "./hooks/check.sh"}]}]},
  "mcpServers": {"github": {"command": "npx", "args": ["-y", "server-github"]}}
}`

func setup(t *testing.T) string {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "settings.json"), []byte(settings), 0o644)
	return d
}

func read(t *testing.T, d, f string) map[string]any {
	b, _ := os.ReadFile(filepath.Join(d, f))
	var m map[string]any
	json.Unmarshal(b, &m)
	return m
}

func TestStripRemovesExecutables(t *testing.T) {
	d := setup(t)
	if err := Strip(d); err != nil {
		t.Fatal(err)
	}
	s := read(t, d, "settings.json")
	if _, ok := s["hooks"]; ok {
		t.Fatal("hooks still live")
	}
	if _, ok := s["mcpServers"]; ok {
		t.Fatal("mcpServers still live")
	}
	if s["env"].(map[string]any)["FOO"] != "1" {
		t.Fatal("non-executable settings damaged")
	}
	p, _ := Pending(d)
	if len(p) != 2 {
		t.Fatalf("pending = %v", p)
	}
}

func TestApproveAllRestores(t *testing.T) {
	d := setup(t)
	orig := read(t, d, "settings.json")
	Strip(d)
	if err := Approve(d, "all"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(orig, read(t, d, "settings.json")) {
		t.Fatal("approve all did not restore settings")
	}
	if p, _ := Pending(d); len(p) != 0 {
		t.Fatalf("pending after approve: %v", p)
	}
}

func TestApproveSingleMCP(t *testing.T) {
	d := setup(t)
	Strip(d)
	if err := Approve(d, "mcp:github"); err != nil {
		t.Fatal(err)
	}
	s := read(t, d, "settings.json")
	if _, ok := s["mcpServers"].(map[string]any)["github"]; !ok {
		t.Fatal("github mcp not restored")
	}
	if _, ok := s["hooks"]; ok {
		t.Fatal("hooks restored without approval")
	}
}

func TestStripNoExecutablesNoop(t *testing.T) {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "settings.json"), []byte(`{"env":{}}`), 0o644)
	if err := Strip(d); err != nil {
		t.Fatal(err)
	}
	if p, _ := Pending(d); len(p) != 0 {
		t.Fatalf("pending: %v", p)
	}
}
```

- [ ] **Step 2: Verify fail.**
- [ ] **Step 3: Implement** — `quarantine.json` shape: `{"hooks": <verbatim hooks value>, "mcpServers": {<name>: <verbatim entry>}}`. Strip: parse settings as `map[string]json.RawMessage`, move keys, write both files (settings via marshal-indent). Pending: hooks → one id per event key (`hook:<event>:<i>` per matcher entry); mcp → `mcp:<name>`. Approve: "all" merges everything back and truncates quarantine.json; `mcp:<name>` moves one entry; single-hook approval restores the whole event entry (`hook:<event>:<i>`) — hooks within one event share a declaration and are approved per entry. Keep it ~120 lines, pure stdlib.
- [ ] **Step 4: Verify pass** — `go test ./...` → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: structural quarantine for hooks and MCP servers"`

---

### Task 9: `sherpa clone` — stage, validate, quarantine, install

**Not delegable** (composes the safety pipeline; spec §6.2 atomicity).

**Files:**
- Create: `internal/cli/cmd_clone.go`, `internal/gitutil/gitutil.go`
- Test: `internal/cli/cmd_clone_test.go`, `internal/gitutil/gitutil_test.go`

**Interfaces:**
- Consumes: `stack.Parse/Validate` (5), `quarantine.Strip` (8), `state` (3).
- Produces:
  - `gitutil.Clone(url, dest string) error`, `gitutil.Run(dir string, args ...string) (string, error)` — thin `git` wrappers (consumed by 12, 13).
  - `sherpa clone <git-url> [--name n] [--approve-all]`: clone to staging dir under `$SHERPA_HOME/staging/` → parse+validate manifest (violations abort) → `quarantine.Strip` → create branch `local` at upstream head (upstream remains `origin/main`) → **atomic rename** staging→`profiles/<name>` → add to state (Origin=url, Active unchanged!) → print review-gate report (Task 10 wires interactivity; for now print pending capabilities + how to approve).
  - Clone **must not** switch the active profile; user activates with `use`/`try` explicitly.

- [ ] **Step 1: Failing tests** — fixture: build a local "expert repo" with `git init`; test that clone (a) installs to `profiles/jane`, (b) leaves `Active` unchanged, (c) strips executables from installed settings.json, (d) aborts cleanly on validation failure (no `profiles/<name>` dir, no state entry — spec §6.2), (e) creates branch `local` at the upstream head (no tracking config needed — diff/update reference `origin/main` and tags explicitly).

```go
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
```

(Task 9's clone always installs **fully quarantined** — that is exactly what the first test asserts. The `--approve-all` flag and interactive approval arrive with the review gate in Task 10; the strip-then-approve round-trip is what makes "approve all" identical to a reviewed full approval.)

- [ ] **Step 2: Verify fail.**
- [ ] **Step 3: Implement** — `gitutil` first (Clone via `git clone`, Run via CombinedOutput, returns trimmed output). Then `cmd_clone.go`: staging dir `$SHERPA_HOME/staging/<name>`; `defer os.RemoveAll(staging)` on any failure path; steps: clone → read+parse `stack.yaml` → `Validate` → `Strip` → append `stack.GitignoreContent` write (stacks should ship it, but enforce) → `gitutil.Run(dir, "checkout", "-b", "local")` → `os.Rename(staging, final)` → state append+save. Name defaults to manifest name.
- [ ] **Step 4: Verify pass** — `go test ./...` → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: sherpa clone with staged atomic install and quarantine"`

---

### Task 10: Review gate + `sherpa try`

**Not delegable** (consent UX, spec §3.5 + §10.6).

**Files:**
- Create: `internal/review/gate.go`, `internal/cli/cmd_try.go`
- Modify: `internal/cli/cmd_clone.go` (wire gate into clone)
- Test: `internal/review/gate_test.go`

**Interfaces:**
- Consumes: `stack.Manifest.Executes` (5), `quarantine.Pending/Approve` (8), `launch.Claude` (7).
- Produces:

```go
// RunGate shows every pending capability with its declared purpose and, for
// hooks, the full script content. mode: Interactive | ApproveAll | KeepQuarantined.
// Returns the ids the user approved. Reads y/n/a(ll)/q from in.
func RunGate(dir string, m *stack.Manifest, mode Mode, in io.Reader, out io.Writer) ([]string, error)
```

- `sherpa try <git-url-or-profile>`: if URL → clone flow (Task 9) with gate; then launch subprocess session under the profile (Task 7) **without changing `state.Active`** — exiting the session = reverted (spec §3.5).
- `sherpa clone` gains the same gate after install; `--approve-all` flag maps to `ApproveAll` (decision §10.6).
- **Atomicity note (spec §6.2):** the gate runs *after* the atomic install on purpose. If the user quits the gate or it errors, the profile stays installed **fully quarantined** — a safe, consistent state (nothing can execute), not a partial install. No rollback is needed; `sherpa show`/re-running the gate later can approve capabilities.

- [ ] **Step 1: Failing tests** — with two pending capabilities: Interactive with input `y\nn\n` approves only the first (returned ids match, settings.json contains only that one); input `a\n` approves all; `ApproveAll` mode approves without reading input; `KeepQuarantined` approves nothing; hook script content appears in gate output.
- [ ] **Step 2: Verify fail.**
- [ ] **Step 3: Implement** — gate iterates `quarantine.Pending`, prints a block per capability: id, declared purpose from manifest (match hook path/mcp name), for hooks `os.ReadFile` the script and print indented; prompt `approve? [y/n/a/q]`; call `quarantine.Approve` per yes. `cmd_try.go` composes clone(if URL)+gate+launch.
- [ ] **Step 4: Verify pass** — `go test ./...` → PASS. Also manual: `go build ./cmd/sherpa && SHERPA_HOME=$(mktemp -d) ./sherpa try <fixture-repo>` — eyeball the gate rendering.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: install review gate and sherpa try"`

---

### Task 11: Sanitizer (spec §3.4) **[CODEX]**

**Files:**
- Create: `internal/sanitize/scan.go`, `internal/sanitize/testdata/` (corpus)
- Test: `internal/sanitize/scan_test.go`

**Interfaces:**
- Produces (consumed by Task 12): `sanitize.Scan(dir string, allowed []string) (findings []Finding)` with `type Finding struct { File string; Line int; Kind string; Excerpt string }`. Kinds: `secret` (blocks publish), `home-path` and `email` (warn). Scans only allowlisted tracked paths.

- [ ] **Step 1: Failing tests + corpus.** Corpus files under `testdata/`: one clean stack; one with planted `ghp_` + `github_pat_` tokens, `sk-ant-` and `sk-proj-` keys, `AKIA` AWS id, a 40-char hex secret in settings env, an `xoxb-` Slack token; one with `/Users/tim/...` path and `tim@example.com`. Test asserts: **every planted secret found** (list them explicitly by file+kind in the test — 100% catch on the corpus is the bar, spec §9), clean stack yields zero `secret` findings, home path and email yield warn-kind findings.
- [ ] **Step 2: Verify fail.**
- [ ] **Step 3: Implement** — regex table (`ghp_[A-Za-z0-9]{36}`, `github_pat_[A-Za-z0-9_]{22,}`, `sk-[A-Za-z0-9\-_]{20,}`, `AKIA[0-9A-Z]{16}`, `xoxb-[A-Za-z0-9\-]+`, generic: assignments whose value has Shannon entropy > 4.0 and length ≥ 20) + `/(Users|home)/[a-z0-9_\-]+/` + email regex. Walk only `allowed` paths. ~150 lines.
- [ ] **Step 4: Verify pass** — `go test ./internal/sanitize/ -v` → PASS, every corpus row listed.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: publish sanitizer with planted-secret corpus"`

---

### Task 12: `sherpa save`, `diff`, `publish`

**Files:**
- Create: `internal/cli/cmd_save.go`, `internal/cli/cmd_publish.go`
- Test: `internal/cli/cmd_save_test.go`

**Interfaces:**
- Consumes: `gitutil` (9), `sanitize` (11), `stack` (5), `state` (3).
- Produces:
  - `sherpa save [-m msg]` — in active profile: `git add -A && git commit` (only tracked paths change thanks to gitignore); refuses on `mine`? No — saving mine is *encouraged* (permanent personal history, decision §10.2). Default message `sherpa: save <timestamp>`.
  - `sherpa diff` — active profile: `git diff origin/main...local --stat` + full patch of tracked files; for `mine` (no origin): `git diff HEAD`.
  - `sherpa publish --remote <git-url>` — fail-closed pipeline (spec §3.4/§6.5): `sanitize.Scan` (any `secret` finding → abort, print findings, **no override flag**) → warn-findings require typed `yes` → bump `version` in stack.yaml + commit → show `git diff` of what will be pushed since last publish → confirm → **tag first, then push**: `git tag v<version>` followed by `git push <remote> local:main refs/tags/v<version>`. `forked_from` in manifest is left untouched (provenance, decision §10.8).

- [ ] **Step 1: Failing tests** — reuse `makeExpertRepo` + clone from Task 9 tests: save commits a modification on `local` (assert new commit, upstream ref unmoved); diff output contains the changed line; publish to a second bare fixture remote succeeds for a clean stack (assert tag `v2` exists on remote) and **aborts with nonzero exit and no push** when a `ghp_…` token is planted (assert remote has no `v2`).
- [ ] **Step 2: Verify fail.**
- [ ] **Step 3: Implement** — thin compositions over `gitutil.Run`; publish confirmation reads from `ctx.Stdin` so tests can drive it.
- [ ] **Step 4: Verify pass** — `go test ./...` → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: save/diff/publish with fail-closed sanitizer"`

---

### Task 13: `sherpa update` — fetch, review, merge, atomic swap (spec §3.3, §6.3)

**Not delegable** (the hardest correctness task).

**Files:**
- Create: `internal/cli/cmd_update.go`, `internal/update/merge.go`
- Test: `internal/update/merge_test.go`

**Interfaces:**
- Consumes: `gitutil` (9), `state` (3).
- Produces: `update.Merge(profileDir, targetRef string) (MergeResult, error)`:

```go
type MergeResult struct {
	Merged      bool     // true = local now includes targetRef
	Conflicts   []string // conflicted file paths (Merged=false)
	BackupRef   string   // refs/sherpa/backup-<unix> — always set before any mutation
}
```

Algorithm (locked by spec §3.3): 1) `git update-ref refs/sherpa/backup-<ts> refs/heads/local`; 2) `git worktree add --detach <tmp> local`; 3) in tmp: `git merge --no-ff <targetRef>`; 4a) success → `git update-ref refs/heads/local <tmp HEAD>`, `git -C profile reset --hard local`, remove worktree (note: `update-ref` is plumbing and *is* permitted on a branch checked out in the profile worktree — unlike `git branch -f`; the immediate `reset --hard` re-syncs the profile's working tree, and the detached tmp worktree never has `local` checked out); 4b) conflict → collect `git diff --name-only --diff-filter=U`, `git merge --abort`, remove worktree, return conflicts — **profile and `local` untouched**. CLI command: fetch origin → find newest `v*` tag → print CHANGELOG.md section + `git diff local...<tag> --stat` → confirm → Merge → on conflicts, print the plain-language list ("both you and upstream changed: <file>") and the manual-resolution hint (`git -C <profile> merge <tag>` yourself, or wait for guided flow in Phase 2 — printing the exact command is the v1 "guided" flow).

- [ ] **Step 1: Failing tests** — fixtures via `gitutil` + `os/exec` git: (a) clean merge: upstream adds a file, local changed another → Merged=true, profile working tree contains both changes, backup ref exists and points at pre-merge local; (b) conflict: both sides change same line of CLAUDE.md → Merged=false, Conflicts==["CLAUDE.md"], `local` ref byte-identical to before (assert sha), profile working tree clean (`git status --porcelain` empty — spec §6.3); (c) kill-safety: simulate failure between update-ref and reset by calling the two halves directly — after re-running Merge, state converges (idempotence).
- [ ] **Step 2: Verify fail.**
- [ ] **Step 3: Implement** per the locked algorithm, ~120 lines. Worktree tmp dir under `$SHERPA_HOME/tmp/`.
- [ ] **Step 4: Verify pass** — `go test ./internal/update/ -v` → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: sherpa update with backup refs and atomic worktree merge"`

---

### Task 14: Full-loop integration test (spec §9)

**Files:**
- Create: `internal/integration/loop_test.go`

**Interfaces:** consumes everything; produces confidence.

- [ ] **Step 1: Write the test** — one test function walking the whole Phase 1 loop with real git fixtures, fake `claude` binary, `SHERPA_HOME`+`SHERPA_CLAUDE_DIR` in temp dirs:
init → clone expert v1 (gate: approve-all) → try (fake claude sees expert `CLAUDE_CONFIG_DIR`) → modify installed profile + save → expert publishes v2 upstream (commit+tag in fixture) → update (clean merge path) → assert merged content → make conflicting local+upstream edits → update → assert conflict reported, local sha unchanged → back → assert active==mine and **byte-identical `mine` tree vs. a snapshot taken right after init** (the sacred check) → publish own fork to a bare remote with `forked_from` set → assert remote tag + manifest lineage intact.
- [ ] **Step 2: Run** — `go test ./internal/integration/ -v` → PASS (fix whatever it flushes out).
- [ ] **Step 3: Commit** — `git add -A && git commit -m "test: full Phase 1 loop integration test"`

---

### Task 15: Static index + site **[CODEX]**

**Files:**
- Create: `index/index.json`, `index/site/index.html`, `internal/cli/cmd_search.go`
- Test: `internal/cli/cmd_search_test.go`

**Interfaces:**
- `index.json` schema (also the Phase 2 API seed):

```json
{ "stacks": [ { "ref": "@jane/rust-reviewer", "name": "rust-reviewer", "owner": "@jane",
    "harness": "claude-code", "summary": "…", "tags": ["rust"],
    "repo_url": "https://…/jane/rust-reviewer.git", "version": 14,
    "forked_from": null } ] }
```

- `sherpa search <query>` fetches `$SHERPA_INDEX_URL` (default: the hosted index; tests use a `file://`-style local path or httptest server), matches query against name/summary/tags **filtered to `harness == "claude-code"`**, prints `ref  summary  (harness)` rows and the `sherpa try <repo_url>` hint.
- `index/site/index.html`: single self-contained page (inline CSS/JS, no CDN) that fetches `../index.json`, renders a searchable list — each entry shows **harness badge**, name, owner, summary, tags, version, `forked_from` provenance when set, and a copy-button with `sherpa try <repo_url>`. Dark/light via `prefers-color-scheme`.

- [ ] **Step 1: Failing test** — `cmd_search_test.go`: httptest server serving a two-stack index (one `claude-code`, one `codex`); search "rust" prints the claude-code stack, never the codex one; empty result exits 0 with "no stacks found".
- [ ] **Step 2: Verify fail.**
- [ ] **Step 3: Implement** search + write `index.json` with 2-3 real seed entries (own stacks) + the HTML page.
- [ ] **Step 4: Verify** — `go test ./...` → PASS; open `index/site/index.html` locally and eyeball.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: static index, site, and sherpa search"`

---

### Task 16: README + release build **[CODEX]**

**Files:**
- Create: `README.md`, `Makefile`

- [ ] **Step 1: README** — what SherpA is (3 sentences from spec §1-2), install (`go build ./cmd/sherpa`), the command table from spec §4.1 (Phase 1 subset only), the trust model in one paragraph (quarantine, approve-all flag, versions immutable), link to the spec.
- [ ] **Step 2: Makefile** — `make test` (`go test ./...`), `make build` (CGO_ENABLED=0 builds for darwin/arm64, darwin/amd64, linux/amd64 into `dist/`), `make fmt`.
- [ ] **Step 3: Verify** — `make test && make build && ./dist/sherpa-darwin-arm64 version` (on this machine) → prints version.
- [ ] **Step 4: Commit** — `git add -A && git commit -m "docs: README and release Makefile"`

---

## Execution order & dependencies

1 (spike, gates everything) → 2 → 3 → {4, 5} → 6 → 7 → 8 → 9 → 10 → {11, 13 in parallel} → 12 → 14 → {15, 16 in parallel}.
[CODEX] tasks: 2, 6, 11, 15, 16 — dispatch per the Codex Delegation rules; Claude reviews every codex diff before commit.

## Deliberately out of plan (Phase ≥2, per spec)

Registry API/backend, website beyond the static page, follows/notifications, MCP server, `sherpa compare`, trial journal, sandboxed try, Codex harness, Forgejo/Railway deployment.
