# Phase 2 · 2b-ii — Codex Adapter Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Register Codex as a second, fully-working harness — `sherpa init --harness codex`, per-harness baseline profiles (`mine` → `mine-claude`/`mine-codex`), harness-aware `back`, and a codex clone/try/back round-trip — using the values pinned by the `CODEX_HOME` spike.

**Architecture:** 2b-i made everything harness-driven behind `harness.For(name)`. 2b-ii adds the Codex adapter to the registry, a `state.Baselines` map (harness → baseline profile name) with the never-ambiguous naming rule, and routes `init`/`back`/credential-seeding through the baseline of the relevant harness. It also clears the 2b-i carry-in checklist.

**Tech Stack:** Go ≥1.22, stdlib + yaml.v3. System `git`/`codex`. No new deps.

## Global Constraints

- Spec: `docs/superpowers/specs/2026-07-10-phase2-2b-codex-harness-design.md` (§4-7, §10); spike: `docs/superpowers/spikes/2026-07-11-codex-home.md` — re-read both before starting.
- Branch: `phase-2b-ii`. Module `sherpa`. gofmt-clean, `go vet` clean, stdlib+yaml.v3 only.
- **Claude-code behavior must not regress.** All existing tests stay green; a single-harness user who only ever runs `sherpa init` still gets a profile named `mine` and `back` still returns to it.
- **Baseline naming (spec §6):** a lone baseline is `mine`; adding a second harness renames the existing lone `mine` to `mine-<alias>` and creates the new one as `mine-<alias>`, so with ≥2 harnesses every baseline is harness-tagged. `back`/credential-seeding resolve `state.Baselines[activeProfile.Harness]`.
- **Codex adapter values are spike-pinned** (spike doc "Pinned adapter values"): `CODEX_HOME`, bin `codex` (env `SHERPA_CODEX_BIN`), credential `auth.json`, no keychain export, `SetupStateSources` none / `Seed` returns `("",nil,nil)`, AllowedPaths incl. `AGENTS.md AGENTS.override.md config.toml rules/ skills/`, unpublishable `auth.json` + non-empty login signatures.
- **Security (2b-i carry-in #2):** every registered harness MUST return non-empty `SetupStateFilenames()` and `LoginSignatures()` — a registration-time test enforces this. The codex barrier rests on `auth.json` + its token signatures.
- **`config.toml` is publishable** (sanitized by the content scan); `auth.json` is unpublishable.
- Commit after every task; `feat:`/`fix:`/`refactor:`/`test:`.

## Codex Delegation

Codex implements; controller verifies + commits. Reviews: Opus, **Fable for Task 2** (the baseline rename — hardest correctness), and a Codex cross-review on Task 1 (security signatures). Use the `codex-call` skill's wrapper for dispatch (`~/.claude/skills/codex-call/codex-run.sh --mode workspace-write --cwd <repo> --prompt-file <task>`); Codex cannot write `.git`; offline env `GOFLAGS=-mod=mod GOPROXY=off GOCACHE=/private/tmp/sherpa-go-build`.

---

### Task 1: Codex harness adapter + registry + non-empty-barrier guard

**Files:**
- Create: `internal/harness/codex.go`, `internal/harness/codex_test.go`
- Modify: `internal/harness/harness.go` (register codex), `internal/harness/registry_guard_test.go` (tripwire now expects both)
- Test: `internal/harness/harness_test.go` (barrier guard)

**Interfaces:**
- Consumes: the `Harness` interface (2b-i).
- Produces: `harness.For("codex")` returns the Codex adapter; `harness.Names()` = `["claude-code","codex"]`.

- [ ] **Step 1: Write failing tests** (`codex_test.go`) — assert the spike-pinned values:

```go
package harness

import "testing"

func TestCodexAdapterValues(t *testing.T) {
	h, err := For("codex")
	if err != nil {
		t.Fatal(err)
	}
	if h.Name() != "codex" || h.Alias() != "codex" {
		t.Fatalf("name/alias: %q/%q", h.Name(), h.Alias())
	}
	if h.ConfigDirEnv() != "CODEX_HOME" || h.LaunchBin() != "codex" || h.LaunchBinEnv() != "SHERPA_CODEX_BIN" {
		t.Fatalf("launch identity: %q %q %q", h.ConfigDirEnv(), h.LaunchBin(), h.LaunchBinEnv())
	}
	if h.DefaultConfigDir("/home/u") != "/home/u/.codex" {
		t.Fatalf("config dir: %q", h.DefaultConfigDir("/home/u"))
	}
	if got := h.CredentialFiles(); len(got) != 1 || got[0] != "auth.json" {
		t.Fatalf("cred files: %v", got)
	}
	if got := h.SetupStateFilenames(); len(got) != 1 || got[0] != "auth.json" {
		t.Fatalf("setup filenames: %v", got)
	}
	if len(h.LoginSignatures()) == 0 {
		t.Fatal("login signatures must be non-empty")
	}
	// no keychain export
	if err := h.PrepareBaselineCredentials(t.TempDir()); err != nil {
		t.Fatalf("PrepareBaselineCredentials must be a no-op for codex: %v", err)
	}
	// no curated seed
	rel, content, err := h.Seed([]byte(`{"x":1}`))
	if err != nil || rel != "" || content != nil {
		t.Fatalf("Seed must be empty for codex: %q %v %v", rel, content, err)
	}
	// AllowedPaths includes codex stack files
	must := map[string]bool{"AGENTS.md": false, "config.toml": false, "rules/": false, "skills/": false}
	for _, p := range h.AllowedPaths() {
		if _, ok := must[p]; ok {
			must[p] = true
		}
	}
	for p, seen := range must {
		if !seen {
			t.Errorf("AllowedPaths missing %q", p)
		}
	}
}
```

Barrier guard (`harness_test.go`): every registered harness has non-empty setup filenames + login signatures.

```go
func TestEveryHarnessHasNonEmptyBarrier(t *testing.T) {
	for _, name := range Names() {
		h, _ := For(name)
		if len(h.SetupStateFilenames()) == 0 {
			t.Errorf("%s: empty SetupStateFilenames weakens its publish barrier", name)
		}
		if len(h.LoginSignatures()) == 0 {
			t.Errorf("%s: empty LoginSignatures weakens its publish barrier", name)
		}
	}
}
```

Update `registry_guard_test.go`: expect `Names()` == `["claude-code","codex"]` (sorted).

- [ ] **Step 2: Verify fail** — `go test ./internal/harness/` → FAIL.
- [ ] **Step 3: Implement** `codex.go`:

```go
package harness

import (
	"path/filepath"
)

type Codex struct{}

func (Codex) Name() string        { return "codex" }
func (Codex) Alias() string       { return "codex" }
func (Codex) ConfigDirEnv() string { return "CODEX_HOME" }
func (Codex) LaunchBin() string    { return "codex" }
func (Codex) LaunchBinEnv() string { return "SHERPA_CODEX_BIN" }
func (Codex) DefaultConfigDir(home string) string { return filepath.Join(home, ".codex") }

// auth.json is Codex's on-disk credential (spike: no keychain). Linked from the
// baseline into each profile; unpublishable.
func (Codex) CredentialFiles() []string     { return []string{"auth.json"} }
func (Codex) SetupStateFilenames() []string { return []string{"auth.json"} }

// No keychain export and no curated onboarding seed (spike: auth.json alone
// authenticates; nothing to strip/curate like Claude's ~/.claude.json).
func (Codex) PrepareBaselineCredentials(baselineDir string) error { return nil }
func (Codex) SetupStateSources(configDir string) []string         { return nil }
func (Codex) CapturedName() string                                { return ".sherpa-codex-setup.json" }
func (Codex) Seed(captured []byte) (string, []byte, error)        { return "", nil, nil }

// LoginSignatures: markers found in auth.json. Non-empty is mandatory (barrier).
func (Codex) LoginSignatures() []string {
	return []string{"OPENAI_API_KEY", `"access_token"`, `"refresh_token"`, `"id_token"`, `"tokens"`, `"account_id"`}
}

func (Codex) AllowedPaths() []string {
	return []string{
		"stack.yaml", "README.md", "CHANGELOG.md", "quarantine.json",
		"AGENTS.md", "AGENTS.override.md", "config.toml", "rules/", "skills/", "hooks/",
	}
}

func (Codex) GitignoreContent() string { return codexGitignore }

const codexGitignore = `*
!/.gitignore
!/stack.yaml
!/README.md
!/CHANGELOG.md
!/quarantine.json
!/AGENTS.md
!/AGENTS.override.md
!/config.toml
!/rules/
!/rules/**
!/skills/
!/skills/**
!/hooks/
!/hooks/**
`
```

In `harness.go`, register it: `registry = map[string]Harness{"claude-code": ClaudeCode{}, "codex": Codex{}}`.

- [ ] **Step 4: Verify pass** — `go test ./internal/harness/ && go test ./...` → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: register Codex harness adapter (spike-pinned values) + non-empty-barrier guard"`

---

### Task 2: `state.Baselines` + migration + `renameProfile` (hardest — baseline rename)

**Files:**
- Modify: `internal/state/state.go`
- Create: `internal/cli/baseline.go` (rename + baseline helpers)
- Test: `internal/state/state_test.go`, `internal/cli/baseline_test.go`

**Interfaces:**
- Produces (consumed by Tasks 3, 4):
  - `state.State.Baselines map[string]string` (harness → baseline profile name); `state.Load` back-fills it (see migration).
  - `baseline.Name(st *state.State, h string) (string, bool)` — the baseline profile name for a harness.
  - `renameProfile(home string, st *state.State, old, new string) error` — atomically renames `profiles/old`→`profiles/new` on disk AND updates `st` in memory (Profiles key+Name+Path, Active if it referenced old, any Baselines value == old). Does NOT Save (caller commits). Fails before mutating `st` if the disk rename fails.

- [ ] **Step 1: Write failing tests**

State migration (`state_test.go`): a state.json with a `mine` profile and no `baselines` key, after `Load`, has `Baselines[mine.Harness]=="mine"`.

```go
func TestLoadMigratesBaselinesFromMine(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(home, 0o700)
	os.WriteFile(filepath.Join(home, "state.json"),
		[]byte(`{"active":"mine","profiles":{"mine":{"name":"mine","path":"/p","harness":"claude-code"}}}`), 0o600)
	st, err := Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if st.Baselines["claude-code"] != "mine" {
		t.Fatalf("migration: %v", st.Baselines)
	}
}
```

Rename (`baseline_test.go`): set up a home with `profiles/mine` (a dir), state Active=="mine", Baselines{claude-code:mine}; `renameProfile(home, st, "mine", "mine-claude")` → the dir moved, `st.Profiles["mine-claude"].Path` points at the new dir, `st.Profiles["mine"]` gone, `st.Active=="mine-claude"`, `st.Baselines["claude-code"]=="mine-claude"`; a rename onto an existing name errors without mutating.

- [ ] **Step 2: Verify fail** — `go test ./internal/state/ ./internal/cli/ -run 'Baseline|Rename'` → FAIL.
- [ ] **Step 3: Implement**:
  - `state.go`: add `Baselines map[string]string \`json:"baselines"\`` to `State`; in `Load`, after unmarshal, `if s.Baselines == nil { s.Baselines = map[string]string{} }` and migrate: `if len(s.Baselines) == 0 { if m, ok := s.Profiles["mine"]; ok { s.Baselines[m.Harness] = "mine" } }`. Initialize `Baselines` in the empty-state constructor too.
  - `baseline.go`:

```go
package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sherpa/internal/state"
)

func baselineName(st *state.State, harnessName string) (string, bool) {
	n, ok := st.Baselines[harnessName]
	return n, ok
}

// renameProfile moves profiles/old -> profiles/new on disk and updates st in memory.
// It does not Save; the caller commits st and, on Save failure, must roll the dir back.
func renameProfile(home string, st *state.State, old, newName string) error {
	if _, ok := st.Profiles[newName]; ok {
		return fmt.Errorf("profile %q already exists", newName)
	}
	p, ok := st.Profiles[old]
	if !ok {
		return fmt.Errorf("profile %q not found", old)
	}
	oldDir := filepath.Join(home, "profiles", old)
	newDir := filepath.Join(home, "profiles", newName)
	if err := os.Rename(oldDir, newDir); err != nil {
		return err // st untouched
	}
	delete(st.Profiles, old)
	p.Name = newName
	p.Path = newDir
	st.Profiles[newName] = p
	if st.Active == old {
		st.Active = newName
	}
	for h, name := range st.Baselines {
		if name == old {
			st.Baselines[h] = newName
		}
	}
	return nil
}
```

- [ ] **Step 4: Verify pass** — `go test ./...` → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: state.Baselines with migration + atomic renameProfile helper"`

---

### Task 3: `sherpa init --harness <name>` with the baseline naming rule

**Files:**
- Modify: `internal/cli/cmd_init.go`
- Test: `internal/cli/cmd_init_test.go`

**Interfaces:**
- Consumes: `harness.For` (1), `state.Baselines`/`renameProfile`/`baselineName` (2).
- Produces: `sherpa init [--harness <name>] [--refresh]` creating a per-harness baseline with the naming rule; `SHERPA_CODEX_DIR` override analogous to `SHERPA_CLAUDE_DIR`.

- [ ] **Step 1: Write failing tests** — (a) first `init` (no flag) → profile `mine`, `Baselines[claude-code]=="mine"`, Active=="mine" (unchanged behavior). (b) `init --harness codex` after a lone claude `mine` → the claude baseline is renamed to `mine-claude`, a new `mine-codex` exists, `Baselines=={claude-code:mine-claude, codex:mine-codex}`, and Active is unchanged except if it was `mine` it is now `mine-claude`. (c) `init --harness codex` when a codex baseline already exists → error (use --refresh). Use fixture config dirs via `SHERPA_CLAUDE_DIR`/`SHERPA_CODEX_DIR` (each with a fixture instruction file + a fixture auth/setup source) so no real config is touched.
- [ ] **Step 2: Verify fail.**
- [ ] **Step 3: Implement** — generalize `cmdInit`:
  - Parse `--harness <name>` (default `claude-code`) and `--refresh`. Resolve `h, err := harness.For(name)`.
  - Generalize `claudeDir()` → `configDir(h harness.Harness) string`: env override `SHERPA_<UPPER-ALIAS>_DIR` (i.e. `SHERPA_CLAUDE_DIR`/`SHERPA_CODEX_DIR`; build the env key as `"SHERPA_"+strings.ToUpper(h.Alias())+"_DIR"`), else `h.DefaultConfigDir(home)`.
  - `captureSetupState` takes `h`: loop `h.SetupStateSources(configDir(h))` writing to `mineDir/h.CapturedName()`; a harness with no sources (codex) captures nothing (no error).
  - **Naming rule transaction:**
    - If `Baselines[name]` exists: with `--refresh`, re-capture that baseline and return; without, error "already initialized for <harness>".
    - Determine the new baseline's name: if `len(Baselines)==0` → `"mine"`. Else → `"mine-"+h.Alias()`. Additionally, if this is the *second* harness and the sole existing baseline is the bare `"mine"`, first `renameProfile(home, st, "mine", "mine-"+existingHarnessAlias)` (existingHarnessAlias from `harness.For(existingHarness).Alias()`), updating `Baselines` accordingly (renameProfile already fixes the Baselines value).
    - Import the new baseline into `profiles/<newName>` via `profile.Import(configDir(h), dest, h.GitignoreContent())`, then `captureSetupState(dest, h)`, then set `st.Profiles[newName]` (Harness=name), `st.Baselines[name]=newName`, and `st.Active=newName` only if there was no active profile yet (first init); otherwise leave Active (a rename of the old mine already moved Active if needed).
    - **Save last (single commit point).** On Save failure, roll back: `os.RemoveAll(dest)` and, if a rename happened, `renameProfile` back (or restore the dir name) — mirror clone's rename-last discipline.
- [ ] **Step 4: Verify** — `go test ./...` → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: sherpa init --harness with never-ambiguous baseline naming"`

---

### Task 4: Harness-aware `back` + per-harness baseline routing + carry-in cleanups

**Files:**
- Modify: `internal/cli/cmd_switch.go` (back), `internal/cli/cmd_run.go`, `internal/cli/cmd_try.go`, `internal/cli/cmd_profile.go`, `internal/cli/cmd_search.go`, `internal/stack/manifest.go`
- Test: `internal/cli/cmd_switch_test.go`, existing suites

**Interfaces:**
- Consumes: `baselineName` (2), `harness.For`/`harness.Names` (1).
- Produces: `back` returns to the active profile's harness baseline; run/try/profile seed credentials from that baseline; search accepts any registered harness.

- [ ] **Step 1: Write failing tests** — (a) with two harnesses installed and a codex profile active, `sherpa back` sets Active to `mine-codex` (not `mine`/`mine-claude`); with a single claude harness, `back` still goes to `mine`. (b) search over an index containing a codex stack now RETURNS it (registry membership, not just claude-code).
- [ ] **Step 2: Verify fail.**
- [ ] **Step 3: Implement**:
  - `cmd_switch.go` `cmdBack`: load state, resolve the active profile's harness, `target, ok := baselineName(st, active.Harness)`; if !ok fall back to `"mine"`; `return cmdUse(ctx, []string{target})`.
  - `cmd_run.go`/`cmd_try.go`/`cmd_profile.go`: replace `mine := st.Profiles["mine"]` with the baseline for the relevant profile's harness: `bn, _ := baselineName(st, <profile>.Harness); baseline := st.Profiles[bn]`. Use `baseline.Path` everywhere `mine.Path` was used (SeedSetup, PrepareBaselineCredentials, launch.Launch baselineDir). For `run`, `<profile>` is the active profile; for `try`, the tried profile; for `profile setup`, the target.
  - `cmd_search.go`: change the harness filter from `harness.Default().Name()` to registry membership — accept a stack whose harness is in `harness.Names()` (build a set from `harness.Names()`); this makes codex stacks visible.
  - `manifest.go`: refresh the stale violation text "phase 1 (claude-code only)" → "unsupported harness %q" (it is now reached only via the nil guard).
  - Warning strings in `cmd_run.go`/`cmd_try.go` ("claude may ask you to log in") → harness-neutral ("the tool may ask you to log in").
- [ ] **Step 4: Verify** — `go test ./...` → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: harness-aware back + per-harness baseline routing; search accepts registered harnesses"`

---

### Task 5: Codex round-trip integration test + remaining carry-in cleanups

**Files:**
- Modify: `internal/integration/loop_test.go`, `internal/cli/cmd_clone_test.go`, `internal/launch/launch_test.go`
- Test: as above

**Interfaces:** consumes everything.

- [ ] **Step 1: Write the codex round-trip test** — in `loop_test.go`, add `TestCodexHarnessRoundTrip`: fixture `~/.codex`-style dir (AGENTS.md + a fake `auth.json` with an oauth-ish marker) via `SHERPA_CODEX_DIR`; a fake `codex` binary (env `SHERPA_CODEX_BIN`) that writes its `$CODEX_HOME` to a file (mirror `fakeClaude`); build a codex expert stack fixture (stack.yaml `harness: codex`, AGENTS.md, config.toml). Drive via `cli.Run`: `init --harness codex` → assert `mine-codex` baseline (and that a pre-existing claude `mine` got renamed to `mine-claude` if present) → `clone` the codex stack → `try` it → assert the fake codex saw `CODEX_HOME` = the codex profile dir → `back` → Active is the codex baseline. Also assert `auth.json` cannot be published from the codex profile (barrier).
- [ ] **Step 2: Verify fail/iterate** — `go test ./internal/integration/ -run Codex -v`.
- [ ] **Step 3: Remaining carry-in cleanups** — `cmd_clone_test.go`: `TestCloneOverwritesTamperedGitignore` assert against `harness.For(m.Harness).GitignoreContent()` (not `Default()`); `launch_test.go`: rename `TestEnsureCredentialFile*` → `TestPrepareBaselineCredentials*` (names only; bodies already call the new method). Update the `registry_guard_test.go` comment to note codex is now expected.
- [ ] **Step 4: Verify** — `go test ./... -count=1` → all packages PASS; gofmt/vet clean.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "test: codex round-trip integration + clear 2b-i carry-in checklist"`

---

## Execution order & dependencies

1 → 2 → 3 → 4 → 5. Task 2 (baseline rename) is the correctness-critical one — review on Fable. Task 1's security signatures get a Codex cross-review (non-empty barrier + auth.json signature set). The full suite green-gate holds at every task.

## Deliverables checklist (spec §4-7, §10 + carry-ins)

- [ ] Codex adapter registered with spike-pinned values (Task 1); `For("codex")` works.
- [ ] Non-empty-barrier guard for every harness (Task 1).
- [ ] `state.Baselines` + migration + atomic rename (Task 2).
- [ ] `init --harness` with never-ambiguous naming (mine → mine-claude/mine-codex) (Task 3).
- [ ] Harness-aware `back` + per-harness baseline credential/seed routing (Task 4).
- [ ] Carry-ins: search registry membership, stale strings, test renames, tamper-test harness source (Tasks 4, 5).
- [ ] Codex clone/try/back round-trip + auth.json unpublishable, proven by integration test (Task 5).
