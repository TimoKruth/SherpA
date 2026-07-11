#!/usr/bin/env bash
# spike/codex-home.sh — probe what CODEX_HOME isolates for SherpA's Codex adapter (2b-ii).
#
# Mirrors the Phase-1 CLAUDE_CONFIG_DIR spike. Creates a throwaway CODEX_HOME with
# fixture files (AGENTS.md marker, config.toml, rules/, skills/), runs `codex exec`
# probes against it, and reports what loads + what runtime state is written.
#
# The real ~/.codex is only ever READ (never modified). Auth handling:
#   - First probe runs with NO auth.json in the temp home, to learn whether a fresh
#     CODEX_HOME requires auth.json or falls back to keychain/env.
#   - If (and only if) SPIKE_COPY_AUTH=1, the real ~/.codex/auth.json is COPIED into
#     the temp home to enable an authenticated probe. The temp dir is then auto-deleted
#     on EXIT/INT/TERM so the copied credential never lingers. Contents are NEVER printed.
set -uo pipefail

BASE="$(mktemp -d)"
DIR="$BASE/codex-home"
WORK="$BASE/workdir"
mkdir -p "$DIR" "$WORK"

echo "=== codex: $(codex --version 2>&1 | head -1)"
echo "=== CODEX_HOME: $DIR"
echo "=== workdir:    $WORK"

# ---------- fixtures ----------
cat > "$DIR/AGENTS.md" <<'EOF'
# Spike global instructions
No matter what is asked, include the exact token CODEX_SPIKE_MARKER_OK in every reply.
EOF

cat > "$DIR/config.toml" <<'EOF'
# spike config — intentionally no model override (account default), so probes reach a reply.
# (A prior run proved config.toml IS loaded from CODEX_HOME: a model= line here was applied.)
EOF

mkdir -p "$DIR/rules"
cat > "$DIR/rules/spike.md" <<'EOF'
When asked about SPIKE_RULE, respond SPIKE_RULE_OK.
EOF

mkdir -p "$DIR/skills/spike-probe"
cat > "$DIR/skills/spike-probe/SKILL.md" <<'EOF'
---
name: spike-probe
description: Spike probe skill. Use when asked about SPIKE_SKILL.
---
Respond SPIKE_SKILL_OK.
EOF

echo "--- files BEFORE ---"
find "$DIR" -type f | sort

run_codex() {
  local prompt="$1"
  # stdin closed (codex exec waits on an open stdin) + skip-git-repo-check (temp workdir
  # is not a trusted git repo, which otherwise blocks the run before auth is even checked).
  (cd "$WORK" && env CODEX_HOME="$DIR" codex exec --sandbox read-only --skip-git-repo-check "$prompt" </dev/null 2>&1) || true
}

echo; echo "=== PROBE 0: auth — does a fresh CODEX_HOME (no auth.json) work? ==="
[ -f "$DIR/auth.json" ] && echo "auth.json present (unexpected)" || echo "auth.json ABSENT in temp home"
run_codex "Reply in one short line." | tail -8

if [ "${SPIKE_COPY_AUTH:-0}" = "1" ]; then
  trap 'rm -rf "$BASE"' EXIT INT TERM   # armed BEFORE the credential lands; auto-clean on any exit
  echo; echo "=== copying real ~/.codex/auth.json into temp home (contents never printed) ==="
  cp ~/.codex/auth.json "$DIR/auth.json"
  chmod 600 "$DIR/auth.json"
  echo "auth.json copied; size $(wc -c < "$DIR/auth.json") bytes"

  echo; echo "=== PROBE 1: AGENTS.md global-instruction pickup ==="
  run_codex "What token must you include per your global instructions? One line." | tail -6

  echo; echo "=== PROBE 2: rules/ pickup ==="
  run_codex "Follow SPIKE_RULE and reply accordingly. One line." | tail -6

  echo; echo "=== PROBE 3: skills/ pickup ==="
  run_codex "If you have a spike-probe skill, invoke SPIKE_SKILL. One line." | tail -6
else
  echo; echo "(set SPIKE_COPY_AUTH=1 to run authenticated probes 1-3)"
fi

echo; echo "--- files AFTER (depth 2) ---"
find "$DIR" -maxdepth 2 | sort

echo; echo "--- runtime-state check: what did codex create beyond the fixtures? ---"
for f in history.jsonl sessions logs log installation_id version.json .codex-global-state.json session_index.jsonl cache tmp; do
  [ -e "$DIR/$f" ] && echo "  wrote: $f"
done

echo; echo "--- leak check: did codex write into the real ~/.codex during the run? (mtimes only) ---"
ls -1t ~/.codex 2>/dev/null | head -3 | while read -r x; do echo "  recent ~/.codex entry: $x"; done

echo; echo "temp base: $BASE  (auto-deleted if SPIKE_COPY_AUTH=1; else inspect then: rm -rf $BASE)"
