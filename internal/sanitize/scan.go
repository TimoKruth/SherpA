package sanitize

import (
	"bufio"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Finding is a publish-time sanitizer result. Secret findings block publish;
// home-path and email findings are warnings.
type Finding struct {
	File    string
	Line    int
	Kind    string
	Excerpt string
}

type detector struct {
	kind string
	re   *regexp.Regexp
}

var secretDetectors = []detector{
	{kind: "secret", re: regexp.MustCompile(`ghp_[A-Za-z0-9]{36}`)},
	{kind: "secret", re: regexp.MustCompile(`github_pat_[A-Za-z0-9_]{22,}`)},
	{kind: "secret", re: regexp.MustCompile(`sk-[A-Za-z0-9\-_]{20,}`)},
	{kind: "secret", re: regexp.MustCompile(`AKIA[0-9A-Z]{16}`)},
	{kind: "secret", re: regexp.MustCompile(`xoxb-[A-Za-z0-9\-]+`)},
}

var (
	homePathRE     = regexp.MustCompile(`/(Users|home)/[A-Za-z0-9_\-]+/?`)
	emailRE        = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	assignmentRE   = regexp.MustCompile(`(?i)["']?([A-Za-z0-9_\-]*(token|secret|key|password)[A-Za-z0-9_\-]*)["']?\s*[:=]\s*(?:"([^"]*)"|'([^']*)'|([^"',\s]+))`)
	hexSecretRE    = regexp.MustCompile(`^[0-9a-fA-F]{32,}$`)
	minSecretLen   = 20
	minSecretScore = 4.0
)

var setupStateNames = map[string]bool{".claude.json": true, ".sherpa-setup.json": true}
var oauthSignatures = []string{"oauthAccount", "claudeAiOauth", `"accessToken"`, `"refreshToken"`}

func containsOAuthSig(s string) bool {
	for _, sig := range oauthSignatures {
		if strings.Contains(s, sig) {
			return true
		}
	}
	return false
}

// Scan scans allowlisted paths under dir and returns publish sanitizer findings.
// Paths in allowed are relative to dir; entries ending in "/" are walked as
// directories. Invalid, absolute, parent-traversing, and missing paths are skipped.
func Scan(dir string, allowed []string) (findings []Finding, err error) {
	seen := map[string]bool{}
	for _, allowedPath := range allowed {
		paths, err := expandAllowed(dir, allowedPath)
		if err != nil {
			return nil, err
		}
		for _, path := range paths {
			if seen[path] {
				continue
			}
			seen[path] = true
			fileFindings, err := scanFile(dir, path)
			if err != nil {
				return nil, err
			}
			findings = append(findings, fileFindings...)
		}
	}
	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}
		if findings[i].Line != findings[j].Line {
			return findings[i].Line < findings[j].Line
		}
		return findings[i].Kind < findings[j].Kind
	})
	return findings, nil
}

// ScanSetupState flags any allowlisted setup-state file (by name) or allowlisted
// file whose content carries an OAuth login signature. Fail-closed input to
// publish; Kind "setup-state".
func ScanSetupState(dir string, allowed []string) ([]Finding, error) {
	var out []Finding
	seen := map[string]bool{}
	for _, allowedPath := range allowed {
		paths, err := expandAllowed(dir, allowedPath)
		if err != nil {
			return nil, err
		}
		for _, rel := range paths {
			if seen[rel] {
				continue
			}
			seen[rel] = true
			fileFindings, err := scanSetupStateFile(dir, rel)
			if err != nil {
				return nil, err
			}
			out = append(out, fileFindings...)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		if out[i].Line != out[j].Line {
			return out[i].Line < out[j].Line
		}
		return out[i].Kind < out[j].Kind
	})
	return out, nil
}

func scanSetupStateFile(root, rel string) ([]Finding, error) {
	name := filepath.Base(filepath.FromSlash(rel))
	if setupStateNames[name] {
		return []Finding{{File: rel, Kind: "setup-state", Excerpt: name}}, nil
	}
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return nil, err
	}
	if containsOAuthSig(string(b)) {
		return []Finding{{File: rel, Kind: "setup-state", Excerpt: "oauth login signature"}}, nil
	}
	return nil, nil
}

func ScanPatchSetupState(patch string) []Finding {
	var out []Finding
	for _, line := range strings.Split(patch, "\n") {
		if strings.HasPrefix(line, "+++ ") {
			path := strings.TrimSpace(strings.TrimPrefix(line, "+++ "))
			if strings.HasPrefix(path, "\"") && strings.HasSuffix(path, "\"") {
				path = strings.TrimSuffix(strings.TrimPrefix(path, "\""), "\"")
			}
			name := filepath.Base(strings.TrimPrefix(path, "b/"))
			if setupStateNames[name] {
				out = append(out, Finding{File: name, Kind: "setup-state", Excerpt: name})
			}
			continue
		}
		if strings.HasPrefix(line, "+") && containsOAuthSig(line) {
			out = append(out, Finding{Kind: "setup-state", Excerpt: "oauth login signature"})
		}
	}
	return out
}

func expandAllowed(dir, allowedPath string) ([]string, error) {
	clean, ok := cleanAllowed(allowedPath)
	if !ok {
		return nil, nil
	}
	full := filepath.Join(dir, filepath.FromSlash(clean))
	info, err := os.Lstat(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("sanitize: stat allowlisted path %q: %w", clean, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil, nil
	}
	if !info.IsDir() {
		return []string{clean}, nil
	}

	var files []string
	err = filepath.WalkDir(full, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if name == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeType != 0 {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("sanitize: walk allowlisted path %q: %w", clean, err)
	}
	return files, nil
}

func cleanAllowed(path string) (string, bool) {
	if path == "" || filepath.IsAbs(path) {
		return "", false
	}
	path = strings.TrimPrefix(filepath.ToSlash(path), "./")
	clean := filepath.ToSlash(filepath.Clean(path))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false
	}
	return strings.TrimSuffix(clean, "/"), true
}

func scanFile(root, rel string) ([]Finding, error) {
	f, err := os.Open(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return nil, fmt.Errorf("sanitize: open allowlisted file %q: %w", rel, err)
	}
	defer f.Close()

	var findings []Finding
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for lineNo := 1; scanner.Scan(); lineNo++ {
		line := scanner.Text()
		findings = append(findings, scanLine(rel, lineNo, line)...)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("sanitize: scan allowlisted file %q: %w", rel, err)
	}
	return findings, nil
}

// ScanPatch scans unified diff text and returns findings from added lines only.
// Diff metadata and +++ file headers are skipped; findings are attributed to the
// current diff target and use line 0 because git log patches may span history.
func ScanPatch(patch string) ([]Finding, error) {
	var findings []Finding
	file := ""
	scanner := bufio.NewScanner(strings.NewReader(patch))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "+++ ") {
			file = diffTargetFile(strings.TrimSpace(strings.TrimPrefix(line, "+++ ")))
			continue
		}
		if !strings.HasPrefix(line, "+") || strings.HasPrefix(line, "+++") {
			continue
		}
		findings = append(findings, scanLine(file, 0, strings.TrimPrefix(line, "+"))...)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("sanitize: scan patch: %w", err)
	}
	return findings, nil
}

func diffTargetFile(target string) string {
	switch {
	case target == "/dev/null":
		return ""
	case strings.HasPrefix(target, "b/"):
		return strings.TrimPrefix(target, "b/")
	default:
		return target
	}
}

func scanLine(file string, lineNo int, line string) []Finding {
	var matches []detectedValue
	for _, d := range secretDetectors {
		matches = appendMatches(matches, d.kind, d.re.FindAllString(line, -1))
	}
	for _, match := range assignmentRE.FindAllStringSubmatch(line, -1) {
		value := assignmentValue(match)
		if genericSecretCandidate(value) {
			matches = append(matches, detectedValue{kind: "secret", value: value})
		}
	}
	matches = appendMatches(matches, "home-path", homePathRE.FindAllString(line, -1))
	matches = appendMatches(matches, "email", emailRE.FindAllString(line, -1))

	values := make([]string, 0, len(matches))
	for _, match := range matches {
		values = append(values, match.value)
	}

	var findings []Finding
	for _, match := range matches {
		findings = append(findings, Finding{
			File:    file,
			Line:    lineNo,
			Kind:    match.kind,
			Excerpt: maskedExcerpt(line, values),
		})
	}
	return findings
}

type detectedValue struct {
	kind  string
	value string
}

func appendMatches(findings []detectedValue, kind string, matches []string) []detectedValue {
	for _, match := range matches {
		findings = append(findings, detectedValue{kind: kind, value: match})
	}
	return findings
}

func assignmentValue(match []string) string {
	for _, value := range match[3:] {
		if value != "" {
			return value
		}
	}
	return ""
}

func genericSecretCandidate(value string) bool {
	if genericSecretExempt(value) {
		return false
	}
	if hexSecretRE.MatchString(value) {
		return true
	}
	return len(value) >= minSecretLen && shannon(value) > minSecretScore
}

func genericSecretExempt(value string) bool {
	if strings.Contains(value, "://") {
		return true
	}
	return strings.IndexFunc(value, unicode.IsSpace) >= 0
}

func maskedExcerpt(line string, values []string) string {
	excerpt := line
	for _, value := range values {
		excerpt = strings.ReplaceAll(excerpt, value, maskValue(value))
	}
	return excerpt
}

func maskValue(value string) string {
	mask := value
	if utf8.RuneCountInString(value) > 4 {
		prefix := []rune(value)[:4]
		mask = string(prefix) + "…"
	}
	return mask
}

func shannon(value string) float64 {
	if value == "" {
		return 0
	}
	counts := map[rune]int{}
	var total int
	for _, r := range value {
		counts[r]++
		total++
	}
	var entropy float64
	for _, count := range counts {
		p := float64(count) / float64(total)
		entropy -= p * math.Log2(p)
	}
	return entropy
}
