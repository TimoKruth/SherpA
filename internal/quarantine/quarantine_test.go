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

// twoHookSettings has two distinguishable matcher entries under one event so we
// can prove that approving by sequence id selects the intended entry even after
// an earlier approval removed a lower-numbered one (positional ids would shift).
const twoHookSettings = `{
  "hooks": {"PreToolUse": [
    {"matcher": "A", "hooks": [{"type": "command", "command": "./hooks/a.sh"}]},
    {"matcher": "B", "hooks": [{"type": "command", "command": "./hooks/b.sh"}]}
  ]}
}`

func matcherOf(entry any) string {
	return entry.(map[string]any)["matcher"].(string)
}

func TestApproveHookBySeqStableAfterEarlierApproval(t *testing.T) {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "settings.json"), []byte(twoHookSettings), 0o644)
	Strip(d)
	p, _ := Pending(d)
	if len(p) != 2 || p[0] != "hook:PreToolUse:0" || p[1] != "hook:PreToolUse:1" {
		t.Fatalf("pending seq ids = %v", p)
	}
	// Approve seq 0 (matcher A). Seq 1 (matcher B) must remain addressable as
	// "hook:PreToolUse:1", NOT renumber to 0.
	if err := Approve(d, "hook:PreToolUse:0"); err != nil {
		t.Fatal(err)
	}
	if p, _ := Pending(d); len(p) != 1 || p[0] != "hook:PreToolUse:1" {
		t.Fatalf("pending after approving seq 0 = %v", p)
	}
	if err := Approve(d, "hook:PreToolUse:1"); err != nil {
		t.Fatal(err)
	}
	s := read(t, d, "settings.json")
	arr := s["hooks"].(map[string]any)["PreToolUse"].([]any)
	if len(arr) != 2 {
		t.Fatalf("expected both hooks restored, got %d", len(arr))
	}
	got := map[string]bool{matcherOf(arr[0]): true, matcherOf(arr[1]): true}
	if !got["A"] || !got["B"] {
		t.Fatalf("wrong hooks restored: %v", got)
	}
	if p, _ := Pending(d); len(p) != 0 {
		t.Fatalf("pending after both approvals = %v", p)
	}
}

// TestStripWritesQuarantineBeforeSettings pins the crash-safety write order:
// quarantine.json must land BEFORE settings.json is stripped, so a crash
// between the two writes leaves capabilities live-and-recoverable rather than
// lost. We block the settings write by occupying its temp path with a
// directory, then confirm quarantine.json exists and settings is untouched.
func TestStripWritesQuarantineBeforeSettings(t *testing.T) {
	d := setup(t)
	if err := os.Mkdir(filepath.Join(d, "settings.json.tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Strip(d); err == nil {
		t.Fatal("expected settings write to fail")
	}
	if _, err := os.Stat(filepath.Join(d, "quarantine.json")); err != nil {
		t.Fatal("quarantine.json not written before settings failed")
	}
	s := read(t, d, "settings.json")
	if _, ok := s["hooks"]; !ok {
		t.Fatal("hooks lost despite settings write failure")
	}
	// Recovery: clear the blocker and re-Strip. Dedupe must make it converge
	// without duplicating what is already quarantined.
	os.Remove(filepath.Join(d, "settings.json.tmp"))
	if err := Strip(d); err != nil {
		t.Fatal(err)
	}
	if p, _ := Pending(d); len(p) != 3 {
		t.Fatalf("recovery strip not idempotent: pending = %v", p)
	}
	s = read(t, d, "settings.json")
	if _, ok := s["hooks"]; ok {
		t.Fatal("hooks still live after recovery strip")
	}
}

// TestApproveConvergesFromCrashState simulates a crash mid-Approve where
// settings already received the hook back but quarantine still lists it (the
// settings write commits before the quarantine removal). Re-running Approve
// must converge: no duplicate hook in settings, and the entry cleared from
// quarantine.
func TestApproveConvergesFromCrashState(t *testing.T) {
	d := setup(t)
	Strip(d)
	// The hook is present in BOTH files: forge settings with the hook already
	// restored while quarantine (from Strip) still holds it.
	crash := `{"env":{"FOO":"1"},"hooks":{"PreToolUse":[{"matcher":"*","hooks":[{"type":"command","command":"./hooks/check.sh"}]}]}}`
	os.WriteFile(filepath.Join(d, "settings.json"), []byte(crash), 0o644)
	if err := Approve(d, "hook:PreToolUse:0"); err != nil {
		t.Fatal(err)
	}
	s := read(t, d, "settings.json")
	arr := s["hooks"].(map[string]any)["PreToolUse"].([]any)
	if len(arr) != 1 {
		t.Fatalf("duplicate hook after crash-state approve: %d entries", len(arr))
	}
	for _, id := range mustPending(t, d) {
		if id == "hook:PreToolUse:0" {
			t.Fatal("hook still pending after approve")
		}
	}
}

func mustPending(t *testing.T, d string) []string {
	p, err := Pending(d)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestReStripNewestWins pins the deliberate newest-wins semantic: when a fresh
// (different) live value for an already-quarantined mcp name or the permissions
// unit is stripped, it replaces the quarantined value — quarantine holds the
// pending capabilities of the CURRENT stack version.
func TestReStripNewestWins(t *testing.T) {
	d := setup(t)
	Strip(d)
	next := `{"mcpServers":{"github":{"command":"NEW"}},"permissions":{"allow":["Bash(new:*)"]}}`
	os.WriteFile(filepath.Join(d, "settings.json"), []byte(next), 0o644)
	Strip(d)
	q := read(t, d, "quarantine.json")
	gh := q["mcpServers"].(map[string]any)["github"].(map[string]any)
	if gh["command"] != "NEW" {
		t.Fatalf("mcp newest-wins failed: %v", gh)
	}
	perm := q["permissions"].(map[string]any)["allow"].([]any)
	if len(perm) != 1 || perm[0] != "Bash(new:*)" {
		t.Fatalf("permissions newest-wins failed: %v", perm)
	}
}
