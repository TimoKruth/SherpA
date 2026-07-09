# SherpA

SherpA is a Phase 1 CLI for sharing and trying complete Claude Code setups as versioned, sanitized stacks. Power users build global instructions, skills, subagents, hooks, MCP servers, settings, and keybindings that can meaningfully change agent performance, but those setups are hard to discover, try, revert, fork, and update safely. SherpA starts with Claude Code only: experts can publish a stack, users can try or clone it, `sherpa back` restores the protected `mine` profile, and local changes are saved as a fork that can track upstream.

Design spec: [docs/superpowers/specs/2026-07-08-follow-the-expert-design.md](docs/superpowers/specs/2026-07-08-follow-the-expert-design.md)

## Install

```sh
go build ./cmd/sherpa
```

## Commands

Phase 1 implements the following commands.

| Command | Behavior |
|---|---|
| `sherpa init` | Import `~/.claude` as the protected `mine` profile. |
| `sherpa search <query>` | Search a Phase 1 JSON index from `SHERPA_INDEX_URL` and print matching Claude Code stacks. |
| `sherpa try <git-url-or-profile>` | Clone if needed, show the review gate, and launch a Claude Code session under that profile without changing the active profile. |
| `sherpa clone <git-url>` | Clone, quarantine, validate, and install a stack as a persistent profile without activating it. |
| `sherpa back` | Switch the active profile back to `mine`. |
| `sherpa use <profile>` | Switch the active profile to an installed profile. |
| `sherpa run [args...]` | Run Claude Code under the active profile, reusing credentials from `mine` when available. |
| `sherpa save [-m msg]` | Commit modifications in the active profile's local branch. |
| `sherpa diff` | Show local profile changes, or a fork's changes against upstream. |
| `sherpa update [<profile>]` | Fetch upstream tags, show changelog and diffstat, then merge after confirmation. |
| `sherpa publish --remote <git-url>` | Scan, review, bump the stack version, tag, and push a new stack version. |
| `sherpa status` | Show the active profile and installed profiles. |

## Trust Model

SherpA treats imported stacks as untrusted until reviewed: installation structurally quarantines executable capabilities from `settings.json` into `quarantine.json`, including hooks, MCP servers, and the permissions class that can weaken Claude Code permission prompts. The review gate shows pending capabilities and accepts `y`, `n`, `a`, or `q`; `--approve-all` is available as an informed-consent shortcut for users who intentionally want to restore every quarantined capability. Published stack versions are immutable: `publish` bumps `stack.yaml`, commits the new snapshot, tags it as `v<N>`, and pushes that versioned content.
