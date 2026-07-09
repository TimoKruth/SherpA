package stack

import (
	"os"
	"path/filepath"
	"testing"
)

const goodYAML = `
name: rust-reviewer
owner: "@jane"
version: 14
harness: claude-code
summary: "Rust review setup"
tags: [rust]
executes:
  hooks:
    - path: hooks/check.sh
      event: PreToolUse
      purpose: "guard"
  mcp_servers: []
parameters: []
`

func writeStack(t *testing.T, withHookFile bool) string {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "stack.yaml"), []byte(goodYAML), 0o644)
	os.WriteFile(filepath.Join(d, "settings.json"), []byte(`{}`), 0o644)
	if withHookFile {
		os.MkdirAll(filepath.Join(d, "hooks"), 0o755)
		os.WriteFile(filepath.Join(d, "hooks", "check.sh"), []byte("#!/bin/sh"), 0o755)
	}
	return d
}

func TestParseAndValidateOK(t *testing.T) {
	m, err := Parse([]byte(goodYAML))
	if err != nil || m.Name != "rust-reviewer" || m.Version != 14 {
		t.Fatalf("%+v %v", m, err)
	}
	if v := m.Validate(writeStack(t, true)); len(v) != 0 {
		t.Fatalf("violations: %v", v)
	}
}

func TestValidateRejectsWrongHarness(t *testing.T) {
	m, _ := Parse([]byte(goodYAML))
	m.Harness = "pi"
	if v := m.Validate(writeStack(t, true)); len(v) == 0 {
		t.Fatal("want harness violation")
	}
}

func TestValidateRejectsUndeclaredHook(t *testing.T) {
	d := writeStack(t, true)
	os.WriteFile(filepath.Join(d, "hooks", "sneaky.sh"), []byte("#!/bin/sh"), 0o755)
	m, _ := Parse([]byte(goodYAML))
	if v := m.Validate(d); len(v) == 0 {
		t.Fatal("want undeclared-hook violation")
	}
}

func TestValidateRejectsMissingDeclaredHook(t *testing.T) {
	m, _ := Parse([]byte(goodYAML))
	if v := m.Validate(writeStack(t, false)); len(v) == 0 {
		t.Fatal("want missing-hook violation")
	}
}

func TestValidateRejectsLiveExecutablesInSettings(t *testing.T) {
	d := writeStack(t, true)
	os.WriteFile(filepath.Join(d, "settings.json"),
		[]byte(`{"hooks":{"PreToolUse":[{"command":"evil"}]}}`), 0o644)
	m, _ := Parse([]byte(goodYAML))
	if v := m.Validate(d); len(v) == 0 {
		t.Fatal("want live-executable violation")
	}
}
