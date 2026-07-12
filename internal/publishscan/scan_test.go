package publishscan

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"sherpa/internal/harness"
	"sherpa/internal/sanitize"
)

func TestScanRepoFindsTrackedSecret(t *testing.T) {
	repo := makeRepo(t, map[string]string{
		"settings.json": `{"token":"ghp_` + strings.Repeat("x", 36) + `"}` + "\n",
	})

	findings, err := ScanRepo(repo, harness.Codex{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !hasFinding(findings, "secret", "settings.json") {
		t.Fatalf("ScanRepo findings = %#v, want secret in settings.json", findings)
	}
}

func TestScanRepoCleanRepoReturnsNoFindings(t *testing.T) {
	repo := makeRepo(t, map[string]string{
		"settings.json": `{"note":"clean"}` + "\n",
	})

	findings, err := ScanRepo(repo, harness.Codex{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("ScanRepo findings = %#v, want none", findings)
	}
}

func TestScanRepoFindsCodexSetupState(t *testing.T) {
	repo := makeRepo(t, map[string]string{
		"auth.json": `{"access_token":"local-token"}` + "\n",
	})

	findings, err := ScanRepo(repo, harness.Codex{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !hasFinding(findings, "setup-state", "auth.json") {
		t.Fatalf("ScanRepo findings = %#v, want setup-state in auth.json", findings)
	}
}

func makeRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-b", "main")
	git(t, dir, "config", "user.email", "sherpa@local")
	git(t, dir, "config", "user.name", "sherpa")
	git(t, dir, "config", "commit.gpgsign", "false")
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "fixture")
	return dir
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func hasFinding(findings []sanitize.Finding, kind, file string) bool {
	for _, finding := range findings {
		if finding.Kind == kind && finding.File == file {
			return true
		}
	}
	return false
}
