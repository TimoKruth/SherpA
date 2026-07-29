# SherpA

SherpA is a CLI and registry for sharing and trying complete agent setups as versioned, sanitized stacks. Power users build global instructions, skills, subagents, hooks, MCP servers, settings, and keybindings that can meaningfully change agent performance, but those setups are hard to discover, try, revert, fork, update, and follow safely. SherpA supports Claude Code and Codex harnesses: experts can publish a stack, users can try or clone it, `sherpa back` restores the protected `mine` profile, and local changes are saved as a fork that can track upstream.

Design spec: [docs/superpowers/specs/2026-07-08-follow-the-expert-design.md](docs/superpowers/specs/2026-07-08-follow-the-expert-design.md)

## Install

Download the binary for your platform from the
[releases page](https://github.com/TimoKruth/SherpA/releases), verify it, and
put it on your `PATH`:

```sh
VERSION=v0.1.0-beta.1          # see the releases page for the current tag
ASSET=sherpa-darwin-arm64      # or -darwin-amd64, -linux-amd64, -linux-arm64,
                               #    -windows-amd64.exe, -windows-arm64.exe
BASE=https://github.com/TimoKruth/SherpA/releases/download/$VERSION

curl -fsSLO "$BASE/$ASSET"
curl -fsSLO "$BASE/SHA256SUMS"
shasum -a 256 --ignore-missing --check SHA256SUMS
chmod +x "$ASSET"
sudo mv "$ASSET" /usr/local/bin/sherpa
```

During the beta the releases are prereleases, so `/releases/latest/` does not
resolve to them — use the explicit tag above.

Check the install with `sherpa version`, and list the commands with
`sherpa help`. `sherpa search` works out of the box; set `SHERPA_REGISTRY_URL`
only to point at a different registry.

### Uninstall

```sh
sudo rm -f /usr/local/bin/sherpa   # the binary
rm -rf ~/.sherpa                   # profiles, session, and local state
```

`~/.sherpa` holds every installed profile, so removing it discards work
committed with `sherpa save` that was never published. To drop a single stack
instead, use `sherpa remove <profile>`. Your own harness configuration
(`~/.claude`) is never touched by either.

Builds are published for macOS, Linux, and Windows on both `amd64` and `arm64`.
On macOS the binary is unsigned, so the first run needs Gatekeeper approval:
`xattr -d com.apple.quarantine /usr/local/bin/sherpa`.

Check the install with `sherpa version`.

From source instead:

```sh
go build ./cmd/sherpa
```

## Commands

The current CLI implements the following commands.

| Command | Behavior |
|---|---|
| `sherpa init` | Import `~/.claude` as the protected `mine` profile. |
| `sherpa login` / `sherpa logout` | Create or remove an issuer-scoped GitHub-backed registry session. |
| `sherpa search <query>` | Search `SHERPA_REGISTRY_URL`, falling back to the Phase 1 JSON index when configured. |
| `sherpa try <git-url-or-@owner/name-or-profile>` | Clone if needed, show the review gate, and launch under that profile without changing the active profile. |
| `sherpa clone <git-url-or-@owner/name>` | Clone, quarantine, validate, and install a stack without activating it; registry refs auto-follow best effort. |
| `sherpa back` | Switch the active profile back to `mine`. |
| `sherpa use <profile>` | Switch the active profile to an installed profile. |
| `sherpa remove <profile> [--yes]` | Delete an installed profile and its directory. Refuses the active profile and the protected baseline. |
| `sherpa run [args...]` | Run Claude Code under the active profile, reusing credentials from `mine` when available. |
| `sherpa save [-m msg]` | Commit modifications in the active profile's local branch. |
| `sherpa diff` | Show local profile changes, or a fork's changes against upstream. |
| `sherpa update [<profile>]` | Fetch upstream tags, show changelog and diffstat, then merge after confirmation. |
| `sherpa publish --remote <git-url>` / `--registry <url>` | Scan, review, bump, tag, and publish a new immutable version. |
| `sherpa follow @owner/name` / `sherpa unfollow @owner/name` | Manage an issuer-scoped registry follow. |
| `sherpa updates [--limit N]` | List pending immutable versions without marking them seen. |
| `sherpa updates --seen @owner/name@vN` | Explicitly mark a followed version reviewed. |
| `sherpa trial record|list|share` | Keep a local private trial journal and explicitly share verdict-only feedback. |
| `sherpa status` | Show local profiles first, then a bounded online/cached pending-update summary. |

## Registry Configuration

Set `SHERPA_REGISTRY_URL` to the canonical registry origin before `sherpa login`, search, follow,
or updates. Login writes an issuer-scoped `0600` session under `SHERPA_HOME`; a staging session is
never sent to production. `SHERPA_REGISTRY_TOKEN` is an optional admin/CI publish bypass and is not
used by personal follow, update, or trial-sharing commands.

Registry and website deployment variables, OAuth callback ownership, backup/restore, rollback,
and live staging gates are documented in
[docs/deployment/railway.md](docs/deployment/railway.md). Uptime monitoring and
alert delivery are documented in
[docs/deployment/monitoring.md](docs/deployment/monitoring.md).

## Trust Model

SherpA treats imported stacks as untrusted until reviewed: installation structurally quarantines executable capabilities from `settings.json` into `quarantine.json`, including hooks, MCP servers, and the permissions class that can weaken Claude Code permission prompts. The review gate shows pending capabilities and accepts `y`, `n`, `a`, or `q`; `--approve-all` is available as an informed-consent shortcut for users who intentionally want to restore every quarantined capability. Published stack versions are immutable: `publish` bumps `stack.yaml`, commits the new snapshot, tags it as `v<N>`, and pushes that versioned content.
