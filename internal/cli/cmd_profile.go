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
	mine := st.Profiles["mine"]
	// Clear any seeded setup so the tool runs its own first-run flow.
	rel, _, _ := harness.Default().Seed([]byte("{}")) // rel = target filename (".claude.json"); content ignored
	_ = os.Remove(filepath.Join(p.Path, rel))
	if err := launch.EnsureCredentialFile(mine.Path); err != nil {
		fmt.Fprintf(ctx.Stderr, "warning: could not prepare credentials (%v)\n", err)
	}
	fmt.Fprintf(ctx.Stdout, "fresh setup for %q (active profile unchanged)\n", name)
	stdio := launch.Stdio{In: ctx.Stdin, Out: ctx.Stdout, Err: ctx.Stderr}
	// No SeedSetup call: this is the fresh path.
	return launch.Claude(p.Path, mine.Path, launch.CredentialFiles, nil, stdio)
}
