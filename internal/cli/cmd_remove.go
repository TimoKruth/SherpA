package cli

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"

	profilecopy "sherpa/internal/profile"
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

	// State from older installations is read without rewriting its recorded path.
	// Never let a stale or edited path turn profile removal into source deletion.
	dir := profileDir(ctx.Home, name, profile)
	expected, err := filepath.Abs(filepath.Join(ctx.Home, "profiles", name))
	if err != nil {
		return err
	}
	actual, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if !profilecopy.ValidName(name) || actual != expected {
		return fmt.Errorf("refusing to remove a directory outside SherpA's profile storage")
	}
	if info, err := os.Lstat(dir); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to remove a linked profile directory")
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
