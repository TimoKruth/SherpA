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
// External links, cycles and oversized trees fail before copying any files.
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
	entries, err := InspectConfig(src, h, identity)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0700); err != nil {
		return err
	}
	for _, entry := range entries {
		target := filepath.Join(dst, entry.Path)
		if entry.Info.IsDir() {
			if err := os.MkdirAll(target, 0700); err != nil {
				return err
			}
		} else if err := copyFile(entry.Resolved, target, 0600|entry.Info.Mode().Perm()&0100); err != nil {
			return err
		}
	}
	return nil
}

// ConfigEntry describes one materialized entry. LinkTarget is populated for symlinks.
type ConfigEntry struct {
	Path, Resolved, LinkTarget string
	Info                       os.FileInfo
}

// InspectConfig and CopyConfig use the same bounded traversal. Links must stay
// within the source directory; external trees must be copied in and reviewed first.
func InspectConfig(src string, h harness.Harness, identity bool) ([]ConfigEntry, error) {
	src, err := filepath.Abs(src)
	if err != nil {
		return nil, err
	}
	src, err = filepath.EvalSymlinks(src)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(src)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("setup source must be a directory")
	}
	paths := append([]string{}, h.AllowedPaths()...)
	if identity {
		paths = append(paths, h.CredentialFiles()...)
		paths = append(paths, h.CapturedName())
	}
	scanner := configScanner{root: src, ancestors: map[string]bool{}}
	for _, rel := range paths {
		rel = strings.TrimSuffix(rel, "/")
		if _, err := os.Lstat(filepath.Join(src, rel)); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return scanner.entries, err
		}
		if err := scanner.walk(filepath.Join(src, rel), rel); err != nil {
			return scanner.entries, err
		}
	}
	return scanner.entries, nil
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

type configScanner struct {
	root      string
	ancestors map[string]bool
	entries   []ConfigEntry
	bytes     int64
}

func (c *configScanner) walk(src, rel string) error {
	linkInfo, err := os.Lstat(src)
	if err != nil {
		return err
	}
	real, err := filepath.EvalSymlinks(src)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(c.root, real)
	if err != nil || (relative != "." && !filepath.IsLocal(relative)) {
		return fmt.Errorf("setup link %s resolves outside source to %s; copy the intended files into the source directory and review them before importing", rel, real)
	}
	info, err := os.Stat(real)
	if err != nil {
		return err
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return fmt.Errorf("unsupported setup file: %s", rel)
	}
	entry := ConfigEntry{Path: rel, Resolved: real, Info: info}
	if linkInfo.Mode()&os.ModeSymlink != 0 {
		entry.LinkTarget = real
	}
	c.entries = append(c.entries, entry)
	if info.Mode().IsRegular() {
		c.bytes += info.Size()
	}
	if len(c.entries) > 20000 || c.bytes > 128<<20 {
		return fmt.Errorf("setup exceeds 20,000 entries or 128 MiB")
	}
	if !info.IsDir() {
		return nil
	}
	if c.ancestors[real] {
		return fmt.Errorf("cyclic setup link: %s", rel)
	}
	c.ancestors[real] = true
	defer delete(c.ancestors, real)
	entries, err := os.ReadDir(real)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() == ".git" {
			continue
		}
		if err := c.walk(filepath.Join(real, e.Name()), filepath.Join(rel, e.Name())); err != nil {
			return err
		}
	}
	return nil
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
