// Package gitutil provides thin wrappers over the `git` command line used by
// local profile versioning. It shells out rather than linking a git
// library so behaviour matches exactly what a user would see on the terminal.
package gitutil

import (
	"fmt"
	"os/exec"
	"strings"
)

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
