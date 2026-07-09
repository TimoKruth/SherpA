package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sherpa/internal/profile"
	"sherpa/internal/state"
)

// gitignoreContent is temporary; Task 5 replaces it with stack.GitignoreContent
// (do not import internal/stack here yet - it doesn't exist until Task 5).
const gitignoreContent = `*
!/.gitignore
!/stack.yaml
!/README.md
!/CHANGELOG.md
!/CLAUDE.md
!/settings.json
!/keybindings.json
!/quarantine.json
!/skills/
!/skills/**
!/agents/
!/agents/**
!/hooks/
!/hooks/**
`

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
	if err := profile.Import(claudeDir(), dest, gitignoreContent); err != nil {
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
