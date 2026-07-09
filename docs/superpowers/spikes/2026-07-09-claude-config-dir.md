# Spike: `CLAUDE_CONFIG_DIR` isolation coverage

- **Date:** 2026-07-09
- **Machine:** macOS (Darwin 25.5.0), `claude` 2.1.205 (Claude Code)
- **Probe script:** `spike/claude-config-dir.sh`
- **Method:** created a throwaway config dir with fixture `CLAUDE.md`, `settings.json`
  (env sentinel + `SessionStart` hook + permission allow), `skills/spike-probe-skill/`,
  and `agents/spike-probe-agent.md`; ran non-interactive `claude -p` probes from a
  separate temp workdir with `CLAUDE_CONFIG_DIR` pointing at the fixture dir and the
  parent Claude Code session's env vars stripped. The real `~/.claude` was never
  modified (read-only inspection only). Temp dirs were deleted after evidence capture.

## Verdict

**Isolation is complete for config reads and runtime-state writes**, within the
evidence gathered: every fixture loaded from the override dir, all files the probes
created landed inside it, `~/.claude.json`'s `projects` map never gained the probe
workdir (checked after every run), and no content from the real global config
(CLAUDE.md text, skills, plugin skills, agents) surfaced in any probe reply. Note:
`~/.claude` itself was not diffed before/after (the probes ran from inside a live
Claude Code session whose own writes to `~/.claude` would confound mtime/checksum
comparison), so writes of probe state into `~/.claude` are ruled out by the file
inventories of the override dir, not by a direct diff. The one surprise is **auth**: the macOS
Keychain credential is *not* used when `CLAUDE_CONFIG_DIR` is set — a fresh override
dir starts logged out, and a `.credentials.json` file inside the dir is what carries
auth (details below). This is workable for SherpA and actually simplifies the
credential-link design: it is a single file.

## (a) What loads from `CLAUDE_CONFIG_DIR`

| Item | Loads from override dir? | Evidence |
| --- | --- | --- |
| `CLAUDE.md` | **yes** | Probe reply: "My global CLAUDE.md instructs me to always include the word SPIKE_MARKER_OK in every reply. SPIKE_MARKER_OK" — and every subsequent probe reply carried `SPIKE_MARKER_OK`. The user's real global CLAUDE.md content never surfaced. |
| `settings.json` — hooks | **yes** | The fixture `SessionStart` hook fired on every run (marker file created), even on the *unauthenticated* first runs. |
| `settings.json` — `env` | **yes** | Hook command `echo "sentinel=$SPIKE_SENTINEL"` wrote `sentinel=spike_sentinel_42` — the `env` block from the fixture settings.json was injected into the hook's environment. (Direct verification through the model's Bash tool was blocked by the sandbox denying `$`-expansion; the hook path is the conclusive evidence.) |
| `skills/` | **yes** | Skill-list probe returned `spike-probe-skill` plus built-in harness skills only. None of the real setup's skills or plugin skills (e.g. `superpowers:*`, `ralph-loop:*`) appeared. |
| `agents/` | **yes** | Agent-list probe returned `spike-probe-agent` plus built-in agent types only. |
| plugins | **yes (by negative evidence)** | No plugin was installed in the override dir, and no plugin-provided skills/commands from the real `~/.claude/plugins` setup leaked into the probes. Positive test (installing a plugin into the override dir) not performed. |

## (b) Credentials / auth (→ credential-link list)

Observed sequence:

1. Fresh override dir, no credential material → all probes returned
   `Not logged in · Please run /login`. On this machine the real credential lives in
   the **macOS Keychain** (generic password, service `Claude Code-credentials`,
   account `timokruth`; there is **no** `~/.claude/.credentials.json`). The keychain
   holds one global entry — no per-config-dir variants — and it is **ignored** when
   `CLAUDE_CONFIG_DIR` is set.
2. Grafting `oauthAccount` + `hasCompletedOnboarding: true` into the override dir's
   `.claude.json` → still `Not logged in`. Account metadata does not carry auth.
3. Writing the keychain secret to `$CLAUDE_CONFIG_DIR/.credentials.json` (mode 600;
   a JSON blob with top-level keys `claudeAiOauth` and `mcpOAuth`) → **logged in**,
   all probes succeed.
4. Removing `oauthAccount` from `.claude.json` again, keeping only
   `.credentials.json` → **still logged in** (`PONG SPIKE_MARKER_OK`), and Claude
   Code re-populated `oauthAccount` in the override `.claude.json` by itself after
   the run.

**Credential-link list (pinned):** see bottom of this doc. `.credentials.json` is
both **sufficient and necessary**. For profile setup on macOS-with-keychain
machines, SherpA must seed it once via
`security find-generic-password -s "Claude Code-credentials" -w > <profile>/.credentials.json`
(the probe script automates this behind `SPIKE_COPY_CREDS=1`); thereafter profiles
can share auth by hardlinking/symlinking that one file.

## (c) Runtime noise created inside the override dir (→ ignore list)

Created by the probes inside the override dir (nothing was created outside it):

- `.claude.json` (per-config-dir state: machineID, userID, migration flags,
  `oauthAccount` after login, `projects` map, …)
- `backups/` (`.claude.json.backup.*`)
- `projects/<escaped-workdir-path>/` (session transcripts)
- `sessions/`, `session-env/<uuid>` (per-session state)
- `.last-cleanup`, `mcp-needs-auth-cache.json`
- hook output artifacts (fixture-specific)

Additional noise seen in the real `~/.claude` (expect these to appear in a profile
dir over time): `history.jsonl`, `file-history/`, `shell-snapshots/`,
`stats-cache.json`, `debug/`, `cache/`, `paste-cache/`, `downloads/`, `tasks/`,
`teams/`, `chrome/`, `.last-update-result.json`.

## (d) Gaps / things still read from `~/.claude` or `~/.claude.json`

- **None observed.** `~/.claude.json`'s `projects` map did not gain the probe
  workdir (checked programmatically after every run: `LEAK … : no`), the real
  global CLAUDE.md/settings/skills/plugins never surfaced in probe output, and
  every file the probes created appeared inside the override dir per the
  before/after inventories. A direct before/after diff of `~/.claude` was not
  taken (see Verdict for why), so that specific cross-check is not part of the
  evidence.
- **Auth gap (design-relevant, not an isolation break):** the keychain credential
  is not shared into override dirs. Impact on spec §3.2: profile creation needs a
  one-time credential seed step; ongoing sharing is a single-file link.
- **Untested / inconclusive:** positive plugin install into an override dir;
  whether `hasCompletedOnboarding: true` is required in a fresh profile's
  `.claude.json` (it was set during the successful runs; a brand-new profile might
  otherwise show first-run onboarding); `settings.json` `env` injection into the
  model's own Bash tool (sandbox blocked the direct probe; hook-env injection is
  confirmed); interactive-mode-only files such as `history.jsonl` (probes were
  non-interactive `claude -p`).

---

## Pinned lists (consumed by later tasks verbatim)

### Tracked-path allowlist (`internal/stack/allowlist.go`)

Paths relative to the profile/config dir; all verified to load from the override dir:

```
CLAUDE.md
settings.json
skills/
agents/
```

(Candidates for later extension once positively verified: `plugins/`, `commands/`,
`keybindings.json`.)

### Credential-link list (`internal/launch` CredentialFiles)

```
.credentials.json
```

Single file, mode 600, JSON with top-level keys `claudeAiOauth` and `mcpOAuth`.
Sufficient and necessary for auth under `CLAUDE_CONFIG_DIR`. On macOS default
installs the master copy lives in the Keychain (service `Claude Code-credentials`)
and must be exported once per machine to seed the first profile.
