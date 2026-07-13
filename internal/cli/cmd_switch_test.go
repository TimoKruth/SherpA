package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

func TestBackRequiresActiveHarnessBaselineWithoutFallingBackToMine(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SHERPA_HOME", home)
	st, _ := state.Load(home)
	st.Active = "casey"
	st.Profiles["mine"] = state.Profile{Name: "mine", Path: home + "/profiles/mine", Harness: "claude-code"}
	st.Profiles["casey"] = state.Profile{Name: "casey", Path: home + "/profiles/casey", Origin: "https://x/casey.git", Harness: "codex"}
	st.Baselines["claude-code"] = "mine"
	if err := st.Save(home); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := Run([]string{"back"}, &out, &errb); code == 0 {
		t.Fatal("back must fail when the active harness has no baseline")
	}
	if !strings.Contains(errb.String(), `no baseline for harness "codex"`) {
		t.Fatalf("stderr = %q", errb.String())
	}
	st, _ = state.Load(home)
	if st.Active != "casey" {
		t.Fatalf("back changed active = %q", st.Active)
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

func TestStatusPrintsLocalStateAndCachedUpdatesWhenRegistryFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	home := setupHome(t)
	t.Setenv("SHERPA_REGISTRY_URL", srv.URL)
	if err := saveRegistrySession(home, srv.URL, "user-session", "bob"); err != nil {
		t.Fatal(err)
	}
	st, _ := state.Load(home)
	st.Registries[srv.URL] = state.RegistryState{CachedUpdates: []state.UpdateSummary{{Owner: "alice", Stack: "reviewer", Version: 3, SeenVersion: 1}}}
	if err := st.Save(home); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"status"}, &out, &errOut); code != 0 {
		t.Fatalf("status failed: %s", errOut.String())
	}
	if !strings.Contains(out.String(), "active: mine") || !strings.Contains(out.String(), "@alice/reviewer  v1 -> v3") {
		t.Fatalf("stdout = %q", out.String())
	}
	if strings.Count(errOut.String(), "warning:") != 1 {
		t.Fatalf("stderr = %q", errOut.String())
	}
}

func TestStatusRefreshesPendingUpdatesWithoutMarkingSeen(t *testing.T) {
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"updates": []map[string]any{{
			"ref": "@alice/reviewer", "owner": "alice", "name": "reviewer", "version": 2, "seen_version": 1,
			"published_at": "2026-07-13T12:00:00Z",
		}}})
	}))
	defer srv.Close()
	home := setupHome(t)
	t.Setenv("SHERPA_REGISTRY_URL", srv.URL)
	if err := saveRegistrySession(home, srv.URL, "user-session", "bob"); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"status"}, &out, &errOut); code != 0 {
		t.Fatal(errOut.String())
	}
	if len(methods) != 1 || methods[0] != "GET /v1/me/updates" {
		t.Fatalf("requests = %v", methods)
	}
	if !strings.Contains(out.String(), "@alice/reviewer  v1 -> v2") {
		t.Fatalf("stdout = %q", out.String())
	}
}
