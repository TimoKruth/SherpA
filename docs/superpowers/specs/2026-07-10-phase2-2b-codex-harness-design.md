# SherpA Phase 2 · Sub-project 2b — Codex Harness (multi-harness generalization)

**Status:** Design for approval
**Date:** 2026-07-10
**Branch:** (to be created) phase-2b
**Parent specs:** `2026-07-08-follow-the-expert-design.md` (§10.5 harness roadmap), `2026-07-10-phase2-2a-setup-state-inheritance-design.md` (§3.1 harness seam)

## 1. Goal

Add **Codex CLI** as the second harness, proving the adapter abstraction and delivering the
2026-07-08 decision to "go fast to at least Codex." A user can `init`, `clone`, `try`,
`back`, `save`, `diff`, `update`, and `publish` Codex stacks exactly as they do Claude-Code
stacks — the flows are identical; only the harness-specific files differ.

## 2. The core problem: Phase 1/2a hardcode claude-code

Today, "claude-code" is baked into many places: `claudeDir()`, `launch.Claude` (binary
`claude`, env `CLAUDE_CONFIG_DIR`), `launch.CredentialFiles` (`.credentials.json`),
`stack.AllowedPaths` / `stack.GitignoreContent` (CLAUDE.md/skills/agents), the
`harness.ClaudeCode` setup-state adapter, and `manifest.Validate` (rejects non-claude-code).
2b makes every one of these **harness-driven**, selected by a profile's / stack's declared
harness.

## 3. Internal build order (de-risks a large refactor)

**2b-i — Generalize + refactor (this spec's plan; zero behavior change).** Grow
`internal/harness.Harness` to own *all* harness-specific concerns; route every hardcoded
claude-code path through `harness.For(name)`; ship exactly one registered harness
(claude-code). Every existing test stays green — this is a pure refactor with no new
behavior. Low risk, and it creates the clean seam 2b-ii plugs into.

**2b-ii — Codex adapter (separate plan, after a spike).** A day-1 `CODEX_HOME` spike (like
Phase 1's) empirically pins Codex's config-dir coverage, credential file, and global-
instructions file. Then: the Codex adapter, `sherpa init --harness codex`, per-harness
baseline, `manifest.Validate` accepting codex, and harness-aware `back`. 2b-ii's task-level
plan is written after the spike, because accurate adapter tasks require verified internals.

## 4. The generalized Harness interface (2b-i)

```go
package harness

type Harness interface {
    Name() string                            // "claude-code" / "codex"
    Alias() string                           // short user-facing tag: "claude" / "codex" (baseline naming §6)

    // Isolation + launch
    ConfigDirEnv() string                    // "CLAUDE_CONFIG_DIR" / "CODEX_HOME"
    DefaultConfigDir(home string) string     // filepath.Join(home, ".claude") / ".codex"
    LaunchBin() string                       // "claude" / "codex"; overridable via a per-harness test env

    // Machine-local, inherited from the harness's baseline, never published
    CredentialFiles() []string               // [".credentials.json"] / ["auth.json"]
    SetupStateSources(configDir string) []string
    CapturedName() string
    Seed(captured []byte) (targetRel string, content []byte, err error) // may return ("", nil, nil) = nothing to seed

    // Stack content (tracked / publishable)
    AllowedPaths() []string                  // per-harness allowlist
    GitignoreContent() string                // whitelist derived from AllowedPaths

    // Unpublishable barrier (2a §3.3), per harness
    SetupStateFilenames() []string           // basenames that must never be pushed: [".claude.json",".sherpa-setup.json"] / ["auth.json"]
    LoginSignatures() []string               // content signatures: claude oauth set / codex token set
}

func For(name string) (Harness, error)       // registry; error on unknown harness
func Default() Harness                        // claude-code (back-compat)
func Names() []string                         // registered harness names
```

Registry: a package-level `map[string]Harness`; `For` returns a clear error for an
unregistered harness. `AllowedPaths`/`GitignoreContent`/setup-state move **out of**
`internal/stack` and `internal/launch` into the adapter; `stack` and `launch` call through
the harness they're handed.

## 5. Routing by harness (2b-i)

- **State**: `state.Profile.Harness` already exists and is populated. Every operation that is
  currently claude-code-specific resolves its harness via `harness.For(profile.Harness)`.
- **Launch** (`internal/launch`): `Claude(...)` becomes `Launch(h harness.Harness, profileDir,
  baselineDir string, args, stdio)` — sets `h.ConfigDirEnv()=profileDir`, execs `h.LaunchBin()`,
  links `h.CredentialFiles()`, seeds via `h.Seed`. The claude-specific `EnsureCredentialFile`
  keychain export stays claude-only (a Claude-adapter method; Codex uses `auth.json` directly,
  no keychain export — to be confirmed by the 2b-ii spike).
- **Clone / validate / gitignore**: `installStack` and `enforceGitignore` take the stack's
  declared harness and use `h.AllowedPaths()` / `h.GitignoreContent()`. `manifest.Validate`
  gains the harness and checks against `harness.For(m.Harness)` instead of the literal
  "claude-code" (2b-i keeps only claude-code registered, so behavior is unchanged; 2b-ii adds
  codex).
- **Publish**: the setup-state barrier (2a) uses `h.SetupStateFilenames()` /
  `h.LoginSignatures()` for the harness of the profile being published. The exact-pushed-set
  scan (`git ls-files`) is harness-agnostic and unchanged.

## 6. The `mine` / baseline model with multiple harnesses (2b-ii — decided 2026-07-10)

**One baseline profile per harness, with the name kept unambiguous at all times** (user
decision): while only one harness has a baseline it is simply `mine`; the instant a second
harness is added, every baseline becomes harness-tagged so it is never unclear which `mine`
is which.

- **Harness alias**: each adapter exposes a short `Alias()` for user-facing names —
  claude-code → `claude`, codex → `codex`.
- `state` gains `Baselines map[string]string` (harness name → baseline profile name).
- **Naming rule (dynamic):**
  - First baseline ever created (any harness): named `mine`.
  - Creating a baseline for a *second* harness triggers a rename: the existing lone `mine`
    is renamed to `mine-<alias-of-its-harness>` **and** the new one is created as
    `mine-<alias-of-new-harness>`. So with ≥2 harnesses, both are `mine-claude`,
    `mine-codex`, etc. — never a bare `mine`.
  - Any baseline created while ≥1 already exists is directly named `mine-<alias>`.
  - Renaming updates the profile's state entry, the `Baselines` map, the `Active` pointer if
    it referenced the renamed profile, and renames the on-disk `profiles/<name>` directory
    (updating `state.Profile.Path`). It never rewrites the profile's git history.
- `sherpa init` creates the claude-code baseline (default harness); `sherpa init --harness
  codex` creates the codex baseline, applying the naming rule (renaming an existing lone
  `mine` as needed). `--refresh` re-captures an existing baseline.
- **`back` and credential/setup seeding resolve the baseline for the active profile's
  harness** via `Baselines[active.Harness]`, never a hardcoded "mine". `back` on a codex
  profile returns to the codex baseline.
- A single global `Active` pointer is kept (Phase-1 model). `run`/`try` launch the active
  profile under *its* harness. Rationale: Claude Code and Codex are separate tools you run
  separately; the active pointer is "what SherpA launches next," and its harness is intrinsic
  to the profile.
- Out of scope: renaming *back* to a bare `mine` when a harness is removed (there is no
  remove-harness command yet); once tagged, baselines stay tagged.

Alternative considered: a separate active pointer per harness. Rejected as over-engineered —
one global active with harness-intrinsic profiles plus the `Baselines` map keeps `back`
correct. Revisit only if usage shows the single pointer is confusing.

## 7. Codex adapter — hypothesis (2b-ii, pinned by the spike)

From Codex docs (to verify): `CODEX_HOME` overrides `~/.codex`, rooting `config.toml`,
`auth.json` (credentials), `AGENTS.md` (+`AGENTS.override.md`, global instructions),
`rules/*.md`, and runtime `history.jsonl`/`logs/`/`sessions/`.

Provisional adapter (spike confirms/adjusts):
- `ConfigDirEnv`=`CODEX_HOME`, `DefaultConfigDir`=`~/.codex`, `LaunchBin`=`codex`.
- `CredentialFiles`=`["auth.json"]`; `SetupStateSources`/`Seed`: likely **none** (Codex's
  logged-in state is `auth.json`, already linked; no separate curated onboarding file) — so
  `Seed` returns `("", nil, nil)` and `SetupStateFilenames`=`["auth.json"]`.
- `AllowedPaths`=`["stack.yaml","README.md","CHANGELOG.md","AGENTS.md","AGENTS.override.md",
  "config.toml","rules/","hooks/","quarantine.json"]` (spike confirms which load from
  `CODEX_HOME`); `GitignoreContent` derived.
- `LoginSignatures`: Codex token/key markers in `auth.json` (spike pins exact keys).
- **Security note**: `config.toml` is a *publishable* stack file (the expert's model/settings)
  but can carry `[model_providers]` with API keys — the existing sanitizer content-scan
  already covers keys; `auth.json` is the machine-local credential and is unpublishable via
  the barrier.

## 8. Files touched (2b-i)

- Move into `internal/harness`: allowlist/gitignore (from `internal/stack/allowlist.go`),
  credential-file list + launch specifics (from `internal/launch`).
- Modify: `internal/stack/manifest.go` (Validate takes/uses the harness), `internal/launch/*`
  (generalized `Launch`), `internal/cli/*` (cmd_init, cmd_run, cmd_try, cmd_clone, cmd_publish,
  cmd_profile — resolve harness via profile/stack), keeping claude-code the only registered
  harness so all tests pass unchanged.
- Tests: a harness-registry test; adapter-parity tests; every existing test stays green
  (the refactor's proof of "no behavior change").

## 9. Testing strategy

- **2b-i**: the whole existing suite (12 packages) must stay green — that is the refactor's
  success criterion. Add: `harness.For`/`Names`/registry tests; a test that the claude-code
  adapter's `AllowedPaths`/`GitignoreContent` byte-match the values `internal/stack` exposed
  before the move (no drift); launch routes through the adapter's env/bin.
- **2b-ii**: the `CODEX_HOME` spike doc; Codex adapter unit tests; a codex clone/try/back
  round-trip with a fake `codex` binary (mirroring the fake-claude pattern); `auth.json`
  unpublishable test; per-harness `back` test; a codex stack fixture in the integration test.

## 10. Decision log / open questions

1. **Build order** (decided): 2b-i refactor (zero behavior change) → 2b-ii codex-after-spike.
2. **Baseline model** (decided 2026-07-10, §6): one baseline per harness; a lone baseline is
   `mine`, but adding a second harness renames both to `mine-<alias>` (`mine-claude`,
   `mine-codex`) so it is never ambiguous which `mine` is which; `back` resolves by the active
   profile's harness via `state.Baselines`; single global active pointer.
3. **Codex setup-state** (spike-pinned): expected to be `auth.json`-only (credential link, no
   curated seed) — the spike confirms whether Codex has any `~/.claude.json`-analog to seed.
4. **`config.toml` publishability** (decided): publishable stack file, sanitized by the
   existing content scan; `auth.json` is the unpublishable machine-local credential.
