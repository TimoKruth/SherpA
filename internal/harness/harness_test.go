package harness

import "testing"

func TestForKnownAndUnknown(t *testing.T) {
	h, err := For("claude-code")
	if err != nil || h.Name() != "claude-code" {
		t.Fatalf("For(claude-code) = %v, %v", h, err)
	}
	h, err = For("codex")
	if err != nil || h.Name() != "codex" {
		t.Fatalf("For(codex) = %v, %v", h, err)
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

func TestEveryHarnessHasNonEmptyBarrier(t *testing.T) {
	for _, name := range Names() {
		h, _ := For(name)
		if len(h.SetupStateFilenames()) == 0 {
			t.Errorf("%s: empty SetupStateFilenames weakens its publish barrier", name)
		}
		if len(h.LoginSignatures()) == 0 {
			t.Errorf("%s: empty LoginSignatures weakens its publish barrier", name)
		}
	}
}
