package cli

import (
	"fmt"
	"os"
	"path/filepath"
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
	st, err := state.Load(ctx.Home)
	if err != nil {
		return err
	}
	if _, ok := st.Profiles["mine"]; ok {
		return fmt.Errorf("already initialized (profile 'mine' exists)")
	}
	dest := filepath.Join(ctx.Home, "profiles", "mine")
	if err := profile.Import(claudeDir(), dest, stack.GitignoreContent); err != nil {
		os.RemoveAll(dest)
		return err
	}
	st.Profiles["mine"] = state.Profile{Name: "mine", Path: dest, Harness: "claude-code"}
	st.Active = "mine"
	if err := st.Save(ctx.Home); err != nil {
		return err
	}
	fmt.Fprintf(ctx.Stdout, "imported %s as profile 'mine' (your original config is untouched)\n", claudeDir())
	return nil
}
