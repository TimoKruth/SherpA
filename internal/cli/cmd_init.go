package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sherpa/internal/harness"
	"sherpa/internal/profile"
	"sherpa/internal/stack"
	"sherpa/internal/state"
)

func init() { register("init", cmdInit) }

func claudeDir() string {
	if d := os.Getenv("SHERPA_CLAUDE_DIR"); d != "" {
		return d
	}
	u, _ := os.UserHomeDir()
	return filepath.Join(u, ".claude")
}

func cmdInit(ctx *Ctx, args []string) error {
	refresh := false
	for _, a := range args {
		if a == "--refresh" {
			refresh = true
		} else {
			return fmt.Errorf("unknown argument %q", a)
		}
	}
	st, err := state.Load(ctx.Home)
	if err != nil {
		return err
	}
	dest := filepath.Join(ctx.Home, "profiles", "mine")
	_, exists := st.Profiles["mine"]
	if refresh {
		if !exists {
			return fmt.Errorf("nothing to refresh: run `sherpa init` first")
		}
		if err := captureSetupState(dest); err != nil {
			return err
		}
		fmt.Fprintln(ctx.Stdout, "refreshed setup state for profile 'mine'")
		return nil
	}
	if exists {
		return fmt.Errorf("already initialized (profile 'mine' exists); use --refresh to re-capture setup state")
	}
	if err := profile.Import(claudeDir(), dest, stack.GitignoreContent); err != nil {
		os.RemoveAll(dest)
		return err
	}
	if err := captureSetupState(dest); err != nil {
		os.RemoveAll(dest)
		return err
	}
	st.Profiles["mine"] = state.Profile{Name: "mine", Path: dest, Harness: "claude-code"}
	st.Active = "mine"
	if err := st.Save(ctx.Home); err != nil {
		os.RemoveAll(dest)
		return err
	}
	fmt.Fprintf(ctx.Stdout, "imported %s as profile 'mine' (your original config is untouched)\n", claudeDir())
	return nil
}

// captureSetupState copies each existing harness setup-state source into mine as
// the untracked captured blob (0600). Missing source is not an error.
func captureSetupState(mineDir string) error {
	h := harness.Default()
	for _, src := range h.SetupStateSources(claudeDir()) {
		b, err := os.ReadFile(src)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(mineDir, h.CapturedName()), b, 0o600); err != nil {
			return err
		}
	}
	return nil
}
