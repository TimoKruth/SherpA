package sanitize

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

var claudeSetupStateNames = []string{".claude.json", ".sherpa-setup.json"}
var claudeLoginSignatures = []string{"oauthAccount", "claudeAiOauth", `"accessToken"`, `"refreshToken"`}

func TestScanFindsEveryPlantedSecret(t *testing.T) {
	dir := filepath.Join("testdata", "secrets-stack")
	findings, err := Scan(dir, []string{"settings.json", "CLAUDE.md"})
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}

	want := []struct {
		name  string
		file  string
		kind  string
		value string
	}{
		{"settings.json/secret/ghp", "settings.json", "secret", "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ1234567890"},
		{"settings.json/secret/github_pat", "settings.json", "secret", "github_pat_11AA22BB33CC44DD55EE66"},
		{"settings.json/secret/sk-ant", "settings.json", "secret", "sk-ant-abcdefghijklmnopqrstuvwxyz"},
		{"settings.json/secret/sk-proj", "settings.json", "secret", "sk-proj-ABCDEFGHIJKLMNOPQRSTUVWXYZ123456"},
		{"settings.json/secret/akia", "settings.json", "secret", "AKIAABCDEFGHIJKLMNOP"},
		{"settings.json/secret/xoxb", "settings.json", "secret", "xoxb-123456789012-ABCDEFGHIJKLMN-opQRST"},
		{"settings.json/secret/generic-entropy", "settings.json", "secret", "R4nD0mZ9qX7pL2vB6sC8eT5yU3iO1"},
		{"settings.json/secret/hex", "settings.json", "secret", "0123456789abcdef0123456789abcdef01234567"},
	}

	for _, w := range want {
		t.Run(w.name, func(t *testing.T) {
			f := findByPrefix(findings, w.file, w.kind, w.value[:4])
			if f == nil {
				t.Fatalf("missing finding for %s %s", w.file, w.value)
			}
			if f.Line == 0 {
				t.Fatalf("finding has no 1-indexed line: %+v", *f)
			}
			if strings.Contains(f.Excerpt, w.value) {
				t.Fatalf("excerpt leaked full secret %q in %q", w.value, f.Excerpt)
			}
		})
	}
}

func TestScanCleanStackHasNoSecretFindings(t *testing.T) {
	dir := filepath.Join("testdata", "clean-stack")
	findings, err := Scan(dir, []string{"settings.json", "CLAUDE.md", "skills/"})
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}
	for _, f := range findings {
		if f.Kind == "secret" {
			t.Fatalf("clean stack produced secret finding: %+v", f)
		}
	}
}

func TestScanWarnsForPersonalDataKinds(t *testing.T) {
	dir := filepath.Join("testdata", "personal-stack")
	findings, err := Scan(dir, []string{"settings.json", "README.md"})
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}

	if findByKind(findings, "settings.json", "home-path") == nil {
		t.Fatalf("missing home-path warning in settings.json: %+v", findings)
	}
	if findByKind(findings, "settings.json", "email") == nil {
		t.Fatalf("missing email warning in settings.json: %+v", findings)
	}
	if findByKind(findings, "README.md", "home-path") == nil {
		t.Fatalf("missing home-path warning in README.md: %+v", findings)
	}
	if findByKind(findings, "README.md", "email") == nil {
		t.Fatalf("missing email warning in README.md: %+v", findings)
	}
	for _, f := range findings {
		if f.Kind != "home-path" && f.Kind != "email" {
			t.Fatalf("personal data produced blocking kind: %+v", f)
		}
	}
}

func TestScanOnlyWalksAllowlistedPaths(t *testing.T) {
	dir := filepath.Join("testdata", "secrets-stack")
	findings, err := Scan(dir, []string{"CLAUDE.md"})
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("non-allowlisted settings.json was scanned: %+v", findings)
	}
}

func TestMaskedExcerptsMaskEverySensitiveValueOnLine(t *testing.T) {
	ghp := "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ1234567890"
	anthropic := "sk-ant-abcdefghijklmnopqrstuvwxyz"
	findings := scanLine("settings.json", 1, `{"github":"`+ghp+`","anthropic":"`+anthropic+`"}`)

	if len(findings) != 2 {
		t.Fatalf("expected two findings, got %d: %+v", len(findings), findings)
	}
	for _, f := range findings {
		if strings.Contains(f.Excerpt, ghp) {
			t.Fatalf("excerpt leaked ghp token in %q", f.Excerpt)
		}
		if strings.Contains(f.Excerpt, anthropic) {
			t.Fatalf("excerpt leaked anthropic key in %q", f.Excerpt)
		}
	}
}

func TestScanPatchScansOnlyAddedLinesWithCurrentDiffTarget(t *testing.T) {
	token := "ghp_" + strings.Repeat("x", 36)
	patch := strings.Join([]string{
		"diff --git a/CLAUDE.md b/CLAUDE.md",
		"index 1111111..2222222 100644",
		"--- a/CLAUDE.md",
		"+++ b/CLAUDE.md",
		"@@ -1,2 +1,2 @@",
		"-removed " + token,
		"+added " + token,
		"diff --git a/README.md b/README.md",
		"--- a/README.md",
		"+++ b/README.md",
		"@@ -1 +1 @@",
		"+clean line",
	}, "\n")

	findings, err := ScanPatch(patch)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want one added-line finding", findings)
	}
	f := findings[0]
	if f.File != "CLAUDE.md" || f.Line != 0 || f.Kind != "secret" {
		t.Fatalf("finding = %+v, want CLAUDE.md line 0 secret", f)
	}
	if strings.Contains(f.Excerpt, token) {
		t.Fatalf("excerpt leaked full token: %q", f.Excerpt)
	}
}

func TestScanSetupStateFlagsFileByName(t *testing.T) {
	d := t.TempDir()
	if err := os.MkdirAll(filepath.Join(d, "skills", "local"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(d, "skills", "local", ".claude.json"), []byte(`{"x":1}`), 0o600)
	f, err := ScanSetupState(d, []string{"skills/"}, claudeSetupStateNames, claudeLoginSignatures)
	if err != nil || len(f) == 0 {
		t.Fatalf("want setup-state finding, got %v err %v", f, err)
	}
	if f[0].Kind != "setup-state" {
		t.Fatalf("kind = %q", f[0].Kind)
	}
}

func TestScanSetupStateFlagsOAuthContent(t *testing.T) {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "settings.json"), []byte(`{"note":"has oauthAccount here"}`), 0o644)
	f, _ := ScanSetupState(d, []string{"settings.json"}, claudeSetupStateNames, claudeLoginSignatures)
	if len(f) == 0 {
		t.Fatal("want finding for oauth signature in a normal file")
	}
}

func TestScanSetupStateSkipsNonAllowlistedLocalLoginFiles(t *testing.T) {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, ".claude.json"), []byte(`{"oauthAccount":{"id":"acct"}}`), 0o600)
	os.WriteFile(filepath.Join(d, ".credentials.json"), []byte(`{"accessToken":"token"}`), 0o600)
	os.WriteFile(filepath.Join(d, "settings.json"), []byte(`{"note":"clean"}`), 0o644)
	f, err := ScanSetupState(d, []string{"settings.json"}, claudeSetupStateNames, claudeLoginSignatures)
	if err != nil {
		t.Fatal(err)
	}
	if len(f) != 0 {
		t.Fatalf("non-allowlisted login files were scanned: %+v", f)
	}
}

func TestScanSetupStateCleanDirNoFindings(t *testing.T) {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "CLAUDE.md"), []byte("# ok"), 0o644)
	f, _ := ScanSetupState(d, []string{"CLAUDE.md"}, claudeSetupStateNames, claudeLoginSignatures)
	if len(f) != 0 {
		t.Fatalf("clean dir flagged: %v", f)
	}
}

func TestScanPatchSetupStateFlagsAddedOAuth(t *testing.T) {
	patch := "+++ b/x.json\n+  \"accessToken\": \"zzz\"\n"
	if len(ScanPatchSetupState(patch, claudeSetupStateNames, claudeLoginSignatures)) == 0 {
		t.Fatal("want setup-state finding in patch")
	}
}

func TestScanPatchSetupStateFlagsQuotedSetupStatePath(t *testing.T) {
	patch := "+++ \"b/dir-\\303\\274/.claude.json\"\n+{}\n"
	if len(ScanPatchSetupState(patch, claudeSetupStateNames, claudeLoginSignatures)) == 0 {
		t.Fatal("want setup-state finding for quoted patch path")
	}
}

func TestScanReturnsScannerErrors(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "long.txt"), []byte(strings.Repeat("a", 1024*1024+1)), 0o600); err != nil {
		t.Fatal(err)
	}

	findings, err := Scan(dir, []string{"long.txt"})
	if err == nil {
		t.Fatalf("expected scanner error, got findings: %+v", findings)
	}
}

func TestScanReturnsUnreadableAllowlistedFileErrors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod-based unreadable file test is Unix-specific")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(path, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(path, 0o600)
	})

	findings, err := Scan(dir, []string{"secret.txt"})
	if err == nil {
		t.Fatalf("expected unreadable file error, got findings: %+v", findings)
	}
}

func findByPrefix(findings []Finding, file, kind, prefix string) *Finding {
	for i := range findings {
		f := &findings[i]
		if f.File == file && f.Kind == kind && strings.Contains(f.Excerpt, prefix) {
			return f
		}
	}
	return nil
}

func findByKind(findings []Finding, file, kind string) *Finding {
	for i := range findings {
		f := &findings[i]
		if f.File == file && f.Kind == kind {
			return f
		}
	}
	return nil
}
