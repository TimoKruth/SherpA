package profile

import (
	"os"
	"path/filepath"
	"sherpa/internal/harness"
	"testing"
)

func TestConfigMaterializesLinkedSkillsAndExcludesRuntime(t *testing.T) {
	src := t.TempDir()
	dest := filepath.Join(t.TempDir(), "copy")
	linked := t.TempDir()
	os.WriteFile(filepath.Join(linked, "SKILL.md"), []byte("source skill"), 0600)
	os.WriteFile(filepath.Join(src, "history.jsonl"), []byte("private conversation"), 0600)
	os.WriteFile(filepath.Join(src, "auth.json"), []byte("credential"), 0600)
	if err := os.Symlink(linked, filepath.Join(src, "skills")); err != nil {
		t.Skip(err)
	}
	if err := ImportConfig(src, dest, harness.Codex{}); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dest, "skills", "SKILL.md"), []byte("changed"), 0600)
	b, _ := os.ReadFile(filepath.Join(linked, "SKILL.md"))
	if string(b) != "source skill" {
		t.Fatal("wrote through source link")
	}
	if _, err := os.Stat(filepath.Join(dest, "history.jsonl")); !os.IsNotExist(err) {
		t.Fatal("copied conversation")
	}
	if info, err := os.Stat(filepath.Join(dest, "auth.json")); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("credential missing or public")
	}
	runtime := filepath.Join(t.TempDir(), "runtime")
	if err := CopyConfig(dest, runtime, harness.Codex{}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(runtime, "auth.json")); !os.IsNotExist(err) {
		t.Fatal("identity copied as configuration")
	}
}
func TestConfigRejectsCyclesAndPreservesExistingDestination(t *testing.T) {
	src := t.TempDir()
	os.Mkdir(filepath.Join(src, "skills"), 0700)
	if err := os.Symlink(src, filepath.Join(src, "skills", "loop")); err != nil {
		t.Skip(err)
	}
	if err := CopyConfig(src, t.TempDir(), harness.Codex{}, false); err == nil {
		t.Fatal("accepted cyclic skill tree")
	}
	existing := t.TempDir()
	os.WriteFile(filepath.Join(existing, "sentinel"), []byte("keep"), 0600)
	if err := ImportConfig(src, existing, harness.Codex{}); err == nil {
		t.Fatal("accepted existing destination")
	}
	b, _ := os.ReadFile(filepath.Join(existing, "sentinel"))
	if string(b) != "keep" {
		t.Fatal("changed destination")
	}
}
