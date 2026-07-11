package harness

import "testing"

func TestCodexAdapterValues(t *testing.T) {
	h, err := For("codex")
	if err != nil {
		t.Fatal(err)
	}
	if h.Name() != "codex" || h.Alias() != "codex" {
		t.Fatalf("name/alias: %q/%q", h.Name(), h.Alias())
	}
	if h.ConfigDirEnv() != "CODEX_HOME" || h.LaunchBin() != "codex" || h.LaunchBinEnv() != "SHERPA_CODEX_BIN" {
		t.Fatalf("launch identity: %q %q %q", h.ConfigDirEnv(), h.LaunchBin(), h.LaunchBinEnv())
	}
	if h.DefaultConfigDir("/home/u") != "/home/u/.codex" {
		t.Fatalf("config dir: %q", h.DefaultConfigDir("/home/u"))
	}
	if got := h.CredentialFiles(); len(got) != 1 || got[0] != "auth.json" {
		t.Fatalf("cred files: %v", got)
	}
	if got := h.SetupStateFilenames(); len(got) != 1 || got[0] != "auth.json" {
		t.Fatalf("setup filenames: %v", got)
	}
	if len(h.LoginSignatures()) == 0 {
		t.Fatal("login signatures must be non-empty")
	}
	for _, sig := range h.LoginSignatures() {
		if sig == "OPENAI_API_KEY" {
			t.Fatal("bare OPENAI_API_KEY signature blocks publishable config.toml env_key values")
		}
	}
	// no keychain export
	if err := h.PrepareBaselineCredentials(t.TempDir()); err != nil {
		t.Fatalf("PrepareBaselineCredentials must be a no-op for codex: %v", err)
	}
	// no curated seed
	rel, content, err := h.Seed([]byte(`{"x":1}`))
	if err != nil || rel != "" || content != nil {
		t.Fatalf("Seed must be empty for codex: %q %v %v", rel, content, err)
	}
	// AllowedPaths includes codex stack files
	must := map[string]bool{"AGENTS.md": false, "config.toml": false, "rules/": false, "skills/": false}
	for _, p := range h.AllowedPaths() {
		if _, ok := must[p]; ok {
			must[p] = true
		}
	}
	for p, seen := range must {
		if !seen {
			t.Errorf("AllowedPaths missing %q", p)
		}
	}
}
