package launch

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"sherpa/internal/harness"
)

// CredentialFiles: pinned to Task 1 spike findings — on macOS, auth under
// CLAUDE_CONFIG_DIR is carried only by $CONFIG_DIR/.credentials.json.
var CredentialFiles = []string{".credentials.json"}

type Stdio struct {
	In       io.Reader
	Out, Err io.Writer
}

func bin() string {
	if b := os.Getenv("SHERPA_CLAUDE_BIN"); b != "" {
		return b
	}
	return "claude"
}

func securityBin() string {
	if b := os.Getenv("SHERPA_SECURITY_BIN"); b != "" {
		return b
	}
	return "security"
}

// EnsureCredentialFile makes sure mineDir/.credentials.json exists so it can be
// linked into a profile. A fresh macOS machine may hold auth only in the
// Keychain (no .credentials.json on disk); this exports it once into the file.
// If the file already exists it is left untouched. The secret is written to the
// 0600 file only — never to logs or stdout.
func EnsureCredentialFile(mineDir string) error {
	dst := filepath.Join(mineDir, ".credentials.json")
	if _, err := os.Stat(dst); err == nil {
		return nil // already present, never overwrite
	}
	cmd := exec.Command(securityBin(), "find-generic-password", "-s", "Claude Code-credentials", "-w")
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("keychain export of Claude credentials failed: %w", err)
	}
	if err := os.WriteFile(dst, out, 0o600); err != nil {
		return fmt.Errorf("writing credential file: %w", err)
	}
	return nil
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

func Claude(profileDir, mineDir string, credFiles []string, args []string, stdio Stdio) error {
	for _, f := range credFiles {
		dst := filepath.Join(profileDir, f)
		if _, err := os.Stat(dst); err == nil {
			continue // never overwrite
		}
		src := filepath.Join(mineDir, f)
		if b, err := os.ReadFile(src); err == nil {
			os.WriteFile(dst, b, 0o600)
		}
	}
	cmd := exec.Command(bin(), args...)
	cmd.Env = withClaudeConfigDir(os.Environ(), profileDir)
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

func withClaudeConfigDir(env []string, profileDir string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if strings.HasPrefix(kv, "CLAUDE_CONFIG_DIR=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "CLAUDE_CONFIG_DIR="+profileDir)
}
