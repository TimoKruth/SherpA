#!/usr/bin/env bash
# Suite 40 — the registry-backed commands, against a live registry.
#
# Covers: search, clone @owner/name, follow, updates, updates --seen, trial
# record/list/share, unfollow, and (opt-in) publish --registry plus update.
#
# This suite talks to a real server and writes real personal state (follows,
# trial verdicts) under the tester's registry account. It therefore refuses to
# run unless SHERPA_BETA_ONLINE=1 is set.
#
# `sherpa login` cannot be scripted: it is a GitHub device flow that needs a
# browser. Log in normally first, then this suite copies the resulting session
# into its sandbox so the rest of the run still leaves ~/.sherpa untouched.
#
#   sherpa login
#   SHERPA_BETA_ONLINE=1 scripts/beta/40-registry-online.sh
#
# Publishing creates an immutable version on the registry and cannot be undone,
# so it is behind a second opt-in:
#
#   SHERPA_BETA_ONLINE=1 SHERPA_BETA_PUBLISH=1 scripts/beta/40-registry-online.sh

SUITE="40-registry-online"
# shellcheck source=lib.sh
. "$(dirname "$0")/lib.sh"

if [ "${SHERPA_BETA_ONLINE:-}" != "1" ]; then
	printf 'skipped: set SHERPA_BETA_ONLINE=1 to run the live-registry suite\n'
	exit 0
fi

REAL_HOME="${SHERPA_REAL_HOME:-$HOME/.sherpa}"
SESSION_SRC="$REAL_HOME/registry-session.json"
if [ ! -f "$SESSION_SRC" ]; then
	printf 'error: no registry session at %s\n' "$SESSION_SRC" >&2
	printf 'run `sherpa login` first; the device flow needs a browser and cannot be scripted\n' >&2
	exit 2
fi

sandbox_init
make_fake_harnesses
make_claude_baseline

# Adopt the tester's session, then pin the registry to the issuer that session
# belongs to. A session minted for staging must never be sent to production, and
# the CLI enforces that; matching the URL here keeps the failure honest.
install -m 600 "$SESSION_SRC" "$SHERPA_HOME/registry-session.json"
export SHERPA_REGISTRY_URL="$(jq -r '.registry_url' "$SESSION_SRC")"
LOGIN_NAME="$(jq -r '.login // ""' "$SESSION_SRC")"
note "registry: $SHERPA_REGISTRY_URL   account: ${LOGIN_NAME:-unknown}"

t_ok "init" init

section "search against the live registry"
QUERY="${SHERPA_BETA_QUERY:-review}"
t_ok "search returns without error" search "$QUERY"
a_not_contains "search does not return placeholder entries" "$OUT" "example.invalid"

STACK_REF="${SHERPA_BETA_STACK:-}"
if [ -z "$STACK_REF" ]; then
	STACK_REF="$(printf '%s\n' "$OUT" | awk '/^@/ {print $1; exit}')"
fi
if [ -z "$STACK_REF" ]; then
	fail "a stack to exercise was found" "search '$QUERY' returned nothing; set SHERPA_BETA_STACK=@owner/name"
	summary
fi
pass "resolved a stack to exercise: $STACK_REF"
STACK_OWNER="$(printf '%s' "$STACK_REF" | sed 's|^@||; s|/.*||')"
STACK_NAME="$(printf '%s' "$STACK_REF" | sed 's|.*/||')"

section "clone by registry ref"
t_ok "clone $STACK_REF" clone "$STACK_REF" --name beta-subject --review=keep
SUBJECT_DIR="$(profile_dir beta-subject)"
a_file "stack installed" "$SUBJECT_DIR/stack.yaml"
a_eq "clone recorded the registry origin" \
	"$(state_get '.profiles."beta-subject".registry.owner // ""')" "$STACK_OWNER"
a_eq "clone left the active profile alone" "$(active_profile)" "mine"

section "follow, updates, seen"
t_ok "follow $STACK_REF" follow "$STACK_REF"
t_ok "updates lists pending versions" updates
t_ok "updates honours --limit" updates --limit 5
INSTALLED_VERSION="$(state_get '.profiles."beta-subject".registry.version // 0')"
STDIN_DATA=""
t_ok "updates --seen marks a version reviewed" \
	updates --seen "@$STACK_OWNER/$STACK_NAME@v$INSTALLED_VERSION"
t_ok "status reports the registry summary" status

section "trial journal"
t_ok "trial record" trial record beta-subject --verdict keep-with-notes --notes "beta smoke run"
t_ok "trial list" trial list
a_contains "trial list shows the entry" "$OUT" "beta-subject"
TRIAL_ID="$(state_get '.trials[-1].id // ""')"
if [ -n "$TRIAL_ID" ]; then
	pass "trial entry has an id"
	t_ok "trial share sends the verdict only" trial share "$TRIAL_ID"
	a_eq "notes were kept local" "$(state_get '.trials[-1].notes // ""')" "beta smoke run"
	a_not_contains "share output does not echo the notes" "$OUT" "beta smoke run"
else
	fail "trial entry has an id" "no trial recorded in state.json"
fi
t_ok "trial list --profile filters" trial list --profile beta-subject

section "unfollow"
t_ok "unfollow $STACK_REF" unfollow "$STACK_REF"

section "publish to the registry"
if [ "${SHERPA_BETA_PUBLISH:-}" != "1" ]; then
	note "skipped: set SHERPA_BETA_PUBLISH=1 to publish immutable versions to $SHERPA_REGISTRY_URL"
else
	PUB_NAME="${SHERPA_BETA_PUBLISH_NAME:-beta-smoke-$(date +%Y%m%d%H%M%S)}"
	PUB_PROFILE="$PUB_NAME-p"

	# The scratch stack has to be a real git repository for clone to accept it,
	# and it must live outside profiles/ so clone is not asked to install a
	# directory over itself. owner is deliberately omitted: the registry only
	# accepts the session login, so publish infers and records it, and leaving
	# it out exercises that path.
	PUB_SRC="$SANDBOX/fixtures/$PUB_NAME"
	mkdir -p "$PUB_SRC"
	cat >"$PUB_SRC/stack.yaml" <<EOF
name: $PUB_NAME
version: 1
harness: claude-code
summary: Disposable beta smoke-test stack
EOF
	printf 'beta smoke stack\n' >"$PUB_SRC/README.md"
	printf '# Changelog\n\n## v1\n\n- initial\n' >"$PUB_SRC/CHANGELOG.md"
	printf 'beta smoke instructions v1\n' >"$PUB_SRC/CLAUDE.md"
	git_init_repo "$PUB_SRC"

	PUB_FAILS_BEFORE=$FAIL_COUNT
	t_ok "clone the scratch stack into a profile" clone "$PUB_SRC" --name "$PUB_PROFILE" --review=keep
	t_ok "activate the scratch profile" use "$PUB_PROFILE"
	STDIN_DATA=$'yes\nyes\n'
	t_ok "publish v1 to the registry" publish --registry "$SHERPA_REGISTRY_URL"
	a_contains "publish recorded the inferred owner" "$OUT" "recorded owner @${LOGIN_NAME}"
	printf 'beta smoke instructions v2\n' >"$(profile_dir "$PUB_PROFILE")/CLAUDE.md"
	t_ok "save v2 content" save -m "v2"
	STDIN_DATA=$'yes\nyes\n'
	t_ok "publish v2 to the registry" publish --registry "$SHERPA_REGISTRY_URL"
	t_ok "back to the baseline" back

	t_ok "the published stack is searchable" search "$PUB_NAME"
	a_contains "search finds the freshly published stack" "$OUT" "$PUB_NAME"

	if [ "$FAIL_COUNT" -eq "$PUB_FAILS_BEFORE" ]; then
		note "published @${LOGIN_NAME}/${PUB_NAME} v1 and v2 — immutable until the beta reset"
	else
		note "publish did NOT complete; check $SHERPA_REGISTRY_URL for a partial @${LOGIN_NAME}/${PUB_NAME}"
	fi
fi

summary
