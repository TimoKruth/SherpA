package cli

import (
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
	target := "mine"
	if active, ok := st.Profiles[st.Active]; ok {
		if baseline, ok := baselineName(st, active.Harness); ok {
			target = baseline
		}
	}
	return cmdUse(ctx, []string{target})
}

func cmdStatus(ctx *Ctx, args []string) error {
	st, err := state.Load(ctx.Home)
	if err != nil {
		return err
	}
	fmt.Fprintf(ctx.Stdout, "active: %s\n", st.Active)
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
		fmt.Fprintf(ctx.Stdout, "  %s  %s\n", name, origin)
	}
	return nil
}
