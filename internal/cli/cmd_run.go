package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sherpa/internal/harness"
	"sherpa/internal/launch"
	"sherpa/internal/profile"
	"sherpa/internal/state"
)

func init() { register("run", cmdRun) }
func cmdRun(ctx *Ctx, args []string) error {
	fresh := false
	var pass []string
	for _, a := range args {
		if a == "--fresh-setup" {
			fresh = true
		} else {
			pass = append(pass, a)
		}
	}
	st, err := state.Load(ctx.Home)
	if err != nil {
		return err
	}
	p, ok := st.Profiles[st.Active]
	if !ok {
		return fmt.Errorf("no active profile; run sherpa init first")
	}
	return runProfile(ctx, st, p, pass, fresh)
}
func runProfile(ctx *Ctx, st *state.State, p state.Profile, args []string, fresh bool) error {
	h, err := harness.For(p.Harness)
	if err != nil {
		return err
	}
	baseline, err := baselineProfile(st, p.Harness)
	if err != nil {
		return err
	}
	dir := p.Path
	if p.Name == baseline.Name {
		root := filepath.Join(ctx.Home, "sessions")
		if err := os.MkdirAll(root, 0700); err != nil {
			return err
		}
		dir, err = os.MkdirTemp(root, "baseline-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		if err := profile.CopyConfig(p.Path, dir, h, false); err != nil {
			return err
		}
	}
	if !fresh {
		if err := launch.SeedSetup(dir, baseline.Path, h); err != nil {
			return err
		}
		if err := launch.CopyCredentials(h, dir, baseline.Path); err != nil {
			return err
		}
		// Credential export, if needed, targets the launch copy, never the baseline.
		if err := h.PrepareBaselineCredentials(dir); err != nil {
			fmt.Fprintf(ctx.Stderr, "warning: %v; the tool may ask you to log in\n", err)
		}
	}
	source := dir
	if fresh {
		source = filepath.Join(ctx.Home, "no-inherited-credentials")
	}
	return launch.Launch(h, dir, source, args, launch.Stdio{In: ctx.Stdin, Out: ctx.Stdout, Err: ctx.Stderr})
}
