package cli

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"sherpa/internal/state"
)

const (
	defaultUpdatesLimit  = 25
	maxTerminalTextBytes = 512
)

type updatesRequest struct {
	limit   int
	seen    bool
	owner   string
	name    string
	version int
}

func init() { register("updates", cmdUpdates) }

func cmdUpdates(ctx *Ctx, args []string) error {
	req, err := parseUpdatesArgs(args)
	if err != nil {
		return err
	}
	base, client, err := socialClientFromEnvironment(ctx.Home)
	if err != nil {
		return err
	}
	retryPendingFollows(ctx, base, client)
	if req.seen {
		if _, err := client.MarkSeen(context.Background(), req.owner, req.name, req.version); err != nil {
			return err
		}
		markCachedSeen(ctx.Home, base, req.owner, req.name, req.version)
		fmt.Fprintf(ctx.Stdout, "marked @%s/%s@v%d reviewed\n", req.owner, req.name, req.version)
		return nil
	}
	page, err := client.ListUpdates(context.Background(), req.limit, "")
	if err != nil {
		return err
	}
	cacheRegistryUpdates(ctx.Home, base, page.Updates)
	if len(page.Updates) == 0 {
		fmt.Fprintln(ctx.Stdout, "no pending updates")
		return nil
	}
	for _, update := range page.Updates {
		fmt.Fprintf(ctx.Stdout, "@%s/%s  v%d -> v%d  %s\n", terminalText(update.Owner, 100), terminalText(update.Name, 100), update.SeenVersion, update.Version, formatUpdateTime(update.PublishedAt))
		if changelog := terminalText(update.Changelog, maxTerminalTextBytes); changelog != "" {
			fmt.Fprintf(ctx.Stdout, "  %s\n", changelog)
		}
		fmt.Fprintln(ctx.Stdout, "  review locally with: sherpa update <profile>")
	}
	if page.NextCursor != "" {
		fmt.Fprintf(ctx.Stdout, "showing the first %d updates; rerun with a larger --limit up to %d\n", req.limit, registrySocialListMax)
	}
	return nil
}

func parseUpdatesArgs(args []string) (updatesRequest, error) {
	req := updatesRequest{limit: defaultUpdatesLimit}
	sawLimit := false
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "--limit":
			if sawLimit {
				return updatesRequest{}, fmt.Errorf("--limit may be specified once")
			}
			sawLimit = true
			i++
			if i >= len(args) {
				return updatesRequest{}, fmt.Errorf("--limit requires a value")
			}
			limit, err := strconv.Atoi(args[i])
			if err != nil || limit < 1 || limit > registrySocialListMax {
				return updatesRequest{}, fmt.Errorf("--limit must be between 1 and %d", registrySocialListMax)
			}
			req.limit = limit
		case strings.HasPrefix(arg, "--limit="):
			if sawLimit {
				return updatesRequest{}, fmt.Errorf("--limit may be specified once")
			}
			sawLimit = true
			limit, err := strconv.Atoi(strings.TrimPrefix(arg, "--limit="))
			if err != nil || limit < 1 || limit > registrySocialListMax {
				return updatesRequest{}, fmt.Errorf("--limit must be between 1 and %d", registrySocialListMax)
			}
			req.limit = limit
		case arg == "--seen":
			i++
			if i >= len(args) || req.seen {
				return updatesRequest{}, fmt.Errorf("usage: sherpa updates --seen @owner/name@vN")
			}
			owner, name, version, ok := parseRegistryVersionRef(args[i])
			if !ok {
				return updatesRequest{}, fmt.Errorf("seen ref must be @owner/name@vN")
			}
			req.seen, req.owner, req.name, req.version = true, owner, name, version
		case strings.HasPrefix(arg, "-"):
			return updatesRequest{}, fmt.Errorf("unknown flag %q", arg)
		default:
			return updatesRequest{}, fmt.Errorf("unexpected argument %q", arg)
		}
	}
	if req.seen && sawLimit {
		return updatesRequest{}, fmt.Errorf("--seen cannot be combined with --limit")
	}
	return req, nil
}

func parseRegistryVersionRef(ref string) (string, string, int, bool) {
	i := strings.LastIndex(ref, "@v")
	if i <= 0 {
		return "", "", 0, false
	}
	owner, name, ok := parseRegistryStackRef(ref[:i])
	if !ok {
		return "", "", 0, false
	}
	versionText := ref[i+2:]
	if versionText == "" || strings.IndexFunc(versionText, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return "", "", 0, false
	}
	version, err := strconv.Atoi(versionText)
	if err != nil || version < 1 {
		return "", "", 0, false
	}
	return owner, name, version, true
}

func cacheRegistryUpdates(home, base string, updates []registryUpdate) {
	st, err := state.Load(home)
	if err != nil {
		return
	}
	registry := st.Registries[base]
	registry.CachedUpdates = summarizeRegistryUpdates(updates)
	registry.LastCheckedAt = time.Now().UTC()
	st.Registries[base] = registry
	_ = st.Save(home)
}

func summarizeRegistryUpdates(updates []registryUpdate) []state.UpdateSummary {
	summaries := make([]state.UpdateSummary, 0, len(updates))
	for _, update := range updates {
		summaries = append(summaries, state.UpdateSummary{
			Owner: update.Owner, Stack: update.Name, Version: update.Version,
			SeenVersion: update.SeenVersion, Changelog: terminalText(update.Changelog, maxTerminalTextBytes),
			PublishedAt: update.PublishedAt,
		})
	}
	return summaries
}

func markCachedSeen(home, base, owner, name string, version int) {
	st, err := state.Load(home)
	if err != nil {
		return
	}
	registry := st.Registries[base]
	for i := range registry.CachedUpdates {
		update := &registry.CachedUpdates[i]
		if update.Owner == owner && update.Stack == name && version > update.SeenVersion {
			update.SeenVersion = version
		}
	}
	st.Registries[base] = registry
	_ = st.Save(home)
}

func terminalText(value string, maxBytes int) string {
	var b strings.Builder
	for _, r := range value {
		if unicode.IsControl(r) {
			r = ' '
		}
		width := utf8.RuneLen(r)
		if b.Len()+width > maxBytes {
			break
		}
		b.WriteRune(r)
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func formatUpdateTime(value time.Time) string {
	if value.IsZero() {
		return "time unknown"
	}
	return value.UTC().Format("2006-01-02")
}
