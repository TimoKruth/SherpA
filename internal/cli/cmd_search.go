package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

const supportedSearchHarness = "claude-code"

func init() {
	register("search", cmdSearch)
}

type indexDocument struct {
	Stacks []indexStack `json:"stacks"`
}

type indexStack struct {
	Ref        string   `json:"ref"`
	Name       string   `json:"name"`
	Owner      string   `json:"owner"`
	Harness    string   `json:"harness"`
	Summary    string   `json:"summary"`
	Tags       []string `json:"tags"`
	RepoURL    string   `json:"repo_url"`
	Version    int      `json:"version"`
	ForkedFrom *string  `json:"forked_from"`
}

func cmdSearch(ctx *Ctx, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: sherpa search <query>")
	}
	query := strings.TrimSpace(strings.Join(args, " "))
	if query == "" {
		return fmt.Errorf("usage: sherpa search <query>")
	}
	indexURL := os.Getenv("SHERPA_INDEX_URL")
	if indexURL == "" {
		return fmt.Errorf("SHERPA_INDEX_URL is required for Phase 1 search")
	}

	doc, err := loadIndex(indexURL)
	if err != nil {
		return err
	}
	matches := searchStacks(doc.Stacks, query)
	if len(matches) == 0 {
		fmt.Fprintln(ctx.Stdout, "no stacks found")
		return nil
	}
	for _, stack := range matches {
		fmt.Fprintf(ctx.Stdout, "%s  %s  (%s)\n", stack.Ref, stack.Summary, stack.Harness)
	}
	fmt.Fprintf(ctx.Stdout, "try: sherpa try %s\n", matches[0].RepoURL)
	return nil
}

func loadIndex(indexURL string) (indexDocument, error) {
	body, err := readIndexURL(indexURL)
	if err != nil {
		return indexDocument{}, err
	}
	var doc indexDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		return indexDocument{}, fmt.Errorf("index: invalid JSON: %w", err)
	}
	return doc, nil
}

func readIndexURL(indexURL string) ([]byte, error) {
	u, err := url.Parse(indexURL)
	if err != nil {
		return nil, fmt.Errorf("index URL: %w", err)
	}
	switch u.Scheme {
	case "http", "https":
		resp, err := http.Get(indexURL)
		if err != nil {
			return nil, fmt.Errorf("index fetch: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return nil, fmt.Errorf("index fetch: %s", resp.Status)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("index fetch: %w", err)
		}
		return body, nil
	case "file":
		return os.ReadFile(u.Path)
	case "":
		return os.ReadFile(indexURL)
	default:
		return nil, fmt.Errorf("unsupported index URL scheme %q", u.Scheme)
	}
}

func searchStacks(stacks []indexStack, query string) []indexStack {
	q := strings.ToLower(query)
	var matches []indexStack
	for _, stack := range stacks {
		if stack.Harness != supportedSearchHarness {
			continue
		}
		if stackMatches(stack, q) {
			matches = append(matches, stack)
		}
	}
	return matches
}

func stackMatches(stack indexStack, query string) bool {
	if strings.Contains(strings.ToLower(stack.Name), query) {
		return true
	}
	if strings.Contains(strings.ToLower(stack.Summary), query) {
		return true
	}
	for _, tag := range stack.Tags {
		if strings.Contains(strings.ToLower(tag), query) {
			return true
		}
	}
	return false
}
