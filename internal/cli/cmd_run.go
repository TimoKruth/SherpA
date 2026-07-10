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
	mine, ok := st.Profiles["mine"]
	if !ok {
		return fmt.Errorf("no `mine` profile (run `sherpa init` first)")
	}

	if !fresh {
		if err := launch.SeedSetup(active.Path, mine.Path, h); err != nil {
			fmt.Fprintf(ctx.Stderr, "warning: could not seed setup state (%v); tool may onboard\n", err)
		}
	}

	// Best-effort: make sure mine has a credential file to link from. On a fresh
	// machine auth may live only in the Keychain. Never fatal — claude can still
	// prompt for login.
	if err := h.PrepareBaselineCredentials(mine.Path); err != nil {
		fmt.Fprintf(ctx.Stderr, "warning: could not prepare credentials (%v); claude may ask you to log in\n", err)
	}

	stdio := launch.Stdio{In: ctx.Stdin, Out: ctx.Stdout, Err: ctx.Stderr}
	return launch.Launch(h, active.Path, mine.Path, passthrough, stdio)
}
