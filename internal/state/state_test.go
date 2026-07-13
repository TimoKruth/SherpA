package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadMissingGivesEmpty(t *testing.T) {
	s, err := Load(t.TempDir())
	if err != nil || s.Active != "" || len(s.Profiles) != 0 {
		t.Fatalf("got %+v, %v", s, err)
	}
}

func TestSaveLoadRoundtrip(t *testing.T) {
	home := t.TempDir()
	s, _ := Load(home)
	s.Active = "mine"
	s.Profiles["mine"] = Profile{Name: "mine", Path: filepath.Join(home, "profiles", "mine"), Harness: "claude-code"}
	if err := s.Save(home); err != nil {
		t.Fatal(err)
	}
	s2, err := Load(home)
	if err != nil || s2.Active != "mine" || s2.Profiles["mine"].Harness != "claude-code" {
		t.Fatalf("roundtrip: %+v, %v", s2, err)
	}
}

func TestLoadMigratesBaselinesFromMine(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "state.json"),
		[]byte(`{"active":"mine","profiles":{"mine":{"name":"mine","path":"/p","harness":"claude-code","future_profile_field":true}},"baselines":null,"registries":null,"trials":null,"future_state_field":{"version":2}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if st.Baselines["claude-code"] != "mine" {
		t.Fatalf("migration: %v", st.Baselines)
	}
}

func TestRegistryStateRoundTripCanonicalizesAndMerges(t *testing.T) {
	home := t.TempDir()
	checked := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	raw := map[string]any{
		"profiles": map[string]any{
			"reviewer": map[string]any{
				"name": "reviewer", "path": "/profiles/reviewer", "harness": "codex",
				"registry": map[string]any{"registry_url": "HTTPS://Registry.Example:443/", "owner": "alice", "stack": "reviewer", "version": 2},
			},
		},
		"registries": map[string]any{
			"https://registry.example": map[string]any{
				"pending_follows": []string{"@alice/reviewer", "@alice/reviewer"},
				"cached_updates":  []map[string]any{{"owner": "alice", "stack": "reviewer", "version": 2}},
			},
			"HTTPS://REGISTRY.EXAMPLE:443/": map[string]any{
				"pending_follows": []string{"@alice/builder"},
				"cached_updates":  []map[string]any{{"owner": "alice", "stack": "reviewer", "version": 3}},
				"last_checked_at": checked,
			},
		},
		"trials": []map[string]any{{"id": "trial-1", "registry_url": "https://REGISTRY.example/", "owner": "alice", "stack": "reviewer", "version": 2}},
	}
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "state.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Registries) != 1 {
		t.Fatalf("registries = %#v", st.Registries)
	}
	registry := st.Registries["https://registry.example"]
	if strings.Join(registry.PendingFollows, ",") != "@alice/builder,@alice/reviewer" || len(registry.CachedUpdates) != 1 || registry.CachedUpdates[0].Version != 3 || !registry.LastCheckedAt.Equal(checked) {
		t.Fatalf("registry state = %#v", registry)
	}
	if got := st.Profiles["reviewer"].Registry; got == nil || got.RegistryURL != "https://registry.example" {
		t.Fatalf("profile registry = %#v", got)
	}
	if st.Trials[0].RegistryURL != "https://registry.example" {
		t.Fatalf("trial registry = %q", st.Trials[0].RegistryURL)
	}
	if err := st.Save(home); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(home, "state.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode = %v, err=%v", info.Mode().Perm(), err)
	}
	saved, err := os.ReadFile(filepath.Join(home, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(saved), "access_token") || strings.Contains(string(saved), "session-token") {
		t.Fatalf("state contains token field: %s", saved)
	}
}

func TestSaveRepairsExistingTemporaryFilePermissions(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "state.json.tmp"), []byte("old"), 0o666); err != nil {
		t.Fatal(err)
	}
	st, err := Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Save(home); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(home, "state.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode = %v, err=%v", info.Mode().Perm(), err)
	}
}

func TestLoadRejectsInvalidRegistryStateIssuer(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "state.json"), []byte(`{"registries":{"file:///tmp/registry":{}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(home); err == nil || !strings.Contains(err.Error(), "registry state key") {
		t.Fatalf("Load error = %v", err)
	}
}
