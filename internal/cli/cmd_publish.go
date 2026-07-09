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

	version, err := bumpStackVersion(profile.Path)
	if err != nil {
		return err
	}
	if err := commitAll(profile.Path, fmt.Sprintf("sherpa: publish v%d", version)); err != nil {
		return err
	}

	if err := showPublishDiff(ctx.Stdout, profile.Path); err != nil {
		return err
	}
	if err := confirm(input, ctx.Stdout, fmt.Sprintf("Publish v%d to %s? Type yes to continue: ", version, remote)); err != nil {
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
		value.Kind = yaml.ScalarNode
		value.Tag = "!!int"
		value.Value = strconv.Itoa(next)
		value.Style = 0
		var out bytes.Buffer
		enc := yaml.NewEncoder(&out)
		enc.SetIndent(2)
		if err := enc.Encode(&doc); err != nil {
			enc.Close()
			return 0, err
		}
		if err := enc.Close(); err != nil {
			return 0, err
		}
		if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
			return 0, err
		}
		return next, nil
	}
	return 0, errors.New("stack.yaml: missing version")
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
