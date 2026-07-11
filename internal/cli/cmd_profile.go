package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"sherpa/internal/harness"
	"sherpa/internal/launch"
	"sherpa/internal/state"
)

func init() { register("profile", cmdProfile) }

func cmdProfile(ctx *Ctx, args []string) error {
	if len(args) < 2 || args[0] != "setup" {
		return fmt.Errorf("usage: sherpa profile setup <name>")
	}
	name := args[1]
	st, err := state.Load(ctx.Home)
	if err != nil {
		return err
	}
	p, ok := st.Profiles[name]
	if !ok {
		return fmt.Errorf("unknown profile %q", name)
	}
	h, err := harness.For(p.Harness)
	if err != nil {
		return err
	}
	bn, ok := baselineName(st, p.Harness)
	if !ok {
		bn = "mine"
	}
	baseline, ok := st.Profiles[bn]
	if !ok {
		if bn == "mine" {
			return fmt.Errorf("no `mine` profile (run `sherpa init` first)")
		}
		return fmt.Errorf("no baseline profile for harness %q (run `sherpa init --harness %s` first)", p.Harness, p.Harness)
	}
	// Clear any seeded setup so the tool runs its own first-run flow.
	rel, _, err := h.Seed([]byte("{}")) // rel = target filename (".claude.json"); content ignored
	if err == nil && rel != "" {
		_ = os.Remove(filepath.Join(p.Path, rel))
	}
	if err := h.PrepareBaselineCredentials(baseline.Path); err != nil {
		fmt.Fprintf(ctx.Stderr, "warning: could not prepare credentials (%v)\n", err)
	}
	fmt.Fprintf(ctx.Stdout, "fresh setup for %q (active profile unchanged)\n", name)
	stdio := launch.Stdio{In: ctx.Stdin, Out: ctx.Stdout, Err: ctx.Stderr}
	// No SeedSetup call: this is the fresh path.
	return launch.Launch(h, p.Path, baseline.Path, nil, stdio)
}
