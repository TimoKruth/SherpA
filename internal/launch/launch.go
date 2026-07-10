package launch

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"sherpa/internal/harness"
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
	dst := filepath.Join(profileDir, rel)
	if _, err := os.Stat(dst); err == nil {
		return nil // never overwrite live state
	}
	return os.WriteFile(dst, content, 0o600)
}

func Launch(h harness.Harness, profileDir, baselineDir string, args []string, stdio Stdio) error {
	for _, f := range h.CredentialFiles() {
		dst := filepath.Join(profileDir, f)
		if _, err := os.Stat(dst); err == nil {
			continue // never overwrite
		}
		src := filepath.Join(baselineDir, f)
		if b, err := os.ReadFile(src); err == nil {
			os.WriteFile(dst, b, 0o600)
		}
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
