package profile

import (
	"bytes"
	"os"
	"path/filepath"
	"sherpa/internal/gitutil"
	"sherpa/internal/harness"
	"strings"
	"testing"
)

func TestConfigMaterializesLinkedSkillsAndExcludesRuntime(t *testing.T) {
	src := t.TempDir()
	dest := filepath.Join(t.TempDir(), "copy")
	linked := filepath.Join(src, "local-skills")
	os.Mkdir(linked, 0700)
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

func TestConfigRejectsExternalLinksBeforeCopying(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(map[bool]string{false: "top-level", true: "nested"}[nested], func(t *testing.T) {
			src, external, dest := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "copy")
			os.WriteFile(filepath.Join(src, "AGENTS.md"), []byte("reviewed"), 0600)
			os.WriteFile(filepath.Join(external, "auth.json"), []byte("secret must not be retained"), 0600)
			link := filepath.Join(src, "skills")
			if nested {
				os.Mkdir(link, 0700)
				link = filepath.Join(link, "outside")
			}
			if err := os.Symlink(external, link); err != nil {
				t.Skip(err)
			}
			entries, err := InspectConfig(src, harness.Codex{}, false)
			if err == nil || !strings.Contains(err.Error(), "outside source") || !strings.Contains(err.Error(), external) {
				t.Fatalf("inspection: %v %v", entries, err)
			}
			if err := CopyConfig(src, dest, harness.Codex{}, false); err == nil {
				t.Fatal("accepted external link")
			}
			if _, err := os.Stat(dest); !os.IsNotExist(err) {
				t.Fatal("copied files before rejecting external link")
			}
		})
	}
}

func TestInitAndVersioningIgnoreInheritedGitRepository(t *testing.T) {
	original, target := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(original, "AGENTS.md"), []byte("original"), 0600)
	if err := InitRepository(original, harness.Codex{}.GitignoreContent()); err != nil {
		t.Fatal(err)
	}
	headBefore, err := gitutil.Run(original, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	index := filepath.Join(original, ".git", "index")
	indexBefore, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_DIR", filepath.Join(original, ".git"))
	t.Setenv("GIT_WORK_TREE", original)
	t.Setenv("GIT_INDEX_FILE", index)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.bare")
	t.Setenv("GIT_CONFIG_VALUE_0", "true")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "missing-config"))
	os.WriteFile(filepath.Join(target, "AGENTS.md"), []byte("target"), 0600)
	if err := InitRepository(target, harness.Codex{}.GitignoreContent()); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(target, "AGENTS.md"), []byte("variant change"), 0600)
	if _, err := gitutil.Run(target, "add", "-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitutil.Run(target, "commit", "-m", "variant"); err != nil {
		t.Fatal(err)
	}
	got, err := gitutil.Run(target, "show", "HEAD:AGENTS.md")
	if err != nil || got != "variant change" {
		t.Fatalf("target was not versioned: %q %v", got, err)
	}
	headAfter, err := gitutil.Run(original, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	indexAfter, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	if headBefore != headAfter || !bytes.Equal(indexBefore, indexAfter) {
		t.Fatal("inherited repository was modified")
	}
}
