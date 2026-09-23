package state

import (
	"os"
	"path/filepath"
	"testing"
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
