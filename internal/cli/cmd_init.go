package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sherpa/internal/harness"
	"sherpa/internal/profile"
	"sherpa/internal/state"
	"strings"
)

func init() { register("init", cmdInit) }

func configDir(h harness.Harness) string {
	envKey := "SHERPA_" + strings.ToUpper(h.Alias()) + "_DIR"
	if d := os.Getenv(envKey); d != "" {
		return d
	}
	u, _ := os.UserHomeDir()
	return h.DefaultConfigDir(u)
}

func cmdInit(ctx *Ctx, args []string) error {
	refresh := false
	harnessName := harness.Default().Name()
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--refresh":
			refresh = true
		case "--harness":
			i++
			if i >= len(args) || args[i] == "" {
				return fmt.Errorf("--harness requires a name")
			}
			harnessName = args[i]
		default:
			return fmt.Errorf("unknown argument %q", args[i])
		}
	}
	h, err := harness.For(harnessName)
	if err != nil {
		return err
	}
	st, err := state.Load(ctx.Home)
	if err != nil {
		return err
	}
	if st.Baselines == nil {
		st.Baselines = map[string]string{}
	}
	baseline, exists := baselineName(st, h.Name())
	if refresh {
		if !exists {
			return fmt.Errorf("nothing to refresh: run `sherpa init` first")
		}
		dest := filepath.Join(ctx.Home, "profiles", baseline)
		if err := captureSetupState(dest, h); err != nil {
			return err
		}
		fmt.Fprintf(ctx.Stdout, "refreshed setup state for profile %q\n", baseline)
		return nil
	}
	if exists {
		return fmt.Errorf("already initialized for %s (profile %q exists); use --refresh to re-capture setup state", h.Name(), baseline)
	}

	wasFirstBaseline := len(st.Baselines) == 0
	newName := "mine"
	if !wasFirstBaseline {
		newName = "mine-" + h.Alias()
	}
	dest := filepath.Join(ctx.Home, "profiles", newName)
	if err := ensureProfileDirAbsent(dest); err != nil {
		return err
	}

	renamedOld := ""
	renamedNew := ""
	if len(st.Baselines) == 1 {
		for existingHarness, existingBaseline := range st.Baselines {
			if existingBaseline != "mine" {
				break
			}
			existing, err := harness.For(existingHarness)
			if err != nil {
				return err
			}
			renamedOld = "mine"
			renamedNew = "mine-" + existing.Alias()
			if err := renameProfile(ctx.Home, st, renamedOld, renamedNew); err != nil {
				return err
			}
		}
	}

	if err := profile.Import(configDir(h), dest, h.GitignoreContent()); err != nil {
		rollbackInit(ctx.Home, st, dest, renamedOld, renamedNew)
		return err
	}
	if err := captureSetupState(dest, h); err != nil {
		rollbackInit(ctx.Home, st, dest, renamedOld, renamedNew)
		return err
	}
	st.Profiles[newName] = state.Profile{Name: newName, Path: dest, Harness: h.Name()}
	st.Baselines[h.Name()] = newName
	if wasFirstBaseline && st.Active == "" {
		st.Active = newName
	}
	if err := st.Save(ctx.Home); err != nil {
		rollbackInit(ctx.Home, st, dest, renamedOld, renamedNew)
		return err
	}
	fmt.Fprintf(ctx.Stdout, "imported %s as profile %q (your original config is untouched)\n", configDir(h), newName)
	return nil
}

func ensureProfileDirAbsent(dest string) error {
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("profile directory already exists: %s (refusing to overwrite)", dest)
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

func rollbackInit(home string, st *state.State, dest, renamedOld, renamedNew string) {
	_ = os.RemoveAll(dest)
	if renamedOld != "" && renamedNew != "" {
		_ = renameProfile(home, st, renamedNew, renamedOld)
	}
}

// captureSetupState copies each existing harness setup-state source into the
// baseline as the untracked captured blob (0600). Missing source is not an error.
func captureSetupState(mineDir string, h harness.Harness) error {
	for _, src := range h.SetupStateSources(configDir(h)) {
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
