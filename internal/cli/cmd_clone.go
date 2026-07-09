package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"sherpa/internal/gitutil"
	"sherpa/internal/quarantine"
	"sherpa/internal/review"
	"sherpa/internal/stack"
	"sherpa/internal/state"
)

func init() {
	register("clone", cmdClone)
}

// cmdClone stages a stack in $SHERPA_HOME/staging, validates and quarantines it,
// and only then atomically renames it into profiles/<name>. Every failure path
// removes the staging tree, so an aborted clone leaves no partial profile and no
// state entry (spec §6.2). Cloning never changes the active profile.
func cmdClone(ctx *Ctx, args []string) error {
	req, err := parseCloneArgs(args)
	if err != nil {
		return err
	}
	installed, err := installStack(ctx, req.url, req.name)
	if err != nil {
		return err
	}
	fmt.Fprintf(ctx.Stdout, "cloned %q into %s (not activated)\n", installed.name, installed.dir)
	approved, err := review.RunGate(installed.dir, installed.manifest, req.mode, ctx.Stdin, ctx.Stdout)
	if err != nil {
		return err
	}
	if len(approved) > 0 {
		fmt.Fprintf(ctx.Stdout, "approved %d capabilities\n", len(approved))
	}
	fmt.Fprintf(ctx.Stdout, "activate with: sherpa use %s\n", installed.name)
	return nil
}

type cloneRequest struct {
	url  string
	name string
	mode review.Mode
}

type installResult struct {
	name     string
	dir      string
	manifest *stack.Manifest
}

func installStack(ctx *Ctx, url, name string) (*installResult, error) {
	// Fail fast on an explicit --name collision before touching the network.
	if name != "" && profileExists(ctx.Home, name) {
		return nil, fmt.Errorf("profile %q already exists", name)
	}

	stagingRoot := filepath.Join(ctx.Home, "staging")
	if err := os.MkdirAll(stagingRoot, 0o700); err != nil {
		return nil, err
	}
	// Stage under a throwaway name: the profile name may only be known after the
	// manifest is parsed, and os.Rename is the single commit point below.
	staging, err := os.MkdirTemp(stagingRoot, "clone-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(staging) // no-op once the tree is renamed into place.

	if err := gitutil.Clone(url, staging); err != nil {
		return nil, err
	}

	b, err := os.ReadFile(filepath.Join(staging, "stack.yaml"))
	if err != nil {
		return nil, fmt.Errorf("stack.yaml: %w", err)
	}
	m, err := stack.Parse(b)
	if err != nil {
		return nil, err
	}
	if name == "" {
		name = m.Name
	}
	// Collision check for the manifest-derived default name (the explicit --name
	// case was already checked before the clone).
	if profileExists(ctx.Home, name) {
		return nil, fmt.Errorf("profile %q already exists", name)
	}

	// Quarantine BEFORE validating: an author ships settings.json with live
	// hooks/mcpServers/permissions, and Validate treats live capabilities as a
	// violation. Strip only relocates JSON keys into quarantine.json — it runs no
	// stack code — so validating the stripped tree still catches undeclared
	// executables while letting a well-formed stack install fully quarantined.
	if err := quarantine.Strip(staging); err != nil {
		return nil, err
	}
	if violations := m.Validate(staging); len(violations) > 0 {
		return nil, fmt.Errorf("stack failed validation:\n  - %s", strings.Join(violations, "\n  - "))
	}
	if err := enforceGitignore(staging); err != nil {
		return nil, err
	}

	// Branch `local` sits at the upstream head; origin/main and tags stay
	// referenceable for later diff/update. No tracking config is set.
	if _, err := gitutil.Run(staging, "checkout", "-b", "local"); err != nil {
		return nil, err
	}
	// Commit the quarantine onto local so Task 13's `git reset --hard local`
	// cannot resurrect the stripped hooks/permissions. Skipped when the stack
	// shipped already-quarantined (clean tree).
	if err := commitQuarantine(staging); err != nil {
		return nil, err
	}

	// Build the full state entry BEFORE the rename so the only fallible step left
	// after the commit point is Save (which rolls back on failure).
	st, err := state.Load(ctx.Home)
	if err != nil {
		return nil, err
	}
	final := filepath.Join(ctx.Home, "profiles", name)
	if err := os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
		return nil, err
	}
	st.Profiles[name] = state.Profile{Name: name, Path: final, Origin: url, Harness: m.Harness}

	// Commit point: the atomic rename makes the profile appear fully-formed or not
	// at all. If the subsequent Save fails, remove the just-installed tree so the
	// clone leaves no trace (spec §6.2).
	if err := os.Rename(staging, final); err != nil {
		return nil, err
	}
	if err := st.Save(ctx.Home); err != nil {
		os.RemoveAll(final)
		return nil, err
	}

	return &installResult{name: name, dir: final, manifest: m}, nil
}

// parseCloneArgs pulls the git URL, optional --name, and review mode out of the
// argument list. The default is keep-quarantined; callers opt into prompting
// with --review=interactive or into the shortcut with --approve-all.
func parseCloneArgs(args []string) (cloneRequest, error) {
	req := cloneRequest{mode: review.KeepQuarantined}
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
		case strings.HasPrefix(a, "-"):
			return req, fmt.Errorf("unknown flag %q", a)
		case req.url == "":
			req.url = a
		default:
			return req, fmt.Errorf("unexpected argument %q", a)
		}
	}
	if req.url == "" {
		return req, fmt.Errorf("usage: sherpa clone <git-url> [--name <name>] [--review interactive|approve-all|keep] [--approve-all]")
	}
	return req, nil
}

func parseReviewMode(s string) (review.Mode, error) {
	switch s {
	case "interactive":
		return review.Interactive, nil
	case "approve-all":
		return review.ApproveAll, nil
	case "keep", "keep-quarantined":
		return review.KeepQuarantined, nil
	default:
		return review.KeepQuarantined, fmt.Errorf("unknown review mode %q", s)
	}
}

// profileExists reports whether a profile with this name is already installed,
// checking both the on-disk directory and the state file.
func profileExists(home, name string) bool {
	if _, err := os.Stat(filepath.Join(home, "profiles", name)); err == nil {
		return true
	}
	if st, err := state.Load(home); err == nil {
		if _, ok := st.Profiles[name]; ok {
			return true
		}
	}
	return false
}

// commitQuarantine records the stripped settings.json, quarantine.json, and
// enforced .gitignore as a single sherpa commit on the current (local) branch,
// so a later hard reset to local keeps the stack quarantined. It is a no-op when
// the working tree is clean (the stack shipped pre-quarantined).
func commitQuarantine(dir string) error {
	status, err := gitutil.Run(dir, "status", "--porcelain")
	if err != nil {
		return err
	}
	if status == "" {
		return nil
	}
	if _, err := gitutil.Run(dir, "add", "-A"); err != nil {
		return err
	}
	_, err = gitutil.Run(dir,
		"-c", "user.email=sherpa@local", "-c", "user.name=sherpa",
		"-c", "commit.gpgsign=false",
		"commit", "-m", "sherpa: install (quarantine applied)")
	return err
}

// enforceGitignore guarantees the installed stack carries the whitelist-style
// .gitignore so untracked runtime state can never be committed. A stack that
// already ships the whitelist form is left untouched.
func enforceGitignore(dir string) error {
	p := filepath.Join(dir, ".gitignore")
	if b, err := os.ReadFile(p); err == nil {
		s := string(b)
		if strings.HasPrefix(strings.TrimSpace(s), "*") && strings.Contains(s, "!/stack.yaml") {
			return nil
		}
	}
	return os.WriteFile(p, []byte(stack.GitignoreContent), 0o644)
}
