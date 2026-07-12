package integration

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"sherpa/internal/registry/api"
	registryauth "sherpa/internal/registry/auth"
	"sherpa/internal/registry/content"
	"sherpa/internal/registry/store"
)

func TestRegistryCLIEndToEnd(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "sherpa-home")
	claudeDir := filepath.Join(root, "real-claude")
	t.Setenv("SHERPA_HOME", home)
	t.Setenv("SHERPA_CLAUDE_DIR", claudeDir)
	t.Setenv("SHERPA_SECURITY_BIN", "/usr/bin/false")

	if err := writeFile(filepath.Join(claudeDir, "CLAUDE.md"), "baseline instructions\n", 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(claudeDir, "settings.json"), `{"theme":"mine","mcpServers":{}}`+"\n", 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(claudeDir, ".credentials.json"), "credential-copy\n", 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(claudeDir+".json", `{"hasCompletedOnboarding":true}`+"\n", 0o600); err != nil {
		t.Fatal(err)
	}

	serverURL := startRegistryServer(t)
	t.Setenv("SHERPA_REGISTRY_URL", serverURL)
	t.Setenv("SHERPA_REGISTRY_TOKEN", "test-token")

	if out, errb, code := runCLI(t, nil, "init"); code != 0 {
		t.Fatalf("init failed: %s\nstdout:\n%s", errb, out)
	}

	cleanRepo := makeRegistryFixtureRepo(t, root, "registryowner", "registry-clean", "Registry clean fixture", nil)
	if out, errb, code := runCLI(t, nil, "clone", cleanRepo, "--name", "registry-clean"); code != 0 {
		t.Fatalf("clone clean fixture failed: %s\nstdout:\n%s", errb, out)
	}
	if out, errb, code := runCLI(t, nil, "use", "registry-clean"); code != 0 {
		t.Fatalf("use clean fixture failed: %s\nstdout:\n%s", errb, out)
	}
	out, errb, code := runCLI(t, "yes\n", "publish", "--registry", serverURL)
	if code != 0 {
		t.Fatalf("registry publish failed: %s\nstdout:\n%s", errb, out)
	}
	if !strings.Contains(out, "published v2") {
		t.Fatalf("publish output missing success:\n%s", out)
	}

	out, errb, code = runCLI(t, nil, "search", "Registry")
	if code != 0 {
		t.Fatalf("registry search failed: %s\nstdout:\n%s", errb, out)
	}
	if !strings.Contains(out, "@registryowner/registry-clean") || !strings.Contains(out, "Registry clean fixture") {
		t.Fatalf("registry search missing published stack:\n%s", out)
	}

	out, errb, code = runCLI(t, nil, "clone", "@registryowner/registry-clean", "--name", "registry-roundtrip")
	if code != 0 {
		t.Fatalf("registry @ref clone failed: %s\nstdout:\n%s", errb, out)
	}
	got := readFile(t, filepath.Join(home, "profiles", "registry-roundtrip", "skills", "review", "SKILL.md"))
	if got != "registry review skill\n" {
		t.Fatalf("registry clone tree mismatch: %q", got)
	}

	secretRepo := makeRegistryFixtureRepo(t, root, "registryowner", "secret-stack", "Registry secret fixture", map[string]string{
		"README.md": "token ghp_" + strings.Repeat("x", 36) + "\n",
	})
	if out, errb, code := runCLI(t, nil, "clone", secretRepo, "--name", "secret-stack"); code != 0 {
		t.Fatalf("clone secret fixture failed: %s\nstdout:\n%s", errb, out)
	}
	if out, errb, code := runCLI(t, nil, "use", "secret-stack"); code != 0 {
		t.Fatalf("use secret fixture failed: %s\nstdout:\n%s", errb, out)
	}
	out, errb, code = runCLI(t, "yes\n", "publish", "--registry", serverURL)
	if code == 0 {
		t.Fatalf("secret publish unexpectedly succeeded:\n%s", out)
	}
	if !strings.Contains(errb, "secret") {
		t.Fatalf("secret publish did not surface findings:\nstdout:\n%s\nstderr:\n%s", out, errb)
	}
	assertSearchDoesNotReturn(t, "secret-stack")

	sideRepo := makeRegistryFixtureRepo(t, root, "registryowner", "side-secret-stack", "Registry side branch fixture", nil)
	if out, errb, code := runCLI(t, nil, "clone", sideRepo, "--name", "side-secret-stack"); code != 0 {
		t.Fatalf("clone side fixture failed: %s\nstdout:\n%s", errb, out)
	}
	sideDir := filepath.Join(home, "profiles", "side-secret-stack")
	if out, errb, code := runCLI(t, nil, "use", "side-secret-stack"); code != 0 {
		t.Fatalf("use side fixture failed: %s\nstdout:\n%s", errb, out)
	}
	git(t, sideDir, "checkout", "-b", "sidebr")
	if err := writeFile(filepath.Join(sideDir, "README.md"), "side token ghp_"+strings.Repeat("y", 36)+"\n", 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, sideDir, "add", "README.md")
	git(t, sideDir, "-c", "user.email=sherpa@local", "-c", "user.name=sherpa", "-c", "commit.gpgsign=false", "commit", "-m", "side branch secret")
	git(t, sideDir, "checkout", "local")

	out, errb, code = runCLI(t, "yes\n", "publish", "--registry", serverURL)
	if code == 0 {
		t.Fatalf("side-branch secret publish unexpectedly succeeded:\n%s", out)
	}
	if !strings.Contains(errb, "secret") || !strings.Contains(errb, "README.md") {
		t.Fatalf("side-branch publish did not surface server findings:\nstdout:\n%s\nstderr:\n%s", out, errb)
	}
	assertSearchDoesNotReturn(t, "side-secret-stack")
}

func startRegistryServer(t *testing.T) string {
	t.Helper()
	dsn := store.StartPostgres(t)
	st, err := store.OpenPostgres(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Fatalf("close postgres: %v", err)
		}
	})
	handler := api.New(st, content.NewBareGit(t.TempDir()), "test-token", &registryauth.FakeGitHubClient{})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv.URL
}

func makeRegistryFixtureRepo(t *testing.T, root, owner, name, summary string, extra map[string]string) string {
	t.Helper()
	repo := filepath.Join(root, name+"-repo")
	files := map[string]string{
		"stack.yaml":             "name: " + name + "\nowner: " + owner + "\nversion: 1\nharness: claude-code\nsummary: " + summary + "\ntags:\n  - registry\n",
		"README.md":              summary + "\n",
		"CLAUDE.md":              "registry fixture instructions\n",
		"skills/review/SKILL.md": "registry review skill\n",
		"settings.json":          "{}\n",
	}
	for path, body := range extra {
		files[path] = body
	}
	for path, body := range files {
		if err := writeFile(filepath.Join(repo, path), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, repo, "init", "-b", "main")
	git(t, repo, "add", "-A")
	git(t, repo, "-c", "user.email=registry.invalid", "-c", "user.name=registry", "-c", "commit.gpgsign=false", "commit", "-m", "v1")
	git(t, repo, "tag", "v1")
	return repo
}

func assertSearchDoesNotReturn(t *testing.T, query string) {
	t.Helper()
	out, errb, code := runCLI(t, nil, "search", query)
	if code != 0 {
		t.Fatalf("search %q failed: %s\nstdout:\n%s", query, errb, out)
	}
	if strings.Contains(out, query) {
		t.Fatalf("blocked stack %q was searchable:\n%s", query, out)
	}
}
