package cli

import (
	"fmt"

	"sherpa/internal/launch"
	"sherpa/internal/state"
)

func init() {
	register("run", cmdRun)
}

func cmdRun(ctx *Ctx, args []string) error {
	st, err := state.Load(ctx.Home)
	if err != nil {
		return err
	}
	active, ok := st.Profiles[st.Active]
	if !ok {
		return fmt.Errorf("no active profile (run `sherpa init` first)")
	}
	mine, ok := st.Profiles["mine"]
	if !ok {
		return fmt.Errorf("no `mine` profile (run `sherpa init` first)")
	}

	// Best-effort: make sure mine has a credential file to link from. On a fresh
	// machine auth may live only in the Keychain. Never fatal — claude can still
	// prompt for login.
	if err := launch.EnsureCredentialFile(mine.Path); err != nil {
		fmt.Fprintf(ctx.Stderr, "warning: could not prepare credentials (%v); claude may ask you to log in\n", err)
	}

	stdio := launch.Stdio{In: ctx.Stdin, Out: ctx.Stdout, Err: ctx.Stderr}
	return launch.Claude(active.Path, mine.Path, launch.CredentialFiles, args, stdio)
}
