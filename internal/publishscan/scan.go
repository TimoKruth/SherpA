// Package publishscan contains the shared fail-closed scan used before publish.
package publishscan

import (
	"fmt"
	"strings"

	"sherpa/internal/gitutil"
	"sherpa/internal/harness"
	"sherpa/internal/sanitize"
)

// ScanRepo runs the full fail-closed publish scan over a git repo checkout: the exact
// pushed working-tree set (git ls-files) for secrets + setup-state, and the history
// patch (git log -m -p over historyRange, or all of it if empty) for both. h supplies
// the harness's SetupStateFilenames/LoginSignatures. Returns all findings (empty = clean).
func ScanRepo(repoDir string, h harness.Harness, historyRange string) ([]sanitize.Finding, error) {
	scanFiles, err := publishScanFiles(repoDir)
	if err != nil {
		return nil, err
	}
	scanFiles = append(scanFiles, ".gitignore")

	findings, err := sanitize.Scan(repoDir, scanFiles)
	if err != nil {
		return nil, fmt.Errorf("sanitize scan failed: %w", err)
	}
	historyPatch, err := scanPublishHistoryPatch(repoDir, historyRange)
	if err != nil {
		return nil, err
	}
	historyFindings, err := sanitize.ScanPatch(historyPatch)
	if err != nil {
		return nil, err
	}
	findings = append(findings, historyFindings...)

	setupFindings, err := sanitize.ScanSetupState(repoDir, scanFiles, h.SetupStateFilenames(), h.LoginSignatures())
	if err != nil {
		return nil, err
	}
	findings = append(findings, setupFindings...)
	findings = append(findings, sanitize.ScanPatchSetupState(historyPatch, h.SetupStateFilenames(), h.LoginSignatures())...)
	return findings, nil
}

func publishScanFiles(dir string) ([]string, error) {
	out, err := gitutil.Run(dir, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, file := range strings.Split(out, "\x00") {
		if file == "" {
			continue
		}
		files = append(files, file)
	}
	return files, nil
}

func scanPublishHistoryPatch(dir, historyRange string) (string, error) {
	args := []string{"log", "-m", "-p"}
	if historyRange != "" {
		args = append(args, historyRange)
	}
	return gitutil.Run(dir, args...)
}
