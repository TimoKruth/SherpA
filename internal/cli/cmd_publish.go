package cli

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"sherpa/internal/gitutil"
	"sherpa/internal/sanitize"
	"sherpa/internal/stack"

	"gopkg.in/yaml.v3"
)

func init() {
	register("publish", cmdPublish)
}

func cmdPublish(ctx *Ctx, args []string) error {
	remote, err := parsePublishArgs(args)
	if err != nil {
		return err
	}
	profile, err := activeProfile(ctx)
	if err != nil {
		return err
	}

	findings, err := sanitize.Scan(profile.Path, stack.AllowedPaths)
	if err != nil {
		return fmt.Errorf("sanitize scan failed: %w", err)
	}
	historyPatch, err := scanPublishHistoryPatch(profile.Path, remote)
	if err != nil {
		return err
	}
	historyFindings, err := sanitize.ScanPatch(historyPatch)
	if err != nil {
		return err
	}
	findings = append(findings, historyFindings...)
	// setup-state / OAuth must never be published (spec 2a §3.3). No override.
	// Tracked files are a subset of AllowedPaths: the canonical gitignore
	// whitelist is stack.AllowedPaths, so scanning AllowedPaths covers exactly
	// what publish pushes. Gitignored machine-local login/setup files are never
	// pushed, so they must not block publish.
	ss, err := sanitize.ScanSetupState(profile.Path, stack.AllowedPaths)
	if err != nil {
		return err
	}
	histSS := sanitize.ScanPatchSetupState(historyPatch)
	if len(ss) > 0 || len(histSS) > 0 {
		printFindings(ctx.Stderr, append(ss, histSS...))
		fmt.Fprintln(ctx.Stderr, "machine-local login/setup files are gitignored and never published; if a tracked stack file contains login content, remove it before publishing.")
		return fmt.Errorf("publish blocked: setup-state/login content must never be shared")
	}
	input := bufio.NewReader(ctx.Stdin)
	if hasSecretFindings(findings) {
		printFindings(ctx.Stderr, findings)
		return errors.New("publish blocked by secret findings")
	}
	if len(findings) > 0 {
		printFindings(ctx.Stdout, findings)
		if err := confirm(input, ctx.Stdout, "Sanitizer warnings found. Type yes to continue: "); err != nil {
			return err
		}
	}

	version, err := nextStackVersion(profile.Path)
	if err != nil {
		return err
	}
	if err := showPublishDiff(ctx.Stdout, profile.Path); err != nil {
		return err
	}
	if err := confirm(input, ctx.Stdout, fmt.Sprintf("publish version %d? (yes/no): ", version)); err != nil {
		return err
	}

	if err := setStackVersion(profile.Path, version); err != nil {
		return err
	}
	if err := commitAll(profile.Path, fmt.Sprintf("sherpa: publish v%d", version)); err != nil {
		return err
	}

	tag := fmt.Sprintf("v%d", version)
	if _, err := gitutil.Run(profile.Path, "tag", tag); err != nil {
		return err
	}
	if _, err := gitutil.Run(profile.Path, "push", remote, "local:main", "refs/tags/"+tag); err != nil {
		return err
	}
	fmt.Fprintf(ctx.Stdout, "published %s to %s\n", tag, remote)
	return nil
}

func parsePublishArgs(args []string) (string, error) {
	var remote string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--remote":
			i++
			if i >= len(args) {
				return "", fmt.Errorf("--remote requires a value")
			}
			remote = args[i]
		case strings.HasPrefix(a, "--remote="):
			remote = strings.TrimPrefix(a, "--remote=")
		case strings.HasPrefix(a, "-"):
			return "", fmt.Errorf("unknown flag %q", a)
		default:
			return "", fmt.Errorf("unexpected argument %q", a)
		}
	}
	if remote == "" {
		return "", fmt.Errorf("usage: sherpa publish --remote <git-url>")
	}
	return remote, nil
}

func hasSecretFindings(findings []sanitize.Finding) bool {
	for _, finding := range findings {
		if finding.Kind == "secret" {
			return true
		}
	}
	return false
}

func printFindings(w io.Writer, findings []sanitize.Finding) {
	for _, finding := range findings {
		fmt.Fprintf(w, "%s:%d: %s: %s\n", finding.File, finding.Line, finding.Kind, finding.Excerpt)
	}
}

func scanPublishHistoryPatch(dir, remote string) (string, error) {
	rangeSpec, err := publishHistoryRange(dir, remote)
	if err != nil {
		return "", err
	}
	args := []string{"log", "-m", "-p", rangeSpec}
	patch, err := gitutil.Run(dir, args...)
	if err != nil {
		return "", err
	}
	return patch, nil
}

func publishHistoryRange(dir, remote string) (string, error) {
	out, err := gitutil.Run(dir, "ls-remote", remote, "main")
	if err != nil {
		return "", err
	}
	if sha := parseRemoteMainSHA(out); sha != "" {
		return sha + "..local", nil
	}
	return "local", nil
}

func parseRemoteMainSHA(out string) string {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if fields[1] == "refs/heads/main" && fields[0] != "" {
			return fields[0]
		}
	}
	return ""
}

func confirm(in *bufio.Reader, out io.Writer, prompt string) error {
	fmt.Fprint(out, prompt)
	line, err := in.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if strings.TrimSpace(line) != "yes" {
		return errors.New("confirmation declined")
	}
	return nil
}

func bumpStackVersion(dir string) (int, error) {
	next, err := nextStackVersion(dir)
	if err != nil {
		return 0, err
	}
	if err := setStackVersion(dir, next); err != nil {
		return 0, err
	}
	return next, nil
}

func nextStackVersion(dir string) (int, error) {
	path := filepath.Join(dir, "stack.yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("stack.yaml: %w", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return 0, fmt.Errorf("stack.yaml: %w", err)
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return 0, errors.New("stack.yaml: expected mapping document")
	}
	mapping := doc.Content[0]
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		key := mapping.Content[i]
		value := mapping.Content[i+1]
		if key.Value != "version" {
			continue
		}
		current, err := strconv.Atoi(value.Value)
		if err != nil || current < 1 {
			return 0, fmt.Errorf("stack.yaml: invalid version %q", value.Value)
		}
		next := current + 1
		tags, err := gitutil.Run(dir, "tag", "-l", "v*")
		if err != nil {
			return 0, err
		}
		seen, maxTag := versionTagNumbers(strings.Split(tags, "\n"))
		// Forked profiles keep fetched upstream tags in the same local namespace,
		// so skip over an upstream-owned v<N> before writing and tagging ours.
		if seen[next] {
			next = maxTag + 1
		}
		return next, nil
	}
	return 0, errors.New("stack.yaml: missing version")
}

func setStackVersion(dir string, version int) error {
	path := filepath.Join(dir, "stack.yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("stack.yaml: %w", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return fmt.Errorf("stack.yaml: %w", err)
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return errors.New("stack.yaml: expected mapping document")
	}
	mapping := doc.Content[0]
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		key := mapping.Content[i]
		value := mapping.Content[i+1]
		if key.Value != "version" {
			continue
		}
		value.Kind = yaml.ScalarNode
		value.Tag = "!!int"
		value.Value = strconv.Itoa(version)
		value.Style = 0
		var out bytes.Buffer
		enc := yaml.NewEncoder(&out)
		enc.SetIndent(2)
		if err := enc.Encode(&doc); err != nil {
			enc.Close()
			return err
		}
		if err := enc.Close(); err != nil {
			return err
		}
		return os.WriteFile(path, out.Bytes(), 0o644)
	}
	return errors.New("stack.yaml: missing version")
}

func showPublishDiff(w io.Writer, dir string) error {
	tag, err := gitutil.Run(dir, "describe", "--tags", "--abbrev=0", "--match", "v[0-9]*")
	if err == nil && tag != "" {
		out, err := gitutil.Run(dir, "diff", tag+"..HEAD")
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "Changes since %s:\n%s\n", tag, out)
		return nil
	}
	out, err := gitutil.Run(dir, "log", "--stat", "--patch", "--reverse")
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "No previous published tag found. Full log:\n%s\n", out)
	return nil
}
