package content

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"sherpa/internal/gitutil"
)

type BareGit struct {
	root string
}

func NewBareGit(root string) *BareGit {
	return &BareGit{root: root}
}

func (b *BareGit) EnsureRepo(owner, name string) error {
	path := b.RepoPath(owner, name)
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat repo: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create repo parent: %w", err)
	}
	if _, err := gitutil.Run(filepath.Dir(path), "init", "--bare", path); err != nil {
		return fmt.Errorf("init bare repo: %w", err)
	}
	return nil
}

func (b *BareGit) StageBundle(bundle []byte, gitTag string) (stageDir, worktreeDir string, err error) {
	if err := os.MkdirAll(b.root, 0o755); err != nil {
		return "", "", fmt.Errorf("create content root: %w", err)
	}

	tmpDir, err := os.MkdirTemp(b.root, ".stage-*")
	if err != nil {
		return "", "", fmt.Errorf("create stage temp dir: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(tmpDir)
		}
	}()

	stageDir = filepath.Join(tmpDir, "repo.git")
	if _, err := gitutil.Run(tmpDir, "init", "--bare", stageDir); err != nil {
		return "", "", fmt.Errorf("init stage repo: %w", err)
	}

	bundlePath := filepath.Join(tmpDir, "bundle")
	if err := os.WriteFile(bundlePath, bundle, 0o600); err != nil {
		return "", "", fmt.Errorf("write bundle: %w", err)
	}
	if _, err := gitutil.Run(stageDir, "fetch", bundlePath, "refs/heads/*:refs/heads/*", "refs/tags/*:refs/tags/*"); err != nil {
		return "", "", fmt.Errorf("fetch bundle: %w", err)
	}
	if err := setDefaultHead(stageDir); err != nil {
		return "", "", err
	}

	worktreeDir = filepath.Join(tmpDir, "worktree")
	if _, err := gitutil.Run(stageDir, "worktree", "add", worktreeDir, gitTag); err != nil {
		return "", "", fmt.Errorf("add worktree: %w", err)
	}

	return stageDir, worktreeDir, nil
}

func (b *BareGit) Commit(owner, name, stageDir string) error {
	if err := removeWorktrees(stageDir); err != nil {
		return err
	}

	dest := b.RepoPath(owner, name)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("create repo parent: %w", err)
	}

	if _, err := os.Stat(dest); err == nil {
		if _, err := gitutil.Run(dest, "fetch", stageDir, "refs/*:refs/*"); err != nil {
			return fmt.Errorf("fetch staged refs: %w", err)
		}
		if err := setDefaultHead(dest); err != nil {
			return err
		}
		if _, err := gitutil.Run(dest, "update-server-info"); err != nil {
			return fmt.Errorf("update server info: %w", err)
		}
		if err := os.RemoveAll(filepath.Dir(stageDir)); err != nil {
			return fmt.Errorf("remove stage: %w", err)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat destination repo: %w", err)
	}

	if err := os.Rename(stageDir, dest); err != nil {
		return fmt.Errorf("promote staged repo: %w", err)
	}
	if err := os.RemoveAll(filepath.Dir(stageDir)); err != nil {
		return fmt.Errorf("remove stage wrapper: %w", err)
	}
	if _, err := gitutil.Run(dest, "update-server-info"); err != nil {
		return fmt.Errorf("update server info: %w", err)
	}
	return nil
}

func (b *BareGit) RepoPath(owner, name string) string {
	return filepath.Join(b.root, "profiles", owner, name+".git")
}

func setDefaultHead(repo string) error {
	if _, err := gitutil.Run(repo, "rev-parse", "--verify", "--quiet", "refs/heads/main"); err != nil {
		return nil
	}
	if _, err := gitutil.Run(repo, "symbolic-ref", "HEAD", "refs/heads/main"); err != nil {
		return fmt.Errorf("set default HEAD: %w", err)
	}
	return nil
}

func removeWorktrees(stageDir string) error {
	out, err := gitutil.Run(stageDir, "worktree", "list", "--porcelain")
	if err != nil {
		return fmt.Errorf("list worktrees: %w", err)
	}

	for _, path := range worktreePaths(out) {
		if _, err := gitutil.Run(stageDir, "worktree", "remove", "--force", path); err != nil {
			return fmt.Errorf("remove worktree %s: %w", path, err)
		}
	}
	if _, err := gitutil.Run(stageDir, "worktree", "prune"); err != nil {
		return fmt.Errorf("prune worktrees: %w", err)
	}
	return nil
}

func worktreePaths(porcelain string) []string {
	var paths []string
	seenMain := false
	for _, line := range strings.Split(porcelain, "\n") {
		path, ok := strings.CutPrefix(line, "worktree ")
		if ok {
			if !seenMain {
				seenMain = true
				continue
			}
			paths = append(paths, path)
		}
	}
	return paths
}
