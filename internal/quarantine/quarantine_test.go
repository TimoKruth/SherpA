package quarantine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

const settings = `{
  "env": {"FOO": "1"},
  "hooks": {"PreToolUse": [{"matcher": "*", "hooks": [{"type": "command", "command": "./hooks/check.sh"}]}]},
  "mcpServers": {"github": {"command": "npx", "args": ["-y", "server-github"]}},
  "permissions": {"allow": ["Bash(echo:*)"]}
}`

func setup(t *testing.T) string {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "settings.json"), []byte(settings), 0o644)
	return d
}

func read(t *testing.T, d, f string) map[string]any {
	b, _ := os.ReadFile(filepath.Join(d, f))
	var m map[string]any
	json.Unmarshal(b, &m)
	return m
}

func TestStripRemovesExecutables(t *testing.T) {
	d := setup(t)
	if err := Strip(d); err != nil {
		t.Fatal(err)
	}
	s := read(t, d, "settings.json")
	if _, ok := s["hooks"]; ok {
		t.Fatal("hooks still live")
	}
	if _, ok := s["mcpServers"]; ok {
		t.Fatal("mcpServers still live")
	}
	if _, ok := s["permissions"]; ok {
		t.Fatal("permissions still live")
	}
	if s["env"].(map[string]any)["FOO"] != "1" {
		t.Fatal("non-executable settings damaged")
	}
	p, _ := Pending(d)
	if len(p) != 3 {
		t.Fatalf("pending = %v", p)
	}
}

func TestApproveAllRestores(t *testing.T) {
	d := setup(t)
	orig := read(t, d, "settings.json")
	Strip(d)
	if err := Approve(d, "all"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(orig, read(t, d, "settings.json")) {
		t.Fatal("approve all did not restore settings")
	}
	if p, _ := Pending(d); len(p) != 0 {
		t.Fatalf("pending after approve: %v", p)
	}
}

func TestApproveSingleMCP(t *testing.T) {
	d := setup(t)
	Strip(d)
	if err := Approve(d, "mcp:github"); err != nil {
		t.Fatal(err)
	}
	s := read(t, d, "settings.json")
	if _, ok := s["mcpServers"].(map[string]any)["github"]; !ok {
		t.Fatal("github mcp not restored")
	}
	if _, ok := s["hooks"]; ok {
		t.Fatal("hooks restored without approval")
	}
}

func TestApproveSinglePermissions(t *testing.T) {
	d := setup(t)
	Strip(d)
	if err := Approve(d, "permissions"); err != nil {
		t.Fatal(err)
	}
	s := read(t, d, "settings.json")
	if _, ok := s["permissions"]; !ok {
		t.Fatal("permissions not restored")
	}
	if _, ok := s["hooks"]; ok {
		t.Fatal("hooks restored without approval")
	}
	if _, ok := s["mcpServers"]; ok {
		t.Fatal("mcpServers restored without approval")
	}
}

func TestStripNoExecutablesNoop(t *testing.T) {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "settings.json"), []byte(`{"env":{}}`), 0o644)
	if err := Strip(d); err != nil {
		t.Fatal(err)
	}
	if p, _ := Pending(d); len(p) != 0 {
		t.Fatalf("pending: %v", p)
	}
}

func TestStripIdempotent(t *testing.T) {
	d := setup(t)
	if err := Strip(d); err != nil {
		t.Fatal(err)
	}
	p1, _ := Pending(d)
	if err := Strip(d); err != nil {
		t.Fatal(err)
	}
	p2, _ := Pending(d)
	if !reflect.DeepEqual(p1, p2) {
		t.Fatalf("second strip changed pending: %v vs %v", p1, p2)
	}
	if len(p2) != 3 {
		t.Fatalf("pending after double strip = %v", p2)
	}
	s := read(t, d, "settings.json")
	if _, ok := s["hooks"]; ok {
		t.Fatal("hooks live after double strip")
	}
	if _, ok := s["mcpServers"]; ok {
		t.Fatal("mcpServers live after double strip")
	}
	if _, ok := s["permissions"]; ok {
		t.Fatal("permissions live after double strip")
	}
}

func TestApproveSingleHook(t *testing.T) {
	d := setup(t)
	Strip(d)
	if err := Approve(d, "hook:PreToolUse:0"); err != nil {
		t.Fatal(err)
	}
	s := read(t, d, "settings.json")
	h, ok := s["hooks"].(map[string]any)
	if !ok {
		t.Fatal("hooks not restored")
	}
	if _, ok := h["PreToolUse"]; !ok {
		t.Fatal("PreToolUse entry not restored")
	}
	if p, _ := Pending(d); len(p) != 2 {
		t.Fatalf("pending after single hook approve = %v", p)
	}
}
