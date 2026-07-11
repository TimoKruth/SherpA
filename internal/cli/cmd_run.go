package cli

import (
	"fmt"

	"sherpa/internal/harness"
	"sherpa/internal/launch"
	"sherpa/internal/state"
)

func init() {
	register("run", cmdRun)
}

func cmdRun(ctx *Ctx, args []string) error {
	fresh := false
	var passthrough []string
	for _, a := range args {
		if a == "--fresh-setup" {
			fresh = true
			continue
		}
		passthrough = append(passthrough, a)
	}

	st, err := state.Load(ctx.Home)
	if err != nil {
		return err
	}
	active, ok := st.Profiles[st.Active]
	if !ok {
		return fmt.Errorf("no active profile (run `sherpa init` first)")
	}
	h, err := harness.For(active.Harness)
	if err != nil {
		return err
	}
	baseline, err := baselineProfile(st, active.Harness)
	if err != nil {
		return err
	}

	if !fresh {
		if err := launch.SeedSetup(active.Path, baseline.Path, h); err != nil {
			fmt.Fprintf(ctx.Stderr, "warning: could not seed setup state (%v); tool may onboard\n", err)
		}
	}

	// Best-effort: make sure the baseline has credentials to link from.
	// Never fatal: the tool can still prompt for login.
	if err := h.PrepareBaselineCredentials(baseline.Path); err != nil {
		fmt.Fprintf(ctx.Stderr, "warning: could not prepare credentials (%v); the tool may ask you to log in\n", err)
	}

	stdio := launch.Stdio{In: ctx.Stdin, Out: ctx.Stdout, Err: ctx.Stderr}
	return launch.Launch(h, active.Path, baseline.Path, passthrough, stdio)
}
