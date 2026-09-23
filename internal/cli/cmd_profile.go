package cli

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sherpa/internal/harness"
	"sherpa/internal/profile"
	"sherpa/internal/review"
	"sherpa/internal/stack"
	"sherpa/internal/state"
	"strings"
)

func init() { register("profile", cmdProfile) }
func cmdProfile(ctx *Ctx, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: sherpa profile create <name> --from <profile> [--instructions <text>] | import <name> --path <config-dir> --harness <name> --trusted | show <name> | setup <name>")
	}
	action, name := args[0], args[1]
	st, err := state.Load(ctx.Home)
	if err != nil {
		return err
	}
	if action == "review" {
		p, ok := st.Profiles[name]
		if !ok {
			return fmt.Errorf("unknown profile %q", name)
		}
		mode := review.KeepQuarantined
		if len(args) == 3 && args[2] == "--approve-all" {
			mode = review.ApproveAll
		} else if len(args) != 2 {
			return fmt.Errorf("usage: sherpa profile review <name> [--approve-all]")
		}
		if mode == review.ApproveAll && st.Baselines[p.Harness] == name {
			return fmt.Errorf("protected baseline; create a variant before approving capabilities")
		}
		h, err := harness.For(p.Harness)
		if err != nil {
			return err
		}
		if err := printSetupReview(ctx, p.Path, h); err != nil {
			return err
		}
		var manifest *stack.Manifest
		if b, err := os.ReadFile(filepath.Join(p.Path, "stack.yaml")); err == nil {
			manifest, err = stack.Parse(b)
			if err != nil {
				return err
			}
		}
		_, err = review.RunGate(p.Path, manifest, mode, ctx.Stdin, ctx.Stdout)
		return err
	}
	if action == "show" || action == "setup" {
		if len(args) != 2 {
			return fmt.Errorf("unexpected arguments")
		}
		p, ok := st.Profiles[name]
		if !ok {
			return fmt.Errorf("unknown profile %q", name)
		}
		if action == "show" {
			fmt.Fprintf(ctx.Stdout, "%s (%s)\n%s\n", p.Name, p.Harness, p.Path)
			return nil
		}
		if st.Baselines[p.Harness] == name {
			return fmt.Errorf("protected baseline; create an experimental profile before changing setup")
		}
		h, err := harness.For(p.Harness)
		if err != nil {
			return err
		}
		if _, err := baselineProfile(st, p.Harness); err != nil {
			return err
		}
		for _, rel := range append(h.CredentialFiles(), ".claude.json") {
			if err := os.Remove(filepath.Join(p.Path, rel)); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		return runProfile(ctx, st, p, nil, true)
	}
	if action != "create" && action != "import" {
		return fmt.Errorf("unknown profile action %q", action)
	}
	if !profile.ValidName(name) || name == "mine" || strings.HasPrefix(name, "mine-") {
		return fmt.Errorf("choose a name of 1–64 letters, digits, hyphens or underscores; mine and mine-* are reserved")
	}
	if _, ok := st.Profiles[name]; ok {
		return fmt.Errorf("profile %q already exists", name)
	}
	fs := flag.NewFlagSet("profile "+action, flag.ContinueOnError)
	fs.SetOutput(ctx.Stderr)
	from := fs.String("from", st.Active, "source profile")
	source := fs.String("path", "", "local config directory")
	harnessName := fs.String("harness", "", "claude-code or codex")
	trusted := fs.Bool("trusted", false, "I reviewed and trust this setup, including hooks, MCP servers and permissions")
	instructions := fs.String("instructions", "", "additional instructions for this variant")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments")
	}
	var h harness.Harness
	if action == "create" {
		if *source != "" || *harnessName != "" || *trusted {
			return fmt.Errorf("create uses --from; use import for a directory")
		}
		p, ok := st.Profiles[*from]
		if !ok {
			return fmt.Errorf("unknown source profile %q", *from)
		}
		*source = p.Path
		h, err = harness.For(p.Harness)
	} else {
		if *source == "" || *harnessName == "" {
			return fmt.Errorf("import requires --path and --harness")
		}
		h, err = resolveHarness(*harnessName)
		if err != nil {
			return err
		}
		fmt.Fprintf(ctx.Stdout, "Import review: %s setup from %s\nConfiguration includes instructions, skills, hooks, MCP commands and permission settings. These may execute code with your account when launched. Review the source directory before trusting it. SherpA copies configuration; it is not an OS sandbox.\n", h.Name(), *source)
		if err := printSetupReview(ctx, *source, h); err != nil {
			return err
		}
		if !*trusted {
			return fmt.Errorf("review the setup, then repeat with --trusted to import it")
		}
	}
	if err != nil {
		return err
	}
	if _, err := baselineProfile(st, h.Name()); err != nil {
		return err
	}
	dest := filepath.Join(ctx.Home, "profiles", name)
	if _, err := os.Lstat(dest); !os.IsNotExist(err) {
		return fmt.Errorf("profile directory already exists or is inaccessible")
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(filepath.Dir(dest), ".import-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err := profile.CopyConfig(*source, stage, h, false); err != nil {
		return err
	}
	if *instructions != "" {
		file := "CLAUDE.md"
		if h.Name() == "codex" {
			file = "AGENTS.md"
		}
		path := filepath.Join(stage, file)
		b, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		b = append(b, []byte("\n\n"+*instructions+"\n")...)
		if err := os.WriteFile(path, b, 0600); err != nil {
			return err
		}
	}
	if err := profile.InitRepository(stage, h.GitignoreContent()); err != nil {
		return err
	}
	if err := os.Rename(stage, dest); err != nil {
		return err
	}
	st.Profiles[name] = state.Profile{Name: name, Path: dest, Harness: h.Name()}
	if err := st.Save(ctx.Home); err != nil {
		os.RemoveAll(dest)
		return err
	}
	fmt.Fprintf(ctx.Stdout, "created %q (%s) at %s; active profile unchanged\n", name, h.Name(), dest)
	return nil
}

func printSetupReview(ctx *Ctx, dir string, h harness.Harness) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("setup source must be a directory")
	}
	fmt.Fprintf(ctx.Stdout, "Review %s configuration at %s (including referenced scripts and servers):\n", h.Name(), dir)
	for _, rel := range h.AllowedPaths() {
		path := filepath.Join(dir, rel)
		info, err := os.Stat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(ctx.Stdout, "  %s\n", rel)
		if !info.IsDir() && (rel == "settings.json" || rel == "config.toml" || rel == "CLAUDE.md" || rel == "AGENTS.md") {
			if info.Size() > 64<<10 {
				fmt.Fprintln(ctx.Stdout, "    (large file: inspect in your editor)")
				continue
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			fmt.Fprintln(ctx.Stdout, terminalLines(string(b), 64<<10))
		}
	}
	return nil
}
