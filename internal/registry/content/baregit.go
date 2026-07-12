package content

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	bundleBranch, err := bundleHeadBranch(stageDir, bundlePath)
	if err != nil {
		return "", "", nil, err
	}
	if bundleBranch != "" {
		err = promoteBundleHeadToMain(stageDir, bundleBranch)
	} else {
		err = setDefaultHead(stageDir)
	}
	if err != nil {
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
		if err := setHeadBranch(dest, branch); err != nil {
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

func (b *BareGit) ListRepositories() ([]RepositoryRef, error) {
	profilesDir := filepath.Join(b.root, "profiles")
	owners, err := os.ReadDir(profilesDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list profile owners: %w", err)
	}

	var repos []RepositoryRef
	for _, ownerEntry := range owners {
		owner := ownerEntry.Name()
		if !ownerEntry.IsDir() || validateSegment(owner) != nil {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(profilesDir, owner))
		if err != nil {
			return nil, fmt.Errorf("list repositories for %s: %w", owner, err)
		}
		for _, entry := range entries {
			if !entry.IsDir() || !strings.HasSuffix(entry.Name(), ".git") {
				continue
			}
			name := strings.TrimSuffix(entry.Name(), ".git")
			if validateOwnerName(owner, name) != nil {
				continue
			}
			repos = append(repos, RepositoryRef{Owner: owner, Name: name})
		}
	}
	slices.SortFunc(repos, func(a, b RepositoryRef) int {
		if c := strings.Compare(a.Owner, b.Owner); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	return repos, nil
}

func (b *BareGit) ListTags(owner, name string) ([]string, error) {
	if err := validateOwnerName(owner, name); err != nil {
		return nil, err
	}
	repoPath := b.RepoPath(owner, name)
	if _, err := os.Stat(repoPath); os.IsNotExist(err) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, fmt.Errorf("stat repository: %w", err)
	}
	out, err := gitutil.Run(repoPath, "tag", "--list")
	if err != nil {
		return nil, fmt.Errorf("list tags: %w", err)
	}
	var tags []string
	for _, tag := range strings.Split(out, "\n") {
		if tag != "" {
			tags = append(tags, tag)
		}
	}
	slices.Sort(tags)
	return tags, nil
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
	return setHeadBranch(repo, branch)
}

func setHeadBranch(repo, branch string) error {
	if _, err := gitutil.Run(repo, "symbolic-ref", "HEAD", "refs/heads/"+branch); err != nil {
		return fmt.Errorf("set default HEAD: %w", err)
	}
	return nil
}

func defaultBranch(repo string) (string, error) {
	if branch, err := gitutil.Run(repo, "symbolic-ref", "--short", "HEAD"); err == nil && branch != "" {
		if _, err := gitutil.Run(repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
			return branch, nil
		}
	}
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

func bundleHeadBranch(repo, bundlePath string) (string, error) {
	out, err := gitutil.Run(filepath.Dir(bundlePath), "bundle", "list-heads", bundlePath, "HEAD")
	if err != nil {
		return "", fmt.Errorf("inspect bundle HEAD: %w", err)
	}
	fields := strings.Fields(out)
	if len(fields) < 2 || fields[1] != "HEAD" {
		return "", nil
	}
	headCommit := fields[0]
	branches, err := gitutil.Run(repo, "for-each-ref", "--format=%(refname:short) %(objectname)", "refs/heads")
	if err != nil {
		return "", fmt.Errorf("list staged branches: %w", err)
	}
	var matches []string
	for _, line := range strings.Split(branches, "\n") {
		parts := strings.Fields(line)
		if len(parts) == 2 && parts[1] == headCommit {
			matches = append(matches, parts[0])
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	return "", nil
}

func promoteBundleHeadToMain(repo, bundleBranch string) error {
	if bundleBranch != "main" {
		if _, err := gitutil.Run(repo, "rev-parse", "--verify", "--quiet", "refs/heads/main"); err == nil {
			if _, err := gitutil.Run(repo, "update-ref", "refs/sherpa/bundle-heads/main", "refs/heads/main"); err != nil {
				return fmt.Errorf("preserve bundled main: %w", err)
			}
		}
		if _, err := gitutil.Run(repo, "update-ref", "refs/heads/main", "refs/heads/"+bundleBranch); err != nil {
			return fmt.Errorf("promote bundle HEAD to main: %w", err)
		}
	}
	return setHeadBranch(repo, "main")
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
