package profile

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sherpa/internal/harness"
	"sort"
	"strings"
)

var namePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

func ValidName(name string) bool { return namePattern.MatchString(name) }

// CopyConfig copies only configuration, never conversations, caches or Git
// metadata. Linked skills are materialized as independent regular files.
// Cycles and oversized trees fail rather than producing partial setups.
func CopyConfig(src, dst string, h harness.Harness, identity bool) error {
	src, err := filepath.Abs(src)
	if err != nil {
		return err
	}
	src, err = filepath.EvalSymlinks(src)
	if err != nil {
		return err
	}
	dst, err = filepath.Abs(dst)
	if err != nil {
		return err
	}
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("setup source must be a directory")
	}
	canonical, err := destinationPath(dst)
	if err != nil {
		return err
	}
	if canonical == src || strings.HasPrefix(canonical, src+string(os.PathSeparator)) {
		return fmt.Errorf("destination must be outside the source setup")
	}
	paths := append([]string{}, h.AllowedPaths()...)
	if identity {
		paths = append(paths, h.CredentialFiles()...)
		paths = append(paths, h.CapturedName())
	}
	if err := os.MkdirAll(dst, 0700); err != nil {
		return err
	}
	copier := configCopier{ancestors: map[string]bool{}}
	for _, rel := range paths {
		rel = strings.TrimSuffix(rel, "/")
		p := filepath.Join(src, rel)
		if _, err := os.Lstat(p); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		if err := copier.copy(p, filepath.Join(dst, rel)); err != nil {
			return err
		}
	}

	return nil
}

// Resolve existing ancestors without creating anything under the source.
func destinationPath(path string) (string, error) {
	real, err := filepath.EvalSymlinks(path)
	if err == nil {
		return real, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return "", err
	}
	real, err = destinationPath(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(real, filepath.Base(path)), nil
}

type configCopier struct {
	ancestors map[string]bool
	files     int
	bytes     int64
}

func (c *configCopier) copy(src, dst string) error {
	real, err := filepath.EvalSymlinks(src)
	if err != nil {
		return err
	}
	info, err := os.Stat(real)
	if err != nil {
		return err
	}
	if info.IsDir() {
		c.files++
		if c.files > 20000 {
			return fmt.Errorf("setup exceeds 20,000 entries")
		}
		if c.ancestors[real] {
			return fmt.Errorf("cyclic setup link: %s", src)
		}
		c.ancestors[real] = true
		defer delete(c.ancestors, real)
		if err := os.MkdirAll(dst, 0700); err != nil {
			return err
		}
		entries, err := os.ReadDir(real)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if e.Name() == ".git" {
				continue
			}
			if err := c.copy(filepath.Join(real, e.Name()), filepath.Join(dst, e.Name())); err != nil {
				return err
			}
		}
		return nil
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("unsupported setup file: %s", src)
	}
	c.files++
	c.bytes += info.Size()
	if c.files > 20000 || c.bytes > 128<<20 {
		return fmt.Errorf("setup exceeds 20,000 files or 128 MiB")
	}
	return copyFile(real, dst, 0600|info.Mode().Perm()&0100)
}
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	closeErr := out.Close()
	if err != nil {
		return err
	}
	return closeErr
}
func InitRepository(dir, gitignore string) error {
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(gitignore), 0600); err != nil {
		return err
	}
	for _, args := range [][]string{{"init", "-b", "local"}, {"add", "-A"}, {"-c", "user.email=sherpa@local", "-c", "user.name=sherpa", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=", "commit", "--allow-empty", "-m", "sherpa: local setup"}} {
		if err := git(dir, args...); err != nil {
			return err
		}
	}
	return nil
}
func ImportConfig(src, dst string, h harness.Harness) error {
	if _, err := os.Lstat(dst); !os.IsNotExist(err) {
		return fmt.Errorf("destination already exists or is inaccessible: %s", dst)
	}
	if err := CopyConfig(src, dst, h, true); err != nil {
		return err
	}
	return InitRepository(dst, h.GitignoreContent())
}

// Digest describes configuration bytes, excluding credentials and runtime state.
func Digest(dir string, h harness.Harness) (string, error) {
	temp, err := os.MkdirTemp("", "sherpa-digest-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(temp)
	if err := CopyConfig(dir, temp, h, false); err != nil {
		return "", err
	}
	var names []string
	err = filepath.WalkDir(temp, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			r, _ := filepath.Rel(temp, p)
			names = append(names, r)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(names)
	hash := sha256.New()
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(temp, n))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(hash, "%d:%s:%d:", len(n), n, len(b))
		hash.Write(b)
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}
