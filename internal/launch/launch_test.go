package launch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClaudeLaunchesWithConfigDirAndLinksCreds(t *testing.T) {
	profile, mine, outDir := t.TempDir(), t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(mine, ".credentials.json"), []byte("secret"), 0o600)
	fake := filepath.Join(outDir, "claude")
	os.WriteFile(fake, []byte("#!/bin/sh\necho \"$CLAUDE_CONFIG_DIR\" > "+outDir+"/env.txt\n"), 0o755)
	t.Setenv("SHERPA_CLAUDE_BIN", fake)

	err := Claude(profile, mine, []string{".credentials.json"}, nil, Stdio{})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(outDir, "env.txt"))
	if strings.TrimSpace(string(got)) != profile {
		t.Fatalf("CLAUDE_CONFIG_DIR = %q, want %q", got, profile)
	}
	cred, err := os.ReadFile(filepath.Join(profile, ".credentials.json"))
	if err != nil || string(cred) != "secret" {
		t.Fatal("credentials not linked into profile")
	}
}

func TestClaudeNeverOverwritesExistingCred(t *testing.T) {
	profile, mine := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(mine, ".credentials.json"), []byte("new"), 0o600)
	os.WriteFile(filepath.Join(profile, ".credentials.json"), []byte("keep"), 0o600)
	t.Setenv("SHERPA_CLAUDE_BIN", "/usr/bin/true")
	Claude(profile, mine, []string{".credentials.json"}, nil, Stdio{})
	b, _ := os.ReadFile(filepath.Join(profile, ".credentials.json"))
	if string(b) != "keep" {
		t.Fatal("overwrote existing credential file")
	}
}

// fakeSecurity writes a shell script standing in for macOS `security`, echoing a
// fake secret to stdout. Registered via SHERPA_SECURITY_BIN so no real keychain
// is ever touched.
func fakeSecurity(t *testing.T, secret string, exitCode int) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "security")
	script := "#!/bin/sh\n"
	if exitCode == 0 {
		script += "printf %s '" + secret + "'\n"
	} else {
		script += "echo 'security: could not be found' 1>&2\nexit 1\n"
	}
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEnsureCredentialFileExportsFromKeychainWhenMissing(t *testing.T) {
	mine := t.TempDir()
	t.Setenv("SHERPA_SECURITY_BIN", fakeSecurity(t, "kc-secret", 0))

	if err := EnsureCredentialFile(mine); err != nil {
		t.Fatalf("EnsureCredentialFile: %v", err)
	}
	dst := filepath.Join(mine, ".credentials.json")
	b, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("expected credential file written: %v", err)
	}
	if string(b) != "kc-secret" {
		t.Fatalf("credential content = %q, want %q", b, "kc-secret")
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("credential perm = %o, want 600", perm)
	}
}

func TestEnsureCredentialFileNeverOverwritesExisting(t *testing.T) {
	mine := t.TempDir()
	os.WriteFile(filepath.Join(mine, ".credentials.json"), []byte("keep"), 0o600)
	// Point at a fake that would export a different secret; it must not run.
	t.Setenv("SHERPA_SECURITY_BIN", fakeSecurity(t, "kc-secret", 0))

	if err := EnsureCredentialFile(mine); err != nil {
		t.Fatalf("EnsureCredentialFile: %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(mine, ".credentials.json"))
	if string(b) != "keep" {
		t.Fatalf("overwrote existing credential file: %q", b)
	}
}

func TestEnsureCredentialFileReturnsErrorOnKeychainFailure(t *testing.T) {
	mine := t.TempDir()
	t.Setenv("SHERPA_SECURITY_BIN", fakeSecurity(t, "", 1))

	err := EnsureCredentialFile(mine)
	if err == nil {
		t.Fatal("expected error when keychain export fails")
	}
	if _, statErr := os.Stat(filepath.Join(mine, ".credentials.json")); statErr == nil {
		t.Fatal("must not write a credential file on failure")
	}
}
