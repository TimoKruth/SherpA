package profile

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

func git(dir string, args ...string) error {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %v: %s: %w", args, out, err)
	}
	return nil
}

// Import copies src into a fresh dest, writes gitignore, inits git on branch "local".
// It never writes into src (spec §6.1: mine is sacred).
func Import(src, dest, gitignore string) error {
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("destination %s already exists", dest)
	}
	if err := copyTree(src, dest); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dest, ".gitignore"), []byte(gitignore), 0o644); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"init", "-b", "local"}, {"add", "-A"},
		{"-c", "user.email=sherpa@local", "-c", "user.name=sherpa", "-c", "commit.gpgsign=false", "commit", "-m", "sherpa: import"},
	} {
		if err := git(dest, args...); err != nil {
			return err
		}
	}
	return nil
}

func copyTree(src, dest string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dest, rel)
		switch {
		case info.IsDir():
			return os.MkdirAll(target, 0o755)
		case !info.Mode().IsRegular():
			return nil // skip symlinks/sockets
		default:
			in, err := os.Open(p)
			if err != nil {
				return err
			}
			defer in.Close()
			out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
			if err != nil {
				return err
			}
			defer out.Close()
			_, err = io.Copy(out, in)
			return err
		}
	})
}
