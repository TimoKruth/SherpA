package cli

import (
	"fmt"
	"strings"
	"time"

	"sherpa/internal/gitutil"
	"sherpa/internal/state"
)

func init() {
	register("save", cmdSave)
	register("diff", cmdDiff)
}

func cmdSave(ctx *Ctx, args []string) error {
	msg, err := parseSaveArgs(args)
	if err != nil {
		return err
	}
	profile, err := activeProfile(ctx)
	if err != nil {
		return err
	}
	if msg == "" {
		msg = "sherpa: save " + time.Now().UTC().Format(time.RFC3339)
	}
	if err := commitAll(profile.Path, msg); err != nil {
		return err
	}
	fmt.Fprintf(ctx.Stdout, "saved %s\n", profile.Name)
	return nil
}

func cmdDiff(ctx *Ctx, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: sherpa diff")
	}
	profile, err := activeProfile(ctx)
	if err != nil {
		return err
	}
	var stat, patch string
	if profile.Origin == "" {
		stat, err = gitutil.Run(profile.Path, "diff", "HEAD", "--stat")
		if err != nil {
			return err
		}
		patch, err = gitutil.Run(profile.Path, "diff", "HEAD")
		if err != nil {
			return err
		}
	} else {
		stat, err = gitutil.Run(profile.Path, "diff", "origin/main...local", "--stat")
		if err != nil {
			return err
		}
		patch, err = gitutil.Run(profile.Path, "diff", "origin/main...local")
		if err != nil {
			return err
		}
	}
	if stat != "" {
		fmt.Fprintln(ctx.Stdout, stat)
	}
	if patch != "" {
		if stat != "" {
			fmt.Fprintln(ctx.Stdout)
		}
		fmt.Fprintln(ctx.Stdout, patch)
	}
	return nil
}

func parseSaveArgs(args []string) (string, error) {
	var msg string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-m":
			i++
			if i >= len(args) {
				return "", fmt.Errorf("-m requires a value")
			}
			msg = args[i]
		case strings.HasPrefix(a, "-m="):
			msg = strings.TrimPrefix(a, "-m=")
		case strings.HasPrefix(a, "-"):
			return "", fmt.Errorf("unknown flag %q", a)
		default:
			return "", fmt.Errorf("unexpected argument %q", a)
		}
	}
	return msg, nil
}

func activeProfile(ctx *Ctx) (state.Profile, error) {
	st, err := state.Load(ctx.Home)
	if err != nil {
		return state.Profile{}, err
	}
	if st.Active == "" {
		return state.Profile{}, fmt.Errorf("no active profile")
	}
	profile, ok := st.Profiles[st.Active]
	if !ok {
		return state.Profile{}, fmt.Errorf("active profile %q not found", st.Active)
	}
	if profile.Path == "" {
		return state.Profile{}, fmt.Errorf("active profile %q has no path", st.Active)
	}
	return profile, nil
}

func commitAll(dir, msg string) error {
	if _, err := gitutil.Run(dir, "add", "-A"); err != nil {
		return err
	}
	_, err := gitutil.Run(dir,
		"-c", "user.email=sherpa@local",
		"-c", "user.name=sherpa",
		"-c", "commit.gpgsign=false",
		"commit", "-m", msg)
	return err
}
