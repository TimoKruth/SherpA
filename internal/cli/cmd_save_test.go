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

func makeBareRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "remote.git")
	if out, err := exec.Command("git", "init", "--bare", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %s", out)
	}
	return dir
}
