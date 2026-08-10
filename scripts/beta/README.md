# Beta smoke scripts

Repeatable functional pass over the SherpA CLI. Run after every reinstall.

```sh
make beta-smoke                                  # offline suites
SHERPA_BIN=./dist/sherpa-darwin-arm64 make beta-smoke
SHERPA_BETA_ONLINE=1 scripts/beta/run-all.sh     # plus the live registry
```

Exit code 0 means every check passed. Each suite prints `PASS`/`FAIL` per check
and a count at the end.

## Safety

The suites redirect `SHERPA_HOME`, `SHERPA_CLAUDE_DIR`, and `SHERPA_CODEX_DIR`
into a temporary directory that is deleted on exit, and `sandbox_init` aborts if
that redirect ever fails. Your real `~/.sherpa`, `~/.claude`, and `~/.codex` are
never read or written by the offline suites. `SHERPA_CLAUDE_BIN` and
`SHERPA_CODEX_BIN` point at stub scripts, so no real harness is launched and no
model is called. Set `KEEP_SANDBOX=1` to keep the directory for debugging.

Suite 40 is the exception: it reads your registry session and writes real
follows and trial verdicts to the live registry. It refuses to run without
`SHERPA_BETA_ONLINE=1`, and publishing needs a second opt-in.

## Command order

Order matters — `update` needs a published v2, which needs `publish`, which
needs an active profile. Running out of order produces false failures.

### Suite 10 — core claude-code lifecycle (offline, automated)

| # | Command | Verified |
|---|---|---|
| 1 | `sherpa version` | exits 0 |
| 2 | `sherpa help` | lists all 22 commands |
| 3 | `sherpa init` | active is `mine`; skills and credentials imported at 0600 |
| 4 | `sherpa status` | exits 0 with no session; names the active profile |
| 5 | `sherpa search <query>` | reads the index; reports empty results cleanly |
| 6 | `sherpa clone <url> --name expert --review=approve-all` | installs without activating |
| 7 | `sherpa try expert` | sets `CLAUDE_CONFIG_DIR` to the profile; active unchanged |
| 8 | `sherpa try expert --fresh-setup` | still launches |
| 9 | `sherpa use expert` | active becomes `expert` |
| 10 | `sherpa run` | launches under the active profile |
| 11 | `sherpa save -m <msg>` | commits; tree clean afterwards |
| 12 | `sherpa diff` | exits 0 |
| 13 | `sherpa update expert` | merges v2, prints changelog, keeps local edits |
| 14 | `sherpa update expert` (conflicting v3) | refused; local branch SHA unmoved |
| 15 | `sherpa publish --remote <bare>` | one tag pushed; fork lineage preserved |
| 16 | `sherpa profile setup expert` | exits 0 |
| 17 | `sherpa back` | active is `mine`; baseline byte-identical |
| 18 | `sherpa remove mine --yes` | refused (protected baseline) |
| 19 | `sherpa remove <active> --yes` | refused (active profile) |
| 20 | `sherpa remove expert --yes` | deletes the profile directory |
| 21 | `sherpa logout` | safe with no session |

### Suite 20 — codex harness (offline, automated)

| # | Command | Verified |
|---|---|---|
| 1 | `sherpa init` | claude baseline |
| 2 | `sherpa init --harness codex` | bare `mine` renamed; both baselines registered |
| 3 | `sherpa clone <url> --name codex-loop --review=approve-all` | `rules/` and `skills/` installed |
| 4 | `sherpa try codex-loop` | sets `CODEX_HOME`; `auth.json` linked from baseline |
| 5 | `sherpa use codex-loop` | active becomes `codex-loop` |
| 6 | `sherpa back` | lands on `mine-codex`, not `mine-claude` |
| 7 | `sherpa save` + `sherpa publish --remote` | refused with a tracked `auth.json`; no refs pushed |

### Suite 30 — trust gate and sanitizer (offline, automated)

| # | Command | Verified |
|---|---|---|
| 1 | `sherpa clone <url>` | defaults to keep-quarantined |
| 2 | `sherpa clone --review=keep` | hooks, mcpServers, permissions all stripped into `quarantine.json` |
| 3 | `sherpa clone --review=interactive` (EOF) | approves nothing |
| 4 | `sherpa clone --review=interactive` (`n`) | approves nothing; prompt shown |
| 5 | `sherpa clone --review=interactive` (`a`) | all three restored to `settings.json` |
| 6 | `sherpa clone --review=interactive` (`q`) | stops without approving |
| 7 | `sherpa clone --review=approve-all` | all three restored |
| 8 | `sherpa clone --approve-all` | same as above |
| 9 | gate rendering | shows declared purposes and the hook script body |
| 10 | `sherpa publish` with a planted `ghp_` token | blocked; no refs pushed |
| 11 | `sherpa publish` after deleting the token | still blocked — the scan covers history |
| 12 | `sherpa publish` with a home path and email | succeeds after an extra warning confirmation |
| 13 | `sherpa clone` of a stack with an undeclared hook | refused by manifest validation |

### Suite 40 — live registry (needs `sherpa login` first)

`sherpa login` is a GitHub device flow that needs a browser and cannot be
scripted. Log in normally, then the suite copies the session into its sandbox.

| # | Command | Verified |
|---|---|---|
| 1 | `sherpa login` | **manual**, once, before the suite |
| 2 | `sherpa search <query>` | returns real entries, no `example.invalid` |
| 3 | `sherpa clone @owner/name --review=keep` | registry origin recorded in state |
| 4 | `sherpa follow @owner/name` | exits 0 |
| 5 | `sherpa updates` / `--limit N` | lists pending versions |
| 6 | `sherpa updates --seen @owner/name@vN` | marks reviewed |
| 7 | `sherpa status` | prints the registry summary |
| 8 | `sherpa trial record <profile> --verdict keep-with-notes --notes ...` | recorded locally |
| 9 | `sherpa trial list` / `--profile NAME` | shows the entry |
| 10 | `sherpa trial share <id>` | verdict sent; notes stay local |
| 11 | `sherpa unfollow @owner/name` | exits 0 |
| 12 | `sherpa publish --registry <url>` ×2 | opt-in via `SHERPA_BETA_PUBLISH=1`; creates immutable v1 and v2 |

## Not covered by these scripts

These need a human and belong on the manual checklist:

- Real `claude` and `codex` launches. The suites prove SherpA points the harness
  at the right directory; they cannot prove the harness reads what it finds
  there. Both were confirmed by hand on 2026-08-06 with the marker probe in
  `MANUAL.md`: Claude Code returned the marker from a profile `CLAUDE.md`, and
  Codex CLI 0.146.1 (`gpt-5.6-sol`) loaded and ran a skill from
  `CODEX_HOME/skills/`. Re-check after a harness upgrade.
- `sherpa login` itself — the browser device flow.
- The website: `/`, `/search`, `/login`, `/auth/callback`, `/dashboard`,
  `/logout`, user and stack pages, `/healthz`, `/robots.txt`, static assets,
  cookie flags, and noindex.
- Binary install and integrity: download, `SHA256SUMS`, Gatekeeper, SmartScreen.
- The collector, export cycle, `/readyz`, and alert delivery.
- Disaster recovery (go-live item 5) and the Storage Box snapshot schedule
  (item 6) — both disruptive and separately approved.

## Known beta gaps

Do not file these as bugs; see `docs/deployment/go-live.md`:

- Unsigned macOS and Windows binaries (items 1–2).
- `sherpa init` is not multi-harness aware; a second baseline may end up
  suffixed (item 10). Suite 20 asserts the current behavior, not the target.
- Exports fire one full interval after start with no run-at-boot (item 4).
- Monitoring shares one VPS with Traefik and Synapse (item 3).
