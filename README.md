# SherpA — local setup comparisons

Run the same prompt against different **Claude Code and Codex configurations**,
compare the responses and project changes, and keep your main setup protected.
V1 works entirely from the CLI, with an optional local web app for side-by-side
results, manual ratings, history, and downloadable HTML reports.

SherpA has no account, registry, publishing service, or telemetry. Orchestration
and storage stay on your machine. The installed harnesses still use their normal
model APIs and credentials, with their normal usage charges.

## Start

Prerequisites: Git, Go 1.26.6 or later, and at least one installed and signed-in
harness (`claude` or `codex`). Build this branch; previously published beta
binaries contain the earlier registry product, not this V1 workflow.

```sh
go build -o sherpa ./cmd/sherpa
./sherpa init
./sherpa serve
```

`init` discovers installed configurations, asks which harness owns `mine`, and
imports independent protected baselines. If both are present, the second gets a
name such as `mine-codex`. Your original `~/.claude` and `~/.codex` directories
are not rewritten. For noninteractive initialization:

```sh
./sherpa init --primary-harness claude-code
```

The local app opens at `127.0.0.1:7331`. Its private access token is in the URL
fragment, never in an HTTP query. Use the URL printed by `serve` to connect a new
browser tab. `--port 0` chooses an available port; `--no-open` prints the URL
without launching a browser. Ctrl+C cancels active comparisons and stops it.

## A complete CLI comparison

Create a variant of your main setup and add instructions, or edit its copied
configuration in the directory printed by `profile show`:

```sh
./sherpa profile create careful-reviewer --from mine \
  --instructions 'Explain your approach before making changes.'
./sherpa profile show careful-reviewer
./sherpa profile review careful-reviewer

./sherpa compare \
  --project /absolute/path/to/git-project \
  --profiles mine,careful-reviewer \
  --prompt 'Review the error handling. Explain the most important issue and propose a fix.' \
  --timeout 5m
```

The comparison prints an ID. Use it to inspect, rate, or export the results:

```sh
./sherpa results
./sherpa results <id>
./sherpa results <id> --format json
./sherpa results rate <id> careful-reviewer --score 5 --notes 'Clearer reasoning.'
./sherpa results <id> --format html --output comparison.html
```

HTML reports are self-contained, work offline, and include responses, diffs,
ratings, diagnostics, configuration fingerprints, and the project fingerprint.
Existing output files are not overwritten. Use `--prompt-file task.txt` for a
multiline prompt. Select 2–8 distinct profiles, including profiles using different
harnesses. A comparison does not change your active profile.

## Switch and maintain setups

| Command | Behavior |
| --- | --- |
| `init` | Capture protected baselines from installed harness configurations. |
| `init --harness codex` | Add a subsequently installed harness. |
| `init --refresh --harness claude-code` | Explicitly refresh captured onboarding/identity state, not baseline instructions or skills. |
| `profile create <name> --from <profile>` | Create an editable local copy, optionally appending `--instructions`. |
| `profile import <name> --path <dir> --harness <harness>` | Preview a local configuration and require trust review before importing. Repeat with `--trusted` after reviewing its scripts, servers, and permissions. |
| `profile review <name>` | Show configuration and any pending legacy quarantined capabilities. `--approve-all` explicitly restores those capabilities on a variant. |
| `profile show <name>` | Print the setup's harness and editable directory. |
| `use <name>` | Select the profile for subsequent `sherpa run` invocations. |
| `back` | Select the protected baseline for the active profile's harness. |
| `run [harness arguments…]` | Launch the selected harness. Protected baseline launches use a disposable configuration copy. |
| `try <name>` | Launch an installed profile without changing the active selection. |
| `profile setup <name>` | Clear a variant's copied login/setup state and launch fresh onboarding. Refuses protected baselines. |
| `save [-m message]` / `diff` | Save an experimental setup's local changes / inspect tracked changes since its last save. |
| `remove <name> [--yes]` | Remove an inactive experimental setup, with confirmation. Baselines cannot be removed. |
| `status` | Show only local profiles and the active selection. No network request. |

Switching affects **SherpA launches**. Direct `claude` and `codex` commands continue
to use their normal configuration. Interactive `run` and `try` operate in your
current project; **`compare` is the command that also copies the project**.

## What a comparison preserves

1. Capture the Git project's current tracked files, including uncommitted changes,
   and nonignored untracked files, once before any trial starts.
2. Capture every selected setup before the first trial. Copy credentials separately;
   do not link credentials or reuse conversations, project trust caches, or history.
3. Run setups sequentially in independent project repositories with independent
   configuration directories. Each receives the exact same prompt and file snapshot.
4. Save responses, Git-visible additions/deletions/changes, stderr, exit status,
   elapsed time, harness version, setup fingerprint, and source revision/fingerprint.
   A failed setup does not prevent the other setups from running.
5. Remove trial credential/runtime directories when execution finishes. Keep the
   input configuration snapshots, project snapshots, changed projects, and results
   locally for inspection. Ratings and notes are private until you export a report.

A timeout applies per setup (default five minutes; maximum one hour). Ctrl+C in the
CLI or **Stop comparison** in the app preserves partial results. A failed,
cancelled, or timed-out CLI comparison exits nonzero while keeping its results.
Outputs and diffs are capped at 2 MiB each, with truncation shown explicitly.

## Boundaries

- V1 is for **trusted local setups**. Copied directories protect against ordinary
  configuration and project writes; they are not a security boundary against
  malicious hooks, MCP servers, instructions, or code. External absolute paths,
  system services, environment variables, and OS credential stores remain shared.
- Codex comparisons explicitly use its `workspace-write` sandbox. Claude comparisons
  use noninteractive `dontAsk` permissions: operations needing approval can be
  denied. SherpA never adds permission-bypass flags. Check diagnostics when a setup
  cannot perform a task.
- Use the repository root as the project directory. Project symlinks, submodules,
  and special files are rejected rather than silently omitted or linked to the
  source. Limits are 20,000 project files / 256 MiB. Ignored dependencies are not
  copied or installed automatically.
- Supported setup files are instructions (`CLAUDE.md`, `AGENTS.md`, overrides),
  `settings.json` / `config.toml`, keybindings, skills, agents, rules, hooks, and
  existing stack/quarantine metadata as applicable to the harness. Linked skill
  directories are materialized into independent files; cycles fail. Setup copies
  are limited to 20,000 files / 128 MiB. Plugin installations and settings stored
  outside these configuration files are not portable in V1.
- Timing includes preparation and harness startup; model responses are
  nondeterministic. Ratings are human judgments, not an automatic quality score.
  Different harnesses keep their different execution and permission semantics.
- A process killed abruptly can leave private trial credentials or a mutation
  lock under `SHERPA_HOME`. After confirming the process has stopped, delete its
  abandoned `comparisons/<id>/run-*/config` directories or the stale
  `.mutation-lock`. Normal completion/cancellation cleans runtime copies.

Storage defaults to `~/.sherpa` (`SHERPA_HOME` overrides it). Comparisons contain
private source code, prompts, outputs, notes, and setup configuration; inspect
reports before sharing them. Existing beta profiles and private trial metadata
are retained when opening an existing home; no beta registry is contacted.
Use a separate `SHERPA_HOME` if you want to keep beta and V1 experiments apart.

For testing/custom installations, `SHERPA_CLAUDE_DIR` / `SHERPA_CODEX_DIR` select
configuration sources and `SHERPA_CLAUDE_BIN` / `SHERPA_CODEX_BIN` select executables.

## Development

```sh
go test -race ./...
go vet ./...
make build
```

Tests use temporary configurations and fake harnesses; they do not call model APIs.
CI also scans reachable vulnerabilities and builds macOS, Linux, and Windows
binaries for arm64 and amd64. See [V1 scope and deferred work](docs/v1-scope.md)
for the future-development branches and [validation](docs/v1-validation.md)
for the acceptance evidence.
