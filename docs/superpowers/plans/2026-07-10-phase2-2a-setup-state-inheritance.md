# Phase 2 · 2a — Setup-State Inheritance Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make launching a profile inherit login/onboarding/defaults from `mine` (no re-login/re-onboarding), with an opt-in `--fresh-setup`, while making the setup state structurally unpublishable.

**Architecture:** A new `internal/harness` package abstracts *where a tool keeps machine-local setup state* and *how to curate it*. `init` captures the setup file into `mine` (untracked); launch seeds a curated copy (identity/onboarding kept, `projects`/caches stripped) into a profile lacking one; `publish` gains a dedicated, no-override hard-block against setup-state/OAuth content in tree and history.

**Tech Stack:** Go ≥1.22, stdlib + `gopkg.in/yaml.v3` (already vendored). System `git`. No new deps.

## Global Constraints

- Spec: `docs/superpowers/specs/2026-07-10-phase2-2a-setup-state-inheritance-design.md` — re-read §3 before starting.
- Branch: `phase-2`. Module `sherpa`. gofmt-clean, `go vet` clean, stdlib+yaml.v3 only.
- **`mine` is sacred**: the captured `mine/.sherpa-setup.json` is written only by `init`/`init --refresh`; no launch/seed/session ever writes back into it. Seeding reads mine, writes the *profile*.
- **Setup state is unpublishable, no override** (spec §3.3): three barriers — allowlist exclusion (already true: `.claude.json`/`.sherpa-setup.json` are not in `stack.AllowedPaths` and not un-ignored by `stack.GitignoreContent`), publish hard-block (Task 4), untracked+0600.
- **Curation is a whitelist, not a blacklist** (spec §3.1): unknown keys default to *dropped*. `projects` and `*Cache*` keys MUST be dropped.
- Not shareable at all in 2a (no "safe subset").
- All new setup/seed files are mode `0600`.
- Tests use `t.TempDir()` and env overrides only — never the real `~/.claude`, `~/.claude.json`, or keychain.
- Commit after every task; messages `feat:/fix:/test:`.

## Codex Delegation

Per project routing: **Codex implements, Opus reviews**; Tasks 1 and 4 are security-critical (curation whitelist; publish hard-block) and get careful review (Opus + a Codex cross-review on Task 4). Codex dispatch pattern (streaming, stdin closed):

```
codex exec --json --sandbox workspace-write "<task text + global constraints>" </dev/null \
  2>"$SCRATCH/err" | tee "$SCRATCH/jsonl" | jq -r 'select(.type=="item.started" or .type=="item.completed" or .type=="turn.completed" or .type=="turn.failed" or .type=="error") | .type+": "+(.item.type // .message // "")'
```
Codex cannot write `.git`; the controller verifies (`go test ./...`, gofmt, vet) and commits. Offline env: `GOFLAGS=-mod=mod GOPROXY=off GOCACHE=/private/tmp/sherpa-go-build`.

---

### Task 1: `internal/harness` — adapter interface + Claude-Code curation

**Files:**
- Create: `internal/harness/harness.go`, `internal/harness/claudecode.go`
- Test: `internal/harness/claudecode_test.go`

**Interfaces:**
- Produces (consumed by Tasks 2, 3, 5):

```go
type Harness interface {
    Name() string
    SetupStateSources(configDir string) []string // given the tool's config dir, absolute setup-state file paths
    CapturedName() string                          // untracked filename inside a profile, e.g. ".sherpa-setup.json"
    Seed(captured []byte) (targetRel string, content []byte, err error) // curated copy to write into a launching profile
}
func Default() Harness   // ClaudeCode{} for 2a
```

- [ ] **Step 1: Write the failing test**

```go
package harness

import (
	"encoding/json"
	"testing"
)

const rawSetup = `{
  "hasCompletedOnboarding": true,
  "oauthAccount": {"emailAddress": "me@example.com"},
  "userID": "u1",
  "installMethod": "native",
  "theme": "dark",
  "sonnet45MigrationComplete": true,
  "effortCalloutDismissed": true,
  "projects": {"/Users/me/x": {"hasTrustDialogAccepted": true, "mcpServers": {"evil": {}}}},
  "modelAccessCache": {"secretish": 1},
  "someFutureUnknownKey": {"nested": true}
}`

func TestSeedKeepsIdentityStripsProjectsAndCaches(t *testing.T) {
	rel, content, err := Default().Seed([]byte(rawSetup))
	if err != nil {
		t.Fatal(err)
	}
	if rel != ".claude.json" {
		t.Fatalf("target rel = %q, want .claude.json", rel)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(content, &m); err != nil {
		t.Fatal(err)
	}
	for _, keep := range []string{"hasCompletedOnboarding", "oauthAccount", "userID", "installMethod", "theme", "sonnet45MigrationComplete", "effortCalloutDismissed"} {
		if _, ok := m[keep]; !ok {
			t.Errorf("expected key %q kept", keep)
		}
	}
	for _, drop := range []string{"projects", "modelAccessCache", "someFutureUnknownKey"} {
		if _, ok := m[drop]; ok {
			t.Errorf("expected key %q dropped (whitelist fails safe)", drop)
		}
	}
}

func TestSeedRejectsInvalidJSON(t *testing.T) {
	if _, _, err := Default().Seed([]byte("{not json")); err == nil {
		t.Fatal("want error on invalid setup json")
	}
}

func TestSetupStateSourcesDerivedFromConfigDir(t *testing.T) {
	got := Default().SetupStateSources("/tmp/x/.claude")
	if len(got) != 1 || got[0] != "/tmp/x/.claude.json" {
		t.Fatalf("sources = %v, want [/tmp/x/.claude.json]", got)
	}
}
```

- [ ] **Step 2: Verify fail** — `go test ./internal/harness/` → FAIL (package/Default undefined).
- [ ] **Step 3: Implement**

```go
// internal/harness/harness.go
package harness

type Harness interface {
	Name() string
	SetupStateSources(configDir string) []string
	CapturedName() string
	Seed(captured []byte) (targetRel string, content []byte, err error)
}

func Default() Harness { return ClaudeCode{} }
```

```go
// internal/harness/claudecode.go
package harness

import (
	"encoding/json"
	"fmt"
	"strings"
)

type ClaudeCode struct{}

func (ClaudeCode) Name() string        { return "claude-code" }
func (ClaudeCode) CapturedName() string { return ".sherpa-setup.json" }

// SetupStateSources: Claude Code keeps setup/identity state in a sibling file
// next to the config dir — configDir "~/.claude" -> "~/.claude.json".
func (ClaudeCode) SetupStateSources(configDir string) []string {
	return []string{configDir + ".json"}
}

// keep is the explicit identity/onboarding/preference whitelist. Everything not
// matched here (and by the family rules below) is dropped — including projects,
// history, and *Cache*. Whitelist fails safe against unknown future keys.
var keep = map[string]bool{
	"hasCompletedOnboarding":               true,
	"hasCompletedClaudeInChromeOnboarding": true,
	"oauthAccount":                         true,
	"userID":                               true,
	"machineID":                            true,
	"installMethod":                        true,
	"firstStartTime":                       true,
	"lastOnboardingVersion":                true,
	"theme":                                true,
	"autoUpdates":                          true,
}

// keepFamily matches setup-safe boolean/timestamp families: migration flags and
// dismissed-callout / seen-notice flags. These carry no secrets or per-project state.
func keepFamily(k string) bool {
	if strings.Contains(k, "Migration") {
		return true
	}
	if strings.HasSuffix(k, "Dismissed") || strings.HasPrefix(k, "hasSeen") || strings.HasPrefix(k, "hasShown") {
		return true
	}
	return false
}

func (ClaudeCode) Seed(captured []byte) (string, []byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(captured, &m); err != nil {
		return "", nil, fmt.Errorf("parse setup state: %w", err)
	}
	out := map[string]json.RawMessage{}
	for k, v := range m {
		if keep[k] || keepFamily(k) {
			out[k] = v
		}
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", nil, err
	}
	return ".claude.json", b, nil
}
```

- [ ] **Step 4: Verify pass** — `go test ./internal/harness/` → PASS.
- [ ] **Step 5: Commit** — `git add internal/harness && git commit -m "feat: harness adapter with curated setup-state seed (claude-code)"`

---

### Task 2: `sherpa init` captures setup state + `--refresh`

**Files:**
- Modify: `internal/cli/cmd_init.go`
- Test: `internal/cli/cmd_init_test.go`

**Interfaces:**
- Consumes: `harness.Default()`, `harness.Harness.SetupStateSources/CapturedName` (Task 1); existing `claudeDir()`, `profile.Import`, `state`.
- Produces: `mine/.sherpa-setup.json` after init when the source exists; `sherpa init --refresh` re-captures into an existing `mine`.

- [ ] **Step 1: Write the failing test** (append to `cmd_init_test.go`)

```go
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
```

(Ensure `cmd_init_test.go` imports `bytes`, `os`, `path/filepath`, `strings`, `testing`.)

- [ ] **Step 2: Verify fail** — `go test ./internal/cli/ -run TestInitCaptures` → FAIL.
- [ ] **Step 3: Implement** — rewrite `cmdInit` to parse `--refresh`, factor capture into a helper:

```go
// internal/cli/cmd_init.go — add import "sherpa/internal/harness"
func cmdInit(ctx *Ctx, args []string) error {
	refresh := false
	for _, a := range args {
		if a == "--refresh" {
			refresh = true
		} else {
			return fmt.Errorf("unknown argument %q", a)
		}
	}
	st, err := state.Load(ctx.Home)
	if err != nil {
		return err
	}
	dest := filepath.Join(ctx.Home, "profiles", "mine")
	_, exists := st.Profiles["mine"]
	if refresh {
		if !exists {
			return fmt.Errorf("nothing to refresh: run `sherpa init` first")
		}
		if err := captureSetupState(dest); err != nil {
			return err
		}
		fmt.Fprintln(ctx.Stdout, "refreshed setup state for profile 'mine'")
		return nil
	}
	if exists {
		return fmt.Errorf("already initialized (profile 'mine' exists); use --refresh to re-capture setup state")
	}
	if err := profile.Import(claudeDir(), dest, stack.GitignoreContent); err != nil {
		return err
	}
	if err := captureSetupState(dest); err != nil {
		os.RemoveAll(dest)
		return err
	}
	st.Profiles["mine"] = state.Profile{Name: "mine", Path: dest, Harness: "claude-code"}
	st.Active = "mine"
	if err := st.Save(ctx.Home); err != nil {
		os.RemoveAll(dest)
		return err
	}
	fmt.Fprintf(ctx.Stdout, "imported %s as profile 'mine' (your original config is untouched)\n", claudeDir())
	return nil
}

// captureSetupState copies each existing harness setup-state source into mine as
// the untracked captured blob (0600). Missing source is not an error.
func captureSetupState(mineDir string) error {
	h := harness.Default()
	for _, src := range h.SetupStateSources(claudeDir()) {
		b, err := os.ReadFile(src)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(mineDir, h.CapturedName()), b, 0o600); err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 4: Verify pass** — `go test ./internal/cli/ -run TestInit` → PASS; `go test ./...` → PASS.
- [ ] **Step 5: Commit** — `git add internal/cli/cmd_init.go internal/cli/cmd_init_test.go && git commit -m "feat: init captures machine-local setup state (+ --refresh)"`

---

### Task 3: Launch seeds curated setup + `--fresh-setup`

**Files:**
- Modify: `internal/launch/launch.go`, `internal/cli/cmd_run.go`, `internal/cli/cmd_try.go`
- Test: `internal/launch/launch_test.go`, `internal/cli/cmd_run_test.go`

**Interfaces:**
- Consumes: `harness.Default()`, `harness.Harness.CapturedName/Seed` (Task 1).
- Produces:
  - `launch.SeedSetup(profileDir, mineDir string, h harness.Harness) error` — writes the curated seed into the profile if it lacks the target file and mine has a captured blob; never overwrites; missing blob is a no-op.
  - `run`/`try` accept `--fresh-setup` (skip seeding); default seeds before launch.

- [ ] **Step 1: Write the failing test** (append to `launch_test.go`)

```go
func TestSeedSetupWritesCuratedWhenAbsent(t *testing.T) {
	profile, mine := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(mine, ".sherpa-setup.json"),
		[]byte(`{"hasCompletedOnboarding":true,"projects":{"x":1}}`), 0o600)
	if err := SeedSetup(profile, mine, harness.Default()); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(profile, ".claude.json"))
	if err != nil {
		t.Fatal("seed not written")
	}
	if !strings.Contains(string(b), "hasCompletedOnboarding") || strings.Contains(string(b), "projects") {
		t.Fatalf("seed not curated: %s", b)
	}
}

func TestSeedSetupNeverOverwrites(t *testing.T) {
	profile, mine := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(mine, ".sherpa-setup.json"), []byte(`{"theme":"dark"}`), 0o600)
	os.WriteFile(filepath.Join(profile, ".claude.json"), []byte(`{"live":"state"}`), 0o600)
	SeedSetup(profile, mine, harness.Default())
	b, _ := os.ReadFile(filepath.Join(profile, ".claude.json"))
	if string(b) != `{"live":"state"}` {
		t.Fatalf("overwrote live state: %s", b)
	}
}

func TestSeedSetupNoBlobNoop(t *testing.T) {
	profile, mine := t.TempDir(), t.TempDir()
	if err := SeedSetup(profile, mine, harness.Default()); err != nil {
		t.Fatalf("want nil on missing blob, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(profile, ".claude.json")); err == nil {
		t.Fatal("wrote a file with no blob to seed from")
	}
}
```

(`launch_test.go` needs import `"sherpa/internal/harness"`.)

- [ ] **Step 2: Verify fail** — `go test ./internal/launch/ -run TestSeedSetup` → FAIL.
- [ ] **Step 3: Implement** — add to `launch.go` (import `"sherpa/internal/harness"`):

```go
// SeedSetup writes a curated setup-state file into profileDir if it has none and
// mine holds a captured blob. Never overwrites an existing profile file (live
// runtime state wins). A missing blob is a no-op — the caller falls back to the
// tool's own first-run onboarding.
func SeedSetup(profileDir, mineDir string, h harness.Harness) error {
	b, err := os.ReadFile(filepath.Join(mineDir, h.CapturedName()))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	rel, content, err := h.Seed(b)
	if err != nil {
		return err
	}
	dst := filepath.Join(profileDir, rel)
	if _, err := os.Stat(dst); err == nil {
		return nil // never overwrite live state
	}
	return os.WriteFile(dst, content, 0o600)
}
```

Then wire into `cmd_run.go`: parse `--fresh-setup` out of `args` before treating the rest as claude args; unless fresh, call `launch.SeedSetup(active.Path, mine.Path, harness.Default())` (best-effort: on error, warn to `ctx.Stderr` and continue) before `launch.EnsureCredentialFile`/`launch.Claude`.

```go
// cmd_run.go — add imports "sherpa/internal/harness"; split args
func cmdRun(ctx *Ctx, args []string) error {
	fresh := false
	var passthrough []string
	for _, a := range args {
		if a == "--fresh-setup" {
			fresh = true
			continue
		}
		passthrough = append(passthrough, a)
	}
	st, err := state.Load(ctx.Home)
	if err != nil {
		return err
	}
	active, ok := st.Profiles[st.Active]
	if !ok {
		return fmt.Errorf("no active profile")
	}
	mine := st.Profiles["mine"]
	if !fresh {
		if err := launch.SeedSetup(active.Path, mine.Path, harness.Default()); err != nil {
			fmt.Fprintf(ctx.Stderr, "warning: could not seed setup state (%v); tool may onboard\n", err)
		}
	}
	if err := launch.EnsureCredentialFile(mine.Path); err != nil {
		fmt.Fprintf(ctx.Stderr, "warning: could not prepare credentials (%v); claude may ask you to log in\n", err)
	}
	stdio := launch.Stdio{In: ctx.Stdin, Out: ctx.Stdout, Err: ctx.Stderr}
	return launch.Claude(active.Path, mine.Path, launch.CredentialFiles, passthrough, stdio)
}
```

(Adapt to the current `cmd_run.go` shape — preserve its existing active/mine resolution; only add the fresh-parse + SeedSetup call.) In `cmd_try.go`: add `--fresh-setup` to `parseTryArgs` (sets `req.fresh bool`), and before the existing `launch.EnsureCredentialFile`/`launch.Claude`, `if !req.fresh { launch.SeedSetup(profileDir, mine.Path, harness.Default()) }` with the same best-effort warning.

- [ ] **Step 4: Add a cmd_run test** confirming a fake claude launched via `run` sees a seeded `.claude.json`, and that `run --fresh-setup` does not create one. Use the existing `SHERPA_CLAUDE_BIN` fake pattern from `cmd_run_test.go`; assert `os.Stat(active/.claude.json)` present without `--fresh-setup` and absent with it.
- [ ] **Step 5: Verify** — `go test ./...` → PASS; gofmt/vet clean.
- [ ] **Step 6: Commit** — `git add -A && git commit -m "feat: launch seeds curated setup state; --fresh-setup opt-out"`

---

### Task 4: `publish` hard-block on setup-state / OAuth (security)

**Files:**
- Modify: `internal/sanitize/scan.go`, `internal/cli/cmd_publish.go`
- Test: `internal/sanitize/scan_test.go`, `internal/cli/cmd_save_test.go`

**Interfaces:**
- Consumes: existing `sanitize.Finding`, `scanPublishHistory` (cmd_publish), publish flow.
- Produces:
  - `sanitize.ScanSetupState(dir string) ([]Finding, error)` — walks all files under `dir` (skip `.git`); flags any file whose basename is `.claude.json` or `.sherpa-setup.json`, or any file containing an OAuth signature (`oauthAccount`, `claudeAiOauth`, `"accessToken"`, `"refreshToken"`). `Kind = "setup-state"`.
  - `sanitize.ScanPatchSetupState(patch string) []Finding` — over `+` added lines and `+++ b/...` headers, same signatures/filenames. `Kind = "setup-state"`.
  - Publish blocks (fail-closed, no override) when either returns findings, over both the working tree and the pushed history range.

- [ ] **Step 1: Write failing tests** (`scan_test.go`)

```go
func TestScanSetupStateFlagsFileByName(t *testing.T) {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, ".claude.json"), []byte(`{"x":1}`), 0o600)
	f, err := ScanSetupState(d)
	if err != nil || len(f) == 0 {
		t.Fatalf("want setup-state finding, got %v err %v", f, err)
	}
	if f[0].Kind != "setup-state" {
		t.Fatalf("kind = %q", f[0].Kind)
	}
}

func TestScanSetupStateFlagsOAuthContent(t *testing.T) {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "settings.json"), []byte(`{"note":"has oauthAccount here"}`), 0o644)
	f, _ := ScanSetupState(d)
	if len(f) == 0 {
		t.Fatal("want finding for oauth signature in a normal file")
	}
}

func TestScanSetupStateCleanDirNoFindings(t *testing.T) {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "CLAUDE.md"), []byte("# ok"), 0o644)
	f, _ := ScanSetupState(d)
	if len(f) != 0 {
		t.Fatalf("clean dir flagged: %v", f)
	}
}

func TestScanPatchSetupStateFlagsAddedOAuth(t *testing.T) {
	patch := "+++ b/x.json\n+  \"accessToken\": \"zzz\"\n"
	if len(ScanPatchSetupState(patch)) == 0 {
		t.Fatal("want setup-state finding in patch")
	}
}
```

- [ ] **Step 2: Verify fail** — `go test ./internal/sanitize/ -run SetupState` → FAIL.
- [ ] **Step 3: Implement** in `scan.go`:

```go
var setupStateNames = map[string]bool{".claude.json": true, ".sherpa-setup.json": true}
var oauthSignatures = []string{"oauthAccount", "claudeAiOauth", `"accessToken"`, `"refreshToken"`}

func containsOAuthSig(s string) bool {
	for _, sig := range oauthSignatures {
		if strings.Contains(s, sig) {
			return true
		}
	}
	return false
}

// ScanSetupState flags any setup-state file (by name) or any file whose content
// carries an OAuth login signature. Fail-closed input to publish; Kind "setup-state".
func ScanSetupState(dir string) ([]Finding, error) {
	var out []Finding
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		r, _ := filepath.Rel(dir, p)
		if setupStateNames[info.Name()] {
			out = append(out, Finding{File: r, Kind: "setup-state", Excerpt: info.Name()})
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if containsOAuthSig(string(b)) {
			out = append(out, Finding{File: r, Kind: "setup-state", Excerpt: "oauth login signature"})
		}
		return nil
	})
	return out, err
}

func ScanPatchSetupState(patch string) []Finding {
	var out []Finding
	for _, line := range strings.Split(patch, "\n") {
		if strings.HasPrefix(line, "+++ ") {
			name := filepath.Base(strings.TrimSpace(strings.TrimPrefix(line, "+++ b/")))
			if setupStateNames[name] {
				out = append(out, Finding{File: name, Kind: "setup-state", Excerpt: name})
			}
			continue
		}
		if strings.HasPrefix(line, "+") && containsOAuthSig(line) {
			out = append(out, Finding{Kind: "setup-state", Excerpt: "oauth login signature"})
		}
	}
	return out
}
```

(`scan.go` has no `rel()` helper — use `filepath.Rel(dir, p)` inline as shown. `Finding` fields are `File, Line, Kind, Excerpt` (Line left zero for setup-state findings, matching the history-scan convention).)

Then in `cmd_publish.go`, before tagging/pushing, add the block (fail-closed, no override):

```go
// setup-state / OAuth must never be published (spec 2a §3.3). No override.
ss, err := sanitize.ScanSetupState(profile.Path)
if err != nil {
	return err
}
histSS := ScanPatchSetupState(historyPatch) // reuse the patch already fetched for the history secret scan
if len(ss) > 0 || len(histSS) > 0 {
	printFindings(ctx.Stderr, append(ss, histSS...))
	return fmt.Errorf("publish blocked: setup-state/login content must never be shared")
}
```

Wire it so the history patch used by `scanPublishHistory` is also passed to `ScanPatchSetupState` (compute the patch once, feed both). If `scanPublishHistory` currently fetches the patch internally, refactor it to `scanPublishHistoryPatch(dir, remote) (patch string, err error)` returning the raw patch, then call both `sanitize.ScanPatch(patch)` and `sanitize.ScanPatchSetupState(patch)`.

- [ ] **Step 4: Add a publish security test** (`cmd_save_test.go`, publish suite): clone/build a profile, write a `.claude.json` with `{"oauthAccount":{...}}` into it, `save`, then `publish --remote <bare>` → must exit nonzero citing setup-state and the bare remote must have **no** tag. (Because the gitignore excludes `.claude.json`, also test the direct signal: place the oauth signature inside an allowlisted file like `settings.json` so it IS tracked, then confirm publish blocks and pushes nothing.)
- [ ] **Step 5: Verify** — `go test ./...` → PASS; gofmt/vet clean.
- [ ] **Step 6: Commit** — `git add -A && git commit -m "feat: publish hard-block on setup-state/OAuth content (tree + history, no override)"`

---

### Task 5: `sherpa profile setup` + integration/isolation tests

**Files:**
- Create: `internal/cli/cmd_profile.go`, `internal/cli/cmd_profile_test.go`
- Modify: `internal/integration/loop_test.go`

**Interfaces:**
- Consumes: `state`, `launch` (Task 3), `harness`.
- Produces: `sherpa profile setup <name>` — removes any seeded `.claude.json` from that profile, then launches it with fresh setup (onboarding runs), without changing `state.Active`.

- [ ] **Step 1: Write the failing test** (`cmd_profile_test.go`)

```go
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
```

- [ ] **Step 2: Verify fail** — `go test ./internal/cli/ -run TestProfileSetup` → FAIL.
- [ ] **Step 3: Implement** `cmd_profile.go`:

```go
package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sherpa/internal/harness"
	"sherpa/internal/launch"
	"sherpa/internal/state"
)

func init() { register("profile", cmdProfile) }

func cmdProfile(ctx *Ctx, args []string) error {
	if len(args) < 2 || args[0] != "setup" {
		return fmt.Errorf("usage: sherpa profile setup <name>")
	}
	name := args[1]
	st, err := state.Load(ctx.Home)
	if err != nil {
		return err
	}
	p, ok := st.Profiles[name]
	if !ok {
		return fmt.Errorf("unknown profile %q", name)
	}
	mine := st.Profiles["mine"]
	// Clear any seeded setup so the tool runs its own first-run flow.
	rel, _, _ := harness.Default().Seed([]byte("{}")) // rel = target filename (".claude.json"); content ignored
	_ = os.Remove(filepath.Join(p.Path, rel))
	if err := launch.EnsureCredentialFile(mine.Path); err != nil {
		fmt.Fprintf(ctx.Stderr, "warning: could not prepare credentials (%v)\n", err)
	}
	fmt.Fprintf(ctx.Stdout, "fresh setup for %q (active profile unchanged)\n", name)
	stdio := launch.Stdio{In: ctx.Stdin, Out: ctx.Stdout, Err: ctx.Stderr}
	// No SeedSetup call: this is the fresh path.
	return launch.Claude(p.Path, mine.Path, launch.CredentialFiles, nil, stdio)
}
```

- [ ] **Step 4: Verify pass** — `go test ./internal/cli/ -run TestProfileSetup` → PASS.
- [ ] **Step 5: Extend the integration loop** — in `loop_test.go`, in the init+try stage: give the fixture `~/.claude.json` a setup blob at init; after `try`, assert the launched profile's `.claude.json` exists, contains `hasCompletedOnboarding`, and does **not** contain `projects`. Add an isolation assertion: two profiles seeded from the same mine each get `.claude.json` without `projects`.
- [ ] **Step 6: Verify** — `go test ./...` → PASS; gofmt/vet clean.
- [ ] **Step 7: Commit** — `git add -A && git commit -m "feat: sherpa profile setup (fresh); setup-state integration + isolation tests"`

---

## Execution order & dependencies

1 → 2 → 3 → 4 → 5. Task 1 is the seam everything else consumes. Tasks 4 (publish hard-block) and 1 (curation whitelist) are the security-critical pair — review both carefully; give Task 4 a Codex cross-review in addition to the standard Opus review.

## Deliverables checklist (spec §2 goals)

- [ ] Profile launch inherits login+onboarding+defaults (Tasks 2,3,5).
- [ ] Isolation preserved — `projects`/caches stripped (Tasks 1,5).
- [ ] `--fresh-setup` + `sherpa profile setup` (Tasks 3,5).
- [ ] Setup state structurally unpublishable — allowlist (already) + publish hard-block (Task 4) + untracked/0600 (Tasks 2,3).
- [ ] Reusable harness seam for 2b (Task 1).
