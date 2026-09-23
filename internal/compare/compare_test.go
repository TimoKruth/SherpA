package compare

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sherpa/internal/state"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0600); err != nil {
		t.Fatal(err)
	}
}
func fixture(t *testing.T) (string, string) {
	t.Helper()
	home := t.TempDir()
	project := t.TempDir()
	write(t, filepath.Join(project, "code.txt"), "original\n")
	write(t, filepath.Join(project, ".gitignore"), "ignored.txt\n")
	if err := initProject(project); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(project, "code.txt"), "dirty source\n")
	write(t, filepath.Join(project, "new.txt"), "untracked\n")
	write(t, filepath.Join(project, "ignored.txt"), "ignored\n")
	st, _ := state.Load(home)
	st.Active = "mine"
	st.Baselines["claude-code"] = "mine"
	st.Baselines["codex"] = "mine-codex"
	for name, h := range map[string]string{"mine": "claude-code", "mine-codex": "codex"} {
		dir := filepath.Join(home, "profiles", name)
		write(t, filepath.Join(dir, "CLAUDE.md"), name)
		write(t, filepath.Join(dir, "AGENTS.md"), name)
		write(t, filepath.Join(dir, ".credentials.json"), "original credential")
		write(t, filepath.Join(dir, "auth.json"), "original credential")
		write(t, filepath.Join(dir, "history.jsonl"), "previous conversation")
		st.Profiles[name] = state.Profile{Name: name, Path: dir, Harness: h}
	}
	if err := st.Save(home); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then echo 'fixture 1.0'; exit 0; fi
config="${CLAUDE_CONFIG_DIR:-$CODEX_HOME}"
test ! -f "$config/history.jsonl" || exit 8
test ! -f ignored.txt || exit 9
test "$(cat code.txt)" = "dirty source" || exit 10
test "$(cat new.txt)" = "untracked" || exit 11
cat
printf '\nresponse from %s\n' "$1"
printf 'changed\n' > code.txt
printf 'new file\n' > result.txt
printf 'session changed config\n' > "$config/CLAUDE.md"
printf 'rotated\n' > "$config/auth.json"
`
	bin := filepath.Join(t.TempDir(), "agent")
	write(t, bin, script)
	if err := os.Chmod(bin, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHERPA_CLAUDE_BIN", bin)
	t.Setenv("SHERPA_CODEX_BIN", bin)
	t.Setenv("SHERPA_SECURITY_BIN", "false")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	return home, project
}
func TestComparisonFreezesInputsIsolatesRunsAndExportsRatings(t *testing.T) {
	home, project := fixture(t)
	c, err := Prepare(home, Request{Project: project, Prompt: "<script>alert('prompt')</script>", Profiles: []string{"mine", "mine-codex"}})
	if err != nil {
		t.Fatal(err)
	}
	// Even source edits made after preparation cannot change the starting point.
	write(t, filepath.Join(project, "code.txt"), "source changed after prepare\n")
	if err := Execute(context.Background(), home, c); err != nil {
		t.Fatal(err)
	}
	if c.Status != "completed" {
		t.Fatalf("%+v", c.Results)
	}
	for i, r := range c.Results {
		if !strings.Contains(r.Output, "<script>") || !strings.Contains(r.Diff, "+changed") || !strings.Contains(r.Diff, "+new file") || r.ToolVersion != "fixture 1.0" || r.ConfigHash == "" {
			t.Fatalf("missing result evidence: %+v", r)
		}
		dir, _ := directory(home, c.ID)
		if _, err := os.Stat(filepath.Join(dir, "run-"+string(rune('0'+i)), "config")); !os.IsNotExist(err) {
			t.Fatal("runtime credentials retained")
		}
	}
	for _, name := range []string{"mine", "mine-codex"} {
		b, _ := os.ReadFile(filepath.Join(home, "profiles", name, "auth.json"))
		if string(b) != "original credential" {
			t.Fatal("baseline credentials changed")
		}
	}
	b, _ := os.ReadFile(filepath.Join(project, "code.txt"))
	if string(b) != "source changed after prepare\n" {
		t.Fatal("source project changed")
	}
	st, _ := state.Load(home)
	if st.Active != "mine" {
		t.Fatal("active profile changed")
	}
	if err := Rate(home, c.ID, "mine", 5, "<script>notes</script>"); err != nil {
		t.Fatal(err)
	}
	saved, err := Load(home, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Results[0].Rating != 5 {
		t.Fatal("rating lost")
	}
	var html bytes.Buffer
	if err := WriteReport(&html, saved); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(html.String(), "<script>") || !strings.Contains(html.String(), "&lt;script&gt;") {
		t.Fatal("report HTML not escaped")
	}
	list, err := List(home)
	if err != nil || len(list) != 1 || list[0].Results[0].Output != "" {
		t.Fatalf("list %v %v", list, err)
	}
}
func TestFailureContinuesAndTimeoutKeepsResults(t *testing.T) {
	home, project := fixture(t)
	bin := filepath.Join(t.TempDir(), "fail")
	write(t, bin, "#!/bin/sh\nif [ \"$1\" = --version ]; then echo fixture; exit; fi\nprintf partial\nexit 7\n")
	os.Chmod(bin, 0700)
	t.Setenv("SHERPA_CLAUDE_BIN", bin)
	c, err := Prepare(home, Request{Project: project, Prompt: "task", Profiles: []string{"mine", "mine-codex"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := Execute(context.Background(), home, c); err != nil {
		t.Fatal(err)
	}
	if c.Status != "completed_with_errors" || c.Results[0].ExitCode != 7 || c.Results[0].Output != "partial" || c.Results[1].Status != "completed" {
		t.Fatalf("%+v", c)
	}
	write(t, bin, "#!/bin/sh\nif [ \"$1\" = --version ]; then echo fixture; exit; fi\nprintf before-timeout\nsleep 30\n")
	c, err = Prepare(home, Request{Project: project, Prompt: "task", Profiles: []string{"mine", "mine-codex"}, TimeoutSeconds: 1})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := Execute(context.Background(), home, c); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 8*time.Second || c.Results[0].Status != "timed_out" || !strings.Contains(c.Results[0].Output, "before-timeout") || c.Results[1].Status != "completed" {
		t.Fatalf("timeout %+v", c.Results)
	}
}
func TestCancelledComparisonSkipsTrials(t *testing.T) {
	home, project := fixture(t)
	c, err := Prepare(home, Request{Project: project, Prompt: "task", Profiles: []string{"mine", "mine-codex"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Execute(ctx, home, c); err != nil {
		t.Fatal(err)
	}
	if c.Status != "cancelled" || c.Results[0].Status != "cancelled" || c.Results[1].Status != "cancelled" {
		t.Fatalf("%+v", c)
	}
}
func TestValidationAndSnapshotSymlinkRefusal(t *testing.T) {
	home, project := fixture(t)
	for _, req := range []Request{{Project: project, Prompt: "", Profiles: []string{"mine", "mine-codex"}}, {Project: project, Prompt: "x", Profiles: []string{"mine", "mine"}}, {Project: project, Prompt: "x", Profiles: []string{"mine", "unknown"}}, {Project: project, Prompt: "x", Profiles: []string{"mine", "mine-codex"}, TimeoutSeconds: -1}} {
		if _, err := Prepare(home, req); err == nil {
			t.Fatalf("accepted invalid request %+v", req)
		}
	}
	if _, err := Load(home, "../../state"); err == nil {
		t.Fatal("accepted traversal ID")
	}
	if err := os.Symlink(filepath.Join(project, "code.txt"), filepath.Join(project, "link")); err != nil {
		t.Skip(err)
	}
	if _, err := Prepare(home, Request{Project: project, Prompt: "x", Profiles: []string{"mine", "mine-codex"}}); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink error %v", err)
	}
	list, err := List(home)
	if err != nil || len(list) != 0 {
		t.Fatalf("failed preparation left results: %v %v", list, err)
	}
}

func TestDiffIncludesChangesCommittedByTheAgent(t *testing.T) {
	home, project := fixture(t)
	bin := filepath.Join(t.TempDir(), "committer")
	write(t, bin, `#!/bin/sh
if [ "$1" = "--version" ]; then echo fixture; exit; fi
cat >/dev/null
printf 'agent committed this\n' > code.txt
git -c core.hooksPath= -c commit.gpgsign=false -c user.name=fixture -c user.email=fixture@local add code.txt
git -c core.hooksPath= -c commit.gpgsign=false -c user.name=fixture -c user.email=fixture@local commit -m changed >/dev/null
`)
	os.Chmod(bin, 0700)
	t.Setenv("SHERPA_CLAUDE_BIN", bin)
	c, err := Prepare(home, Request{Project: project, Prompt: "task", Profiles: []string{"mine", "mine-codex"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := Execute(context.Background(), home, c); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(c.Results[0].Diff, "+agent committed this") {
		t.Fatalf("committed edits disappeared: %+v", c.Results[0])
	}
}
