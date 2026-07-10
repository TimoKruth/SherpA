# Phase 2 · 2b-i — Harness Generalization Refactor Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move every claude-code-specific concern (config-dir env, launch binary, credential files, allowlist, gitignore, setup-state, unpublishable signatures) into the `internal/harness` adapter, and route all Phase-1/2a code through `harness.For(profileHarness)` — with claude-code the only registered harness, so **all existing tests stay green** (zero behavior change).

**Architecture:** Grow the `harness.Harness` interface into the single owner of harness-specific values + a `For(name)`/`Names()` registry. `internal/stack` and `internal/launch` stop hardcoding claude-code and call through a `Harness` they're handed. This is a pure refactor: the claude-code adapter returns exactly today's values, verified by parity tests, so the whole suite passes unchanged. 2b-ii (the Codex adapter, after a `CODEX_HOME` spike) plugs into this seam.

**Tech Stack:** Go ≥1.22, stdlib + yaml.v3. No new deps.

## Global Constraints

- Spec: `docs/superpowers/specs/2026-07-10-phase2-2b-codex-harness-design.md` — §3-5, §8.
- **Zero behavior change.** The success criterion is that every pre-existing test in all 12 packages passes unchanged. Do not modify any existing test's assertions except where a signature it calls changed (then update the call, not the expectation). gofmt-clean, `go vet` clean.
- **claude-code stays the only registered harness** in 2b-i. `harness.For("codex")` must error until 2b-ii.
- **Parity is mandatory**: the claude-code adapter's `AllowedPaths()`/`GitignoreContent()`/`CredentialFiles()`/`SetupStateFilenames()`/`LoginSignatures()` must byte-match the values currently in `internal/stack/allowlist.go` and `internal/sanitize/scan.go`. A parity test guards this.
- Module `sherpa`. Commit after every task; messages `refactor:`/`test:`.

## Codex Delegation

Codex implements; controller verifies (`go test ./...`, gofmt, vet) and commits. Reviews: Opus (this is a refactor — mechanical, but the parity/security-signature routing matters). Codex dispatch (streaming, stdin closed):
```
codex exec --json --sandbox workspace-write "<task + constraints>" </dev/null 2>"$SCRATCH/err" | tee "$SCRATCH/jsonl" | jq -r 'select(.type=="item.started" or .type=="item.completed" or .type=="turn.completed" or .type=="turn.failed" or .type=="error") | .type+": "+(.item.type // .message // "")'
```
Offline env: `GOFLAGS=-mod=mod GOPROXY=off GOCACHE=/private/tmp/sherpa-go-build`. Codex cannot write `.git`.

---

### Task 1: Grow the Harness interface + registry + claude-code adapter values

**Files:**
- Modify: `internal/harness/harness.go` (interface + `For`/`Names`), `internal/harness/claudecode.go` (implement new methods)
- Test: `internal/harness/harness_test.go` (new), `internal/harness/claudecode_test.go` (parity additions)

**Interfaces:**
- Produces (consumed by Tasks 2-5):

```go
type Harness interface {
    Name() string
    Alias() string                              // short user-facing tag: "claude"; used by 2b-ii baseline naming
    // Isolation + launch
    ConfigDirEnv() string
    DefaultConfigDir(home string) string
    LaunchBin() string
    LaunchBinEnv() string                       // env var overriding LaunchBin in tests, e.g. "SHERPA_CLAUDE_BIN"
    // Machine-local, inherited, never published
    CredentialFiles() []string
    PrepareBaselineCredentials(baselineDir string) error // claude: keychain export; others: no-op
    SetupStateSources(configDir string) []string
    CapturedName() string
    Seed(captured []byte) (targetRel string, content []byte, err error)
    // Stack content
    AllowedPaths() []string
    GitignoreContent() string
    // Unpublishable barrier (per harness)
    SetupStateFilenames() []string
    LoginSignatures() []string
}
func For(name string) (Harness, error)
func Names() []string
func Default() Harness   // ClaudeCode{} (unchanged)
```

- [ ] **Step 1: Write failing tests** (`harness_test.go`)

```go
package harness

import "testing"

func TestForKnownAndUnknown(t *testing.T) {
	h, err := For("claude-code")
	if err != nil || h.Name() != "claude-code" {
		t.Fatalf("For(claude-code) = %v, %v", h, err)
	}
	if _, err := For("codex"); err == nil {
		t.Fatal("For(codex) must error until 2b-ii")
	}
	if _, err := For(""); err == nil {
		t.Fatal("For(empty) must error")
	}
}

func TestNamesListsClaudeCode(t *testing.T) {
	found := false
	for _, n := range Names() {
		if n == "claude-code" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Names() = %v, want claude-code", Names())
	}
}
```

Parity tests (`claudecode_test.go`): assert the adapter returns exactly the legacy values (copy the current literals from `internal/stack/allowlist.go` and `internal/sanitize/scan.go` into the test as the expected constants):

```go
func TestClaudeAdapterParity(t *testing.T) {
	h := Default()
	if h.ConfigDirEnv() != "CLAUDE_CONFIG_DIR" || h.LaunchBin() != "claude" || h.LaunchBinEnv() != "SHERPA_CLAUDE_BIN" {
		t.Fatalf("launch identity drift: %q %q %q", h.ConfigDirEnv(), h.LaunchBin(), h.LaunchBinEnv())
	}
	if got := h.CredentialFiles(); len(got) != 1 || got[0] != ".credentials.json" {
		t.Fatalf("cred files drift: %v", got)
	}
	if got := h.SetupStateFilenames(); len(got) != 2 || got[0] != ".claude.json" || got[1] != ".sherpa-setup.json" {
		t.Fatalf("setup filenames drift: %v", got)
	}
	wantSigs := []string{"oauthAccount", "claudeAiOauth", `"accessToken"`, `"refreshToken"`}
	if got := h.LoginSignatures(); !equalStrings(got, wantSigs) {
		t.Fatalf("login sig drift: %v", got)
	}
	// AllowedPaths + GitignoreContent parity: compare against the exact legacy literals.
	if h.DefaultConfigDir("/home/u") != "/home/u/.claude" {
		t.Fatalf("config dir drift: %q", h.DefaultConfigDir("/home/u"))
	}
}
func equalStrings(a, b []string) bool {
	if len(a) != len(b) { return false }
	for i := range a { if a[i] != b[i] { return false } }
	return true
}
```
Also assert `AllowedPaths()` and `GitignoreContent()` equal the legacy literals — paste the exact current `stack.AllowedPaths` slice and `stack.GitignoreContent` string into the test as `wantAllowed`/`wantGitignore` and compare.

- [ ] **Step 2: Verify fail** — `go test ./internal/harness/` → FAIL (methods/For undefined).
- [ ] **Step 3: Implement** — in `harness.go` add the registry:

```go
var registry = map[string]Harness{"claude-code": ClaudeCode{}}

func For(name string) (Harness, error) {
	h, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown harness %q (known: %v)", name, Names())
	}
	return h, nil
}
func Names() []string {
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
```

In `claudecode.go` add the methods, copying values from their current homes:

```go
func (ClaudeCode) Alias() string         { return "claude" }
func (ClaudeCode) ConfigDirEnv() string  { return "CLAUDE_CONFIG_DIR" }
func (ClaudeCode) LaunchBin() string     { return "claude" }
func (ClaudeCode) LaunchBinEnv() string  { return "SHERPA_CLAUDE_BIN" }
func (ClaudeCode) DefaultConfigDir(home string) string { return filepath.Join(home, ".claude") }
func (ClaudeCode) CredentialFiles() []string { return []string{".credentials.json"} }
func (ClaudeCode) SetupStateFilenames() []string { return []string{".claude.json", ".sherpa-setup.json"} }
func (ClaudeCode) LoginSignatures() []string {
	return []string{"oauthAccount", "claudeAiOauth", `"accessToken"`, `"refreshToken"`}
}
func (ClaudeCode) AllowedPaths() []string { return []string{ /* paste the exact current stack.AllowedPaths slice */ } }
func (ClaudeCode) GitignoreContent() string { return claudeGitignore }
const claudeGitignore = `... paste the exact current stack.GitignoreContent string ...`
```

`PrepareBaselineCredentials` moves the keychain-export logic (currently `launch.EnsureCredentialFile`) here — but to avoid a harness→launch dependency and keep the security-sensitive keychain call in one place, implement it in `claudecode.go` (it only needs `os`/`os/exec`, plus the `SHERPA_SECURITY_BIN` override env). Copy the body of `launch.EnsureCredentialFile` verbatim into `(ClaudeCode).PrepareBaselineCredentials`.

- [ ] **Step 4: Verify pass** — `go test ./internal/harness/` → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "refactor: harness interface owns all harness-specific values + For/Names registry"`

---

### Task 2: Route stack allowlist/gitignore + manifest.Validate through the harness

**Files:**
- Modify: `internal/stack/allowlist.go` (delegate or delete), `internal/stack/manifest.go` (Validate takes harness), `internal/cli/cmd_clone.go`, `internal/cli/cmd_init.go`
- Test: `internal/stack/manifest_test.go` (update Validate calls)

**Interfaces:**
- Consumes: `harness.For`, `harness.Harness.AllowedPaths/GitignoreContent` (Task 1).
- Produces: `(*Manifest).Validate(dir string, h harness.Harness) []string` — validates the stack against the harness (registered-harness check replaces the literal `"claude-code"` string). `stack.GitignoreContent`/`AllowedPaths` are removed; callers use `harness.For(name).GitignoreContent()`.

- [ ] **Step 1: Update failing tests** — in `manifest_test.go`, change every `m.Validate(dir)` to `m.Validate(dir, mustHarness(t, m.Harness))` with a helper `mustHarness` that calls `harness.For` and skips/adjusts for the `pi`/wrong-harness cases (the wrong-harness test now expects `harness.For` to fail → Validate reports the unknown-harness violation). Keep the existing violation expectations otherwise.
- [ ] **Step 2: Verify fail** — `go test ./internal/stack/` → FAIL (signature).
- [ ] **Step 3: Implement**:
  - `manifest.go`: `func (m *Manifest) Validate(dir string, h harness.Harness) (violations []string)`. Replace the `if m.Harness != "claude-code"` block with: if `h == nil || h.Name() != m.Harness` append a harness-mismatch violation (callers pass the harness resolved from `m.Harness`; a caller that couldn't resolve passes nil). Keep the hook/executable checks unchanged.
  - `allowlist.go`: delete `AllowedPaths` and `GitignoreContent` (now on the adapter). If `internal/stack` no longer references them, remove the file or leave only the doc comment.
  - `cmd_clone.go`: resolve `h, err := harness.For(m.Harness)` right after `stack.Parse`; on error abort with the message. Pass `h` to `m.Validate(staging, h)` and use `h.GitignoreContent()` in `enforceGitignore` (thread `h` into `enforceGitignore` or compute the content before calling it).
  - `cmd_init.go`: replace `stack.GitignoreContent` in the `profile.Import` call with `harness.Default().GitignoreContent()`.
- [ ] **Step 4: Verify pass** — `go test ./...` → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "refactor: route allowlist/gitignore/Validate through harness"`

---

### Task 3: Generalize the launcher

**Files:**
- Modify: `internal/launch/launch.go`, `internal/cli/cmd_run.go`, `internal/cli/cmd_try.go`, `internal/cli/cmd_profile.go`
- Test: `internal/launch/launch_test.go` (update calls), `internal/cli/cmd_run_test.go`

**Interfaces:**
- Consumes: `harness.Harness` (Task 1).
- Produces:
  - `launch.Launch(h harness.Harness, profileDir, baselineDir string, args []string, stdio Stdio) error` — replaces `Claude(...)`. Links `h.CredentialFiles()`, sets `h.ConfigDirEnv()=profileDir`, execs `h.LaunchBin()` (overridable via `h.LaunchBinEnv()`). Generalize `withClaudeConfigDir` → `withConfigDir(env, envName, profileDir)`.
  - `launch.EnsureCredentialFile` is **removed** from `launch` (moved to the adapter as `PrepareBaselineCredentials` in Task 1). Callers now call `h.PrepareBaselineCredentials(baselineDir)`.

- [ ] **Step 1: Update failing tests** — `launch_test.go`: change `Claude(profile, mine, files, args, stdio)` calls to `Launch(harness.Default(), profile, mine, args, stdio)` (credential files come from the harness now — drop the explicit files arg); the fake-claude tests still assert `CLAUDE_CONFIG_DIR` because the claude adapter's `ConfigDirEnv()` is that. Any test calling `EnsureCredentialFile` becomes `harness.Default().PrepareBaselineCredentials(dir)`.
- [ ] **Step 2: Verify fail** — `go test ./internal/launch/` → FAIL.
- [ ] **Step 3: Implement**:
  - `launch.go`: rename `Claude` → `Launch(h, profileDir, baselineDir, args, stdio)`; use `h.CredentialFiles()` for the link loop, `h.LaunchBin()` resolved through an env override reading `h.LaunchBinEnv()`, and `withConfigDir(os.Environ(), h.ConfigDirEnv(), profileDir)`. Delete `EnsureCredentialFile` and the claude-specific `bin()`/`withClaudeConfigDir` (replace with the generalized helpers). `SeedSetup` is unchanged (already harness-parameterized).
  - `cmd_run.go`: `h, err := harness.For(active.Harness)` (error → return); `h.PrepareBaselineCredentials(mine.Path)` (best-effort warn); `launch.Launch(h, active.Path, mine.Path, passthrough, stdio)`; SeedSetup call already passes a harness — change `harness.Default()` to `h`.
  - `cmd_try.go`: same pattern, resolve `h` from the profile being tried (its manifest/state harness) — for a freshly cloned profile use the clone's manifest harness; for an installed profile use its `state.Profile.Harness`. Use `h` for SeedSetup, PrepareBaselineCredentials, and Launch.
  - `cmd_profile.go`: resolve `h` from the target profile's harness; use `h.Seed`/`h.PrepareBaselineCredentials`/`launch.Launch`.
- [ ] **Step 4: Update `cmd_run_test.go`** if it referenced `launch.Claude`/`EnsureCredentialFile` names; behavior assertions unchanged.
- [ ] **Step 5: Verify** — `go test ./...` → PASS.
- [ ] **Step 6: Commit** — `git add -A && git commit -m "refactor: generalize launcher to launch.Launch(h, ...) routed by profile harness"`

---

### Task 4: Route the publish setup-state barrier through the harness

**Files:**
- Modify: `internal/sanitize/scan.go` (parameterize the filename/signature lists), `internal/cli/cmd_publish.go`
- Test: `internal/sanitize/scan_test.go` (update calls), `internal/cli/cmd_save_test.go` (unchanged assertions)

**Interfaces:**
- Consumes: `harness.Harness.SetupStateFilenames/LoginSignatures` (Task 1).
- Produces:
  - `sanitize.ScanSetupState(dir string, allowed, setupNames, loginSigs []string) ([]Finding, error)`
  - `sanitize.ScanPatchSetupState(patch string, setupNames, loginSigs []string) []Finding`
  - The package-level `setupStateNames`/`oauthSignatures` are removed; callers pass the harness's lists. Claude values unchanged → all publish tests stay green.

- [ ] **Step 1: Update failing tests** — `scan_test.go`: pass the claude lists explicitly to the two functions, e.g. `ScanSetupState(dir, allowed, []string{".claude.json", ".sherpa-setup.json"}, []string{"oauthAccount", "claudeAiOauth", "\"accessToken\"", "\"refreshToken\""})`. Same expectations.
- [ ] **Step 2: Verify fail** — `go test ./internal/sanitize/` → FAIL.
- [ ] **Step 3: Implement**:
  - `scan.go`: change the two signatures to take `setupNames, loginSigs []string`; build a local `map[string]bool` from `setupNames`; `containsOAuthSig` takes `loginSigs`. Delete the package-level `setupStateNames`/`oauthSignatures`.
  - `cmd_publish.go`: resolve `h, err := harness.For(profile.Harness)` (error → return); pass `h.SetupStateFilenames()`, `h.LoginSignatures()` to both `sanitize.ScanSetupState(profile.Path, scanFiles, ...)` and `sanitize.ScanPatchSetupState(historyPatch, ...)`. The generic `sanitize.Scan` call is unchanged.
- [ ] **Step 4: Verify** — `go test ./...` → PASS (all publish/security tests green with claude values).
- [ ] **Step 5: Commit** — `git add -A && git commit -m "refactor: publish setup-state barrier uses per-harness filenames/signatures"`

---

### Task 5: Thread harness through init/search + final green gate

**Files:**
- Modify: `internal/cli/cmd_init.go`, `internal/cli/cmd_search.go`
- Test: existing suite (no new behavior)

**Interfaces:**
- Consumes: `harness.Default()` (Task 1). No new produced API.

- [ ] **Step 1: Implement**:
  - `cmd_init.go`: `claudeDir()` → keep the `SHERPA_CLAUDE_DIR` override but derive the default from `harness.Default().DefaultConfigDir(home)`; the setup-state capture already loops `harness.Default().SetupStateSources(...)`. Init still hardcodes `Harness: "claude-code"` on the `mine` profile (2b-ii adds `--harness`); no behavior change.
  - `cmd_search.go`: replace the literal `const supportedSearchHarness = "claude-code"` usage so the filter uses `harness.Default().Name()` (still claude-code) — keeps a single source of truth without changing behavior.
- [ ] **Step 2: Verify** — `go test ./... -count=1` → all 12 packages PASS; `gofmt -l .` empty; `go vet ./...` clean.
- [ ] **Step 3: Add a cross-package guard test** (`internal/harness/registry_guard_test.go`): assert `len(Names()) == 1 && Names()[0] == "claude-code"` — a tripwire so 2b-ii consciously updates it when registering codex.
- [ ] **Step 4: Commit** — `git add -A && git commit -m "refactor: init/search read harness identity from the adapter; registry tripwire"`

---

## Execution order & dependencies

1 → 2 → 3 → 4 → 5, strictly sequential (each routes more call-sites through Task 1's interface). The whole-suite-green gate at every task is the refactor's safety net.

## Deliverables checklist (spec §4-5, §8)

- [ ] Harness interface owns config env, dir, bin, cred files, allowlist, gitignore, setup-state, unpublishable signatures (Task 1).
- [ ] `For`/`Names` registry; claude-code only; `For("codex")` errors (Tasks 1, 5 tripwire).
- [ ] `stack` allowlist/gitignore + `Validate` routed through harness (Task 2).
- [ ] `launch.Launch(h, …)` routed by profile harness; keychain export is a claude-adapter method (Tasks 1, 3).
- [ ] Publish barrier uses per-harness filenames/signatures (Task 4).
- [ ] Zero behavior change: all 12 packages green throughout; parity tests guard the claude values (Task 1).
