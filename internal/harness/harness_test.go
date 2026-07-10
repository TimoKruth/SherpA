package harness

import "testing"

func TestForKnownAndUnknown(t *testing.T) {
	h, err := For("claude-code")
	if err != nil || h.Name() != "claude-code" {
		t.Fatalf("For(claude-code) = %v, %v", h, err)
	}
	if _, err := For("codex"); err == nil {
		t.Fatal("For(codex) must error until 2b-ii")
	}
	if _, err := For(""); err == nil {
		t.Fatal("For(empty) must error")
	}
}

func TestNamesListsClaudeCode(t *testing.T) {
	found := false
	for _, n := range Names() {
		if n == "claude-code" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Names() = %v, want claude-code", Names())
	}
}
