package harness

import "testing"

func TestRegistryOnlyClaudeCodeUntilCodexRegistration(t *testing.T) {
	names := Names()
	if len(names) != 1 || names[0] != "claude-code" {
		t.Fatalf("Names() = %v, want only claude-code", names)
	}
}
