package content

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sherpa/internal/gitutil"
)

func TestBareGitStageCommitAndClone(t *testing.T) {
	root := t.TempDir()
	store := NewBareGit(filepath.Join(root, "content"))

	firstRepo := buildStackRepo(t, root, "v1", map[string]string{
		"stack.yaml": "name: reviewer\nversion: 1\n",
		"CLAUDE.md":  "Review code carefully.\n",
	})
	firstBundle := createBundle(t, firstRepo, root, "first.bundle")

	stageDir, worktreeDir, err := store.StageBundle(firstBundle, "v1")
	if err != nil {
		t.Fatalf("stage first bundle: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stageDir) })
	t.Cleanup(func() { _ = os.RemoveAll(worktreeDir) })

	assertFile(t, worktreeDir, "stack.yaml", "name: reviewer\nversion: 1\n")
	assertFile(t, worktreeDir, "CLAUDE.md", "Review code carefully.\n")

	if err := store.Commit("alice", "reviewer", stageDir); err != nil {
		t.Fatalf("commit first bundle: %v", err)
	}
	if _, err := os.Stat(store.RepoPath("alice", "reviewer")); err != nil {
		t.Fatalf("repo path stat: %v", err)
	}

	cloneDir := filepath.Join(root, "clone")
	if err := gitutil.Clone("file://"+store.RepoPath("alice", "reviewer"), cloneDir); err != nil {
		t.Fatalf("clone committed repo: %v", err)
	}
	assertFile(t, cloneDir, "stack.yaml", "name: reviewer\nversion: 1\n")
	assertFile(t, cloneDir, "CLAUDE.md", "Review code carefully.\n")
	assertGitOutput(t, cloneDir, "v1", "tag", "-l", "v1")

	secondRepo := buildStackRepo(t, root, "v2", map[string]string{
		"stack.yaml": "name: reviewer\nversion: 2\n",
		"CLAUDE.md":  "Review code with stricter rules.\n",
		"NEW.md":     "This file must not appear without commit.\n",
	})
	secondBundle := createBundle(t, secondRepo, root, "second.bundle")

	secondStage, secondWorktree, err := store.StageBundle(secondBundle, "v2")
	if err != nil {
		t.Fatalf("stage second bundle: %v", err)
	}
	defer os.RemoveAll(secondStage)
	defer os.RemoveAll(secondWorktree)

	assertFile(t, secondWorktree, "NEW.md", "This file must not appear without commit.\n")

	cloneAfterDiscard := filepath.Join(root, "clone-after-discard")
	if err := gitutil.Clone("file://"+store.RepoPath("alice", "reviewer"), cloneAfterDiscard); err != nil {
		t.Fatalf("clone after discarded stage: %v", err)
	}
	assertFile(t, cloneAfterDiscard, "stack.yaml", "name: reviewer\nversion: 1\n")
	if _, err := os.Stat(filepath.Join(cloneAfterDiscard, "NEW.md")); !os.IsNotExist(err) {
		t.Fatalf("NEW.md exists after uncommitted stage, stat err = %v", err)
	}
	assertGitOutput(t, cloneAfterDiscard, "", "tag", "-l", "v2")
}

func buildStackRepo(t *testing.T, root, tag string, files map[string]string) string {
	t.Helper()

	dir := filepath.Join(root, "src-"+tag)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir source repo: %v", err)
	}
	if _, err := gitutil.Run(dir, "init", "-b", "main"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	if _, err := gitutil.Run(dir, "config", "user.email", "test@example.com"); err != nil {
		t.Fatalf("git config email: %v", err)
	}
	if _, err := gitutil.Run(dir, "config", "user.name", "Test User"); err != nil {
		t.Fatalf("git config name: %v", err)
	}
	for name, body := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir file parent: %v", err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if _, err := gitutil.Run(dir, "add", "-A"); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if _, err := gitutil.Run(dir, "commit", "-m", "stack "+tag); err != nil {
		t.Fatalf("git commit: %v", err)
	}
	if _, err := gitutil.Run(dir, "tag", tag); err != nil {
		t.Fatalf("git tag: %v", err)
	}
	return dir
}

func createBundle(t *testing.T, repo, root, name string) []byte {
	t.Helper()

	path := filepath.Join(root, name)
	if _, err := gitutil.Run(repo, "bundle", "create", path, "--all"); err != nil {
		t.Fatalf("git bundle create: %v", err)
	}
	bundle, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read bundle: %v", err)
	}
	return bundle
}

func assertFile(t *testing.T, root, name, want string) {
	t.Helper()

	got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if string(got) != want {
		t.Fatalf("%s = %q, want %q", name, string(got), want)
	}
}

func assertGitOutput(t *testing.T, dir, want string, args ...string) {
	t.Helper()

	got, err := gitutil.Run(dir, args...)
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	if got != want {
		t.Fatalf("git %s = %q, want %q", strings.Join(args, " "), got, want)
	}
}
