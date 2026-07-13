package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sherpa/internal/state"
)

func TestParseUpdatesArgsStrict(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		limit   int
		seen    bool
		version int
	}{
		{nil, 25, false, 0},
		{[]string{"--limit", "3"}, 3, false, 0},
		{[]string{"--limit=50"}, 50, false, 0},
		{[]string{"--seen", "@alice/reviewer@v2"}, 25, true, 2},
	} {
		req, err := parseUpdatesArgs(tc.args)
		if err != nil || req.limit != tc.limit || req.seen != tc.seen || req.version != tc.version {
			t.Fatalf("parseUpdatesArgs(%v) = %#v, %v", tc.args, req, err)
		}
	}
	for _, args := range [][]string{{"--limit", "0"}, {"--limit", "51"}, {"--limit", "x"}, {"--limit", "2", "--limit=3"}, {"--seen"}, {"--seen", "@a/b"}, {"--seen", "@a/b@v0"}, {"--seen", "@a/b@v+2"}, {"--seen", "https://x/a@v2"}, {"--seen", "@a/b@v2", "extra"}, {"--seen", "@a/b@v2", "--limit", "25"}, {"--unknown"}} {
		if _, err := parseUpdatesArgs(args); err == nil {
			t.Fatalf("parseUpdatesArgs(%v) succeeded", args)
		}
	}
}

func TestUpdatesListsBoundedTerminalSafeDataAndCachesIt(t *testing.T) {
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		if r.Header.Get("Authorization") != "Bearer user-session" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"updates": []map[string]any{{
				"ref": "@alice/reviewer\u001b[31m", "owner": "alice", "name": "reviewer", "version": 3,
				"seen_version": 1, "changelog": "safe\n\u001b[2Jchanged", "published_at": "2026-07-13T12:00:00Z",
			}},
		})
	}))
	defer srv.Close()
	home := setupHome(t)
	t.Setenv("SHERPA_REGISTRY_URL", srv.URL)
	t.Setenv("SHERPA_REGISTRY_TOKEN", "admin-must-not-be-used")
	if err := saveRegistrySession(home, srv.URL, "user-session", "bob"); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(home, "state.json"))
	var out, errOut bytes.Buffer
	if code := Run([]string{"updates", "--limit", "7"}, &out, &errOut); code != 0 {
		t.Fatal(errOut.String())
	}
	if strings.Contains(out.String(), "\x1b") || !strings.Contains(out.String(), "v1 -> v3") || !strings.Contains(out.String(), "safe [2Jchanged") || !strings.Contains(out.String(), "sherpa update <profile>") {
		t.Fatalf("output = %q", out.String())
	}
	if query != "limit=7" {
		t.Fatalf("query = %q", query)
	}
	after, _ := os.ReadFile(filepath.Join(home, "state.json"))
	if bytes.Equal(before, after) {
		t.Fatal("successful listing did not refresh cache")
	}
	st, _ := state.Load(home)
	updates := st.Registries[srv.URL].CachedUpdates
	if len(updates) != 1 || updates[0].Version != 3 || strings.Contains(updates[0].Changelog, "\x1b") {
		t.Fatalf("cached updates = %#v", updates)
	}
}

func TestUpdatesSeenIsExplicitMutation(t *testing.T) {
	var body map[string]int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/v1/me/follows/alice/reviewer/seen" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ref": "@alice/reviewer", "owner": "alice", "name": "reviewer", "last_seen_version": 4, "followed_at": "2026-07-13T12:00:00Z"})
	}))
	defer srv.Close()
	home := setupHome(t)
	t.Setenv("SHERPA_REGISTRY_URL", srv.URL)
	if err := saveRegistrySession(home, srv.URL, "user-session", "bob"); err != nil {
		t.Fatal(err)
	}
	st, _ := state.Load(home)
	st.Registries[srv.URL] = state.RegistryState{CachedUpdates: []state.UpdateSummary{{Owner: "alice", Stack: "reviewer", Version: 4, SeenVersion: 1}}}
	_ = st.Save(home)
	var out, errOut bytes.Buffer
	if code := Run([]string{"updates", "--seen", "@alice/reviewer@v4"}, &out, &errOut); code != 0 {
		t.Fatal(errOut.String())
	}
	if body["version"] != 4 || !strings.Contains(out.String(), "marked @alice/reviewer@v4 reviewed") {
		t.Fatalf("body=%v output=%q", body, out.String())
	}
	st, _ = state.Load(home)
	if got := st.Registries[srv.URL].CachedUpdates[0].SeenVersion; got != 4 {
		t.Fatalf("cached seen = %d", got)
	}
}

func TestUpdatesFailureDoesNotChangeStateOrProfile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("remote-secret"))
	}))
	defer srv.Close()
	home := setupHome(t)
	t.Setenv("SHERPA_REGISTRY_URL", srv.URL)
	if err := saveRegistrySession(home, srv.URL, "user-session", "bob"); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(home, "state.json"))
	var out, errOut bytes.Buffer
	ctx := &Ctx{Home: home, Stdout: &out, Stderr: &errOut, Stdin: strings.NewReader("")}
	if err := cmdUpdates(ctx, nil); !errors.Is(err, errRegistryUnavailable) {
		t.Fatalf("error = %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(home, "state.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("failed listing changed state")
	}
	if strings.Contains(errOut.String(), "remote-secret") {
		t.Fatalf("stderr leaked response: %q", errOut.String())
	}
}
