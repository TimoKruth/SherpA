// Package update implements the merge half of `sherpa update`: merging an
// upstream release into the profile's `local` branch inside a detached
// temporary worktree, so the profile working tree never sees a half-finished
// merge and the pre-merge state is always pinned by a backup ref first
// (spec §3.3, §6.3).
package update

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"sherpa/internal/gitutil"
)

type MergeResult struct {
	Merged    bool     // true = local now includes targetRef
	Conflicts []string // conflicted file paths (Merged=false)
	BackupRef string   // refs/sherpa/backup-<unix> — always set before any mutation
}

// Merge merges targetRef into the profile's `local` branch. tmpRoot is where
// the temporary worktree is created ($SHERPA_HOME/tmp). On conflict the merge
// is aborted automatically and both the profile working tree and `local` are
// left untouched; the conflicting paths come back in Conflicts with a nil
// error. Every step before the final update-ref/reset pair only adds refs or
// a disposable worktree, so a crash at any point is recoverable by re-running.
func Merge(profileDir, tmpRoot, targetRef string) (MergeResult, error) {
	var res MergeResult
	// Clear worktree registrations whose directories are gone (e.g. a crashed
	// earlier run whose tmp dir was cleaned up out from under git).
	gitutil.Run(profileDir, "worktree", "prune")

	// 1) Pin the pre-merge state before any mutation.
	backup := backupRefName(profileDir)
	if _, err := gitutil.Run(profileDir, "update-ref", backup, "refs/heads/local"); err != nil {
		return res, err
	}
	res.BackupRef = backup

	// 2) Detached temp worktree at local. --detach means `local` is never
	// checked out there, so moving the ref later needs no branch juggling.
	if err := os.MkdirAll(tmpRoot, 0o700); err != nil {
		return res, err
	}
	tmp := filepath.Join(tmpRoot, fmt.Sprintf("update-%d-%d", os.Getpid(), time.Now().UnixNano()))
	if _, err := gitutil.Run(profileDir, "worktree", "add", "--detach", tmp, "local"); err != nil {
		return res, err
	}
	defer func() {
		// Remove in all cases: a leaked registration would linger in .git.
		// If `worktree remove` balks, deleting the directory and pruning
		// clears the registration anyway.
		gitutil.Run(profileDir, "worktree", "remove", "--force", tmp)
		os.RemoveAll(tmp)
		gitutil.Run(profileDir, "worktree", "prune")
	}()

	// 3) Merge inside the temp worktree only.
	if _, err := gitutil.Run(tmp,
		"-c", "user.email=sherpa@local",
		"-c", "user.name=sherpa",
		"-c", "commit.gpgsign=false",
		"merge", "--no-ff", "-m", "sherpa: merge "+targetRef, targetRef); err != nil {
		unmerged, uerr := gitutil.Run(tmp, "diff", "--name-only", "--diff-filter=U")
		if uerr != nil || unmerged == "" {
			return res, err // not a content conflict — surface the git failure
		}
		// 4b) Auto-abort. Only the temp worktree ever held the conflict, so
		// profile and `local` are untouched regardless of the abort outcome.
		gitutil.Run(tmp, "merge", "--abort")
		res.Conflicts = strings.Split(unmerged, "\n")
		return res, nil
	}

	// 4a) Publish: move `local` to the merge commit, then sync the profile
	// working tree. update-ref is plumbing and is permitted on a branch that
	// is checked out in the profile worktree (unlike `git branch -f`); the
	// immediate reset --hard re-syncs the profile to the moved ref.
	head, err := gitutil.Run(tmp, "rev-parse", "HEAD")
	if err != nil {
		return res, err
	}
	if _, err := gitutil.Run(profileDir, "update-ref", "refs/heads/local", head); err != nil {
		return res, err
	}
	if _, err := gitutil.Run(profileDir, "reset", "--hard", "local"); err != nil {
		// `local` already points at the merge; only the working-tree sync is
		// missing (the same state a crash in this window leaves behind).
		return res, fmt.Errorf("update was published to local but syncing the profile working tree failed: %w\n"+
			"run `git -C %s reset --hard local` to finish the update (pre-update state is preserved in %s)",
			err, profileDir, backup)
	}
	res.Merged = true
	return res, nil
}

// backupRefName picks refs/sherpa/backup-<unix-ts>, suffixing -2, -3, … if a
// backup from the same second already exists. Old backups are never deleted
// or overwritten (spec §3.3).
func backupRefName(profileDir string) string {
	base := fmt.Sprintf("refs/sherpa/backup-%d", time.Now().Unix())
	name := base
	for i := 2; refExists(profileDir, name); i++ {
		name = fmt.Sprintf("%s-%d", base, i)
	}
	return name
}

func refExists(dir, ref string) bool {
	_, err := gitutil.Run(dir, "rev-parse", "--verify", "--quiet", ref)
	return err == nil
}
