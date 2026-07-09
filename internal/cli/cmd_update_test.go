package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// publishUpstreamV2 commits changes in the origin repo and tags them v2, the
// way `sherpa publish` would on the expert's side.
func publishUpstreamV2(t *testing.T, repo string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitOut(t, repo, "add", "-A")
	gitOut(t, repo, "-c", "user.email=j@x", "-c", "user.name=j", "commit", "-m", "v2")
	gitOut(t, repo, "tag", "v2")
}

func cloneForUpdate(t *testing.T, repo, name string) string {
	t.Helper()
	var out, errb bytes.Buffer
	if code := Run([]string{"clone", repo, "--name", name}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	return filepath.Join(os.Getenv("SHERPA_HOME"), "profiles", name)
}

func TestUpdateCleanMergeBringsInUpstreamV2(t *testing.T) {
	home := setupHome(t)
	repo := makeExpertRepo(t, true)
	dir := cloneForUpdate(t, repo, "up-test")

	// A saved local customization that must survive the update.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("my local note\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := commitAll(dir, "local note"); err != nil {
		t.Fatal(err)
	}
	publishUpstreamV2(t, repo, map[string]string{
		"NEWFILE.md":   "upstream addition\n",
		"CHANGELOG.md": "# Changelog\n\n## v2\n\n- adds NEWFILE\n\n## v1\n\n- initial release\n",
		"stack.yaml":   "name: jane-stack\nowner: \"@jane\"\nversion: 2\nharness: claude-code\nsummary: x\nforked_from: '@origin/base@v1'\n",
	})

	var out, errb bytes.Buffer
	ctx := &Ctx{Home: home, Stdout: &out, Stderr: &errb, Stdin: strings.NewReader("yes\n")}
	if err := cmdUpdate(ctx, []string{"up-test"}); err != nil {
		t.Fatalf("update failed: %v\n%s", err, out.String())
	}
	s := out.String()
	// Review step: top changelog section and diffstat were shown pre-confirm.
	if !strings.Contains(s, "adds NEWFILE") {
		t.Fatalf("changelog v2 section not shown:\n%s", s)
	}
	if strings.Contains(s, "initial release") {
		t.Fatalf("older changelog section leaked into review output:\n%s", s)
	}
	if !strings.Contains(s, "NEWFILE.md") {
		t.Fatalf("diff --stat not shown:\n%s", s)
	}
	// The merge landed: upstream file present, local note preserved, merge commit on local.
	if b, err := os.ReadFile(filepath.Join(dir, "NEWFILE.md")); err != nil || string(b) != "upstream addition\n" {
		t.Fatalf("NEWFILE.md = %q, %v", b, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "README.md")); string(b) != "my local note\n" {
		t.Fatalf("local note lost: %q", b)
	}
	if merges := gitOut(t, dir, "log", "--merges", "--format=%s", "local"); merges == "" {
		t.Fatal("no merge commit on local")
	}
	gitOut(t, dir, "merge-base", "--is-ancestor", "v2", "local")

	// Second run: already up to date, no confirmation needed.
	out.Reset()
	ctx2 := &Ctx{Home: home, Stdout: &out, Stderr: &errb, Stdin: strings.NewReader("")}
	if err := cmdUpdate(ctx2, []string{"up-test"}); err != nil {
		t.Fatalf("up-to-date run failed: %v", err)
	}
	if !strings.Contains(out.String(), "up to date") {
		t.Fatalf("up-to-date output = %q", out.String())
	}
}

func TestUpdateConflictAbortsAndExplains(t *testing.T) {
	home := setupHome(t)
	repo := makeExpertRepo(t, true)
	dir := cloneForUpdate(t, repo, "conflict-test")

	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("# jane local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := commitAll(dir, "local claude edit"); err != nil {
		t.Fatal(err)
	}
	publishUpstreamV2(t, repo, map[string]string{"CLAUDE.md": "# jane upstream\n"})
	before := gitOut(t, dir, "rev-parse", "local")

	var out, errb bytes.Buffer
	ctx := &Ctx{Home: home, Stdout: &out, Stderr: &errb, Stdin: strings.NewReader("yes\n")}
	if err := cmdUpdate(ctx, []string{"conflict-test"}); err == nil {
		t.Fatal("conflicting update must fail")
	}
	s := out.String()
	if !strings.Contains(s, "both you and upstream changed: CLAUDE.md") {
		t.Fatalf("missing plain-language conflict list:\n%s", s)
	}
	if !strings.Contains(s, "untouched") {
		t.Fatalf("output must state profile and local are untouched:\n%s", s)
	}
	if !strings.Contains(s, "git -C "+dir+" merge v2") {
		t.Fatalf("missing manual-resolution hint:\n%s", s)
	}
	if got := gitOut(t, dir, "rev-parse", "local"); got != before {
		t.Fatalf("local moved by aborted update: %s -> %s", before, got)
	}
	if status := gitOut(t, dir, "status", "--porcelain"); status != "" {
		t.Fatalf("profile dirty after aborted update:\n%s", status)
	}
}

func TestUpdateRefusesProfileWithoutUpstream(t *testing.T) {
	home := setupHome(t) // active profile "mine" has no Origin
	var out, errb bytes.Buffer
	ctx := &Ctx{Home: home, Stdout: &out, Stderr: &errb, Stdin: strings.NewReader("")}
	err := cmdUpdate(ctx, nil)
	if err == nil || !strings.Contains(err.Error(), "upstream") {
		t.Fatalf("err = %v, want refusal for missing upstream", err)
	}
}

func TestUpdateRefusesDirtyProfile(t *testing.T) {
	home := setupHome(t)
	repo := makeExpertRepo(t, true)
	dir := cloneForUpdate(t, repo, "dirty-test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("unsaved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	ctx := &Ctx{Home: home, Stdout: &out, Stderr: &errb, Stdin: strings.NewReader("yes\n")}
	err := cmdUpdate(ctx, []string{"dirty-test"})
	if err == nil {
		t.Fatal("dirty profile must refuse to update")
	}
	// The refusal must explain both cases: genuine edits (save them) and an
	// interrupted previous update (reset to the already-updated local).
	if !strings.Contains(err.Error(), "sherpa save") {
		t.Fatalf("err = %v, want the sherpa save hint", err)
	}
	if !strings.Contains(err.Error(), "git -C "+dir+" reset --hard local") {
		t.Fatalf("err = %v, want the interrupted-update recovery hint", err)
	}
	if !strings.Contains(err.Error(), "refs/sherpa/backup-") {
		t.Fatalf("err = %v, want mention of the backup ref", err)
	}
}

func TestNewestVersionTagOrdersNumerically(t *testing.T) {
	got := newestVersionTag([]string{"v9", "v10", "v2", "vibes", "v1.2", ""})
	if got != "v10" {
		t.Fatalf("newestVersionTag = %q, want v10 (numeric, not lexical)", got)
	}
	if got := newestVersionTag(nil); got != "" {
		t.Fatalf("newestVersionTag(nil) = %q, want empty", got)
	}
}
