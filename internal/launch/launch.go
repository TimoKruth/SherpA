package launch

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"sherpa/internal/harness"
	"sherpa/internal/process"
)

type Stdio struct {
	In       io.Reader
	Out, Err io.Writer
}

// SeedSetup writes a curated setup-state file into profileDir if it has none and
// mine holds a captured blob. Never overwrites an existing profile file (live
// runtime state wins). A missing blob is a no-op — the caller falls back to the
// tool's own first-run onboarding.
func SeedSetup(profileDir, mineDir string, h harness.Harness) error {
	b, err := os.ReadFile(filepath.Join(mineDir, h.CapturedName()))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	rel, content, err := h.Seed(b)
	if err != nil {
		return err
	}
	if rel == "" {
		return nil
	}
	dst := filepath.Join(profileDir, rel)
	if info, err := os.Lstat(dst); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("setup state must be a regular file")
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.WriteFile(dst, content, 0o600)
}

func Launch(h harness.Harness, profileDir, baselineDir string, args []string, stdio Stdio) error {
	if err := CopyCredentials(h, profileDir, baselineDir); err != nil {
		return err
	}
	cmd := exec.Command(launchBin(h), args...)
	cmd.Env = withConfigDir(os.Environ(), h.ConfigDirEnv(), profileDir)
	// Default each stream independently so tests can override any subset.
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdio.In, stdio.Out, stdio.Err
	if cmd.Stdin == nil {
		cmd.Stdin = os.Stdin
	}
	if cmd.Stdout == nil {
		cmd.Stdout = os.Stdout
	}
	if cmd.Stderr == nil {
		cmd.Stderr = os.Stderr
	}
	return cmd.Run()
}

func launchBin(h harness.Harness) string {
	if b := os.Getenv(h.LaunchBinEnv()); b != "" {
		return b
	}
	return h.LaunchBin()
}

func withConfigDir(env []string, envName, profileDir string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if strings.HasPrefix(kv, envName+"=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, envName+"="+profileDir)
}

// CopyCredentials never links to or writes the baseline. A copied credential
// can be refreshed by the harness without changing its source.
func CopyCredentials(h harness.Harness, dstDir, srcDir string) error {
	for _, f := range h.CredentialFiles() {
		dst := filepath.Join(dstDir, f)
		if info, err := os.Lstat(dst); err == nil {
			if !info.Mode().IsRegular() {
				return fmt.Errorf("credential must be a regular file: %s", f)
			}
			if err := os.Chmod(dst, 0600); err != nil {
				return err
			}
			continue
		} else if !os.IsNotExist(err) {
			return err
		}
		b, err := os.ReadFile(filepath.Join(srcDir, f))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if err := os.WriteFile(dst, b, 0600); err != nil {
			return err
		}
	}
	return nil
}

// Command runs a noninteractive trial with explicit working and config dirs.
func Command(ctx context.Context, h harness.Harness, configDir, workDir string, args []string) *exec.Cmd {
	cmd := process.Command(ctx, launchBin(h), args...)
	cmd.Dir = workDir
	cmd.Env = withConfigDir(os.Environ(), h.ConfigDirEnv(), configDir)
	return cmd
}
