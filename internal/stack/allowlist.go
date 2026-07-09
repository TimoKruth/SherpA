package stack

// Tracked stack content, pinned by the Task 1 spike. CLAUDE.md,
// settings.json, skills/, and agents/ are spike-VERIFIED to load from
// CLAUDE_CONFIG_DIR (see docs/superpowers/spikes/2026-07-09-claude-config-dir.md).
// keybindings.json is an UNVERIFIED candidate kept for spec compatibility.
// stack.yaml, README.md, CHANGELOG.md, quarantine.json, and hooks/ are
// sherpa-owned conventions; hook scripts execute via settings.json commands,
// not by directory pickup. Everything else in a profile is runtime state and
// stays untracked (spec §3.2).
var AllowedPaths = []string{
	"stack.yaml", "README.md", "CHANGELOG.md", "CLAUDE.md",
	"settings.json", "keybindings.json", "quarantine.json",
	"skills/", "agents/", "hooks/",
}

// GitignoreContent is a whitelist-style ignore for the tracked stack content
// described by AllowedPaths.
const GitignoreContent = `*
!/.gitignore
!/stack.yaml
!/README.md
!/CHANGELOG.md
!/CLAUDE.md
!/settings.json
!/keybindings.json
!/quarantine.json
!/skills/
!/skills/**
!/agents/
!/agents/**
!/hooks/
!/hooks/**
`
