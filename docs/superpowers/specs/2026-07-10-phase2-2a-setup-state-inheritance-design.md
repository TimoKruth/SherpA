# SherpA Phase 2 · Sub-project 2a — Machine-Local Setup-State Inheritance

**Status:** Design for approval
**Date:** 2026-07-10
**Branch:** phase-2
**Parent spec:** `docs/superpowers/specs/2026-07-08-follow-the-expert-design.md` (§3.7)

## Phase 2 decomposition (context)

Phase 2 is six independently-shippable sub-projects, each getting its own spec → plan →
build cycle. Ordered so each ships value alone and unblocks the next:

| # | Sub-project | Depends on |
|---|---|---|
| **2a** | **Setup-state inheritance (this doc)** — inherit login/onboarding/defaults from `mine` | Phase 1 |
| 2b | Codex harness — second harness, proves the adapter abstraction | 2a |
| 2c | Registry API + backend — Forgejo storage + Postgres metadata on Railway | Phase 1 stack format |
| 2d | Website — browse/search/diff/stack pages | 2c |
| 2e | Follows + notifications | 2c, 2d |
| 2f | MCP server + trial journal | 2c, 2a |

2a and 2b are pure-CLI (no backend/accounts/hosting); the backend trio (2c–2e) can start in
parallel once 2a/2b move. This document specifies **2a only**.

## 1. Problem

Launching a profile under `CLAUDE_CONFIG_DIR` forces a full **re-login and re-onboarding**
(login flow, theme, first-run setup) every time, because Claude Code keeps its
setup/identity state in `~/.claude.json` — a file that sits *next to* `~/.claude/` in
`$HOME`, not inside it. `sherpa init` imports the *directory*, so it never captured that
file; a profile therefore has no setup state and the tool onboards from scratch. (Confirmed
in Phase-1 field testing 2026-07-09; see parent spec §3.7.)

## 2. Goals / non-goals

**Goals:**
1. A profile session inherits login + onboarding + CLI defaults from `mine` — no re-login,
   no re-onboarding.
2. Isolation preserved: no per-project trust, MCP approvals, or history leak between
   profiles.
3. An opt-in `--fresh-setup` deliberately runs the tool's own first-run flow in the
   isolated profile (different account per expert, onboarding testing, clean-room trials).
4. **Setup state is structurally unpublishable** — it carries your OAuth login and must be
   impossible to share by accident.
5. A reusable harness-adapter seam so 2b (Codex) implements the same behavior by declaring
   different files.

**Non-goals (2a):**
- Sharing any setup/preferences between users. Even a "safe" subset (theme, keybindings) is
  **out of scope** — deferred to a future sub-project that designs a provably-safe
  shareable-preferences format. For now, setup state never leaves the machine.
- Codex/other harnesses (2b+). 2a ships the Claude-Code adapter and the interface.
- The Config Vault / GUI tools (Phase 4).

## 3. Design

### 3.1 The harness adapter seam

New package `internal/harness`. This is the reusable abstraction 2b will implement for
Codex; 2a ships one implementation (Claude Code) and the interface.

```go
package harness

// Harness describes where a tool keeps machine-local setup/identity state and how
// to curate it for safe inheritance between isolated profiles.
type Harness interface {
    Name() string                              // "claude-code"
    // SetupStateSources returns absolute source paths (resolved from $HOME) that hold
    // setup/identity state and must be captured at init. e.g. ["~/.claude.json"].
    SetupStateSources() []string
    // CapturedName is the untracked filename the captured blob is stored under inside a
    // profile, e.g. ".sherpa-setup.json". Never in AllowedPaths.
    CapturedName() string
    // Seed produces the file to write into a launching profile's config dir from the
    // captured blob: a CURATED copy (identity/onboarding kept; projects + volatile caches
    // stripped). Returns (targetRelPath, content).
    Seed(captured []byte) (targetRelPath string, content []byte, err error)
}
```

Claude-Code adapter specifics:
- `SetupStateSources() = ["$HOME/.claude.json"]`
- `CapturedName() = ".sherpa-setup.json"`
- `Seed`: parse the captured `.claude.json`; **keep** an allowlist of identity/onboarding/
  preference keys (`hasCompletedOnboarding`, `oauthAccount`, `userID`, `machineID`,
  `installMethod`, `firstStartTime`, `lastOnboardingVersion`, theme/appearance keys, the
  `*MigrationComplete`/`*Migration*` flags, dismissed-callout flags); **drop** everything
  not on the allowlist — critically `projects` (per-project trust, MCP approvals, history)
  and the `*Cache*` keys. Write result to target rel path `.claude.json`. Unknown future
  keys default to **dropped** (whitelist, not blacklist).

> Note: `oauthAccount` is kept because it is the account *identity* Claude reads alongside
> `.credentials.json`; the actual token stays in `.credentials.json` (already linked, §3.2).
> Both are machine-local and unpublishable (§3.3).

### 3.2 Capture at init, seed at launch

- **`sherpa init`** (extend `internal/cli/cmd_init.go` + `internal/profile`): after importing
  `~/.claude/` into `mine`, for each `SetupStateSources()` path that exists, copy it into
  `mine/<CapturedName()>` (0600, untracked). Missing source → skip (no error; a machine that
  never onboarded simply has nothing to inherit yet).
- **`sherpa init --refresh`**: re-capture the setup file into an existing `mine` without
  re-importing the whole directory. (For when you log in / change theme after init.)
- **Launch** (extend `internal/launch` + `cmd_run.go`/`cmd_try.go`): before exec, if the
  target profile has no `.claude.json` AND mine has a captured blob AND not `--fresh-setup`,
  compute `Seed(captured)` and write it into the profile (0600, untracked). Never overwrite
  an existing profile `.claude.json` (a prior session's live state wins). Best-effort: on
  seed failure, warn to stderr and continue (you get onboarding, not a crash) — same posture
  as credential prep.

### 3.3 Unpublishability — defense in depth (the security requirement)

The captured/seeded setup state carries OAuth login. Three independent barriers ensure it
can never reach a published stack:

1. **Not in the allowlist.** `CapturedName()` (`.sherpa-setup.json`) and the profile's live
   `.claude.json` are absent from `stack.AllowedPaths`, and the canonical
   `stack.GitignoreContent` does not un-ignore them. (Phase-1 already made `enforceGitignore`
   always write the canonical whitelist, so an author cannot tamper the gitignore to
   un-ignore them — that hole is already closed.)
2. **Publish hard-block (new).** `sherpa publish` fails closed if any of these appear in the
   files it would push: a file named `.claude.json` or `.sherpa-setup.json` (any directory),
   OR content matching an OAuth/login signature (`oauthAccount`, `claudeAiOauth`,
   `accessToken`/`refreshToken` keys). This is a dedicated check, not reliant on the generic
   secret scanner, and has **no override flag**. It runs over both the working tree and the
   pushed history range (reusing the Phase-1 history scan).
3. **Credential/setup files are 0600 and untracked by construction** — `save`'s `git add -A`
   cannot stage them because the gitignore excludes them (barrier 1); barrier 2 is the
   backstop if barriers 1 and the enforcement ever regress.

Test that a profile which has been launched (so it has a live `.claude.json` with a real-ish
oauth structure) **cannot** publish — publish must abort citing the setup-state block, and
the remote must receive nothing.

### 3.4 `--fresh-setup`

- `sherpa try --fresh-setup <target>` and `sherpa run --fresh-setup`: skip seeding; the tool
  runs its own first-run flow inside the isolated profile. `back` still reverts instantly;
  `mine` untouched.
- Persistent variant `sherpa profile setup <name>`: same as launching that profile with
  `--fresh-setup` once (removes any seeded `.claude.json` first so onboarding runs), for a
  profile you want permanently on a different account.
- Fresh-setup writes stay inside the profile; they are never copied back to `mine`.

### 3.5 `mine` stays sacred

The captured `mine/.sherpa-setup.json` is written only by `init` / `init --refresh`. No
launch, seed, or profile session ever writes back into it. Seeding reads mine's blob and
writes into the *profile*, never the reverse.

## 4. Files touched

- Create: `internal/harness/harness.go` (interface), `internal/harness/claudecode.go`
  (adapter), `internal/harness/claudecode_test.go`.
- Modify: `internal/profile/importer.go` (capture setup sources on import),
  `internal/cli/cmd_init.go` (`--refresh` flag; wire capture),
  `internal/launch/launch.go` (seed-before-exec + `--fresh-setup` skip),
  `internal/cli/cmd_run.go`, `internal/cli/cmd_try.go` (`--fresh-setup` flag; pass through),
  `internal/cli/cmd_publish.go` (setup-state hard-block).
- Create: `internal/cli/cmd_profile.go` (`sherpa profile setup <name>`).
- Tests alongside each.

## 5. Error handling / invariants

- Missing setup source at init → skip silently (nothing to inherit).
- Seed parse failure → warn + continue (onboarding, not crash).
- Never overwrite a profile's existing live `.claude.json`.
- `mine` setup blob is write-once per `init`/`--refresh`; never written from a session.
- Publish is fail-closed on setup-state presence, no override.

## 6. Testing strategy

- `harness` unit: `Seed` keeps identity/onboarding keys, strips `projects` + `*Cache*`,
  drops an unknown key; deterministic output.
- `init` captures `~/.claude.json` → `mine/.sherpa-setup.json` (fixture $HOME); `--refresh`
  re-captures; missing source → no file, no error.
- launch seeds `.claude.json` into a profile lacking one; skips when present; skips on
  `--fresh-setup`; never writes mine.
- **security**: a launched profile with a live oauth-bearing `.claude.json` cannot be
  published — publish aborts on the setup-state block; remote gets no tag. Also: a stack
  whose `.gitignore` tries to un-ignore `.sherpa-setup.json` still cannot (barrier 1) and
  still cannot publish it (barrier 2).
- isolation: two profiles seeded from the same mine do not share `projects`/trust.
- full-loop: extend the Phase-1 integration test — after init+try, the fake-claude sees a
  seeded `.claude.json` (assert `hasCompletedOnboarding` present, `projects` absent).

## 7. Alternatives considered

- **Keep `projects`, strip only its MCP approvals.** More convenient (no trust re-prompt) but
  more complex and riskier (MCP bypass surface). Rejected for 2a; revisit only if the
  per-project trust re-prompt proves annoying in practice.
- **Symlink mine's `.claude.json` into profiles.** Rejected: Claude mutates the file at
  runtime, so a symlink would write per-project state back into mine (violates §3.5) and
  share state across profiles (violates isolation).
- **Blacklist bad keys instead of whitelisting good ones.** Rejected: a new Claude release
  could add a sensitive key we don't know to exclude. Whitelist fails safe.

## 8. Open question (for approval)

Curation policy confirmed by user 2026-07-10: **strip `projects` entirely**; setup state is
**not shareable at all** in 2a (a safe shareable-preferences subset is a separate future
sub-project). No open questions remain.
