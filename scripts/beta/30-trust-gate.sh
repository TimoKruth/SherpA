#!/usr/bin/env bash
# Suite 30 — the trust model: quarantine, the review gate, and the publish
# sanitizer. These are the checks whose failure is a launch blocker rather than
# a follow-up, so they get their own suite.
#
# Covers: clone/try --review interactive|approve-all|keep, --approve-all,
# the y/n/a/q answers, secret detection, and manifest validation.

SUITE="30-trust-gate"
# shellcheck source=lib.sh
. "$(dirname "$0")/lib.sh"

sandbox_init
make_fake_harnesses
make_claude_baseline

EXPERT="$SANDBOX/fixtures/expert"
make_expert_repo "$EXPERT"

t_ok "init" init

# settings_has <profile-dir> <key> — is the capability live in settings.json?
settings_has() { jq -e --arg k "$2" 'has($k)' "$1/settings.json" >/dev/null 2>&1; }
quarantine_has() { jq -e --arg k "$2" 'has($k)' "$1/quarantine.json" >/dev/null 2>&1; }

check_quarantined() { # check_quarantined <label> <dir>
	local label="$1" dir="$2" key
	a_file "$label: quarantine.json written" "$dir/quarantine.json"
	for key in hooks mcpServers permissions; do
		if settings_has "$dir" "$key"; then
			fail "$label: $key stripped from settings.json" "still live"
		else
			pass "$label: $key stripped from settings.json"
		fi
		if quarantine_has "$dir" "$key"; then
			pass "$label: $key held in quarantine.json"
		else
			fail "$label: $key held in quarantine.json" "not found"
		fi
	done
}

check_approved() { # check_approved <label> <dir>
	local label="$1" dir="$2" key
	for key in hooks mcpServers permissions; do
		if settings_has "$dir" "$key"; then
			pass "$label: $key restored to settings.json"
		else
			fail "$label: $key restored to settings.json" "still quarantined"
		fi
	done
}

section "--review=keep leaves every capability quarantined"
t_ok "clone with --review=keep" clone "$EXPERT" --name keep-mode --review=keep
check_quarantined "keep" "$(profile_dir keep-mode)"

section "clone defaults to keep-quarantined"
# The safe default matters: an unattended clone must not silently grant
# capabilities, so interactive review is opt-in rather than the fallback.
t_ok "clone with no review flag" clone "$EXPERT" --name default-mode
check_quarantined "default" "$(profile_dir default-mode)"

section "interactive: EOF on stdin approves nothing"
t_ok "clone interactively with no answers" clone "$EXPERT" --name eof-mode --review=interactive
check_quarantined "eof" "$(profile_dir eof-mode)"

section "interactive: answering n to every capability"
STDIN_DATA=$'n\nn\nn\nn\n'
t_ok "clone answering n" clone "$EXPERT" --name deny-mode --review=interactive
a_contains "gate showed the approve prompt" "$OUT" "approve?"
check_quarantined "deny" "$(profile_dir deny-mode)"

section "interactive: answering a approves the rest"
STDIN_DATA=$'a\n'
t_ok "clone answering a" clone "$EXPERT" --name all-mode --review=interactive
check_approved "all" "$(profile_dir all-mode)"

section "interactive: q stops without approving the remainder"
STDIN_DATA=$'q\n'
t_ok "clone answering q" clone "$EXPERT" --name quit-mode --review=interactive
check_quarantined "quit" "$(profile_dir quit-mode)"

section "--review=approve-all and the --approve-all shorthand"
t_ok "clone with --review=approve-all" clone "$EXPERT" --name approve-mode --review=approve-all
check_approved "approve-all" "$(profile_dir approve-mode)"
t_ok "clone with --approve-all" clone "$EXPERT" --name shorthand-mode --approve-all
check_approved "shorthand" "$(profile_dir shorthand-mode)"

section "the gate discloses what it is asking about"
# Walk every capability with n so each one is rendered; q would return before
# the later entries are ever shown.
STDIN_DATA=$'n\nn\nn\nn\n'
sherpa_run clone "$EXPERT" --name disclose-mode --review=interactive
a_contains "gate names the hook purpose" "$OUT" "Checks writes"
a_contains "gate names the MCP purpose" "$OUT" "Reads docs"
a_contains "gate shows the hook script body" "$OUT" "exit 0"

section "publish sanitizer blocks secrets"
SECRET_REMOTE="$SANDBOX/fixtures/secret.git"
make_bare_remote "$SECRET_REMOTE"
t_ok "activate a profile to publish from" use approve-mode
SECRET_DIR="$(profile_dir approve-mode)"
# A syntactically valid but fake GitHub token: ghp_ plus 36 characters.
printf 'expert instructions\ntoken ghp_%s\n' "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" \
	>"$SECRET_DIR/CLAUDE.md"
t_ok "save the planted secret" save -m "planted secret fixture"
STDIN_DATA=$'yes\nyes\n'
t_fail "publish is blocked by the secret scan" publish --remote "$SECRET_REMOTE"
a_eq "blocked publish pushed no refs" "$(remote_refs "$SECRET_REMOTE")" ""

# Deleting the secret from the worktree is not enough: the scan covers history,
# so a committed secret keeps the stack unpublishable until the history itself
# is rewritten. This is the property that stops a token leaking via `git log`.
printf 'expert instructions\n' >"$SECRET_DIR/CLAUDE.md"
t_ok "remove the secret from the worktree" save -m "remove planted secret"
STDIN_DATA=$'yes\nyes\n'
t_fail "publish stays blocked while the secret is in history" publish --remote "$SECRET_REMOTE"
a_eq "still no refs pushed" "$(remote_refs "$SECRET_REMOTE")" ""

section "publish proceeds when only warnings are present"
# A profile with no secret anywhere in its history.
WARN_REMOTE="$SANDBOX/fixtures/warn.git"
make_bare_remote "$WARN_REMOTE"
t_ok "activate a clean profile" use shorthand-mode
WARN_DIR="$(profile_dir shorthand-mode)"
printf 'expert instructions\nsee /Users/someone/notes and mail me@example.com\n' \
	>"$WARN_DIR/CLAUDE.md"
t_ok "save the warning-only content" save -m "home path and email"
# Warnings add a confirmation of their own ahead of the version confirmation,
# so a warning-carrying publish needs one more yes than a clean one.
STDIN_DATA=$'yes\nyes\nyes\n'
t_ok "publish succeeds with warnings only" publish --remote "$WARN_REMOTE"
a_contains "publish surfaced the sanitizer warnings" "$OUT" "Sanitizer warnings found"
a_eq "publish created exactly one tag" \
	"$(remote_tags "$WARN_REMOTE" | wc -w | tr -d ' ')" "1"

section "manifest validation rejects an undeclared hook"
BAD="$SANDBOX/fixtures/undeclared"
mkdir -p "$BAD/hooks"
cat >"$BAD/stack.yaml" <<'EOF'
name: undeclared-hook
owner: "@expert"
version: 1
harness: claude-code
summary: Ships a hook it never declares
EOF
printf 'undeclared hook stack\n' >"$BAD/README.md"
printf '# Changelog\n\n## v1\n\n- initial\n' >"$BAD/CHANGELOG.md"
printf 'instructions\n' >"$BAD/CLAUDE.md"
printf '#!/bin/sh\nexit 0\n' >"$BAD/hooks/sneaky.sh"
chmod 755 "$BAD/hooks/sneaky.sh"
git_init_repo "$BAD"
t_fail "clone refuses an undeclared hook" clone "$BAD" --name undeclared --review=keep
a_no_file "undeclared stack was not installed" "$(profile_dir undeclared)/CLAUDE.md"

summary
