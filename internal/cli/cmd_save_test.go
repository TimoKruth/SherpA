package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveCommitsActiveProfileWithoutMovingUpstream(t *testing.T) {
	home := setupHome(t)
	repo := makeExpertRepo(t, true)
	var out, errb bytes.Buffer
	if code := Run([]string{"clone", repo, "--name", "save-test"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	out.Reset()
	errb.Reset()
	if code := Run([]string{"use", "save-test"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	dir := filepath.Join(home, "profiles", "save-test")
	beforeLocal := gitOut(t, dir, "rev-parse", "local")
	beforeOrigin := gitOut(t, dir, "rev-parse", "origin/main")

	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("# jane\n\nsaved line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errb.Reset()
	if code := Run([]string{"save", "-m", "test save"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	afterLocal := gitOut(t, dir, "rev-parse", "local")
	afterOrigin := gitOut(t, dir, "rev-parse", "origin/main")
	if afterLocal == beforeLocal {
		t.Fatal("save did not create a new local commit")
	}
	if afterOrigin != beforeOrigin {
		t.Fatalf("save moved origin/main: before %s after %s", beforeOrigin, afterOrigin)
	}
	if got := gitOut(t, dir, "log", "-1", "--format=%s"); got != "test save" {
		t.Fatalf("commit subject = %q", got)
	}
}

func TestDiffShowsActiveProfilePatch(t *testing.T) {
	home := setupHome(t)
	repo := makeExpertRepo(t, true)
	var out, errb bytes.Buffer
	if code := Run([]string{"clone", repo, "--name", "diff-test"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	out.Reset()
	errb.Reset()
	if code := Run([]string{"use", "diff-test"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	dir := filepath.Join(home, "profiles", "diff-test")
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("# jane\n\nchanged for diff\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := Run([]string{"save", "-m", "diff change"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}

	out.Reset()
	errb.Reset()
	if code := Run([]string{"diff"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	if !strings.Contains(out.String(), "+changed for diff") {
		t.Fatalf("diff output missing changed line:\n%s", out.String())
	}
}

func TestPublishBumpsTagsPushesAndPreservesForkProvenance(t *testing.T) {
	home := setupHome(t)
	repo := makeExpertRepo(t, true)
	var out, errb bytes.Buffer
	if code := Run([]string{"clone", repo, "--name", "publish-test"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	out.Reset()
	errb.Reset()
	if code := Run([]string{"use", "publish-test"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	remote := makeBareRepo(t)
	ctx := &Ctx{Home: home, Stdout: &out, Stderr: &errb, Stdin: strings.NewReader("yes\n")}
	if err := cmdPublish(ctx, []string{"--remote", remote}); err != nil {
		t.Fatal(err)
	}
	if tag := gitOut(t, remote, "tag", "-l", "v2"); tag != "v2" {
		t.Fatalf("remote tag = %q, want v2", tag)
	}
	stackYAML := gitOut(t, remote, "show", "main:stack.yaml")
	if !strings.Contains(stackYAML, "version: 2") {
		t.Fatalf("remote stack.yaml did not publish v2:\n%s", stackYAML)
	}
	if !strings.Contains(stackYAML, "forked_from: '@origin/base@v1'") &&
		!strings.Contains(stackYAML, "forked_from: \"@origin/base@v1\"") &&
		!strings.Contains(stackYAML, "forked_from: @origin/base@v1") {
		t.Fatalf("forked_from not preserved:\n%s", stackYAML)
	}
}

func TestPublishSkipsFetchedUpstreamTagCollision(t *testing.T) {
	home := setupHome(t)
	repo := makeExpertRepo(t, true)
	gitOut(t, repo, "tag", "v2")
	gitOut(t, repo, "tag", "v3")

	var out, errb bytes.Buffer
	if code := Run([]string{"clone", repo, "--name", "publish-collision-test"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	out.Reset()
	errb.Reset()
	if code := Run([]string{"use", "publish-collision-test"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	remote := makeBareRepo(t)
	ctx := &Ctx{Home: home, Stdout: &out, Stderr: &errb, Stdin: strings.NewReader("yes\n")}
	if err := cmdPublish(ctx, []string{"--remote", remote}); err != nil {
		t.Fatal(err)
	}
	if tag := gitOut(t, remote, "tag", "-l", "v4"); tag != "v4" {
		t.Fatalf("remote tag = %q, want v4", tag)
	}
	stackYAML := gitOut(t, remote, "show", "main:stack.yaml")
	if !strings.Contains(stackYAML, "version: 4") {
		t.Fatalf("remote stack.yaml did not publish v4:\n%s", stackYAML)
	}
}

func TestPublishAllowsGitignoredLocalLoginFiles(t *testing.T) {
	home := setupHome(t)
	repo := makeExpertRepo(t, true)
	var out, errb bytes.Buffer
	if code := Run([]string{"clone", repo, "--name", "local-login-publish-test"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	out.Reset()
	errb.Reset()
	if code := Run([]string{"use", "local-login-publish-test"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	dir := filepath.Join(home, "profiles", "local-login-publish-test")
	if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(`{"oauthAccount":{"id":"acct"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(`{"accessToken":"local-token"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("# jane\n\nclean publish change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errb.Reset()
	if code := Run([]string{"save", "-m", "clean stack change"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}

	remote := makeBareRepo(t)
	out.Reset()
	errb.Reset()
	ctx := &Ctx{Home: home, Stdout: &out, Stderr: &errb, Stdin: strings.NewReader("yes\n")}
	if err := cmdPublish(ctx, []string{"--remote", remote}); err != nil {
		t.Fatalf("publish with gitignored local login files failed: %s", errb.String())
	}
	if tag := gitOut(t, remote, "tag", "-l", "v2"); tag != "v2" {
		t.Fatalf("remote tag = %q, want v2", tag)
	}
}

func TestPublishBlocksUncommittedGitignoreSetupStateAndDoesNotPush(t *testing.T) {
	home := setupHome(t)
	repo := makeExpertRepo(t, true)
	var out, errb bytes.Buffer
	if code := Run([]string{"clone", repo, "--name", "gitignore-setup-state-publish-test"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	out.Reset()
	errb.Reset()
	if code := Run([]string{"use", "gitignore-setup-state-publish-test"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	dir := filepath.Join(home, "profiles", "gitignore-setup-state-publish-test")
	b, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), append(b, []byte("# oauthAccount\n")...), 0o644); err != nil {
		t.Fatal(err)
	}

	remote := makeBareRepo(t)
	out.Reset()
	errb.Reset()
	ctx := &Ctx{Home: home, Stdout: &out, Stderr: &errb, Stdin: strings.NewReader("yes\nyes\n")}
	if err := cmdPublish(ctx, []string{"--remote", remote}); err == nil {
		t.Fatal("publish with uncommitted .gitignore setup-state/OAuth content must fail")
	}
	if !strings.Contains(errb.String(), "setup-state") {
		t.Fatalf("publish error did not mention setup-state findings: %q", errb.String())
	}
	assertRemoteStayedEmpty(t, remote)
}

func TestPublishBlocksUncommittedGitignoreSecretAndDoesNotPush(t *testing.T) {
	home := setupHome(t)
	repo := makeExpertRepo(t, true)
	var out, errb bytes.Buffer
	if code := Run([]string{"clone", repo, "--name", "gitignore-secret-publish-test"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	out.Reset()
	errb.Reset()
	if code := Run([]string{"use", "gitignore-secret-publish-test"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	dir := filepath.Join(home, "profiles", "gitignore-secret-publish-test")
	b, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	token := "ghp_" + strings.Repeat("x", 36)
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), append(b, []byte("# "+token+"\n")...), 0o644); err != nil {
		t.Fatal(err)
	}

	remote := makeBareRepo(t)
	out.Reset()
	errb.Reset()
	ctx := &Ctx{Home: home, Stdout: &out, Stderr: &errb, Stdin: strings.NewReader("yes\nyes\n")}
	if err := cmdPublish(ctx, []string{"--remote", remote}); err == nil {
		t.Fatal("publish with uncommitted .gitignore secret content must fail")
	}
	if !strings.Contains(errb.String(), "secret") {
		t.Fatalf("publish error did not mention secret findings: %q", errb.String())
	}
	assertRemoteStayedEmpty(t, remote)
}

func TestPublishBlocksTrackedOutsideAllowlistSetupStateAndDoesNotPush(t *testing.T) {
	home := setupHome(t)
	repo := makeExpertRepo(t, true)
	var out, errb bytes.Buffer
	if code := Run([]string{"clone", repo, "--name", "outside-allowlist-setup-state-publish-test"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	out.Reset()
	errb.Reset()
	if code := Run([]string{"use", "outside-allowlist-setup-state-publish-test"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	dir := filepath.Join(home, "profiles", "outside-allowlist-setup-state-publish-test")
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("clean note\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, dir, "add", "-f", "notes.txt")
	gitOut(t, dir, "-c", "user.email=sherpa@local", "-c", "user.name=sherpa", "-c", "commit.gpgsign=false", "commit", "-m", "track outside allowlist note")
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("oauthAccount\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	remote := makeBareRepo(t)
	out.Reset()
	errb.Reset()
	ctx := &Ctx{Home: home, Stdout: &out, Stderr: &errb, Stdin: strings.NewReader("yes\nyes\n")}
	if err := cmdPublish(ctx, []string{"--remote", remote}); err == nil {
		t.Fatal("publish with tracked outside-allowlist setup-state/OAuth content must fail")
	}
	if !strings.Contains(errb.String(), "setup-state") {
		t.Fatalf("publish error did not mention setup-state findings: %q", errb.String())
	}
	assertRemoteStayedEmpty(t, remote)
}

func TestPublishBlocksTrackedOutsideAllowlistSecretAndDoesNotPush(t *testing.T) {
	home := setupHome(t)
	repo := makeExpertRepo(t, true)
	var out, errb bytes.Buffer
	if code := Run([]string{"clone", repo, "--name", "outside-allowlist-secret-publish-test"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	out.Reset()
	errb.Reset()
	if code := Run([]string{"use", "outside-allowlist-secret-publish-test"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	dir := filepath.Join(home, "profiles", "outside-allowlist-secret-publish-test")
	token := "ghp_" + strings.Repeat("x", 36)
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("clean note\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, dir, "add", "-f", "notes.txt")
	gitOut(t, dir, "-c", "user.email=sherpa@local", "-c", "user.name=sherpa", "-c", "commit.gpgsign=false", "commit", "-m", "track outside allowlist note")
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte(token+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	remote := makeBareRepo(t)
	out.Reset()
	errb.Reset()
	ctx := &Ctx{Home: home, Stdout: &out, Stderr: &errb, Stdin: strings.NewReader("yes\nyes\n")}
	if err := cmdPublish(ctx, []string{"--remote", remote}); err == nil {
		t.Fatal("publish with tracked outside-allowlist secret content must fail")
	}
	if !strings.Contains(errb.String(), "secret") {
		t.Fatalf("publish error did not mention secret findings: %q", errb.String())
	}
	assertRemoteStayedEmpty(t, remote)
}

func TestPublishSecretAbortIsNonzeroAndDoesNotPush(t *testing.T) {
	home := setupHome(t)
	repo := makeExpertRepo(t, true)
	var out, errb bytes.Buffer
	if code := Run([]string{"clone", repo, "--name", "secret-publish-test"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	out.Reset()
	errb.Reset()
	if code := Run([]string{"use", "secret-publish-test"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	dir := filepath.Join(home, "profiles", "secret-publish-test")
	token := "ghp_" + strings.Repeat("x", 36)
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("# jane\n"+token+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	remote := makeBareRepo(t)
	out.Reset()
	errb.Reset()
	if code := Run([]string{"publish", "--remote", remote}, &out, &errb); code == 0 {
		t.Fatal("publish with a planted secret must fail")
	}
	if tag := gitOut(t, remote, "tag", "-l", "v2"); tag != "" {
		t.Fatalf("blocked publish pushed tag %q", tag)
	}
	if refs := gitOut(t, remote, "for-each-ref", "--format=%(refname)"); refs != "" {
		t.Fatalf("blocked publish pushed refs:\n%s", refs)
	}
	if !strings.Contains(errb.String(), "secret") {
		t.Fatalf("publish error did not mention secret findings: %q", errb.String())
	}
}

func TestPublishScansCommittedSecretRemovedFromWorkingTree(t *testing.T) {
	home := setupHome(t)
	repo := makeExpertRepo(t, true)
	var out, errb bytes.Buffer
	if code := Run([]string{"clone", repo, "--name", "history-secret-test"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	out.Reset()
	errb.Reset()
	if code := Run([]string{"use", "history-secret-test"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	dir := filepath.Join(home, "profiles", "history-secret-test")
	token := "ghp_" + strings.Repeat("x", 36)
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("# jane\n"+token+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, dir, "add", "CLAUDE.md")
	gitOut(t, dir, "-c", "user.email=sherpa@local", "-c", "user.name=sherpa", "-c", "commit.gpgsign=false", "commit", "-m", "add secret")
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("# jane\nclean again\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, dir, "add", "CLAUDE.md")
	gitOut(t, dir, "-c", "user.email=sherpa@local", "-c", "user.name=sherpa", "-c", "commit.gpgsign=false", "commit", "-m", "remove secret")
	if status := gitOut(t, dir, "status", "--porcelain"); status != "" {
		t.Fatalf("fixture working tree not clean:\n%s", status)
	}

	remote := makeBareRepo(t)
	out.Reset()
	errb.Reset()
	if code := Run([]string{"publish", "--remote", remote}, &out, &errb); code == 0 {
		t.Fatal("publish with a historical secret must fail")
	}
	if !strings.Contains(errb.String(), "secret") {
		t.Fatalf("publish error did not mention secret findings: %q", errb.String())
	}
	if tag := gitOut(t, remote, "tag", "-l", "v2"); tag != "" {
		t.Fatalf("blocked publish pushed tag %q", tag)
	}
	if refs := gitOut(t, remote, "for-each-ref", "--format=%(refname)"); refs != "" {
		t.Fatalf("blocked publish pushed refs:\n%s", refs)
	}
}

func TestPublishScansHistoricalSetupStateRemovedFromWorkingTree(t *testing.T) {
	home := setupHome(t)
	repo := makeExpertRepo(t, true)
	var out, errb bytes.Buffer
	if code := Run([]string{"clone", repo, "--name", "history-setup-state-test"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	out.Reset()
	errb.Reset()
	if code := Run([]string{"use", "history-setup-state-test"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	dir := filepath.Join(home, "profiles", "history-setup-state-test")
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"note":"oauthAccount"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, dir, "add", "settings.json")
	gitOut(t, dir, "-c", "user.email=sherpa@local", "-c", "user.name=sherpa", "-c", "commit.gpgsign=false", "commit", "-m", "add oauth setup state")
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"note":"clean"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, dir, "add", "settings.json")
	gitOut(t, dir, "-c", "user.email=sherpa@local", "-c", "user.name=sherpa", "-c", "commit.gpgsign=false", "commit", "-m", "remove oauth setup state")
	if status := gitOut(t, dir, "status", "--porcelain"); status != "" {
		t.Fatalf("fixture working tree not clean:\n%s", status)
	}

	remote := makeBareRepo(t)
	out.Reset()
	errb.Reset()
	ctx := &Ctx{Home: home, Stdout: &out, Stderr: &errb, Stdin: strings.NewReader("yes\nyes\n")}
	if err := cmdPublish(ctx, []string{"--remote", remote}); err == nil {
		t.Fatal("publish with historical setup-state/OAuth content must fail")
	}
	if !strings.Contains(errb.String(), "setup-state") {
		t.Fatalf("publish error did not mention setup-state findings: %q", errb.String())
	}
	assertRemoteStayedEmpty(t, remote)
}

func TestPublishScansMergeCommitSetupStateRemovedFromWorkingTree(t *testing.T) {
	home := setupHome(t)
	repo := makeExpertRepo(t, true)
	var out, errb bytes.Buffer
	if code := Run([]string{"clone", repo, "--name", "merge-setup-state-test"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	out.Reset()
	errb.Reset()
	if code := Run([]string{"use", "merge-setup-state-test"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	dir := filepath.Join(home, "profiles", "merge-setup-state-test")
	gitOut(t, dir, "checkout", "-b", "side")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("side branch change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, dir, "add", "README.md")
	gitOut(t, dir, "-c", "user.email=sherpa@local", "-c", "user.name=sherpa", "-c", "commit.gpgsign=false", "commit", "-m", "side change")
	gitOut(t, dir, "checkout", "local")
	gitOut(t, dir, "merge", "--no-ff", "--no-commit", "side")
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"note":"oauthAccount"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, dir, "add", "settings.json")
	gitOut(t, dir, "-c", "user.email=sherpa@local", "-c", "user.name=sherpa", "-c", "commit.gpgsign=false", "commit", "-m", "merge with oauth setup state")
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"note":"clean"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, dir, "add", "settings.json")
	gitOut(t, dir, "-c", "user.email=sherpa@local", "-c", "user.name=sherpa", "-c", "commit.gpgsign=false", "commit", "-m", "remove merge oauth setup state")
	if status := gitOut(t, dir, "status", "--porcelain"); status != "" {
		t.Fatalf("fixture working tree not clean:\n%s", status)
	}

	remote := makeBareRepo(t)
	out.Reset()
	errb.Reset()
	ctx := &Ctx{Home: home, Stdout: &out, Stderr: &errb, Stdin: strings.NewReader("yes\nyes\n")}
	if err := cmdPublish(ctx, []string{"--remote", remote}); err == nil {
		t.Fatal("publish with merge-commit setup-state/OAuth content must fail")
	}
	if !strings.Contains(errb.String(), "setup-state") {
		t.Fatalf("publish error did not mention setup-state findings: %q", errb.String())
	}
	assertRemoteStayedEmpty(t, remote)
}

func TestPublishBlocksSetupStateOAuthAndDoesNotPush(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, dir string)
	}{
		{
			name: "allowlisted oauth signature",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"note":"oauthAccount"}`), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "allowlisted setup state file",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				skillsDir := filepath.Join(dir, "skills", "local")
				if err := os.MkdirAll(skillsDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(skillsDir, ".claude.json"), []byte(`{"theme":"dark"}`), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := setupHome(t)
			repo := makeExpertRepo(t, true)
			var out, errb bytes.Buffer
			profileName := "setup-state-publish-test"
			if code := Run([]string{"clone", repo, "--name", profileName}, &out, &errb); code != 0 {
				t.Fatal(errb.String())
			}
			out.Reset()
			errb.Reset()
			if code := Run([]string{"use", profileName}, &out, &errb); code != 0 {
				t.Fatal(errb.String())
			}
			dir := filepath.Join(home, "profiles", profileName)
			tt.setup(t, dir)
			out.Reset()
			errb.Reset()
			if code := Run([]string{"save", "-m", "setup-state fixture"}, &out, &errb); code != 0 {
				t.Fatal(errb.String())
			}

			remote := makeBareRepo(t)
			out.Reset()
			errb.Reset()
			if code := Run([]string{"publish", "--remote", remote}, &out, &errb); code == 0 {
				t.Fatal("publish with setup-state/OAuth content must fail")
			}
			if !strings.Contains(errb.String(), "setup-state") {
				t.Fatalf("publish error did not mention setup-state findings: %q", errb.String())
			}
			if tag := gitOut(t, remote, "tag", "-l", "v2"); tag != "" {
				t.Fatalf("blocked publish pushed tag %q", tag)
			}
			if refs := gitOut(t, remote, "for-each-ref", "--format=%(refname)"); refs != "" {
				t.Fatalf("blocked publish pushed refs:\n%s", refs)
			}
		})
	}
}

func TestPublishDeclineDoesNotBurnVersionOrCommit(t *testing.T) {
	home := setupHome(t)
	repo := makeExpertRepo(t, true)
	var out, errb bytes.Buffer
	if code := Run([]string{"clone", repo, "--name", "decline-publish-test"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	out.Reset()
	errb.Reset()
	if code := Run([]string{"use", "decline-publish-test"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	dir := filepath.Join(home, "profiles", "decline-publish-test")
	beforeHead := gitOut(t, dir, "rev-parse", "local")
	beforeStack := gitOut(t, dir, "show", "local:stack.yaml")
	remote := makeBareRepo(t)

	ctx := &Ctx{Home: home, Stdout: &out, Stderr: &errb, Stdin: strings.NewReader("no\n")}
	if err := cmdPublish(ctx, []string{"--remote", remote}); err == nil {
		t.Fatal("declined publish must fail")
	}
	if !strings.Contains(out.String(), "publish version 2? (yes/no)") {
		t.Fatalf("publish prompt did not name the pending version:\n%s", out.String())
	}
	afterHead := gitOut(t, dir, "rev-parse", "local")
	if afterHead != beforeHead {
		t.Fatalf("declined publish created a commit: before %s after %s", beforeHead, afterHead)
	}
	afterStackBytes, err := os.ReadFile(filepath.Join(dir, "stack.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(afterStackBytes) != beforeStack+"\n" && string(afterStackBytes) != beforeStack {
		t.Fatalf("declined publish changed stack.yaml:\n%s", afterStackBytes)
	}
}

func makeBareRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "remote.git")
	if out, err := exec.Command("git", "init", "--bare", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %s", out)
	}
	return dir
}

func assertRemoteStayedEmpty(t *testing.T, remote string) {
	t.Helper()
	if tag := gitOut(t, remote, "tag", "-l", "v2"); tag != "" {
		t.Fatalf("blocked publish pushed tag %q", tag)
	}
	if refs := gitOut(t, remote, "for-each-ref", "--format=%(refname)"); refs != "" {
		t.Fatalf("blocked publish pushed refs:\n%s", refs)
	}
}
