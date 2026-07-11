package launch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sherpa/internal/harness"
)

func TestLaunchLaunchesWithConfigDirAndLinksCreds(t *testing.T) {
	profile, mine, outDir := t.TempDir(), t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(mine, ".credentials.json"), []byte("secret"), 0o600)
	fake := filepath.Join(outDir, "claude")
	os.WriteFile(fake, []byte("#!/bin/sh\necho \"$CLAUDE_CONFIG_DIR\" > "+outDir+"/env.txt\n"), 0o755)
	t.Setenv("SHERPA_CLAUDE_BIN", fake)

	err := Launch(harness.Default(), profile, mine, nil, Stdio{})
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

func TestLaunchOverridesInheritedConfigDir(t *testing.T) {
	profile, mine, outDir := t.TempDir(), t.TempDir(), t.TempDir()
	fake := filepath.Join(outDir, "claude")
	os.WriteFile(fake, []byte("#!/bin/sh\necho \"$CLAUDE_CONFIG_DIR\" > "+outDir+"/env.txt\n"), 0o755)
	t.Setenv("SHERPA_CLAUDE_BIN", fake)
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	if err := Launch(harness.Default(), profile, mine, nil, Stdio{}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(outDir, "env.txt"))
	if strings.TrimSpace(string(got)) != profile {
		t.Fatalf("CLAUDE_CONFIG_DIR = %q, want %q", got, profile)
	}
}

func TestLaunchNeverOverwritesExistingCred(t *testing.T) {
	profile, mine := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(mine, ".credentials.json"), []byte("new"), 0o600)
	os.WriteFile(filepath.Join(profile, ".credentials.json"), []byte("keep"), 0o600)
	t.Setenv("SHERPA_CLAUDE_BIN", "/usr/bin/true")
	Launch(harness.Default(), profile, mine, nil, Stdio{})
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

func TestPrepareBaselineCredentialsExportsFromKeychainWhenMissing(t *testing.T) {
	mine := t.TempDir()
	t.Setenv("SHERPA_SECURITY_BIN", fakeSecurity(t, "kc-secret", 0))

	if err := harness.Default().PrepareBaselineCredentials(mine); err != nil {
		t.Fatalf("PrepareBaselineCredentials: %v", err)
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

func TestPrepareBaselineCredentialsNeverOverwritesExisting(t *testing.T) {
	mine := t.TempDir()
	os.WriteFile(filepath.Join(mine, ".credentials.json"), []byte("keep"), 0o600)
	// Point at a fake that would export a different secret; it must not run.
	t.Setenv("SHERPA_SECURITY_BIN", fakeSecurity(t, "kc-secret", 0))

	if err := harness.Default().PrepareBaselineCredentials(mine); err != nil {
		t.Fatalf("PrepareBaselineCredentials: %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(mine, ".credentials.json"))
	if string(b) != "keep" {
		t.Fatalf("overwrote existing credential file: %q", b)
	}
}

func TestPrepareBaselineCredentialsReturnsErrorOnKeychainFailure(t *testing.T) {
	mine := t.TempDir()
	t.Setenv("SHERPA_SECURITY_BIN", fakeSecurity(t, "", 1))

	err := harness.Default().PrepareBaselineCredentials(mine)
	if err == nil {
		t.Fatal("expected error when keychain export fails")
	}
	if _, statErr := os.Stat(filepath.Join(mine, ".credentials.json")); statErr == nil {
		t.Fatal("must not write a credential file on failure")
	}
}

func TestSeedSetupWritesCuratedWhenAbsent(t *testing.T) {
	profile, mine := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(mine, ".sherpa-setup.json"),
		[]byte(`{"hasCompletedOnboarding":true,"projects":{"x":1}}`), 0o600)
	if err := SeedSetup(profile, mine, harness.Default()); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(profile, ".claude.json"))
	if err != nil {
		t.Fatal("seed not written")
	}
	if !strings.Contains(string(b), "hasCompletedOnboarding") || strings.Contains(string(b), "projects") {
		t.Fatalf("seed not curated: %s", b)
	}
}

func TestSeedSetupNeverOverwrites(t *testing.T) {
	profile, mine := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(mine, ".sherpa-setup.json"), []byte(`{"theme":"dark"}`), 0o600)
	os.WriteFile(filepath.Join(profile, ".claude.json"), []byte(`{"live":"state"}`), 0o600)
	SeedSetup(profile, mine, harness.Default())
	b, _ := os.ReadFile(filepath.Join(profile, ".claude.json"))
	if string(b) != `{"live":"state"}` {
		t.Fatalf("overwrote live state: %s", b)
	}
}

func TestSeedSetupNoBlobNoop(t *testing.T) {
	profile, mine := t.TempDir(), t.TempDir()
	if err := SeedSetup(profile, mine, harness.Default()); err != nil {
		t.Fatalf("want nil on missing blob, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(profile, ".claude.json")); err == nil {
		t.Fatal("wrote a file with no blob to seed from")
	}
}
