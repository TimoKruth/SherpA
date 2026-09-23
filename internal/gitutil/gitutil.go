// Package gitutil provides thin wrappers over the `git` command line used by
// local profile versioning. It shells out rather than linking a git
// library so behaviour matches exactly what a user would see on the terminal.
package gitutil

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sherpa/internal/process"
	"strings"
	"time"
)

// Run executes `git -C dir args...` and returns the trimmed combined output.
// On failure the trimmed output is returned alongside a wrapping error so
// callers can both inspect and surface what git printed.
func Run(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := Command(ctx, dir, args...).CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil {
		return s, fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, s)
	}
	return s, nil
}

// Environment prevents inherited repository/index/config overrides from redirecting writes.
func Environment() []string {
	var env []string
	for _, v := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(v), "GIT_") {
			env = append(env, v)
		}
	}
	return append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
}

// Command disables hooks, signing, filesystem monitors and global templates.
func Command(ctx context.Context, dir string, args ...string) *exec.Cmd {
	base := []string{"-c", "core.hooksPath=", "-c", "commit.gpgsign=false", "-c", "core.fsmonitor=false", "-c", "init.templateDir=", "-c", "user.name=SherpA", "-c", "user.email=sherpa@local", "-C", dir}
	cmd := process.Command(ctx, "git", append(base, args...)...)
	cmd.Env = Environment()
	return cmd
}
