package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"sherpa/internal/state"
)

func TestRenameProfileMovesDirAndUpdatesState(t *testing.T) {
	home := t.TempDir()
	oldDir := filepath.Join(home, "profiles", "mine")
	if err := os.MkdirAll(oldDir, 0o700); err != nil {
		t.Fatal(err)
	}
	st := &state.State{
		Active: "mine",
		Profiles: map[string]state.Profile{
			"mine": {Name: "mine", Path: oldDir, Harness: "claude-code"},
		},
		Baselines: map[string]string{"claude-code": "mine"},
	}

	newDir := filepath.Join(home, "profiles", "mine-claude")
	if err := renameProfile(home, st, "mine", "mine-claude"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Fatalf("old dir still exists or stat failed with unexpected error: %v", err)
	}
	if info, err := os.Stat(newDir); err != nil || !info.IsDir() {
		t.Fatalf("new dir not moved into place: info=%v err=%v", info, err)
	}
	if _, ok := st.Profiles["mine"]; ok {
		t.Fatalf("old profile still present: %+v", st.Profiles)
	}
	p, ok := st.Profiles["mine-claude"]
	if !ok {
		t.Fatalf("new profile missing: %+v", st.Profiles)
	}
	if p.Name != "mine-claude" || p.Path != newDir || p.Harness != "claude-code" {
		t.Fatalf("renamed profile = %+v, want name/path updated and harness preserved", p)
	}
	if st.Active != "mine-claude" {
		t.Fatalf("active = %q", st.Active)
	}
	if st.Baselines["claude-code"] != "mine-claude" {
		t.Fatalf("baselines = %+v", st.Baselines)
	}
}

func TestRenameProfileExistingNameLeavesStateUntouched(t *testing.T) {
	home := t.TempDir()
	mineDir := filepath.Join(home, "profiles", "mine")
	existingDir := filepath.Join(home, "profiles", "mine-claude")
	if err := os.MkdirAll(mineDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(existingDir, 0o700); err != nil {
		t.Fatal(err)
	}
	st := &state.State{
		Active: "mine",
		Profiles: map[string]state.Profile{
			"mine":        {Name: "mine", Path: mineDir, Harness: "claude-code"},
			"mine-claude": {Name: "mine-claude", Path: existingDir, Harness: "claude-code"},
		},
		Baselines: map[string]string{"claude-code": "mine"},
	}
	want := cloneState(st)

	if err := renameProfile(home, st, "mine", "mine-claude"); err == nil {
		t.Fatal("want error renaming onto an existing profile")
	}
	if !reflect.DeepEqual(st, want) {
		t.Fatalf("state mutated on failed rename:\ngot  %+v\nwant %+v", st, want)
	}
	if info, err := os.Stat(mineDir); err != nil || !info.IsDir() {
		t.Fatalf("old dir changed on failed rename: info=%v err=%v", info, err)
	}
}

func TestRenameProfileDiskFailureLeavesStateUntouched(t *testing.T) {
	home := t.TempDir()
	missingDir := filepath.Join(home, "profiles", "mine")
	st := &state.State{
		Active: "mine",
		Profiles: map[string]state.Profile{
			"mine": {Name: "mine", Path: missingDir, Harness: "claude-code"},
		},
		Baselines: map[string]string{"claude-code": "mine"},
	}
	want := cloneState(st)

	if err := renameProfile(home, st, "mine", "mine-claude"); err == nil {
		t.Fatal("want error when on-disk profile dir is missing")
	}
	if !reflect.DeepEqual(st, want) {
		t.Fatalf("state mutated after disk rename failure:\ngot  %+v\nwant %+v", st, want)
	}
}

func cloneState(st *state.State) *state.State {
	cp := &state.State{
		Active:    st.Active,
		Profiles:  map[string]state.Profile{},
		Baselines: map[string]string{},
	}
	for k, v := range st.Profiles {
		cp.Profiles[k] = v
	}
	for k, v := range st.Baselines {
		cp.Baselines[k] = v
	}
	return cp
}
