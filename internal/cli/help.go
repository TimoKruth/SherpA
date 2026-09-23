package cli

import (
	"fmt"
	"io"
	"sort"
	"text/tabwriter"
)

func init() { register("help", cmdHelp) }

var commandSummaries = map[string]string{
	"init":    "Import installed Claude Code / Codex setups as protected baselines",
	"serve":   "Open the local comparison workspace (loopback only)",
	"compare": "Run one prompt across profiles in independent project snapshots",
	"results": "List or inspect saved comparisons; rate their results",
	"profile": "Create, import, inspect, or set up a local profile",
	"use":     "Select the profile used by sherpa run",
	"back":    "Return to the protected baseline for the active harness",
	"status":  "List local profiles and the active setup",
	"try":     "Launch an installed profile without changing the active setup",
	"run":     "Launch the active profile (baseline launches use a disposable copy)",
	"save":    "Commit an experimental profile's local configuration changes",
	"diff":    "Show changes in the active profile since its last save",
	"remove":  "Remove an inactive experimental profile",
	"help":    "Show this help", "version": "Print the sherpa version",
}

func cmdHelp(ctx *Ctx, args []string) error { writeHelp(ctx.Stdout); return nil }
func writeHelp(out io.Writer) {
	fmt.Fprintln(out, "SherpA — compare local agent setups, keep your main setup protected\n\nUsage:\n  sherpa <command> [args]\n\nCommands:")
	names := []string{}
	for n := range commandSummaries {
		names = append(names, n)
	}
	sort.Strings(names)
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, n := range names {
		fmt.Fprintf(w, "  %s\t%s\n", n, commandSummaries[n])
	}
	w.Flush()
	fmt.Fprintln(out, "\nStart: sherpa init, then sherpa serve\nEnvironment: SHERPA_HOME (default ~/.sherpa), SHERPA_CLAUDE_BIN, SHERPA_CODEX_BIN")
}
