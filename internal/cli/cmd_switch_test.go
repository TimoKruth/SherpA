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
