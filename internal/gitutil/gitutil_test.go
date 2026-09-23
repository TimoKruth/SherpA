package gitutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// fixtureRepo builds a local git repo with one commit on branch main and
// returns its path.
func fixtureRepo(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, "f.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, a := range [][]string{
		{"init", "-b", "main"},
		{"add", "-A"},
		{"-c", "user.email=a@b", "-c", "user.name=a", "commit", "-m", "x"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", d}, a...)...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", a, out)
		}
	}
	return d
}

func TestRunReportsBranchAndErrors(t *testing.T) {
	dest := fixtureRepo(t)
	out, err := Run(dest, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if out != "main" {
		t.Fatalf("branch = %q, want main (output must be trimmed)", out)
	}
	if _, err := Run(dest, "checkout", "-b", "local"); err != nil {
		t.Fatal(err)
	}
	out, _ = Run(dest, "rev-parse", "--abbrev-ref", "HEAD")
	if out != "local" {
		t.Fatalf("after checkout -b local, branch = %q", out)
	}
	if _, err := Run(dest, "definitely-not-a-git-command"); err == nil {
		t.Fatal("Run must return an error for a failing git invocation")
	}
}
