# "Follow the Expert" — Design for an Agent-Setup Sharing Platform

**Status:** Draft for discussion
**Date:** 2026-07-08
**Working title:** *Sherpa* (placeholder — an expert who guides you up the mountain)

## 1. Problem

Power users of coding agents (Claude Code first, Codex/Cursor later) build elaborate setups:
global instructions (CLAUDE.md), skills, subagents, hooks, MCP servers, settings, keybindings.
These setups meaningfully change agent performance — but today there is no way to:

- **discover** which experts have great setups,
- **try** an expert's complete setup with one command,
- **compare** whether it actually performs better than your own,
- **revert** instantly if it doesn't,
- **fork** it with your own modifications while still tracking the expert's updates,
- **get notified** when an expert you follow improves their setup.

Closest existing things (dotfiles repos, awesome-lists, MCP registries, Claude Code plugin
marketplaces) each cover a fragment: none cover the *complete profile* with the
clone/try/revert/fork/follow lifecycle.

## 2. Goals and non-goals

**Goals (v1):**

1. Experts can publish their complete Claude Code setup as a versioned, sanitized **Stack**.
2. Anyone can `sherpa try @expert` and be running that setup in under a minute.
3. Reverting to your own setup is one command and always safe (`sherpa back`).
4. Your modifications to an expert's stack are saved as a **fork that tracks upstream**.
5. Followers are notified of updates and can review a diff before merging.
6. A website renders stacks human-readably (browse, search, diff, changelog, follow).
7. Agents can search the registry themselves via an **MCP server**.

**Non-goals (v1):**

- Multi-harness support (Codex/Cursor/Gemini profiles) — architecture allows it, v1 is Claude Code only.
- Automated benchmark leaderboards — v1 ships a manual A/B comparison flow; community benchmarks are Phase 3.
- Monetization/paid stacks.
- Hosting arbitrary MCP servers — stacks *reference* MCP servers, they don't run them server-side.

## 3. Core concepts

### 3.1 Stack — the unit of sharing

A **Stack** is a git repository that mirrors a Claude Code config directory, plus a manifest:

```
my-stack/
├── stack.yaml          # manifest (see below)
├── README.md           # expert's description: philosophy, what's in it, how to use it
├── CHANGELOG.md        # human-readable changes per version
├── CLAUDE.md           # global instructions
├── settings.json       # sanitized settings
├── keybindings.json
├── skills/…            # skills (or plugin references)
├── agents/…            # subagent definitions
└── hooks/…             # hook scripts
```

`stack.yaml` (manifest):

```yaml
name: rust-reviewer
owner: "@jane"
version: 14                    # monotonically increasing; git tag v14
harness: claude-code
harness_min_version: "2.0"
summary: "Setup tuned for Rust code review and refactoring"
tags: [rust, code-review, refactoring]

# Everything that executes or talks to the network MUST be declared here.
# The CLI refuses to install a stack whose files contain undeclared hooks/MCP servers.
executes:
  hooks:
    - path: hooks/pre-commit-check.sh
      event: PreToolUse
      purpose: "Blocks edits to generated files"
  mcp_servers:
    - name: github
      transport: stdio
      command: "npx @modelcontextprotocol/server-github"
      purpose: "PR review tools"

# Personal values are parameters, never literals. Filled at install time
# from the installer's env/keychain; publish-time scanner enforces this.
parameters:
  - name: GITHUB_TOKEN
    description: "Token for the github MCP server"
    required: false
```

### 3.2 Profiles — the technical core of clone/revert

Claude Code supports `CLAUDE_CONFIG_DIR`. Sherpa exploits this so that **your own setup is
never mutated**:

```
~/.sherpa/
├── profiles/
│   ├── mine/               # symlink or import of your real ~/.claude (read-only to sherpa)
│   ├── jane-rust-reviewer/ # cloned stack = git repo = a complete config dir
│   └── simonw-writing/
├── active                  # which profile new sessions use
└── state.json              # follows, trial journal, last-update-check
```

- `sherpa try/clone` creates a new profile directory; it never writes into `~/.claude`.
- Switching = pointing `CLAUDE_CONFIG_DIR` at a profile. `sherpa try` does this by
  launching `claude` **as a subprocess** with the env var set (a child process cannot
  export into its parent shell); persistent switching (`sherpa use`) goes through a shell
  wrapper/alias that reads `~/.sherpa/active`.
- **`sherpa back` never loses data** — it just switches the pointer back to `mine`, and
  works offline. Scope honestly stated: it governs *new* sessions; already-running Claude
  sessions or IDE integrations keep their profile until restarted, and sherpa warns when
  it detects them.
- **Tracked vs. runtime split**: Claude Code writes session state, caches, logs, plugin
  installs, and history into its config dir. A profile therefore separates the *stack*
  (git-tracked, allowlisted by the manifest) from *runtime state* (everything else,
  ignored via a whitelist-style `.gitignore`: ignore all, un-ignore manifest paths).
  Without this split, `save`/`diff`/`publish`/dirty-detection would drown in noise and
  risk leaking private state.

Secrets and machine-local values stay outside stacks; parameters resolve from the user's
environment/keychain at profile-build time, so an installed profile's *tracked* files
contain no secrets. Auth: the config dir also holds credentials/OAuth state, so a naive
fresh profile would force re-login. Sherpa links **only the specific credential files**
(never state files like project trust or MCP config, which Claude mutates at runtime) from
`mine` into each profile — untracked, never published, and on macOS mostly Keychain-backed
anyway.

**Day-1 spike (Phase 1, before anything else):** empirically verify exactly what
`CLAUDE_CONFIG_DIR` covers across Claude Code versions — settings, skills, agents, hooks,
plugins, the `.claude.json` state file, credentials — and pin the manifest allowlist and
credential-link list to those findings. The whole design rests on this isolation being
real and complete.

### 3.3 Fork & overlay — git branches, not custom formats

Every installed stack profile is a git clone of the expert's repo:

- `upstream/main` — the expert's published history (one commit per version, tagged).
- `local` — your branch. Any modification you make (`sherpa save` commits it, or the
  post-session hook detects dirty state and offers to commit) lives here.

This buys, for free:

- **Diff**: `sherpa diff` = `git diff upstream/main...local` (also rendered on the website).
- **Update**: `sherpa update` = fetch + show changelog + diff, then merge/rebase your
  `local` onto the new version. Merging is format-aware: semantic (key-level) merge for
  JSON/YAML settings, line merge for Markdown/scripts. Conflicts use a guided flow — each
  one presented in plain language ("Jane changed the code-review skill you also edited")
  with *keep mine / take expert's / edit* choices, and the agent can be asked to propose a
  merge. Your version is always recoverable.
- **Rollback within a stack**: `sherpa rollback` moves the profile to a previous state
  *without discarding local commits* — before every mutating operation (update, rollback,
  merge) sherpa records a backup ref, and destructive resets are simply not offered.
  Update merges run in a temporary worktree and are swapped in atomically on success.
- **Publishing your fork**: your `local` branch can itself be published as a new stack with
  a recorded `forked_from: @jane/rust-reviewer@v14` lineage (visible on the site).

### 3.4 Sanitization — publisher side

`sherpa publish` runs a pipeline before anything leaves the machine:

1. **Path scrub**: rewrite `/Users/<name>/…` to portable forms or parameters.
2. **Secret scan**: entropy + pattern detection (same class of rules as gitleaks) over every
   file; hard-block on findings, no override flag.
3. **Personal-data lint**: emails, real names, project names in settings — warn and prompt.
4. **Review diff**: show exactly what will be published, file by file, before upload.
5. **Executable-surface extraction**: auto-generate the `executes:` section from the files;
   publisher confirms each entry and writes its `purpose`.

### 3.5 Trust model — installer side

Stacks contain hooks (arbitrary shell) and MCP server configs (arbitrary processes). This is
the platform's biggest risk and is treated as a first-class feature, not a disclaimer:

- **One-command clone must not mean one-command execute**: hooks and MCP servers are
  installed *disabled by default* — and this is enforced structurally, not by a dialog:
  sherpa materializes the profile with hook and MCP entries **stripped out of the active
  settings** and held in a quarantine section of the profile; approving a capability in
  the review gate is what writes it into the live config. A stack with undeclared
  executables fails validation server-side at publish and client-side at install. The UX
  target is "trying a browser extension with visible permissions", not `npm install`.
- **Env scrubbing in try mode**: declared parameters are the only environment values
  passed through to a try-mode session where feasible; hooks can otherwise read whatever
  the user's shell exposes. Sandboxed/container try is recommended for unreviewed stacks.
- **Registry scanning — a risk signal, not a guarantee**: on publish, the registry re-runs
  the secret scan, lints hook scripts, and flags suspicious patterns (network calls in
  hooks, `rm -rf`, curl-pipe-sh) for human review before listing. Detecting malicious
  shell is undecidable in general and the product must never present scans as proof of
  safety; the honest promises are *visibility* (nothing executes undeclared or
  unapproved) and *reversibility* (`back` always works).
- **Trust tiers**: `unreviewed` → `scanned` → `verified expert` (identity-checked, e.g.
  linked GitHub with history). Search defaults to scanned-or-better.
- **Try mode is the default on-ramp**: `sherpa try` launches a session as a subprocess
  under the staged profile (no persistent switch — exiting it is reverting) and, where
  available, suggests running inside a sandboxed worktree/container for the first session.
- **Versions are immutable** once published; following an expert never auto-applies updates.

## 4. System components

```
┌────────────┐   publish/pull    ┌──────────────────────────────┐
│ sherpa CLI │◄─────────────────►│ Registry API (REST)          │
│ (+ MCP srv)│                   │  - metadata: Postgres        │
└─────┬──────┘                   │  - content:  git repos       │
      │ CLAUDE_CONFIG_DIR        │  - scan pipeline, webhooks   │
┌─────▼──────┐                   └───────────┬──────────────────┘
│ Claude Code│                               │
└────────────┘                   ┌───────────▼──────────────────┐
                                 │ Website (Next.js)            │
                                 │ browse/search/diff/follow    │
                                 └──────────────────────────────┘
```

### 4.1 CLI (`sherpa`) — the heart of the product

| Command | Behavior |
|---|---|
| `sherpa init` | Import `~/.claude` as the protected `mine` profile |
| `sherpa search <query>` | Search registry (also available to agents via MCP) |
| `sherpa show @jane/rust-reviewer` | Render manifest, README, executable surface, stats |
| `sherpa try @jane/rust-reviewer` | Clone → review gate → launch session under profile (subprocess; exit = revert) |
| `sherpa clone @jane/rust-reviewer` | Same, but persistent profile + auto-follow |
| `sherpa back` | Switch active profile to `mine`. Always instant, always safe |
| `sherpa use <profile>` | Switch between installed profiles |
| `sherpa save [-m msg]` | Commit your modifications on the `local` branch |
| `sherpa diff [@expert]` | Your fork vs upstream, or your `mine` vs an expert's stack |
| `sherpa update [<profile>]` | Fetch new version, show changelog+diff, guided merge |
| `sherpa publish` | Sanitize → review → push a new version of your stack |
| `sherpa follow/unfollow @expert` | Manage follows |
| `sherpa status` | Active profile, dirty state, pending updates for follows |
| `sherpa compare "task…"` | A/B: run the same task under two profiles (§4.5) |

Distribution: `npm i -g sherpa-cli` / Homebrew; single static binary preferred (Go or Rust)
so it works before any language runtime exists.

### 4.2 Registry (backend)

- **Content storage**: each stack is a bare git repo (start: a GitHub org / user-linked
  repos with the registry holding refs+metadata; keeps hosting cost near zero and gives
  experts ownership. The API abstracts this so self-hosted git can replace it later).
- **Metadata (Postgres)**: users, stacks, versions (immutable, with manifest snapshot and
  scan results), follows, stars, install/try counts, fork lineage, notification queue.
- **API (REST + webhooks)**: search, stack detail, version list, publish (upload → scan →
  list), follow management, update feed. Auth via GitHub OAuth; CLI uses device-code flow.
- **Scan pipeline**: async workers re-validating every published version (§3.4/3.5).

### 4.3 Website

- **Home/browse**: search + filters (tags, harness, trust tier), trending, most-followed.
- **Expert profile**: bio, stacks, followers, fork lineage graph.
- **Stack page**: rendered README, *full contents browser* (CLAUDE.md, each skill/agent/hook
  rendered readably), executable-surface panel ("this stack runs these 2 hooks…"),
  changelog, version history with diffs, follower count, **copy-paste install command**
  (`sherpa try @jane/rust-reviewer`).
- **Diff viewer**: version-to-version, and (logged in) *your fork vs upstream*.
- **My dashboard**: profiles installed, follows with pending updates, trial journal.
- Stack: Next.js + the registry API. Server-rendered so stack pages are indexable —
  discovery via Google is a growth channel.

### 4.4 Agent access — MCP server

`sherpa mcp` ships with the CLI and exposes the registry *to the agent itself*:

- `search_stacks(query, tags)` — find experts relevant to the current task.
- `inspect_stack(ref)` — manifest, README, executable surface (so the agent can summarize
  what it does and what it would run).
- `list_follow_updates()` — pending updates for the user's follows.
- `stage_stack(ref)` — download + validate into a staged profile, **returning the review
  report; activation always requires explicit human confirmation in the CLI.** The MCP
  surface is read-only by design: an agent can find and stage, never activate or publish.

This enables: "Claude, find me a well-regarded setup for embedded Rust and tell me how it
differs from mine" — the agent searches, inspects, runs `sherpa diff`, and reports.

### 4.5 "Does it actually perform better?" — comparison flow

The founding motivation. v1 keeps it honest and manual; automation comes later:

1. **A/B trial**: `sherpa compare --profiles mine,jane-rust-reviewer "refactor X / review PR Y"`
   runs the same prompt in two fresh sessions (separate git worktrees when in a repo, so
   outputs can't collide), one per profile, and presents both transcripts/diffs side by side.
2. **Trial journal**: while a non-`mine` profile is active, `sherpa back` and session-end
   ask one question — *keep, keep-with-notes, or revert?* — building a personal record of
   what actually helped. Aggregated (opt-in, anonymous) keep-rates feed the site's ranking.
3. **Context-fit ranking, not star leaderboards**: setups are context-sensitive, so the
   site ranks within declared fit ("works well for large TypeScript monorepos") using tags,
   harness version, keep-rate-after-trial, and follower retention — never a single global
   "best setup" list.
4. **Phase 3 — community benchmarks**: standardized task suites per domain, run in CI
   against stack versions, published as scores on the stack page. Explicitly out of v1
   scope: designing fair agent benchmarks is its own project.

### 4.6 Notifications

- **Registry side**: on publish, fan out to followers — web feed + optional email digest
  (immediate/daily/weekly).
- **Local side**: `sherpa status` shows pending updates; an optional SessionStart hook
  prints a one-liner at most once per day per stack:
  `⬆ @jane/rust-reviewer v14 → v15: "tightened review checklist". Run: sherpa update`.
- Never auto-apply. The update command always shows changelog + diff first.

## 5. Data model (registry, abbreviated)

```
users(id, handle, github_id, display_name, trust_tier, created_at)
stacks(id, owner_id, name, summary, tags[], harness, forked_from_version_id?, created_at)
stack_versions(id, stack_id, version, git_tag, manifest jsonb, scan_report jsonb,
               changelog_md, published_at)          -- immutable
follows(user_id, stack_id, notify_pref, created_at)
events(id, type, stack_version_id, actor_id, created_at)   -- feed + notification source
trial_feedback(user_id, stack_version_id, verdict, notes?, created_at)  -- opt-in
```

Client state (`~/.sherpa/state.json`): active profile, installed profiles with their
upstream refs, follow cache, last notification check, trial journal.

## 6. Error handling & safety invariants

1. **`mine` is sacred**: sherpa never writes into the user's original config; `sherpa back`
   is a pure pointer switch and works even if the registry is down or a profile is corrupt.
2. **Interrupted install/update** leaves the previously active profile active (staging dir
   + atomic rename; git operations happen in the staged copy).
3. **Failed merge on update**: `local` branch is untouched until the merge commit succeeds;
   `sherpa update --abort` returns to the pre-update state.
4. **Registry unavailable**: everything already installed keeps working; only
   search/publish/update need the network.
5. **Publish is fail-closed**: any secret-scan hit or undeclared executable blocks publish.

## 7. Alternatives considered

**A. Pure GitHub convention, no backend** — experts tag repos `sherpa-stack`; a static site
indexes them; the CLI clones directly.
*Pros*: near-zero cost, ships in days, validates demand. *Cons*: no scan pipeline or trust
tiers (the differentiator vs. "just share your dotfiles"), no follow notifications beyond
GitHub watch, no fork lineage or install stats. **Rejected as the end state, but Phase 1
deliberately stays close to it** (GitHub-backed storage) so we get its cheapness.

**B. Registry + website + CLI + MCP (chosen)** — described above. The registry owns
metadata, scanning, follows, and notifications; git owns content and versioning; profiles
own safety. Each layer uses boring, proven technology.

**C. Build on Claude Code plugin marketplaces** — distribute setups as plugins.
*Pros*: native install UX. *Cons*: plugins don't cover the full profile (global CLAUDE.md,
settings, keybindings), no profile isolation → no guaranteed instant revert, no fork/overlay
story. **Rejected as the substrate; adopted as an interop feature** (a stack may reference
plugins, and a plugin marketplace can be generated from a stack).

## 8. Phasing

**Phase 1 — CLI + profiles (validate the core loop, ~weeks):**
Prove exactly four things: profile isolation (starting with the day-1
`CLAUDE_CONFIG_DIR` spike), publish sanitization, the install review gate, and reversible
switching. Concretely: `init/try/clone/back/use/save/diff/update` against plain GitHub
repos with `stack.yaml`; a static index site listing ~20 hand-recruited expert stacks.
No backend, no accounts, no notifications yet. *Success criterion: a stranger clones an
expert setup, works with it for a day, and reverts or keeps it — with zero damage to
their own config.*

**Phase 2 — Registry + website + follows:** search API, stack pages with rendered contents
and diffs, GitHub OAuth, follow + notifications, scan pipeline, MCP server, trial journal.

**Phase 3 — Trust & evaluation:** verified experts, community benchmark suites, keep-rate
ranking, multi-harness (Codex/Cursor profiles), fork-lineage graphs, `sherpa compare`
automation.

## 9. Testing strategy

- **CLI**: unit tests per command against a fixture registry (local git remotes); golden
  tests for the sanitizer (corpus of configs with planted secrets/paths — must catch 100%
  of planted secrets); integration test of the full loop
  `init → clone → modify → save → upstream publishes v2 → update → conflict → resolve → back`.
- **Safety invariants** (§6) each get an explicit test, including kill-mid-operation tests
  for atomicity.
- **Registry**: API contract tests; scan-pipeline corpus (malicious hook samples must be
  flagged); immutability of published versions.
- **Website**: rendering snapshot tests for stack pages incl. hostile content (XSS via
  README/CLAUDE.md markdown).

## 10. Open questions (for discussion)

**Decisions from 2026-07-08 discussion:**

- **Whole-profile first (Q3: decided)** — v1 clones complete setups only ("full copy
  first"); cherry-picking single skills is a later feature.
- **Website timing (decided)** — Phase 1 ships CLI + static index; the real website
  follows shortly after as the immediate Phase 2 priority.
- **Trust posture (Q: open)** — quarantine-by-default vs. mandatory sandboxed try-mode
  still under discussion; see elaboration in the discussion thread. Recommendation:
  quarantine flow as default, `--sandboxed` try as a first-class option, risk-tiered
  defaults (unreviewed publishers → sandbox suggested, hooks stay quarantined).

1. **Naming**: "Sherpa" is a placeholder. Also: "stack" vs "setup" vs "profile" as the
   user-facing noun?
2. **Scope of `mine` import**: import your real `~/.claude` read-only, or copy it so `mine`
   is also a versioned stack from day one (recommended: copy + auto-git-init)?
3. **Partial adoption**: v1 is whole-profile only. Cherry-picking single skills from an
   expert ("just take her code-review skill") is a likely fast-follow — does it need to be
   in v1?
4. **Registry hosting economics**: GitHub-backed storage keeps Phase 1/2 nearly free;
   at what point (if ever) move content into our own git hosting?
5. **Codex/multi-harness**: stacks are harness-tagged from day one; is a Codex profile
   (AGENTS.md + config.toml) worth including in Phase 1 to widen the audience? When
   multi-harness lands, portability is per-harness adapters + compatibility badges — never
   a lowest-common-denominator abstract format.

## Appendix A — Prior art (research summary, GPT 5.5-assisted)

| Existing thing | What it proves | What's missing |
|---|---|---|
| Dotfiles / nix / home-manager | People share personal environments | Too machine-specific; no safe trial/revert, no agent focus |
| VS Code Profiles + Settings Sync | Bundled, exportable customization sets | Editor-centric, not expert-centric; no forks/follows |
| cursor.directory & rule lists | Demand for "copy the smart person's rules" | Loose fragments, not complete versioned revertible setups |
| MCP registries (smithery, mcp.so) | Discovery for individual servers | Servers, not opinionated whole setups |
| Claude Code plugin marketplaces | Native distribution of skills/agents/hooks | Unit is a plugin, not a full profile; no isolation/revert |
| ComfyUI workflow sharing | Best *pattern* analog: publish → browse → run → remix | Different domain |
| Hugging Face Hub | Best *scale* analog: git-backed repos, cards, versions, follows | Models/datasets, not agent environments |

Gap confirmed: no trusted, reversible, agent-native registry where the atomic unit is an
expert's **complete agent setup** with a clone/revert/fork/follow lifecycle.

## Appendix B — Harness portability survey (verified 2026-07-08)

Two independent axes, kept distinct in the product:

1. **Mechanism portability** — can the profile-isolation trick work for a given harness?
2. **Content portability** — does a stack written for one harness help in another?
   (Answer: mostly no. Stacks are harness-tagged; adapters may later translate the
   portable subset — instruction files, MCP declarations — but skills/hooks/settings are
   harness-specific. No lowest-common-denominator format.)

Mechanism survey:

| Harness | Isolation mechanism | Status |
|---|---|---|
| Claude Code | `CLAUDE_CONFIG_DIR` | Clean override (day-1 spike pins exact coverage) |
| Codex CLI | `CODEX_HOME` — root for config.toml, auth, skills, sessions | Clean override, documented |
| Gemini CLI | `GEMINI_CLI_HOME` — parent dir for `.gemini/` | Clean override, documented |
| Qwen Code / iFlow / other Gemini-CLI forks | fork-specific home dirs; likely inherit the mechanism | Verify per fork |
| OpenCode / Crush (multi-provider, incl. Mistral/Chinese models) | XDG-based → `XDG_CONFIG_HOME` | Clean override |
| Aider | config file paths + env | Workable |
| Cursor / GUI IDEs, Mistral Code (VS Code-based) | no config-dir override | Fallback only: transactional install (preimage backup → restore) |

Important nuance: **model provider ≠ harness.** GLM, Kimi, DeepSeek, Qwen and Mistral
models are widely used *through* Claude-Code- or OpenAI-compatible endpoints
(`ANTHROPIC_BASE_URL` etc.), so a Claude Code stack already serves users of those models
unchanged — the model backend can even be a stack parameter. Multi-harness support means
supporting other *harnesses*, and the big CLI ones all have clean isolation hooks.
