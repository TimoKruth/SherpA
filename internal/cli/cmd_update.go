package cli

import (
	"bufio"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"

	"sherpa/internal/gitutil"
	"sherpa/internal/state"
	"sherpa/internal/update"
)

func init() {
	register("update", cmdUpdate)
}

// cmdUpdate fetches the profile's upstream, shows what the newest published
// version changes (changelog + diffstat), and merges it into `local` after an
// explicit confirmation. The merge itself runs in a temp worktree via
// update.Merge, so a conflicting update leaves the profile untouched.
func cmdUpdate(ctx *Ctx, args []string) error {
	profile, err := updateTarget(ctx, args)
	if err != nil {
		return err
	}
	if profile.Origin == "" {
		return fmt.Errorf("profile %q has no upstream to update from", profile.Name)
	}
	// The success path ends in `reset --hard local`, which would silently
	// discard uncommitted work in the profile. A dirty tree can also be the
	// leftover of an update interrupted between update-ref and reset — that
	// state is indistinguishable from real edits, so explain both ways out
	// instead of guessing.
	status, err := gitutil.Run(profile.Path, "status", "--porcelain")
	if err != nil {
		return err
	}
	if status != "" {
		return fmt.Errorf("profile %q has uncommitted changes.\n"+
			"  - If these are your edits: run `sherpa save`, then update again.\n"+
			"  - If a previous update was interrupted: run `git -C %s reset --hard local` to restore the updated state\n"+
			"    (your committed work is untouched; the backup ref refs/sherpa/backup-* also preserves the pre-update state)",
			profile.Name, profile.Path)
	}

	if _, err := gitutil.Run(profile.Path, "fetch", "--tags", "origin"); err != nil {
		return err
	}
	tags, err := gitutil.Run(profile.Path, "tag", "-l", "v*")
	if err != nil {
		return err
	}
	tag := newestVersionTag(strings.Split(tags, "\n"))
	if tag == "" {
		return fmt.Errorf("upstream of %q has no v<N> version tags", profile.Name)
	}
	if _, err := gitutil.Run(profile.Path, "merge-base", "--is-ancestor", tag, "local"); err == nil {
		fmt.Fprintf(ctx.Stdout, "%s is already up to date with %s\n", profile.Name, tag)
		return nil
	}

	fmt.Fprintf(ctx.Stdout, "Newest upstream version: %s\n\n", tag)
	printChangelog(ctx.Stdout, profile.Path, tag)
	stat, err := gitutil.Run(profile.Path, "diff", "local..."+tag, "--stat")
	if err != nil {
		return err
	}
	fmt.Fprintf(ctx.Stdout, "\n%s\n\n", stat)
	if err := confirm(bufio.NewReader(ctx.Stdin), ctx.Stdout,
		fmt.Sprintf("Update %s to %s? Type yes to continue: ", profile.Name, tag)); err != nil {
		return err
	}

	res, err := update.Merge(profile.Path, filepath.Join(ctx.Home, "tmp"), tag)
	if err != nil {
		return err
	}
	if res.Merged {
		fmt.Fprintf(ctx.Stdout, "updated %s to %s (backup: %s)\n", profile.Name, tag, res.BackupRef)
		return nil
	}
	for _, f := range res.Conflicts {
		fmt.Fprintf(ctx.Stdout, "both you and upstream changed: %s\n", f)
	}
	fmt.Fprintf(ctx.Stdout, "The merge was aborted automatically — your profile and local branch are untouched (nothing to --abort).\n")
	fmt.Fprintf(ctx.Stdout, "To resolve by hand: git -C %s merge %s\n", profile.Path, tag)
	return fmt.Errorf("update aborted: %d conflicting file(s)", len(res.Conflicts))
}

// updateTarget resolves the optional positional profile argument, defaulting
// to the active profile.
func updateTarget(ctx *Ctx, args []string) (state.Profile, error) {
	switch len(args) {
	case 0:
		return activeProfile(ctx)
	case 1:
		name := args[0]
		if strings.HasPrefix(name, "-") {
			return state.Profile{}, fmt.Errorf("usage: sherpa update [<profile>]")
		}
		st, err := state.Load(ctx.Home)
		if err != nil {
			return state.Profile{}, err
		}
		profile, ok := st.Profiles[name]
		if !ok {
			return state.Profile{}, fmt.Errorf("profile %q not found", name)
		}
		if profile.Path == "" {
			return state.Profile{}, fmt.Errorf("profile %q has no path", name)
		}
		return profile, nil
	default:
		return state.Profile{}, fmt.Errorf("usage: sherpa update [<profile>]")
	}
}

// newestVersionTag picks the highest v<N> tag by integer version — v10 beats
// v9 even though it sorts lower lexically. Tags that are not v<integer> are
// ignored.
func newestVersionTag(tags []string) string {
	best, bestN := "", 0
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if !strings.HasPrefix(tag, "v") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimPrefix(tag, "v"))
		if err != nil || n < 1 {
			continue
		}
		if n > bestN {
			best, bestN = tag, n
		}
	}
	return best
}

// printChangelog shows the top section of the CHANGELOG.md shipped at tag, or
// notes its absence. Sections are delimited by "## " headings; a changelog
// without them is printed whole.
func printChangelog(w io.Writer, dir, tag string) {
	body, err := gitutil.Run(dir, "show", tag+":CHANGELOG.md")
	if err != nil {
		fmt.Fprintf(w, "(%s ships no CHANGELOG.md)\n", tag)
		return
	}
	fmt.Fprintln(w, changelogTop(body))
}

func changelogTop(text string) string {
	lines := strings.Split(text, "\n")
	start := -1
	for i, line := range lines {
		if !strings.HasPrefix(line, "## ") {
			continue
		}
		if start == -1 {
			start = i
			continue
		}
		return strings.TrimSpace(strings.Join(lines[start:i], "\n"))
	}
	if start >= 0 {
		return strings.TrimSpace(strings.Join(lines[start:], "\n"))
	}
	return strings.TrimSpace(text)
}
