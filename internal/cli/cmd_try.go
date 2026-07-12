package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"sherpa/internal/harness"
	"sherpa/internal/launch"
	"sherpa/internal/review"
	"sherpa/internal/stack"
	"sherpa/internal/state"
)

func init() {
	register("try", cmdTry)
}

type tryRequest struct {
	target string
	name   string
	mode   review.Mode
	fresh  bool
}

func cmdTry(ctx *Ctx, args []string) error {
	req, err := parseTryArgs(args)
	if err != nil {
		return err
	}
	st, err := state.Load(ctx.Home)
	if err != nil {
		return err
	}

	profileName := req.target
	profileDir := ""
	profileHarness := ""
	var manifest *stack.Manifest
	var h harness.Harness
	if p, ok := st.Profiles[req.target]; ok {
		if req.name != "" {
			return fmt.Errorf("--name only applies when cloning a new profile")
		}
		profileName, profileDir = p.Name, p.Path
		profileHarness = p.Harness
		h, err = harness.For(p.Harness)
		if err != nil {
			return err
		}
		manifest, err = readOptionalManifest(profileDir)
		if err != nil {
			return err
		}
	} else {
		cloneURL, err := resolveRegistryRef(req.target)
		if err != nil {
			return err
		}
		installed, err := installStack(ctx, cloneURL, req.name)
		if err != nil {
			return err
		}
		profileName, profileDir, manifest = installed.name, installed.dir, installed.manifest
		profileHarness = manifest.Harness
		h, err = harness.For(manifest.Harness)
		if err != nil {
			return err
		}
		fmt.Fprintf(ctx.Stdout, "cloned %q into %s (not activated)\n", profileName, profileDir)
	}
	baseline, err := baselineProfile(st, profileHarness)
	if err != nil {
		return err
	}

	approved, err := review.RunGate(profileDir, manifest, req.mode, ctx.Stdin, ctx.Stdout)
	if err != nil {
		return err
	}
	if len(approved) > 0 {
		fmt.Fprintf(ctx.Stdout, "approved %d capabilities\n", len(approved))
	}
	if !req.fresh {
		if err := launch.SeedSetup(profileDir, baseline.Path, h); err != nil {
			fmt.Fprintf(ctx.Stderr, "warning: could not seed setup state (%v); tool may onboard\n", err)
		}
	}
	if err := h.PrepareBaselineCredentials(baseline.Path); err != nil {
		fmt.Fprintf(ctx.Stderr, "warning: could not prepare credentials (%v); the tool may ask you to log in\n", err)
	}
	fmt.Fprintf(ctx.Stdout, "trying %q (active profile unchanged)\n", profileName)
	stdio := launch.Stdio{In: ctx.Stdin, Out: ctx.Stdout, Err: ctx.Stderr}
	return launch.Launch(h, profileDir, baseline.Path, nil, stdio)
}

func parseTryArgs(args []string) (tryRequest, error) {
	req := tryRequest{mode: review.KeepQuarantined}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--name":
			i++
			if i >= len(args) {
				return req, fmt.Errorf("--name requires a value")
			}
			req.name = args[i]
		case strings.HasPrefix(a, "--name="):
			req.name = strings.TrimPrefix(a, "--name=")
		case a == "--approve-all":
			req.mode = review.ApproveAll
		case a == "--review":
			i++
			if i >= len(args) {
				return req, fmt.Errorf("--review requires a value")
			}
			mode, err := parseReviewMode(args[i])
			if err != nil {
				return req, err
			}
			req.mode = mode
		case strings.HasPrefix(a, "--review="):
			mode, err := parseReviewMode(strings.TrimPrefix(a, "--review="))
			if err != nil {
				return req, err
			}
			req.mode = mode
		case a == "--fresh-setup":
			req.fresh = true
		case strings.HasPrefix(a, "-"):
			return req, fmt.Errorf("unknown flag %q", a)
		case req.target == "":
			req.target = a
		default:
			return req, fmt.Errorf("unexpected argument %q", a)
		}
	}
	if req.target == "" {
		return req, fmt.Errorf("usage: sherpa try <git-url-or-profile> [--name <name>] [--review interactive|approve-all|keep] [--approve-all] [--fresh-setup]")
	}
	return req, nil
}

func readOptionalManifest(dir string) (*stack.Manifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, "stack.yaml"))
	if os.IsNotExist(err) {
		return &stack.Manifest{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("stack.yaml: %w", err)
	}
	return stack.Parse(b)
}
