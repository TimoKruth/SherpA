package cli

import (
	"context"
	"fmt"
	"sort"

	"sherpa/internal/state"
)

func init() {
	register("use", cmdUse)
	register("back", cmdBack)
	register("status", cmdStatus)
}

func cmdUse(ctx *Ctx, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: sherpa use <profile>")
	}
	name := args[0]
	st, err := state.Load(ctx.Home)
	if err != nil {
		return err
	}
	if _, ok := st.Profiles[name]; !ok {
		return fmt.Errorf("unknown profile %q", name)
	}
	st.Active = name
	if err := st.Save(ctx.Home); err != nil {
		return err
	}
	fmt.Fprintf(ctx.Stdout, "switched to profile %q\n", name)
	return nil
}

func cmdBack(ctx *Ctx, args []string) error {
	st, err := state.Load(ctx.Home)
	if err != nil {
		return err
	}
	active, ok := st.Profiles[st.Active]
	if !ok {
		return fmt.Errorf("no active profile (run `sherpa init` first)")
	}
	baseline, err := baselineProfile(st, active.Harness)
	if err != nil {
		return err
	}
	return cmdUse(ctx, []string{baseline.Name})
}

func cmdStatus(ctx *Ctx, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: sherpa status")
	}
	st, err := state.Load(ctx.Home)
	if err != nil {
		return err
	}
	fmt.Fprintf(ctx.Stdout, "active: %s\n", terminalText(st.Active, 100))
	names := make([]string, 0, len(st.Profiles))
	for name := range st.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		origin := st.Profiles[name].Origin
		if origin == "" {
			origin = "(yours)"
		}
		fmt.Fprintf(ctx.Stdout, "  %s  %s\n", terminalText(name, 100), terminalText(origin, maxTerminalTextBytes))
	}
	statusRegistryUpdates(ctx, st)
	return nil
}

func statusRegistryUpdates(ctx *Ctx, st *state.State) {
	rawBase := registryBaseURL()
	if rawBase == "" {
		return
	}
	base, err := normalizeRegistryBase(rawBase)
	if err != nil {
		fmt.Fprintln(ctx.Stderr, "warning: registry updates unavailable (invalid registry URL)")
		return
	}
	session, err := registryUserSession(ctx.Home, base)
	if err != nil {
		printCachedUpdateSummaries(ctx, st.Registries[base].CachedUpdates)
		fmt.Fprintln(ctx.Stderr, "warning: registry updates unavailable; run `sherpa login`")
		return
	}
	client, err := newRegistrySocialClient(base, session.AccessToken)
	if err != nil {
		printCachedUpdateSummaries(ctx, st.Registries[base].CachedUpdates)
		fmt.Fprintln(ctx.Stderr, "warning: registry updates unavailable")
		return
	}
	retryPendingFollows(ctx, base, client)
	page, err := client.ListUpdates(context.Background(), 10, "")
	if err != nil {
		printCachedUpdateSummaries(ctx, st.Registries[base].CachedUpdates)
		fmt.Fprintln(ctx.Stderr, "warning: registry updates unavailable")
		return
	}
	cacheRegistryUpdates(ctx.Home, base, page.Updates)
	printCachedUpdateSummaries(ctx, summarizeRegistryUpdates(page.Updates))
}

func printCachedUpdateSummaries(ctx *Ctx, updates []state.UpdateSummary) {
	pending := 0
	for _, update := range updates {
		if update.Version <= update.SeenVersion {
			continue
		}
		if pending == 0 {
			fmt.Fprintln(ctx.Stdout, "pending updates:")
		}
		fmt.Fprintf(ctx.Stdout, "  @%s/%s  v%d -> v%d\n", terminalText(update.Owner, 100), terminalText(update.Stack, 100), update.SeenVersion, update.Version)
		pending++
		if pending == 10 {
			break
		}
	}
}
