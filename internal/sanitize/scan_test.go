package sanitize

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestScanFindsEveryPlantedSecret(t *testing.T) {
	dir := filepath.Join("testdata", "secrets-stack")
	findings := Scan(dir, []string{"settings.json", "CLAUDE.md"})

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
	findings := Scan(dir, []string{"settings.json", "CLAUDE.md", "skills/"})
	for _, f := range findings {
		if f.Kind == "secret" {
			t.Fatalf("clean stack produced secret finding: %+v", f)
		}
	}
}

func TestScanWarnsForPersonalDataKinds(t *testing.T) {
	dir := filepath.Join("testdata", "personal-stack")
	findings := Scan(dir, []string{"settings.json", "README.md"})

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
	findings := Scan(dir, []string{"CLAUDE.md"})
	if len(findings) != 0 {
		t.Fatalf("non-allowlisted settings.json was scanned: %+v", findings)
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
