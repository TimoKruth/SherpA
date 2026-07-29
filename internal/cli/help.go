package cli

import (
	"fmt"
	"io"
	"sort"
	"text/tabwriter"
)

func init() { register("help", cmdHelp) }

// commandSummaries carries the one-line description shown by `sherpa help`.
// Every registered command must appear here; TestHelpCoversEveryCommand fails
// otherwise, so a new command cannot be added without documenting it.
var commandSummaries = map[string]string{
	"init":     "Import the harness configuration as the protected baseline profile",
	"login":    "Create a GitHub-backed registry session",
	"logout":   "Remove the registry session",
	"status":   "Show the active profile and registry session",
	"search":   "Search the registry for stacks",
	"try":      "Launch a stack without changing the active profile",
	"clone":    "Install a stack without activating it",
	"use":      "Switch the active profile",
	"back":     "Switch back to the protected baseline profile",
	"run":      "Run the harness under the active profile",
	"save":     "Commit changes made in the active profile",
	"diff":     "Show local profile changes, or a fork against upstream",
	"update":   "Fetch upstream versions and merge after confirmation",
	"publish":  "Scan, review, bump, tag, and publish a new version",
	"remove":   "Delete an installed profile and its directory",
	"profile":  "Re-run harness setup for a profile",
	"follow":   "Follow a stack to be told about new versions",
	"unfollow": "Stop following a stack",
	"updates":  "List new versions of stacks you follow",
	"trial":    "Record, list, or share a trial of a stack version",
	"help":     "Show this help",
	"version":  "Print the sherpa version",
}

func cmdHelp(ctx *Ctx, args []string) error {
	writeHelp(ctx.Stdout)
	return nil
}

func writeHelp(out io.Writer) {
	fmt.Fprintln(out, "sherpa — share and try complete agent setups as versioned stacks")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintln(out, "  sherpa <command> [args]")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Commands:")

	names := make([]string, 0, len(commandSummaries))
	for name := range commandSummaries {
		names = append(names, name)
	}
	sort.Strings(names)

	table := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, name := range names {
		fmt.Fprintf(table, "  %s\t%s\n", name, commandSummaries[name])
	}
	_ = table.Flush()

	fmt.Fprintln(out)
	fmt.Fprintln(out, "Environment:")
	fmt.Fprintf(table, "  SHERPA_REGISTRY_URL\tRegistry to use (default %s)\n", DefaultRegistryURL)
	fmt.Fprintf(table, "  SHERPA_HOME\tProfile and state directory (default ~/.sherpa)\n")
	_ = table.Flush()
}
