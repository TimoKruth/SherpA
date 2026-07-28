package cli

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"

	"sherpa/internal/state"
)

func init() { register("remove", cmdRemove) }

// cmdRemove deletes an installed profile and its directory. Removal is
// destructive: work committed inside the profile with `sherpa save` is not
// recoverable afterwards, so it confirms unless --yes is given.
func cmdRemove(ctx *Ctx, args []string) error {
	name := ""
	assumeYes := false
	for _, arg := range args {
		switch arg {
		case "--yes", "-y":
			assumeYes = true
		default:
			if name != "" {
				return fmt.Errorf("usage: sherpa remove <profile> [--yes]")
			}
			name = arg
		}
	}
	if name == "" {
		return fmt.Errorf("usage: sherpa remove <profile> [--yes]")
	}

	st, err := state.Load(ctx.Home)
	if err != nil {
		return err
	}
	profile, ok := st.Profiles[name]
	if !ok {
		return fmt.Errorf("unknown profile %q", name)
	}
	// The baseline is the user's own imported setup, not an installed stack.
	// Removing it would discard the thing `sherpa back` restores to.
	for harness, baseline := range st.Baselines {
		if baseline == name {
			return fmt.Errorf("profile %q is the %s baseline and cannot be removed", name, harness)
		}
	}
	if st.Active == name {
		return fmt.Errorf("profile %q is active — run `sherpa back` before removing it", name)
	}

	if !assumeYes {
		if err := confirm(bufio.NewReader(ctx.Stdin), ctx.Stdout,
			fmt.Sprintf("Remove profile %q and everything in it? This cannot be undone. Type yes to confirm: ", name)); err != nil {
			return err
		}
	}

	// Drop the state entry first. An orphaned directory is harmless, whereas a
	// state entry pointing at a missing directory breaks every other command.
	delete(st.Profiles, name)
	if err := st.Save(ctx.Home); err != nil {
		return err
	}

	dir := profileDir(ctx.Home, name, profile)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("profile %q was removed from state, but its directory %s could not be deleted: %w", name, dir, err)
	}
	fmt.Fprintf(ctx.Stdout, "removed profile %q\n", name)
	return nil
}

// profileDir prefers the recorded path so profiles installed outside the
// default layout are still removed correctly.
func profileDir(home, name string, profile state.Profile) string {
	if profile.Path != "" {
		return profile.Path
	}
	return filepath.Join(home, "profiles", name)
}
