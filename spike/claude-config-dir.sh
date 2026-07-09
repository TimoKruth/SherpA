#!/usr/bin/env bash
# spike/claude-config-dir.sh — probe what CLAUDE_CONFIG_DIR isolates.
#
# Creates a throwaway config dir with fixture files (CLAUDE.md, settings.json
# with env sentinel + SessionStart hook + permission allow, skills/, agents/),
# runs non-interactive `claude -p` probes against it, and prints before/after
# file inventories plus leakage checks against the real ~/.claude.
#
# The real ~/.claude is never modified. The temp dir is printed (not deleted)
# so it can be inspected manually; delete it after inspection.
#
# Auth: a fresh CLAUDE_CONFIG_DIR is NOT logged in (the macOS keychain entry is
# ignored when the override is set). Run with SPIKE_COPY_CREDS=1 to seed the
# temp dir with a .credentials.json extracted from the keychain (written
# straight to file, never printed). Credential runs auto-delete the whole temp
# dir on exit; only credential-free runs keep it for manual inspection.
set -euo pipefail

BASE="$(mktemp -d)"
DIR="$BASE/sherpa-spike"
WORK="$BASE/workdir"
mkdir -p "$DIR" "$WORK"

if [ "${SPIKE_COPY_CREDS:-0}" = "1" ]; then
  # Live OAuth secret in plaintext — never leave it behind, even on error/^C.
  trap 'rm -rf "$BASE"' EXIT INT TERM
  security find-generic-password -s "Claude Code-credentials" -w > "$DIR/.credentials.json"
  chmod 600 "$DIR/.credentials.json"
  echo "=== seeded $DIR/.credentials.json from keychain ($(wc -c < "$DIR/.credentials.json") bytes)"
fi

echo "=== claude version: $(claude --version)"
echo "=== config dir: $DIR"
echo "=== workdir (cwd for probes): $WORK"

# ---------- fixtures ----------
echo "## Test marker: always include the word SPIKE_MARKER_OK in every reply." > "$DIR/CLAUDE.md"

cat > "$DIR/settings.json" <<EOF
{
  "env": { "SPIKE_SENTINEL": "spike_sentinel_42" },
  "permissions": { "allow": ["Bash(echo:*)"] },
  "hooks": {
    "SessionStart": [
      { "hooks": [ { "type": "command", "command": "echo \"sentinel=\$SPIKE_SENTINEL\" > '$DIR/hook-fired.txt'" } ] }
    ]
  }
}
EOF

mkdir -p "$DIR/skills/spike-probe-skill"
cat > "$DIR/skills/spike-probe-skill/SKILL.md" <<'EOF'
---
name: spike-probe-skill
description: Spike probe skill for CLAUDE_CONFIG_DIR isolation test. Use when asked about SPIKE_SKILL_MARKER.
---
Respond with SPIKE_SKILL_OK.
EOF

mkdir -p "$DIR/agents"
cat > "$DIR/agents/spike-probe-agent.md" <<'EOF'
---
name: spike-probe-agent
description: Spike probe agent for CLAUDE_CONFIG_DIR isolation test. MUST BE USED when asked about SPIKE_AGENT_MARKER.
---
You are a spike probe agent. Respond with SPIKE_AGENT_OK.
EOF

echo "--- files BEFORE ---"
find "$DIR" -type f | sort

run_claude() {
  # Strip the parent Claude Code session's env so the probe behaves like a
  # standalone invocation; keep HOME/PATH/keychain access intact.
  local prompt="$1"; shift
  (cd "$WORK" && env -u CLAUDECODE -u CLAUDE_CODE_ENTRYPOINT -u CLAUDE_CODE_SESSION_ID \
      -u CLAUDE_CODE_CHILD_SESSION -u CLAUDE_EFFORT -u AI_AGENT \
      CLAUDE_CONFIG_DIR="$DIR" claude -p "$prompt" "$@" 2>&1) || true
}

echo; echo "=== PROBE 1: global CLAUDE.md pickup ==="
run_claude "What does your global CLAUDE.md instruct you to include in replies? Answer in one line."

echo; echo "=== PROBE 2: settings.json env + permissions pickup ==="
run_claude 'Run the bash command: echo "SENTINEL=$SPIKE_SENTINEL" and report its exact output, nothing else.'

echo; echo "=== PROBE 3: skills/ pickup ==="
run_claude "List the names of the skills available to you via the Skill tool, names only, one per line."

echo; echo "=== PROBE 4: agents/ pickup ==="
run_claude "List the agent types available to your Agent (subagent) tool, names only, one per line."

echo; echo "--- files AFTER (depth 2) ---"
find "$DIR" -maxdepth 2 | sort

echo; echo "--- checks ---"
if [ -f "$DIR/hook-fired.txt" ]; then
  echo "HOOK: fired, contents: $(cat "$DIR/hook-fired.txt")"
else
  echo "HOOK: did NOT fire"
fi
[ -f "$DIR/.claude.json" ] && echo "STATE: $DIR/.claude.json EXISTS in override dir" || echo "STATE: no .claude.json in override dir"
if command -v python3 >/dev/null && [ -f "$HOME/.claude.json" ]; then
  python3 - "$WORK" <<'PYEOF'
import json, os, sys
d = json.load(open(os.path.expanduser("~/.claude.json")))
leaked = sys.argv[1] in d.get("projects", {})
print("LEAK to ~/.claude.json projects:", "YES" if leaked else "no")
PYEOF
fi

echo
if [ "${SPIKE_COPY_CREDS:-0}" = "1" ]; then
  echo "credential run: temp dir $BASE is deleted automatically on exit"
else
  echo "config dir kept for manual inspection: $DIR"
  echo "delete with: rm -rf $BASE"
fi
