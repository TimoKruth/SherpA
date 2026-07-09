package state

import (
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
