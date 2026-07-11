package harness

import "testing"

func TestRegistryIncludesClaudeCodeAndCodex(t *testing.T) {
	names := Names()
	if len(names) != 2 || names[0] != "claude-code" || names[1] != "codex" {
		t.Fatalf("Names() = %v, want [claude-code codex]", names)
	}
}
