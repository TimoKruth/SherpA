// Package gitutil provides thin wrappers over the `git` command line used by
// the clone/diff/update pipeline. It shells out rather than linking a git
// library so behaviour matches exactly what a user would see on the terminal.
package gitutil

import (
	"fmt"
	"os/exec"
	"strings"
)

// Clone runs `git clone url dest`. dest may be an existing empty directory.
func Clone(url, dest string) error {
	if out, err := exec.Command("git", "clone", url, dest).CombinedOutput(); err != nil {
		return fmt.Errorf("git clone %s: %v: %s", url, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Run executes `git -C dir args...` and returns the trimmed combined output.
// On failure the trimmed output is returned alongside a wrapping error so
// callers can both inspect and surface what git printed.
func Run(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil {
		return s, fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, s)
	}
	return s, nil
}
