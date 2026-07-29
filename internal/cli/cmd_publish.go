package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"sherpa/internal/gitutil"
	"sherpa/internal/harness"
	"sherpa/internal/publishscan"
	"sherpa/internal/sanitize"
	"sherpa/internal/stack"

	"gopkg.in/yaml.v3"
)

func init() {
	register("publish", cmdPublish)
}

func cmdPublish(ctx *Ctx, args []string) error {
	req, err := parsePublishArgs(args)
	if err != nil {
		return err
	}
	if req.registry == "" && req.remote == "" {
		req.registry = registryBaseURL()
	}
	profile, err := activeProfile(ctx)
	if err != nil {
		return err
	}
	h, err := harness.For(profile.Harness)
	if err != nil {
		return err
	}

	historyRange, err := publishHistoryRangeForRequest(profile.Path, req)
	if err != nil {
		return err
	}
	findings, err := publishscan.ScanRepo(profile.Path, h, historyRange)
	if err != nil {
		return err
	}
	// setup-state / OAuth must never be published (spec 2a §3.3). No override.
	setupStateFindings := findingsByKind(findings, "setup-state")
	if len(setupStateFindings) > 0 {
		printFindings(ctx.Stderr, setupStateFindings)
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
	if req.registry != "" {
		return publishRegistryVersion(ctx, profile.Path, req.registry, tag)
	}
	remote := req.remote
	if _, err := gitutil.Run(profile.Path, "push", remote, "local:main", "refs/tags/"+tag); err != nil {
		return err
	}
	fmt.Fprintf(ctx.Stdout, "published %s to %s\n", tag, remote)
	return nil
}

type publishRequest struct {
	remote   string
	registry string
}

func parsePublishArgs(args []string) (publishRequest, error) {
	var req publishRequest
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--remote":
			i++
			if i >= len(args) {
				return publishRequest{}, fmt.Errorf("--remote requires a value")
			}
			req.remote = args[i]
		case strings.HasPrefix(a, "--remote="):
			req.remote = strings.TrimPrefix(a, "--remote=")
		case a == "--registry":
			i++
			if i >= len(args) {
				return publishRequest{}, fmt.Errorf("--registry requires a value")
			}
			req.registry = args[i]
		case strings.HasPrefix(a, "--registry="):
			req.registry = strings.TrimPrefix(a, "--registry=")
		case strings.HasPrefix(a, "-"):
			return publishRequest{}, fmt.Errorf("unknown flag %q", a)
		default:
			return publishRequest{}, fmt.Errorf("unexpected argument %q", a)
		}
	}
	if req.remote != "" && req.registry != "" {
		return publishRequest{}, fmt.Errorf("use only one of --remote or --registry")
	}
	if req.remote == "" && req.registry == "" && registryBaseURL() == "" {
		return publishRequest{}, fmt.Errorf("usage: sherpa publish --remote <git-url> or sherpa publish --registry <url>")
	}
	return req, nil
}

func hasSecretFindings(findings []sanitize.Finding) bool {
	for _, finding := range findings {
		if finding.Kind == "secret" {
			return true
		}
	}
	return false
}

func findingsByKind(findings []sanitize.Finding, kind string) []sanitize.Finding {
	var out []sanitize.Finding
	for _, finding := range findings {
		if finding.Kind == kind {
			out = append(out, finding)
		}
	}
	return out
}

func printFindings(w io.Writer, findings []sanitize.Finding) {
	for _, finding := range findings {
		fmt.Fprintf(w, "%s:%d: %s: %s\n", finding.File, finding.Line, finding.Kind, finding.Excerpt)
	}
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

func publishHistoryRangeForRequest(dir string, req publishRequest) (string, error) {
	if req.registry != "" || (req.remote == "" && registryBaseURL() != "") {
		return "local", nil
	}
	return publishHistoryRange(dir, req.remote)
}

func publishRegistryVersion(ctx *Ctx, dir, registryURL, tag string) error {
	m, err := readStackManifest(dir)
	if err != nil {
		return err
	}
	owner := strings.TrimPrefix(m.Owner, "@")
	if owner == "" {
		// The registry rejects any owner other than the session login
		// (authorize.go: owner != identity.Login -> 403), so the only value a
		// user could correctly type here is the one we already know. Default it
		// rather than making them hand-edit stack.yaml.
		session, sessionErr := registryUserSession(ctx.Home, registryURL)
		if sessionErr != nil {
			return fmt.Errorf("stack.yaml has no owner and no registry session to infer it from: %w", sessionErr)
		}
		owner = session.Login
		if owner == "" {
			return fmt.Errorf("stack.yaml: owner is required for registry publish")
		}
		if err := setStackOwner(dir, owner); err != nil {
			return fmt.Errorf("record owner in stack.yaml: %w", err)
		}
		fmt.Fprintf(ctx.Stdout, "recorded owner @%s in stack.yaml\n", owner)
	}

	tmpDir, err := os.MkdirTemp("", "sherpa-publish-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	bundlePath := filepath.Join(tmpDir, "stack.bundle")
	if _, err := gitutil.Run(dir, "bundle", "create", bundlePath, "--all"); err != nil {
		return err
	}

	token, err := registryToken(ctx.Home, registryURL)
	if err != nil {
		return fmt.Errorf("load registry session: %w", err)
	}
	status, body, err := publishToRegistry(registryURL, token, owner, m.Name, bundlePath)
	if err != nil {
		return err
	}
	if status == http.StatusCreated {
		fmt.Fprintf(ctx.Stdout, "published %s to %s\n", tag, registryURL)
		return nil
	}
	printRegistryPublishError(ctx.Stderr, status, body)
	return fmt.Errorf("registry publish failed: HTTP %d", status)
}

func readStackManifest(dir string) (*stack.Manifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, "stack.yaml"))
	if err != nil {
		return nil, fmt.Errorf("stack.yaml: %w", err)
	}
	return stack.Parse(b)
}

func printRegistryPublishError(w io.Writer, status int, body []byte) {
	if status == http.StatusForbidden {
		fmt.Fprintln(w, "you can only publish under @<your-github-login>")
		return
	}
	var resp registryPublishError
	if err := json.Unmarshal(body, &resp); err == nil {
		for _, finding := range resp.Findings {
			fmt.Fprintf(w, "%s:%d: %s: %s\n", finding.File, finding.Line, finding.Kind, finding.Excerpt)
		}
		for _, violation := range resp.Violations {
			fmt.Fprintf(w, "validation: %s\n", violation)
		}
		if resp.Error != "" {
			fmt.Fprintf(w, "%s\n", resp.Error)
		}
		if len(resp.Findings) > 0 || len(resp.Violations) > 0 || resp.Error != "" {
			return
		}
	}
	if len(strings.TrimSpace(string(body))) > 0 {
		fmt.Fprintln(w, strings.TrimSpace(string(body)))
		return
	}
	fmt.Fprintf(w, "registry publish failed: HTTP %d\n", status)
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

// setStackOwner records the owner in stack.yaml, adding the key when absent so
// a stack that has never been published becomes self-describing. It preserves
// the rest of the document, matching setStackVersion.
func setStackOwner(dir, owner string) error {
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
	updated := false
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value != "owner" {
			continue
		}
		value := mapping.Content[i+1]
		value.Kind = yaml.ScalarNode
		value.Tag = "!!str"
		value.Value = owner
		value.Style = 0
		updated = true
		break
	}
	if !updated {
		mapping.Content = append(mapping.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "owner"},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: owner},
		)
	}
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
