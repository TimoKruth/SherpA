package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"sherpa/internal/state"
)

func TestParseRegistryStackRefStrict(t *testing.T) {
	for _, ref := range []string{"@alice/reviewer", "@Alice-1/reviewer.v2", "@a/b_c"} {
		if _, _, ok := parseRegistryStackRef(ref); !ok {
			t.Fatalf("valid ref rejected: %q", ref)
		}
	}
	for _, ref := range []string{"alice/reviewer", "https://registry.example/a/b", "@alice", "@alice/reviewer/more", "@../reviewer", "@alice/.hidden", "@alice/review er", "@alice/reviewer@v2", "@a/" + strings.Repeat("x", 101), "@a/b\x1b"} {
		if _, _, ok := parseRegistryStackRef(ref); ok {
			t.Fatalf("invalid ref accepted: %q", ref)
		}
	}
}

func TestFollowAndUnfollowUseSavedUserSession(t *testing.T) {
	var followCalls, unfollowCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer user-session" {
			t.Errorf("authorization = %q", got)
		}
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/v1/me/follows/alice/reviewer":
			followCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"ref": "@alice/reviewer", "owner": "alice", "name": "reviewer", "latest_version": 2, "followed_at": "2026-07-13T12:00:00Z"})
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/me/follows/alice/reviewer":
			unfollowCalls.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	home := setupHome(t)
	t.Setenv("SHERPA_REGISTRY_URL", srv.URL+"/")
	t.Setenv("SHERPA_REGISTRY_TOKEN", "admin-must-not-be-used")
	if err := saveRegistrySession(home, srv.URL, "user-session", "bob"); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"follow", "@alice/reviewer"}, &out, &errOut); code != 0 {
		t.Fatalf("follow: %s", errOut.String())
	}
	if !strings.Contains(out.String(), "following @alice/reviewer at v2") {
		t.Fatalf("follow output = %q", out.String())
	}
	out.Reset()
	if code := Run([]string{"unfollow", "@alice/reviewer"}, &out, &errOut); code != 0 {
		t.Fatalf("unfollow: %s", errOut.String())
	}
	if followCalls.Load() != 1 || unfollowCalls.Load() != 1 {
		t.Fatalf("calls = follow %d, unfollow %d", followCalls.Load(), unfollowCalls.Load())
	}
}

func TestFollowParsingAndIssuerFailuresMakeNoRequest(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer srv.Close()
	home := setupHome(t)
	t.Setenv("SHERPA_REGISTRY_URL", srv.URL)
	t.Setenv("SHERPA_REGISTRY_TOKEN", "admin")
	if err := saveRegistrySession(home, "https://other.example", "user-session", "bob"); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"follow"}, {"follow", "https://example/a"}, {"follow", "@a/b", "extra"}, {"follow", "--bad"}} {
		var out, errOut bytes.Buffer
		if code := Run(args, &out, &errOut); code == 0 {
			t.Fatalf("%v succeeded", args)
		}
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"follow", "@alice/reviewer"}, &out, &errOut); code == 0 || !strings.Contains(errOut.String(), "registry session belongs to") {
		t.Fatalf("issuer mismatch: code=%d stderr=%q", code, errOut.String())
	}
	t.Setenv("SHERPA_REGISTRY_URL", "")
	errOut.Reset()
	if code := Run([]string{"follow", "@alice/reviewer"}, &out, &errOut); code == 0 || !strings.Contains(errOut.String(), "registry URL is required") {
		t.Fatalf("missing issuer: code=%d stderr=%q", code, errOut.String())
	}
	if calls.Load() != 0 {
		t.Fatalf("unexpected requests = %d", calls.Load())
	}
}

func TestFollowFailureDoesNotChangeProfilesOrState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	home := setupHome(t)
	t.Setenv("SHERPA_REGISTRY_URL", srv.URL)
	if err := saveRegistrySession(home, srv.URL, "user-session", "bob"); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(home, "state.json"))
	var out, errOut bytes.Buffer
	if code := Run([]string{"follow", "@alice/reviewer"}, &out, &errOut); code == 0 {
		t.Fatal("follow succeeded during registry failure")
	}
	after, _ := os.ReadFile(filepath.Join(home, "state.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("failed follow changed local state")
	}
	st, _ := state.Load(home)
	if len(st.Profiles) != 2 || st.Active != "mine" {
		t.Fatalf("profiles changed: %#v", st)
	}
}

func TestFollowRetriesAndRemovesQueuedEntries(t *testing.T) {
	var refs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refs = append(refs, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		name := "builder"
		if strings.HasSuffix(r.URL.Path, "/reviewer") {
			name = "reviewer"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ref": "@alice/" + name, "owner": "alice", "name": name, "latest_version": 1, "followed_at": "2026-07-13T12:00:00Z"})
	}))
	defer srv.Close()
	home := setupHome(t)
	t.Setenv("SHERPA_REGISTRY_URL", srv.URL)
	if err := saveRegistrySession(home, srv.URL, "user-session", "bob"); err != nil {
		t.Fatal(err)
	}
	st, _ := state.Load(home)
	st.Registries[srv.URL] = state.RegistryState{PendingFollows: []string{"@alice/reviewer", "@alice/reviewer"}}
	if err := st.Save(home); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"follow", "@alice/builder"}, &out, &errOut); code != 0 {
		t.Fatal(errOut.String())
	}
	st, _ = state.Load(home)
	if got := st.Registries[srv.URL].PendingFollows; len(got) != 0 {
		t.Fatalf("pending = %v", got)
	}
	if len(refs) != 2 || !strings.HasSuffix(refs[0], "/reviewer") || !strings.HasSuffix(refs[1], "/builder") {
		t.Fatalf("request paths = %v", refs)
	}
}
