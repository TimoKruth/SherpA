package compare

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sherpa/internal/gitutil"
	"sort"
	"strings"
	"time"
)

const maxProjectBytes int64 = 256 << 20

// snapshot captures tracked working-tree files plus nonignored untracked files.
// No shared Git objects, remotes, hooks, symlinks or submodules enter a trial.
func snapshot(source, dest string) (string, string, error) {
	root, err := git(source, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", "", fmt.Errorf("choose a Git project: %w", err)
	}
	real, err := filepath.EvalSymlinks(source)
	if err != nil {
		return "", "", err
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", "", err
	}
	if real != realRoot {
		return "", "", fmt.Errorf("project must be the repository root: %s", root)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := gitutil.Command(ctx, source, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	out, err := cmd.Output()
	if err != nil {
		return "", "", err
	}
	names := strings.Split(string(out), "\x00")
	sort.Strings(names)
	hash := sha256.New()
	var total int64
	seen := map[string]bool{}
	if err := os.MkdirAll(dest, 0700); err != nil {
		return "", "", err
	}
	for _, name := range names {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		if len(seen) > 20000 {
			return "", "", fmt.Errorf("project exceeds 20,000 files")
		}
		if !filepath.IsLocal(name) {
			return "", "", fmt.Errorf("nonlocal project path")
		}
		p := filepath.Join(source, name)
		info, err := os.Lstat(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return "", "", err
		}
		// Check every parent too: a tracked directory may have been replaced by a link.
		resolved, err := filepath.EvalSymlinks(p)
		if err != nil {
			return "", "", err
		}
		if resolved != filepath.Join(real, name) || !info.Mode().IsRegular() {
			return "", "", fmt.Errorf("%s is a symlink, submodule or special file; use a project of regular files", name)
		}
		total += info.Size()
		if total > maxProjectBytes {
			return "", "", fmt.Errorf("project exceeds the 256 MiB snapshot limit")
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return "", "", err
		}
		fmt.Fprintf(hash, "%d:%s:%o:%d:", len(name), name, info.Mode().Perm()&0111, len(b))
		hash.Write(b)
		target := filepath.Join(dest, name)
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return "", "", err
		}
		if err := os.WriteFile(target, b, 0600|info.Mode().Perm()&0100); err != nil {
			return "", "", err
		}
	}
	revision, _ := git(source, "rev-parse", "HEAD")
	return fmt.Sprintf("%x", hash.Sum(nil)), revision, nil
}
func copyProject(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("invalid snapshot file")
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600|info.Mode().Perm()&0100)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, in)
		closeErr := out.Close()
		if err != nil {
			return err
		}
		return closeErr
	})
}
func git(dir string, args ...string) (string, error) {
	return gitutil.Run(dir, args...)
}

func initProject(dir string) error {
	for _, args := range [][]string{{"init", "-b", "trial"}, {"add", "--all", "--force", "--", "."}, {"commit", "--allow-empty", "-m", "comparison starting point"}} {
		if _, err := git(dir, args...); err != nil {
			return err
		}
	}
	return nil
}
