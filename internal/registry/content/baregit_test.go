package content

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sherpa/internal/gitutil"
)

func TestTagCommit(t *testing.T) {
	root := t.TempDir()
	cs := NewBareGit(root)
	src := buildStackRepo(t, root, "v1", map[string]string{"stack.yaml": "name: n\nversion: 1\n"})
	bundle := createBundle(t, src, root, "tag-commit.bundle")
	stage, _, cleanup, err := cs.StageBundle(bundle, "v1")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if err := cs.Commit("alice", "n", stage); err != nil {
		t.Fatal(err)
	}
	got, err := cs.TagCommit("alice", "n", "v1")
	if err != nil || len(got) != 40 {
		t.Fatalf("TagCommit = %q, %v", got, err)
	}
	if _, err := cs.TagCommit("alice", "n", "v2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing tag error = %v", err)
	}
}

func TestBareGitStageCommitAndClone(t *testing.T) {
	root := t.TempDir()
	store := NewBareGit(filepath.Join(root, "content"))

	firstRepo := buildStackRepo(t, root, "v1", map[string]string{
		"stack.yaml": "name: reviewer\nversion: 1\n",
		"CLAUDE.md":  "Review code carefully.\n",
	})
	firstBundle := createBundle(t, firstRepo, root, "first.bundle")

	stageDir, worktreeDir, cleanup, err := store.StageBundle(firstBundle, "v1")
	if err != nil {
		t.Fatalf("stage first bundle: %v", err)
	}
	t.Cleanup(cleanup)

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

	_, secondWorktree, secondCleanup, err := store.StageBundle(secondBundle, "v2")
	if err != nil {
		t.Fatalf("stage second bundle: %v", err)
	}

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

	secondCleanup()
	assertNoStageWrappers(t, filepath.Join(root, "content"))
}

func TestBareGitCommitSecondVersionPreservesTagsAndUpdatesLatest(t *testing.T) {
	root := t.TempDir()
	store := NewBareGit(filepath.Join(root, "content"))

	src := buildStackRepo(t, root, "v1", map[string]string{
		"stack.yaml": "name: reviewer\nversion: 1\n",
	})
	firstBundle := createBundle(t, src, root, "first.bundle")
	firstStage, _, firstCleanup, err := store.StageBundle(firstBundle, "v1")
	if err != nil {
		t.Fatalf("stage first bundle: %v", err)
	}
	t.Cleanup(firstCleanup)
	if err := store.Commit("alice", "reviewer", firstStage); err != nil {
		t.Fatalf("commit first bundle: %v", err)
	}

	commitStackRepo(t, src, "v2", map[string]string{
		"stack.yaml": "name: reviewer\nversion: 2\n",
		"NEW.md":     "second version\n",
	})
	secondBundle := createBundle(t, src, root, "second.bundle")
	secondStage, _, secondCleanup, err := store.StageBundle(secondBundle, "v2")
	if err != nil {
		t.Fatalf("stage second bundle: %v", err)
	}
	t.Cleanup(secondCleanup)
	if err := store.Commit("alice", "reviewer", secondStage); err != nil {
		t.Fatalf("commit second bundle: %v", err)
	}

	repoPath := store.RepoPath("alice", "reviewer")
	assertGitOutput(t, repoPath, "v1", "tag", "-l", "v1")
	assertGitOutput(t, repoPath, "v2", "tag", "-l", "v2")

	cloneDir := filepath.Join(root, "clone-v2")
	if err := gitutil.Clone("file://"+repoPath, cloneDir); err != nil {
		t.Fatalf("clone committed repo: %v", err)
	}
	assertFile(t, cloneDir, "stack.yaml", "name: reviewer\nversion: 2\n")
	assertFile(t, cloneDir, "NEW.md", "second version\n")
}

func TestBareGitCommitDivergentSecondVersionForceUpdatesLatest(t *testing.T) {
	root := t.TempDir()
	store := NewBareGit(filepath.Join(root, "content"))

	firstRepo := buildStackRepo(t, root, "v1", map[string]string{
		"stack.yaml": "name: reviewer\nversion: 1\n",
	})
	firstBundle := createBundle(t, firstRepo, root, "first.bundle")
	firstStage, _, firstCleanup, err := store.StageBundle(firstBundle, "v1")
	if err != nil {
		t.Fatalf("stage first bundle: %v", err)
	}
	t.Cleanup(firstCleanup)
	if err := store.Commit("alice", "reviewer", firstStage); err != nil {
		t.Fatalf("commit first bundle: %v", err)
	}

	secondRepo := buildStackRepo(t, root, "v2", map[string]string{
		"stack.yaml": "name: reviewer\nversion: 2 divergent\n",
		"NEW.md":     "independent history\n",
	})
	secondBundle := createBundle(t, secondRepo, root, "second.bundle")
	secondStage, _, secondCleanup, err := store.StageBundle(secondBundle, "v2")
	if err != nil {
		t.Fatalf("stage divergent second bundle: %v", err)
	}
	t.Cleanup(secondCleanup)
	if err := store.Commit("alice", "reviewer", secondStage); err != nil {
		t.Fatalf("commit divergent second bundle: %v", err)
	}

	repoPath := store.RepoPath("alice", "reviewer")
	assertGitOutput(t, repoPath, "v1", "tag", "-l", "v1")
	assertGitOutput(t, repoPath, "v2", "tag", "-l", "v2")

	cloneDir := filepath.Join(root, "clone-divergent")
	if err := gitutil.Clone("file://"+repoPath, cloneDir); err != nil {
		t.Fatalf("clone committed repo: %v", err)
	}
	assertFile(t, cloneDir, "stack.yaml", "name: reviewer\nversion: 2 divergent\n")
	assertFile(t, cloneDir, "NEW.md", "independent history\n")
}

func TestBareGitRejectsInvalidOwnerNameSegments(t *testing.T) {
	root := t.TempDir()
	contentRoot := filepath.Join(root, "content")
	store := NewBareGit(contentRoot)

	src := buildStackRepo(t, root, "v1", map[string]string{
		"stack.yaml": "name: reviewer\nversion: 1\n",
	})
	bundle := createBundle(t, src, root, "first.bundle")
	stageDir, _, cleanup, err := store.StageBundle(bundle, "v1")
	if err != nil {
		t.Fatalf("stage bundle: %v", err)
	}
	t.Cleanup(cleanup)

	if err := store.Commit("../evil", "reviewer", stageDir); err == nil {
		t.Fatal("Commit accepted invalid owner segment")
	}
	if _, err := os.Stat(filepath.Join(root, "evil")); !os.IsNotExist(err) {
		t.Fatalf("invalid owner created path outside content root, stat err = %v", err)
	}
	if err := store.EnsureRepo("alice", "../evil"); err == nil {
		t.Fatal("EnsureRepo accepted invalid name segment")
	}
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

func commitStackRepo(t *testing.T, dir, tag string, files map[string]string) {
	t.Helper()

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

func assertNoStageWrappers(t *testing.T, root string) {
	t.Helper()

	matches, err := filepath.Glob(filepath.Join(root, ".stage-*"))
	if err != nil {
		t.Fatalf("glob stage wrappers: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("stage wrappers still exist: %v", matches)
	}
}
