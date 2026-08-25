#!/usr/bin/env bash
# Suite 20 — the codex harness, offline and hermetic.
#
# Covers: init --harness codex, multi-harness baseline naming, clone, try, use,
# back, and the publish barrier that refuses to push harness login state.

SUITE="20-core-codex"
# shellcheck source=lib.sh
. "$(dirname "$0")/lib.sh"

sandbox_init
make_fake_harnesses
make_claude_baseline
make_codex_baseline

CODEX_EXPERT="$SANDBOX/fixtures/codex-expert"
make_codex_expert_repo "$CODEX_EXPERT"

section "multi-harness init"
t_ok "claude init" init
t_ok "codex init" init --harness codex
a_eq "bare mine was renamed" "$(state_get '.profiles.mine // "absent"')" "absent"
a_eq "active profile is mine-claude" "$(active_profile)" "mine-claude"
a_eq "claude baseline registered" "$(state_get '.baselines."claude-code"')" "mine-claude"
a_eq "codex baseline registered" "$(state_get '.baselines.codex')" "mine-codex"

CODEX_MINE="$(profile_dir mine-codex)"
a_file "codex baseline imported AGENTS.md" "$CODEX_MINE/AGENTS.md"
a_file "codex baseline imported config.toml" "$CODEX_MINE/config.toml"
a_mode "codex auth.json stays 0600" "$CODEX_MINE/auth.json" "600"

section "clone, try, use, back"
t_ok "clone the codex stack" clone "$CODEX_EXPERT" --name codex-loop --review=approve-all
CODEX_DIR="$(profile_dir codex-loop)"
a_eq "clone left the active profile alone" "$(active_profile)" "mine-claude"
a_file "codex rules installed" "$CODEX_DIR/rules/style.md"
a_file "codex skill installed" "$CODEX_DIR/skills/review/SKILL.md"

t_ok "try launches codex" try codex-loop
a_eq "try pointed CODEX_HOME at the profile" \
	"$(cat "$SHERPA_FAKE_CODEX_MARKER" 2>/dev/null)" "$CODEX_DIR"
a_contains "codex credentials linked from the baseline" \
	"$(cat "$CODEX_DIR/auth.json" 2>/dev/null)" "baseline-token"

t_ok "use the codex profile" use codex-loop
a_eq "active profile is codex-loop" "$(active_profile)" "codex-loop"
t_ok "back from a codex profile" back
a_eq "back landed on the codex baseline" "$(active_profile)" "mine-codex"

section "publish barrier: harness login state must never be pushed"
t_ok "use the codex profile again" use codex-loop
printf '{"tokens":{"access_token":"must-not-publish"}}\n' >"$CODEX_DIR/auth.json"
git -C "$CODEX_DIR" add -f auth.json
t_ok "save the tracked auth fixture" save -m "track codex auth fixture"

CODEX_REMOTE="$SANDBOX/fixtures/codex-fork.git"
make_bare_remote "$CODEX_REMOTE"
STDIN_DATA=$'yes\nyes\n'
t_fail "publish refuses a tracked auth.json" publish --remote "$CODEX_REMOTE"
case "$ERR$OUT" in
*setup-state* | *login*) pass "publish explains the setup-state barrier" ;;
*) fail "publish explains the setup-state barrier" "$(brief "$ERR")" ;;
esac
a_eq "blocked publish pushed no refs" "$(remote_refs "$CODEX_REMOTE")" ""

summary
