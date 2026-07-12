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
	if err := validateOwnerName(owner, name); err != nil {
		return err
	}

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

func (b *BareGit) StageBundle(bundle []byte, gitTag string) (stageDir, worktreeDir string, cleanup func(), err error) {
	if err := os.MkdirAll(b.root, 0o755); err != nil {
		return "", "", nil, fmt.Errorf("create content root: %w", err)
	}

	tmpDir, err := os.MkdirTemp(b.root, ".stage-*")
	if err != nil {
		return "", "", nil, fmt.Errorf("create stage temp dir: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(tmpDir) }
	defer func() {
		if err != nil {
			cleanup()
		}
	}()

	stageDir = filepath.Join(tmpDir, "repo.git")
	if _, err := gitutil.Run(tmpDir, "init", "--bare", stageDir); err != nil {
		return "", "", nil, fmt.Errorf("init stage repo: %w", err)
	}

	bundlePath := filepath.Join(tmpDir, "bundle")
	if err := os.WriteFile(bundlePath, bundle, 0o600); err != nil {
		return "", "", nil, fmt.Errorf("write bundle: %w", err)
	}
	if _, err := gitutil.Run(stageDir, "fetch", bundlePath, "refs/heads/*:refs/heads/*", "refs/tags/*:refs/tags/*"); err != nil {
		return "", "", nil, fmt.Errorf("fetch bundle: %w", err)
	}
	if err := setDefaultHead(stageDir); err != nil {
		return "", "", nil, err
	}

	worktreeDir = filepath.Join(tmpDir, "worktree")
	checkoutRef := gitTag
	if checkoutRef == "" {
		checkoutRef = "HEAD"
	}
	if _, err := gitutil.Run(stageDir, "worktree", "add", worktreeDir, checkoutRef); err != nil {
		return "", "", nil, fmt.Errorf("add worktree: %w", err)
	}

	return stageDir, worktreeDir, cleanup, nil
}

func (b *BareGit) Commit(owner, name, stageDir string) error {
	if err := validateOwnerName(owner, name); err != nil {
		return err
	}
	if err := removeWorktrees(stageDir); err != nil {
		return err
	}

	dest := b.RepoPath(owner, name)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("create repo parent: %w", err)
	}

	if _, err := os.Stat(dest); err == nil {
		branch, err := defaultBranch(stageDir)
		if err != nil {
			return err
		}
		if _, err := gitutil.Run(dest, "fetch", stageDir, "refs/tags/*:refs/tags/*"); err != nil {
			return fmt.Errorf("fetch staged tags: %w", err)
		}
		refspec := fmt.Sprintf("+refs/heads/%s:refs/heads/%s", branch, branch)
		if _, err := gitutil.Run(dest, "fetch", stageDir, refspec); err != nil {
			return fmt.Errorf("fetch staged default branch: %w", err)
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

func (b *BareGit) TagCommit(owner, name, tag string) (string, error) {
	if err := validateOwnerName(owner, name); err != nil {
		return "", err
	}
	out, err := gitutil.Run(b.RepoPath(owner, name), "rev-parse", "--verify", tag+"^{commit}")
	if err != nil {
		return "", ErrNotFound
	}
	return strings.TrimSpace(out), nil
}

func (b *BareGit) RepoPath(owner, name string) string {
	if validateOwnerName(owner, name) != nil {
		return filepath.Join(b.root, "profiles", "_invalid", "_invalid.git")
	}
	return filepath.Join(b.root, "profiles", owner, name+".git")
}

func setDefaultHead(repo string) error {
	branch, err := defaultBranch(repo)
	if err != nil {
		return err
	}
	if _, err := gitutil.Run(repo, "symbolic-ref", "HEAD", "refs/heads/"+branch); err != nil {
		return fmt.Errorf("set default HEAD: %w", err)
	}
	return nil
}

func defaultBranch(repo string) (string, error) {
	if _, err := gitutil.Run(repo, "rev-parse", "--verify", "--quiet", "refs/heads/main"); err == nil {
		return "main", nil
	}

	out, err := gitutil.Run(repo, "for-each-ref", "--format=%(refname:short)", "--sort=refname", "refs/heads")
	if err != nil {
		return "", fmt.Errorf("list branches: %w", err)
	}
	for _, line := range strings.Split(out, "\n") {
		if line != "" {
			return line, nil
		}
	}
	return "", fmt.Errorf("staged repo has no branches")
}

func validateOwnerName(owner, name string) error {
	if err := validateSegment(owner); err != nil {
		return fmt.Errorf("invalid owner: %w", err)
	}
	if err := validateSegment(name); err != nil {
		return fmt.Errorf("invalid name: %w", err)
	}
	return nil
}

func validateSegment(s string) error {
	if s == "" {
		return fmt.Errorf("empty segment")
	}
	if strings.ContainsAny(s, `/\`) {
		return fmt.Errorf("segment contains path separator")
	}
	if s == ".." || strings.HasPrefix(s, ".") {
		return fmt.Errorf("segment must not be hidden or parent traversal")
	}
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '-':
		case r == '.':
		default:
			return fmt.Errorf("segment contains invalid character %q", r)
		}
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
