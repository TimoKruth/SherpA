package cli

import (
	"fmt"
	"sherpa/internal/state"
)

func init() { register("try", cmdTry) }
func cmdTry(ctx *Ctx, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: sherpa try <installed-profile>")
	}
	st, err := state.Load(ctx.Home)
	if err != nil {
		return err
	}
	p, ok := st.Profiles[args[0]]
	if !ok {
		return fmt.Errorf("unknown local profile %q; use sherpa profile create or import", args[0])
	}
	return runProfile(ctx, st, p, nil, false)
}
