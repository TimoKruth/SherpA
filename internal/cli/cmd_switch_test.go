package cli

import (
	"bytes"
	"strings"
	"testing"

	"sherpa/internal/state"
)

func setupHome(t *testing.T) string {
	home := t.TempDir()
	t.Setenv("SHERPA_HOME", home)
	st, _ := state.Load(home)
	st.Active = "mine"
	st.Profiles["mine"] = state.Profile{Name: "mine", Path: home + "/profiles/mine", Harness: "claude-code"}
	st.Profiles["jane"] = state.Profile{Name: "jane", Path: home + "/profiles/jane", Origin: "https://x/jane.git", Harness: "claude-code"}
	st.Baselines["claude-code"] = "mine"
	st.Save(home)
	return home
}

func TestUseAndBack(t *testing.T) {
	home := setupHome(t)
	var out, errb bytes.Buffer
	if code := Run([]string{"use", "jane"}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	st, _ := state.Load(home)
	if st.Active != "jane" {
		t.Fatalf("active = %q", st.Active)
	}
	Run([]string{"back"}, &out, &errb)
	st, _ = state.Load(home)
	if st.Active != "mine" {
		t.Fatalf("back: active = %q", st.Active)
	}
}

func TestBackUsesActiveProfilesHarnessBaseline(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SHERPA_HOME", home)
	st, _ := state.Load(home)
	st.Active = "casey"
	st.Profiles["mine-claude"] = state.Profile{Name: "mine-claude", Path: home + "/profiles/mine-claude", Harness: "claude-code"}
	st.Profiles["mine-codex"] = state.Profile{Name: "mine-codex", Path: home + "/profiles/mine-codex", Harness: "codex"}
	st.Profiles["casey"] = state.Profile{Name: "casey", Path: home + "/profiles/casey", Origin: "https://x/casey.git", Harness: "codex"}
	st.Baselines["claude-code"] = "mine-claude"
	st.Baselines["codex"] = "mine-codex"
	if err := st.Save(home); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := Run([]string{"back"}, &out, &errb); code != 0 {
		t.Fatalf("back failed: %s", errb.String())
	}
	st, _ = state.Load(home)
	if st.Active != "mine-codex" {
		t.Fatalf("back: active = %q, want mine-codex", st.Active)
	}
}

func TestUseUnknownProfileFails(t *testing.T) {
	setupHome(t)
	var out, errb bytes.Buffer
	if code := Run([]string{"use", "ghost"}, &out, &errb); code == 0 {
		t.Fatal("want failure")
	}
}

func TestStatusListsProfiles(t *testing.T) {
	setupHome(t)
	var out, errb bytes.Buffer
	Run([]string{"status"}, &out, &errb)
	s := out.String()
	if !strings.Contains(s, "mine") || !strings.Contains(s, "jane") || !strings.Contains(s, "active") {
		t.Fatalf("status output: %q", s)
	}
}
