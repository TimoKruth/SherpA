#!/usr/bin/env bash
# Suite 10 — the core claude-code lifecycle, offline and hermetic.
#
# Covers: version, help, init, status, search, clone, try, use, run, save,
# diff, update, back, profile, remove, publish, logout.

SUITE="10-core-claude"
# shellcheck source=lib.sh
. "$(dirname "$0")/lib.sh"

sandbox_init
make_fake_harnesses
make_claude_baseline

EXPERT="$SANDBOX/fixtures/expert"
make_expert_repo "$EXPERT"

section "Identity"
t_ok "version exits 0" version
t_ok "help exits 0" help
HELP_OUT="$OUT"
for cmd in init login logout status search try clone use back run save diff \
	update publish remove profile follow unfollow updates trial help version; do
	case "$HELP_OUT" in
	*"  $cmd "*) pass "help lists $cmd" ;;
	*) fail "help lists $cmd" "not present in help output" ;;
	esac
done

section "init and baseline import"
t_ok "init imports the baseline" init
a_eq "active profile is mine" "$(active_profile)" "mine"
MINE="$(profile_dir mine)"
a_file "mine/CLAUDE.md exists" "$MINE/CLAUDE.md"
a_file "mine skill imported" "$MINE/skills/mine/SKILL.md"
a_file "mine credentials imported" "$MINE/.credentials.json"
a_mode "mine credentials stay 0600" "$MINE/.credentials.json" "600"
MINE_SUM="$(tree_sum "$MINE")"

t_ok "status exits 0 without a session" status
a_contains "status names the active profile" "$OUT" "active: mine"

section "search via the Phase 1 index fallback"
cat >"$SANDBOX/fixtures/index.json" <<'EOF'
{"stacks":[{"ref":"@expert/expert-loop","name":"expert-loop","owner":"@expert",
"harness":"claude-code","summary":"Beta smoke fixture","tags":["smoke"],
"repo_url":"https://example.invalid/expert/expert-loop.git","version":1,"forked_from":null}]}
EOF
SHERPA_INDEX_URL="file://$SANDBOX/fixtures/index.json" sherpa_run search expert-loop
if [ "$CODE" -eq 0 ]; then pass "search reads the index"; else fail "search reads the index" "exit=$CODE $(brief "$ERR")"; fi
a_contains "search finds the fixture stack" "$OUT" "@expert/expert-loop"
SHERPA_INDEX_URL="file://$SANDBOX/fixtures/index.json" sherpa_run search nothing-matches-this
a_contains "search reports an empty result" "$OUT" "no stacks found"

section "clone, try, use"
t_ok "clone installs without activating" clone "$EXPERT" --name expert --review=approve-all
EXPERT_DIR="$(profile_dir expert)"
a_eq "clone left the active profile alone" "$(active_profile)" "mine"
a_file "cloned stack has CLAUDE.md" "$EXPERT_DIR/CLAUDE.md"

t_ok "try launches the harness" try expert
a_eq "try pointed CLAUDE_CONFIG_DIR at the profile" \
	"$(cat "$SHERPA_FAKE_CLAUDE_MARKER" 2>/dev/null)" "$EXPERT_DIR"
a_eq "try left the active profile alone" "$(active_profile)" "mine"

rm -f "$SHERPA_FAKE_CLAUDE_MARKER"
t_ok "try --fresh-setup succeeds" try expert --fresh-setup
a_file "fresh-setup still launched the harness" "$SHERPA_FAKE_CLAUDE_MARKER"

t_ok "use switches the active profile" use expert
a_eq "active profile is expert" "$(active_profile)" "expert"

rm -f "$SHERPA_FAKE_CLAUDE_MARKER"
t_ok "run launches the active profile" run
a_eq "run pointed CLAUDE_CONFIG_DIR at the profile" \
	"$(cat "$SHERPA_FAKE_CLAUDE_MARKER" 2>/dev/null)" "$EXPERT_DIR"

section "save and diff"
printf 'expert review skill\nlocal addition\n' >"$EXPERT_DIR/skills/review/SKILL.md"
t_ok "save commits local changes" save -m "local review customization"
a_git_clean "profile tree is clean after save" "$EXPERT_DIR"
t_ok "diff exits 0" diff

section "update"
publish_upstream "$EXPERT" v2 \
	'CHANGELOG.md=# Changelog\n\n## v2\n\n- adds upstream skill\n\n## v1\n\n- initial\n' \
	'skills/upstream/SKILL.md=upstream v2 skill\n'
STDIN_DATA=$'yes\n'
t_ok "update merges v2" update expert
a_contains "update printed the v2 changelog" "$OUT" "adds upstream skill"
a_file "upstream file merged in" "$EXPERT_DIR/skills/upstream/SKILL.md"
a_contains "local edit survived the merge" \
	"$(cat "$EXPERT_DIR/skills/review/SKILL.md")" "local addition"
a_git_clean "profile tree is clean after update" "$EXPERT_DIR"

printf 'local conflict edit\n' >"$EXPERT_DIR/CLAUDE.md"
t_ok "save the conflicting edit" save -m "local claude conflict edit"
BEFORE_SHA="$(git -C "$EXPERT_DIR" rev-parse local)"
publish_upstream "$EXPERT" v3 \
	'CLAUDE.md=upstream conflict edit\n' \
	'CHANGELOG.md=# Changelog\n\n## v3\n\n- edits CLAUDE.md\n\n## v2\n\n- adds upstream skill\n'
STDIN_DATA=$'yes\n'
t_fail "conflicting update is refused" update expert
a_contains "conflict names the contested file" "$OUT$ERR" "CLAUDE.md"
a_eq "conflict left the local branch untouched" \
	"$(git -C "$EXPERT_DIR" rev-parse local)" "$BEFORE_SHA"
a_git_clean "profile tree is clean after refused update" "$EXPERT_DIR"

section "publish"
FORK_REMOTE="$SANDBOX/fixtures/fork.git"
make_bare_remote "$FORK_REMOTE"
STDIN_DATA=$'yes\nyes\n'
t_ok "publish pushes a new immutable version" publish --remote "$FORK_REMOTE"
PUBLISHED_TAGS="$(remote_tags "$FORK_REMOTE")"
a_eq "publish created exactly one tag" "$(printf '%s' "$PUBLISHED_TAGS" | wc -w | tr -d ' ')" "1"
a_contains "published manifest keeps fork lineage" \
	"$(git --git-dir="$FORK_REMOTE" show refs/heads/main:stack.yaml)" \
	'forked_from: "@mentor/base@v7"'

section "profile setup"
t_ok "profile setup re-runs harness setup" profile setup expert

section "back restores the baseline"
t_ok "back returns to mine" back
a_eq "active profile is mine again" "$(active_profile)" "mine"
a_eq "mine is byte-identical after the round trip" "$(tree_sum "$MINE")" "$MINE_SUM"

section "remove guard rails"
t_fail "remove refuses the protected baseline" remove mine --yes
a_file "mine survived the refused remove" "$MINE/CLAUDE.md"
t_ok "use expert before the active-profile check" use expert
t_fail "remove refuses the active profile" remove expert --yes
a_file "expert survived the refused remove" "$EXPERT_DIR/CLAUDE.md"
t_ok "back before removing" back
t_ok "remove deletes an inactive profile" remove expert --yes
a_no_file "expert directory is gone" "$EXPERT_DIR"

section "logout"
t_ok "logout is safe without a session" logout

summary
