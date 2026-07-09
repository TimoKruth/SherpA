package update

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// sherpaIdentity mirrors the -c flags every sherpa-created commit uses.
var sherpaIdentity = []string{
	"-c", "user.email=sherpa@local",
	"-c", "user.name=sherpa",
	"-c", "commit.gpgsign=false",
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commitAll(t *testing.T, dir, msg string) {
	t.Helper()
	git(t, dir, "add", "-A")
	git(t, dir, append(sherpaIdentity, "commit", "-m", msg)...)
}

// makeFixture builds the installed-profile topology from Task 5: an upstream
// repo tagged v1, cloned into a profile that sits on branch `local` one
// sherpa commit ahead of origin/main, plus one user save commit (localEdit).
// Upstream then publishes v2 (upstreamEdit) and the profile fetches it.
func makeFixture(t *testing.T, localEdit, upstreamEdit func(dir string)) (profile, tmpRoot string) {
	t.Helper()
	upstream := t.TempDir()
	git(t, upstream, "init", "-b", "main")
	write(t, upstream, "CLAUDE.md", "# stack\n")
	write(t, upstream, "notes.md", "notes\n")
	commitAll(t, upstream, "v1")
	git(t, upstream, "tag", "v1")

	profile = filepath.Join(t.TempDir(), "profile")
	if out, err := exec.Command("git", "clone", upstream, profile).CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v: %s", err, out)
	}
	git(t, profile, "checkout", "-b", "local")
	write(t, profile, "quarantine.json", "{}\n")
	commitAll(t, profile, "sherpa: install (quarantine applied)")
	localEdit(profile)
	commitAll(t, profile, "local change")

	upstreamEdit(upstream)
	commitAll(t, upstream, "upstream v2")
	git(t, upstream, "tag", "v2")
	git(t, profile, "fetch", "--tags", "origin")

	return profile, filepath.Join(t.TempDir(), "tmp")
}

func worktreeCount(t *testing.T, dir string) int {
	t.Helper()
	return strings.Count(git(t, dir, "worktree", "list", "--porcelain"), "worktree ")
}

func TestMergeCleanAppliesBothSides(t *testing.T) {
	profile, tmpRoot := makeFixture(t,
		func(d string) { write(t, d, "notes.md", "notes\nlocal line\n") },
		func(d string) { write(t, d, "upstream.md", "from upstream\n") },
	)
	before := git(t, profile, "rev-parse", "refs/heads/local")

	res, err := Merge(profile, tmpRoot, "v2")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Merged || len(res.Conflicts) != 0 {
		t.Fatalf("result = %+v, want clean merge", res)
	}
	if !strings.HasPrefix(res.BackupRef, "refs/sherpa/backup-") {
		t.Fatalf("BackupRef = %q", res.BackupRef)
	}
	if got := git(t, profile, "rev-parse", res.BackupRef); got != before {
		t.Fatalf("backup ref points at %s, want pre-merge local %s", got, before)
	}
	// Working tree carries both sides of the merge.
	b, err := os.ReadFile(filepath.Join(profile, "upstream.md"))
	if err != nil || string(b) != "from upstream\n" {
		t.Fatalf("upstream.md = %q, %v", b, err)
	}
	if b, _ := os.ReadFile(filepath.Join(profile, "notes.md")); !strings.Contains(string(b), "local line") {
		t.Fatalf("local change lost: notes.md = %q", b)
	}
	// local now contains v2, working tree is clean, and no worktree leaked.
	git(t, profile, "merge-base", "--is-ancestor", "v2", "local")
	if status := git(t, profile, "status", "--porcelain"); status != "" {
		t.Fatalf("profile dirty after merge:\n%s", status)
	}
	if n := worktreeCount(t, profile); n != 1 {
		t.Fatalf("worktree count = %d, want 1 (temp worktree leaked)", n)
	}
}

func TestMergeConflictAbortsUntouched(t *testing.T) {
	profile, tmpRoot := makeFixture(t,
		func(d string) { write(t, d, "CLAUDE.md", "# stack local\n") },
		func(d string) { write(t, d, "CLAUDE.md", "# stack upstream\n") },
	)
	before := git(t, profile, "rev-parse", "refs/heads/local")

	res, err := Merge(profile, tmpRoot, "v2")
	if err != nil {
		t.Fatal(err)
	}
	if res.Merged {
		t.Fatal("conflicting merge reported Merged=true")
	}
	if len(res.Conflicts) != 1 || res.Conflicts[0] != "CLAUDE.md" {
		t.Fatalf("Conflicts = %v, want [CLAUDE.md]", res.Conflicts)
	}
	// local is byte-identical and the profile working tree untouched (spec §6.3).
	if got := git(t, profile, "rev-parse", "refs/heads/local"); got != before {
		t.Fatalf("local moved: %s -> %s", before, got)
	}
	if status := git(t, profile, "status", "--porcelain"); status != "" {
		t.Fatalf("profile dirty after aborted merge:\n%s", status)
	}
	if b, _ := os.ReadFile(filepath.Join(profile, "CLAUDE.md")); string(b) != "# stack local\n" {
		t.Fatalf("CLAUDE.md touched by aborted merge: %q", b)
	}
	// Backup ref was still taken before any mutation, and no worktree leaked.
	if got := git(t, profile, "rev-parse", res.BackupRef); got != before {
		t.Fatalf("backup ref points at %s, want %s", got, before)
	}
	if n := worktreeCount(t, profile); n != 1 {
		t.Fatalf("worktree count = %d, want 1 (temp worktree leaked)", n)
	}
}

// Kill-safety: a crash in the window between `update-ref refs/heads/local`
// and `reset --hard` leaves the ref moved but the working tree stale and a
// temp worktree leaked. Re-running Merge must converge without a duplicate
// merge commit.
func TestMergeConvergesAfterCrashBetweenUpdateRefAndReset(t *testing.T) {
	profile, tmpRoot := makeFixture(t,
		func(d string) { write(t, d, "notes.md", "notes\nlocal line\n") },
		func(d string) { write(t, d, "upstream.md", "from upstream\n") },
	)

	// First half of the algorithm by hand, stopping before reset/cleanup.
	git(t, profile, "update-ref", "refs/sherpa/backup-crash", "refs/heads/local")
	crashed := filepath.Join(t.TempDir(), "crashed-wt")
	git(t, profile, "worktree", "add", "--detach", crashed, "local")
	git(t, crashed, append(sherpaIdentity, "merge", "--no-ff", "-m", "sherpa: merge v2", "v2")...)
	merged := git(t, crashed, "rev-parse", "HEAD")
	git(t, profile, "update-ref", "refs/heads/local", merged)
	// "Crash": no reset --hard, worktree left registered and on disk.
	if _, err := os.Stat(filepath.Join(profile, "upstream.md")); err == nil {
		t.Fatal("fixture broken: working tree already synced")
	}

	res, err := Merge(profile, tmpRoot, "v2")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Merged {
		t.Fatalf("recovery run: result = %+v", res)
	}
	if got := git(t, profile, "rev-parse", "refs/heads/local"); got != merged {
		t.Fatalf("recovery created a divergent commit: %s, want %s", got, merged)
	}
	if _, err := os.Stat(filepath.Join(profile, "upstream.md")); err != nil {
		t.Fatal("working tree not synced to merged local")
	}
	if status := git(t, profile, "status", "--porcelain"); status != "" {
		t.Fatalf("profile dirty after recovery:\n%s", status)
	}
	// Only the profile itself plus the deliberately leaked crash worktree
	// remain: the recovery run cleaned up its own temp worktree.
	if n := worktreeCount(t, profile); n != 2 {
		t.Fatalf("worktree count = %d, want 2", n)
	}
}
