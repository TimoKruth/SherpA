# Spike: `CODEX_HOME` isolation coverage (SherpA 2b-ii Codex adapter)

**Date:** 2026-07-11 · **codex:** codex-cli 0.141.0 · macOS · script: `spike/codex-home.sh`

Empirically pins what the Codex harness adapter needs, mirroring the Phase-1
`CLAUDE_CONFIG_DIR` spike. The real `~/.codex` was only read; the authenticated probes ran
against a throwaway `CODEX_HOME` with the real `auth.json` copied in (auto-deleted on exit,
contents never printed).

## Verdict: isolation works; adapter values pinned below.

## Findings (with evidence)

**(a) `CODEX_HOME` overrides the config dir and loads stack content from it.** With
`CODEX_HOME` pointed at a temp dir containing fixtures:
- **AGENTS.md** (global instructions) — probe reply contained the fixture's
  `CODEX_SPIKE_MARKER_OK`. Loads. ✓
- **rules/** — the rule fixture was followed. Loads. ✓
- **skills/** — the probe invoked the fixture skill (`SPIKE_SKILL_OK`). Loads. ✓
- **config.toml** — a fixture `model = "gpt-5.5-codex"` was applied (account rejected it with
  a 400), proving config.toml is read from `CODEX_HOME`. ✓

**(b) `auth.json` is the credential file (no keychain).** A fresh `CODEX_HOME` with **no**
`auth.json` → `401 Unauthorized: Missing bearer`. Copying the real `~/.codex/auth.json` in →
authenticated (probes ran). Codex stores auth on disk in `auth.json`, unlike Claude Code's
Keychain. → credential link = `auth.json`; **no keychain-export step** (the codex adapter's
`PrepareBaselineCredentials` is a no-op).

**(c) Runtime state writes into `CODEX_HOME`.** After runs, the temp home held `sessions/`,
`installation_id`, `cache/`, `tmp/`, and (on this machine) several `*.sqlite*` DBs — all
runtime, to be gitignored. → generous ignore list.

**(d) Onboarding/setup-state.** `auth.json` alone was sufficient to authenticate — no
separate curated onboarding file is required to avoid re-login (unlike Claude Code's
`~/.claude.json`). This machine's `~/.codex` also has `.codex-global-state.json`
(personality/onboarding for an OB1-customized Codex); seeding it is an optional future
nicety, **not** needed for 2b-ii. → `SetupStateSources` empty, `Seed` returns `("", nil, nil)`.

## Pinned adapter values (2b-ii `internal/harness` Codex adapter)

- `Name` = `codex`, `Alias` = `codex`
- `ConfigDirEnv` = `CODEX_HOME`; `DefaultConfigDir(home)` = `home/.codex`
- `LaunchBin` = `codex`; `LaunchBinEnv` = `SHERPA_CODEX_BIN`
- `CredentialFiles` = `["auth.json"]`; `PrepareBaselineCredentials` = no-op (no keychain)
- `SetupStateSources` = none; `CapturedName` = `.sherpa-codex-setup.json` (unused until a seed exists); `Seed` = `("", nil, nil)`
- `AllowedPaths` (stack content that loads from `CODEX_HOME` + sherpa conventions):
  `stack.yaml, README.md, CHANGELOG.md, quarantine.json, AGENTS.md, AGENTS.override.md, config.toml, rules/, skills/, hooks/`
- `GitignoreContent` = whitelist derived from the above
- **Unpublishable barrier:** `SetupStateFilenames` = `["auth.json"]`;
  `LoginSignatures` = codex auth markers — pin to: `OPENAI_API_KEY`, `"access_token"`,
  `"refresh_token"`, `"id_token"`, `"tokens"`, `"account_id"` (refine against a real
  `auth.json` shape at implementation time; must be **non-empty** per the 2b-i barrier note).
- **`config.toml` is a publishable stack file** (the expert's model/settings), sanitized by
  the existing content scan; `auth.json` is the machine-local, unpublishable credential
  (user decision 2026-07-10, confirmed by (b)).

## Caveats / follow-ups

- This machine runs an **OB1-customized** Codex (extra `goals_*/memories_*/state_*` sqlite,
  `personality_migration`). Standard Codex is lighter; keep the runtime ignore list generous
  (all `*.sqlite*`, `sessions/`, `logs*`, `cache/`, `tmp/`, `history.jsonl`, `session_index.jsonl`).
- **Leak check inconclusive:** `~/.codex` showed recent mtimes on `models_cache.json`/`logs`
  during the spike, but Codex was being used concurrently for the build, so this can't be
  attributed to the spike. `CODEX_HOME` writes clearly went to the temp dir. Re-verify write
  isolation in a quiet environment before relying on "never touches `~/.codex`".
- `LoginSignatures` values above are a starting hypothesis; verify against a real (never
  printed) `auth.json` key set during implementation.
