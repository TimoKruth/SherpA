package sanitize

import (
	"bufio"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
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
	homePathRE     = regexp.MustCompile(`/(Users|home)/[A-Za-z0-9_\-]+/`)
	emailRE        = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	assignmentRE   = regexp.MustCompile(`(?i)["']?([A-Za-z0-9_\-]*(token|secret|key|password)[A-Za-z0-9_\-]*)["']?\s*[:=]\s*["']?([^"',\s]+)`)
	minSecretLen   = 20
	minSecretScore = 4.0
)

// Scan scans allowlisted paths under dir and returns publish sanitizer findings.
// Paths in allowed are relative to dir; entries ending in "/" are walked as
// directories. Invalid, absolute, parent-traversing, and missing paths are skipped.
func Scan(dir string, allowed []string) (findings []Finding) {
	seen := map[string]bool{}
	for _, allowedPath := range allowed {
		for _, path := range expandAllowed(dir, allowedPath) {
			if seen[path] {
				continue
			}
			seen[path] = true
			findings = append(findings, scanFile(dir, path)...)
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
	return findings
}

func expandAllowed(dir, allowedPath string) []string {
	clean, ok := cleanAllowed(allowedPath)
	if !ok {
		return nil
	}
	full := filepath.Join(dir, filepath.FromSlash(clean))
	info, err := os.Lstat(full)
	if err != nil {
		return nil
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil
	}
	if !info.IsDir() {
		return []string{clean}
	}

	var files []string
	filepath.WalkDir(full, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
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
			return nil
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	return files
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

func scanFile(root, rel string) []Finding {
	f, err := os.Open(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return nil
	}
	defer f.Close()

	var findings []Finding
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for lineNo := 1; scanner.Scan(); lineNo++ {
		line := scanner.Text()
		findings = append(findings, scanLine(rel, lineNo, line)...)
	}
	return findings
}

func scanLine(file string, lineNo int, line string) []Finding {
	var findings []Finding
	for _, d := range secretDetectors {
		findings = appendMatches(findings, file, lineNo, d.kind, line, d.re.FindAllString(line, -1))
	}
	for _, match := range assignmentRE.FindAllStringSubmatch(line, -1) {
		value := match[3]
		if len(value) >= minSecretLen && shannon(value) > minSecretScore {
			findings = append(findings, Finding{
				File:    file,
				Line:    lineNo,
				Kind:    "secret",
				Excerpt: maskedExcerpt(line, value),
			})
		}
	}
	findings = appendMatches(findings, file, lineNo, "home-path", line, homePathRE.FindAllString(line, -1))
	findings = appendMatches(findings, file, lineNo, "email", line, emailRE.FindAllString(line, -1))
	return findings
}

func appendMatches(findings []Finding, file string, lineNo int, kind, line string, matches []string) []Finding {
	for _, match := range matches {
		findings = append(findings, Finding{
			File:    file,
			Line:    lineNo,
			Kind:    kind,
			Excerpt: maskedExcerpt(line, match),
		})
	}
	return findings
}

func maskedExcerpt(line, value string) string {
	mask := value
	if utf8.RuneCountInString(value) > 4 {
		prefix := []rune(value)[:4]
		mask = string(prefix) + "…"
	}
	return strings.Replace(line, value, mask, 1)
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
