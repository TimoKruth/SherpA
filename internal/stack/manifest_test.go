package stack

import (
	"os"
	"path/filepath"
	"testing"

	"sherpa/internal/harness"
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
	if v := m.Validate(writeStack(t, true), mustHarness(t, m.Harness)); len(v) != 0 {
		t.Fatalf("violations: %v", v)
	}
}

func TestValidateRejectsWrongHarness(t *testing.T) {
	m, _ := Parse([]byte(goodYAML))
	m.Harness = "pi"
	if v := m.Validate(writeStack(t, true), mustHarness(t, m.Harness)); len(v) == 0 {
		t.Fatal("want harness violation")
	}
}

func TestValidateRejectsUndeclaredHook(t *testing.T) {
	d := writeStack(t, true)
	os.WriteFile(filepath.Join(d, "hooks", "sneaky.sh"), []byte("#!/bin/sh"), 0o755)
	m, _ := Parse([]byte(goodYAML))
	if v := m.Validate(d, mustHarness(t, m.Harness)); len(v) == 0 {
		t.Fatal("want undeclared-hook violation")
	}
}

func TestValidateRejectsMissingDeclaredHook(t *testing.T) {
	m, _ := Parse([]byte(goodYAML))
	if v := m.Validate(writeStack(t, false), mustHarness(t, m.Harness)); len(v) == 0 {
		t.Fatal("want missing-hook violation")
	}
}

func TestValidateRejectsLiveExecutablesInSettings(t *testing.T) {
	d := writeStack(t, true)
	os.WriteFile(filepath.Join(d, "settings.json"),
		[]byte(`{"hooks":{"PreToolUse":[{"command":"evil"}]}}`), 0o644)
	m, _ := Parse([]byte(goodYAML))
	if v := m.Validate(d, mustHarness(t, m.Harness)); len(v) == 0 {
		t.Fatal("want live-executable violation")
	}
}

func TestValidateRejectsHookPathOutsideHooksDir(t *testing.T) {
	for _, path := range []string{"../evil.sh", "skills/x.sh", "/abs/evil.sh", ""} {
		t.Run(path, func(t *testing.T) {
			d := writeStack(t, true)
			m, _ := Parse([]byte(goodYAML))
			m.Executes.Hooks = append(m.Executes.Hooks, HookDecl{Path: path, Event: "PreToolUse", Purpose: "evil"})
			want := "hook path must be a local path under hooks/: " + path
			if v := m.Validate(d, mustHarness(t, m.Harness)); !contains(v, want) {
				t.Fatalf("want %q, got %v", want, v)
			}
		})
	}
}

func TestValidateRejectsExecutableOutsideHooks(t *testing.T) {
	d := writeStack(t, true)
	os.WriteFile(filepath.Join(d, "run.sh"), []byte("#!/bin/sh"), 0o755)
	m, _ := Parse([]byte(goodYAML))
	want := "undeclared executable outside hooks/: run.sh"
	if v := m.Validate(d, mustHarness(t, m.Harness)); !contains(v, want) {
		t.Fatalf("want %q, got %v", want, v)
	}
}

func TestValidateAllowsExecutableUnderSkills(t *testing.T) {
	d := writeStack(t, true)
	os.MkdirAll(filepath.Join(d, "skills", "review"), 0o755)
	os.WriteFile(filepath.Join(d, "skills", "review", "helper.sh"), []byte("#!/bin/sh"), 0o755)
	m, _ := Parse([]byte(goodYAML))
	if v := m.Validate(d, mustHarness(t, m.Harness)); len(v) != 0 {
		t.Fatalf("violations: %v", v)
	}
}

func TestValidateRejectsInvalidSettingsJSON(t *testing.T) {
	d := writeStack(t, true)
	os.WriteFile(filepath.Join(d, "settings.json"), []byte(`{not json`), 0o644)
	m, _ := Parse([]byte(goodYAML))
	if v := m.Validate(d, mustHarness(t, m.Harness)); len(v) == 0 {
		t.Fatal("want invalid-settings violation")
	}
}

func TestValidateAllowsPrettyPrintedEmptySettings(t *testing.T) {
	d := writeStack(t, true)
	os.WriteFile(filepath.Join(d, "settings.json"),
		[]byte("{\n  \"hooks\": {\n  },\n  \"mcpServers\": [\n  ]\n}"), 0o644)
	m, _ := Parse([]byte(goodYAML))
	if v := m.Validate(d, mustHarness(t, m.Harness)); len(v) != 0 {
		t.Fatalf("violations: %v", v)
	}
}

func mustHarness(t *testing.T, name string) harness.Harness {
	t.Helper()
	h, err := harness.For(name)
	if err != nil {
		if name == "pi" {
			return nil
		}
		t.Fatalf("harness.For(%q): %v", name, err)
	}
	return h
}

func contains(vs []string, want string) bool {
	for _, v := range vs {
		if v == want {
			return true
		}
	}
	return false
}
