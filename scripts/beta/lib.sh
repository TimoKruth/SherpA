#!/usr/bin/env bash
# Shared harness for the SherpA beta smoke scripts.
#
# Every suite sources this file, calls sandbox_init, runs checks, and ends with
# summary. The sandbox never touches the real ~/.sherpa, ~/.claude, or ~/.codex:
# SHERPA_HOME, SHERPA_CLAUDE_DIR, and SHERPA_CODEX_DIR are redirected into a
# temporary directory that is removed on exit. That is what makes these scripts
# safe to re-run on a workstation after every reinstall.
#
# The suites exercise the installed binary, not the source tree, so they verify
# what a tester actually has on their PATH. Override with SHERPA_BIN.

set -uo pipefail

SHERPA_BIN="${SHERPA_BIN:-sherpa}"
SUITE="${SUITE:-suite}"

PASS_COUNT=0
FAIL_COUNT=0
FAILURES=""

# Command output from the most recent sherpa_run.
OUT=""
ERR=""
CODE=0
STDIN_DATA=""

if [ -t 1 ]; then
	C_PASS=$'\033[32m'
	C_FAIL=$'\033[31m'
	C_DIM=$'\033[2m'
	C_BOLD=$'\033[1m'
	C_OFF=$'\033[0m'
else
	C_PASS="" C_FAIL="" C_DIM="" C_BOLD="" C_OFF=""
fi

# ---------------------------------------------------------------- reporting --

pass() {
	PASS_COUNT=$((PASS_COUNT + 1))
	printf '  %sPASS%s  %s\n' "$C_PASS" "$C_OFF" "$1"
}

fail() {
	FAIL_COUNT=$((FAIL_COUNT + 1))
	FAILURES="${FAILURES}  - ${1}"$'\n'
	printf '  %sFAIL%s  %s\n' "$C_FAIL" "$C_OFF" "$1"
	if [ -n "${2:-}" ]; then
		printf '        %s%s%s\n' "$C_DIM" "$2" "$C_OFF"
	fi
}

note() { printf '%s* %s%s\n' "$C_DIM" "$1" "$C_OFF"; }
section() { printf '\n%s%s%s\n' "$C_BOLD" "$1" "$C_OFF"; }

summary() {
	printf '\n%s: %d passed, %d failed\n' "$SUITE" "$PASS_COUNT" "$FAIL_COUNT"
	if [ "$FAIL_COUNT" -gt 0 ]; then
		printf 'Failures:\n%s' "$FAILURES"
		exit 1
	fi
	exit 0
}

# ------------------------------------------------------------------ sandbox --

require_deps() {
	local missing=""
	local tool
	for tool in git jq shasum; do
		command -v "$tool" >/dev/null 2>&1 || missing="$missing $tool"
	done
	if [ -n "$missing" ]; then
		printf 'error: missing required tools:%s\n' "$missing" >&2
		exit 2
	fi
	if ! command -v "$SHERPA_BIN" >/dev/null 2>&1 && [ ! -x "$SHERPA_BIN" ]; then
		printf 'error: sherpa binary %s not found; set SHERPA_BIN to its path\n' "$SHERPA_BIN" >&2
		exit 2
	fi
}

sandbox_cleanup() {
	if [ -n "${SANDBOX:-}" ] && [ -d "$SANDBOX" ]; then
		if [ -n "${KEEP_SANDBOX:-}" ]; then
			printf '\nsandbox kept at %s\n' "$SANDBOX"
		else
			rm -rf "$SANDBOX"
		fi
	fi
}

sandbox_init() {
	require_deps
	# macOS sets TMPDIR with a trailing slash, which would leave a doubled
	# separator in every path we compare against sherpa's own cleaned output.
	local tmpbase="${TMPDIR:-/tmp}"
	SANDBOX="$(mktemp -d "${tmpbase%/}/sherpa-beta.XXXXXX")"
	trap sandbox_cleanup EXIT

	export SHERPA_HOME="$SANDBOX/sherpa-home"
	export SHERPA_CLAUDE_DIR="$SANDBOX/claude"
	export SHERPA_CODEX_DIR="$SANDBOX/codex"

	# A real keychain lookup would prompt and would bind the run to the tester's
	# login. /usr/bin/false makes PrepareBaselineCredentials fall back to the
	# credential file the fixture already wrote.
	export SHERPA_SECURITY_BIN=/usr/bin/false

	# Empty, not unset: an unset registry URL falls back to the beta origin and
	# would make these offline suites depend on the network.
	export SHERPA_REGISTRY_URL=""
	export SHERPA_INDEX_URL=""

	# Pin a git identity so commits work on a machine with no global config.
	export GIT_AUTHOR_NAME="sherpa beta"
	export GIT_AUTHOR_EMAIL="sherpa-beta@example.invalid"
	export GIT_COMMITTER_NAME="sherpa beta"
	export GIT_COMMITTER_EMAIL="sherpa-beta@example.invalid"
	export GIT_CONFIG_GLOBAL="$SANDBOX/gitconfig"
	export GIT_CONFIG_NOSYSTEM=1
	: >"$GIT_CONFIG_GLOBAL"

	# Belt and braces: refuse to run if the redirect did not take, so a future
	# edit cannot quietly point the suite at the tester's real profiles.
	case "$SHERPA_HOME" in
	"$SANDBOX"/*) ;;
	*)
		printf 'error: SHERPA_HOME escaped the sandbox (%s)\n' "$SHERPA_HOME" >&2
		exit 2
		;;
	esac

	mkdir -p "$SHERPA_HOME" "$SANDBOX/bin" "$SANDBOX/fixtures"
	printf '%s%s runs in %s%s\n' "$C_DIM" "$SUITE" "$SANDBOX" "$C_OFF"
}

# Fake harness binaries. They record the config directory they were launched
# with, which is the only externally observable proof that `try`/`run` pointed
# the harness at the right profile.
make_fake_harnesses() {
	export SHERPA_CLAUDE_BIN="$SANDBOX/bin/claude"
	export SHERPA_CODEX_BIN="$SANDBOX/bin/codex"
	export SHERPA_FAKE_CLAUDE_MARKER="$SANDBOX/bin/claude-config-dir.txt"
	export SHERPA_FAKE_CODEX_MARKER="$SANDBOX/bin/codex-home.txt"

	cat >"$SHERPA_CLAUDE_BIN" <<'EOF'
#!/bin/sh
printf '%s\n' "$CLAUDE_CONFIG_DIR" > "$SHERPA_FAKE_CLAUDE_MARKER"
EOF
	cat >"$SHERPA_CODEX_BIN" <<'EOF'
#!/bin/sh
printf '%s\n' "$CODEX_HOME" > "$SHERPA_FAKE_CODEX_MARKER"
EOF
	chmod 755 "$SHERPA_CLAUDE_BIN" "$SHERPA_CODEX_BIN"
}

# ------------------------------------------------------------------ running --

# sherpa_run <args...> — sets OUT, ERR, CODE. Set STDIN_DATA beforehand to feed
# stdin to an interactive prompt; it is consumed and reset by each call.
sherpa_run() {
	local errf="$SANDBOX/.stderr"
	if [ -n "$STDIN_DATA" ]; then
		OUT="$(printf '%s' "$STDIN_DATA" | "$SHERPA_BIN" "$@" 2>"$errf")"
	else
		OUT="$("$SHERPA_BIN" "$@" 2>"$errf" </dev/null)"
	fi
	CODE=$?
	ERR="$(cat "$errf" 2>/dev/null || true)"
	STDIN_DATA=""
}

brief() { printf '%s' "$1" | tr '\n' ' ' | cut -c1-160; }

# t_ok <name> <sherpa args...> — the command must succeed.
t_ok() {
	local name="$1"
	shift
	sherpa_run "$@"
	if [ "$CODE" -eq 0 ]; then
		pass "$name"
	else
		fail "$name" "exit=$CODE  stderr: $(brief "$ERR")"
	fi
}

# t_fail <name> <sherpa args...> — the command must be refused.
t_fail() {
	local name="$1"
	shift
	sherpa_run "$@"
	if [ "$CODE" -ne 0 ]; then
		pass "$name"
	else
		fail "$name" "expected a non-zero exit, got 0"
	fi
}

# --------------------------------------------------------------- assertions --

a_eq() { # a_eq <name> <got> <want>
	if [ "$2" = "$3" ]; then pass "$1"; else fail "$1" "got '$2', want '$3'"; fi
}

a_contains() { # a_contains <name> <haystack> <needle>
	case "$2" in
	*"$3"*) pass "$1" ;;
	*) fail "$1" "missing '$3' in: $(brief "$2")" ;;
	esac
}

a_not_contains() {
	case "$2" in
	*"$3"*) fail "$1" "unexpectedly found '$3'" ;;
	*) pass "$1" ;;
	esac
}

a_file() { # a_file <name> <path>
	if [ -e "$2" ]; then pass "$1"; else fail "$1" "missing $2"; fi
}

a_no_file() {
	if [ -e "$2" ]; then fail "$1" "$2 still exists"; else pass "$1"; fi
}

a_mode() { # a_mode <name> <path> <octal>
	local got
	got="$(file_mode "$2")"
	if [ "$got" = "$3" ]; then pass "$1"; else fail "$1" "$2 mode $got, want $3"; fi
}

file_mode() {
	if stat -f '%Lp' "$1" >/dev/null 2>&1; then
		stat -f '%Lp' "$1"
	else
		stat -c '%a' "$1"
	fi
}

state_get() { jq -r "$1" "$SHERPA_HOME/state.json" 2>/dev/null || printf ''; }
active_profile() { state_get '.active'; }
profile_dir() { printf '%s/profiles/%s' "$SHERPA_HOME" "$1"; }

git_in() { git -C "$1" "${@:2}"; }

a_git_clean() { # a_git_clean <name> <dir>
	local status
	status="$(git -C "$2" status --porcelain 2>&1)"
	if [ -z "$status" ]; then pass "$1"; else fail "$1" "dirty tree: $(brief "$status")"; fi
}

# Checksum of a directory's file paths, permissions, and contents, ignoring
# .git. Used to prove `back` restores the baseline byte for byte.
tree_sum() {
	(
		cd "$1" 2>/dev/null || exit 0
		find . -name .git -prune -o -type f -print |
			LC_ALL=C sort |
			while read -r f; do
				printf '%s %s %s\n' "$f" "$(file_mode "$f")" "$(shasum -a 256 "$f" | awk '{print $1}')"
			done
	) | shasum -a 256 | awk '{print $1}'
}

# ------------------------------------------------------------------ fixtures --

make_claude_baseline() {
	mkdir -p "$SHERPA_CLAUDE_DIR/skills/mine"
	printf 'mine instructions\n' >"$SHERPA_CLAUDE_DIR/CLAUDE.md"
	printf '{"theme":"mine","mcpServers":{}}\n' >"$SHERPA_CLAUDE_DIR/settings.json"
	printf 'mine skill\n' >"$SHERPA_CLAUDE_DIR/skills/mine/SKILL.md"
	printf 'credential-copy\n' >"$SHERPA_CLAUDE_DIR/.credentials.json"
	chmod 600 "$SHERPA_CLAUDE_DIR/.credentials.json"
	printf '{"hasCompletedOnboarding":true,"theme":"dark","projects":{"/tmp/project":{"trust":true}}}\n' \
		>"${SHERPA_CLAUDE_DIR}.json"
	chmod 600 "${SHERPA_CLAUDE_DIR}.json"
}

make_codex_baseline() {
	mkdir -p "$SHERPA_CODEX_DIR"
	printf 'codex baseline\n' >"$SHERPA_CODEX_DIR/AGENTS.md"
	printf 'model = "gpt-5-codex"\n' >"$SHERPA_CODEX_DIR/config.toml"
	printf '{"tokens":{"access_token":"baseline-token"}}\n' >"$SHERPA_CODEX_DIR/auth.json"
	chmod 600 "$SHERPA_CODEX_DIR/auth.json"
}

# A claude-code upstream stack carrying every quarantinable capability class:
# a hook, an MCP server, and a permissions block.
make_expert_repo() { # make_expert_repo <dir>
	local repo="$1"
	mkdir -p "$repo/skills/review" "$repo/hooks"
	cat >"$repo/stack.yaml" <<'EOF'
name: expert-loop
owner: "@expert"
version: 1
harness: claude-code
summary: Beta smoke fixture
forked_from: "@mentor/base@v7"
executes:
  hooks:
    - path: hooks/check.sh
      event: PreToolUse
      purpose: "Checks writes"
  mcp_servers:
    - name: docs
      transport: stdio
      command: "npx @modelcontextprotocol/server-filesystem"
      purpose: "Reads docs"
EOF
	printf 'expert stack\n' >"$repo/README.md"
	printf '# Changelog\n\n## v1\n\n- initial\n' >"$repo/CHANGELOG.md"
	printf 'expert v1 instructions\n' >"$repo/CLAUDE.md"
	printf 'expert review skill\n' >"$repo/skills/review/SKILL.md"
	printf '#!/bin/sh\nexit 0\n' >"$repo/hooks/check.sh"
	chmod 755 "$repo/hooks/check.sh"
	cat >"$repo/settings.json" <<'EOF'
{"theme":"expert","permissions":{"allow":["Bash(rm:*)"]},"hooks":{"PreToolUse":[{"matcher":"Write","hooks":[{"type":"command","command":"hooks/check.sh"}]}]},"mcpServers":{"docs":{"command":"npx","args":["-y","@modelcontextprotocol/server-filesystem"]}}}
EOF
	git_init_repo "$repo"
}

make_codex_expert_repo() { # make_codex_expert_repo <dir>
	local repo="$1"
	mkdir -p "$repo/rules" "$repo/skills/review"
	cat >"$repo/stack.yaml" <<'EOF'
name: codex-loop
owner: "@codex-expert"
version: 1
harness: codex
summary: Codex beta smoke fixture
EOF
	printf 'codex expert stack\n' >"$repo/README.md"
	printf '# Changelog\n\n## v1\n\n- initial\n' >"$repo/CHANGELOG.md"
	printf 'codex expert instructions\n' >"$repo/AGENTS.md"
	printf 'model = "gpt-5-codex"\n' >"$repo/config.toml"
	printf 'prefer direct answers\n' >"$repo/rules/style.md"
	printf 'codex review skill\n' >"$repo/skills/review/SKILL.md"
	git_init_repo "$repo"
}

git_init_repo() {
	local repo="$1"
	git -C "$repo" init -q -b main
	git -C "$repo" add -A
	git -C "$repo" -c commit.gpgsign=false commit -q -m v1
	git -C "$repo" tag v1
}

# Cut a new upstream version in a fixture repo so `update` has something to
# merge. Extra files are passed as name=content pairs.
publish_upstream() { # publish_upstream <repo> <tag> <name=content>...
	local repo="$1" tag="$2"
	shift 2
	local pair name content
	for pair in "$@"; do
		name="${pair%%=*}"
		content="${pair#*=}"
		mkdir -p "$repo/$(dirname "$name")"
		printf '%b' "$content" >"$repo/$name"
	done
	git -C "$repo" add -A
	git -C "$repo" -c commit.gpgsign=false commit -q -m "$tag"
	git -C "$repo" tag "$tag"
}

make_bare_remote() { # make_bare_remote <dir>
	git init -q --bare -b main "$1"
}

remote_tags() { git --git-dir="$1" tag -l 'v*' | tr '\n' ' '; }
remote_refs() { git --git-dir="$1" for-each-ref --format='%(refname)'; }
