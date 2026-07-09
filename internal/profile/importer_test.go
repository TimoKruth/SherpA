package profile

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestImportCopiesAndInitsGit(t *testing.T) {
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "CLAUDE.md"), []byte("# rules"), 0o644)
	os.MkdirAll(filepath.Join(src, "skills", "x"), 0o755)
	os.WriteFile(filepath.Join(src, "skills", "x", "SKILL.md"), []byte("s"), 0o644)
	os.WriteFile(filepath.Join(src, "history.jsonl"), []byte("private"), 0o644)

	dest := filepath.Join(t.TempDir(), "mine")
	gi := "*\n!/.gitignore\n!/CLAUDE.md\n!/skills/\n!/skills/**\n"
	if err := Import(src, dest, gi); err != nil {
		t.Fatal(err)
	}
	// source untouched (sacred): still exactly 3 files
	var srcFiles int
	if err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			srcFiles++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if srcFiles != 3 {
		t.Fatalf("source file count = %d, want 3", srcFiles)
	}
	if _, err := os.Stat(filepath.Join(src, ".gitignore")); !os.IsNotExist(err) {
		t.Fatal("source .gitignore was written")
	}
	if _, err := os.Stat(filepath.Join(src, ".git")); !os.IsNotExist(err) {
		t.Fatal("source git repo was initialized")
	}
	// copied content present
	if _, err := os.Stat(filepath.Join(dest, "skills", "x", "SKILL.md")); err != nil {
		t.Fatal("skill not copied")
	}
	// git repo on branch local, tracked files exclude history.jsonl
	out, err := exec.Command("git", "-C", dest, "ls-files").Output()
	if err != nil {
		t.Fatal(err)
	}
	tracked := string(out)
	if strings.Contains(tracked, "history.jsonl") {
		t.Fatal("runtime noise got tracked")
	}
	if !strings.Contains(tracked, "CLAUDE.md") {
		t.Fatal("CLAUDE.md not tracked")
	}
	br, _ := exec.Command("git", "-C", dest, "branch", "--show-current").Output()
	if strings.TrimSpace(string(br)) != "local" {
		t.Fatalf("branch = %q", br)
	}
}

func TestImportRefusesExistingDest(t *testing.T) {
	dest := t.TempDir() // exists
	if err := Import(t.TempDir(), dest, "*\n"); err == nil {
		t.Fatal("want error on existing dest")
	}
}
