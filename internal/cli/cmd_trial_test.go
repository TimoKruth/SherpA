package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sherpa/internal/state"
)

func TestTrialRecordAllVerdictsSnapshotsIdentityAndUsesUniqueIDs(t *testing.T) {
	home := setupTrialHome(t, "https://registry.example")
	for _, verdict := range []string{"keep", "keep-with-notes", "revert"} {
		var out, errOut bytes.Buffer
		if code := Run([]string{"trial", "record", "reviewer", "--verdict", verdict, "--notes", "private " + verdict}, &out, &errOut); code != 0 {
			t.Fatalf("record %s: %s", verdict, errOut.String())
		}
		if strings.Contains(out.String(), "private") {
			t.Fatalf("record output disclosed notes: %q", out.String())
		}
	}
	st, err := state.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Trials) != 3 {
		t.Fatalf("trials = %#v", st.Trials)
	}
	ids := map[string]bool{}
	wantVerdicts := []string{"keep", "keep_with_notes", "revert"}
	for i, entry := range st.Trials {
		if !validTrialID(entry.ID) || ids[entry.ID] {
			t.Fatalf("invalid/duplicate ID = %q", entry.ID)
		}
		ids[entry.ID] = true
		if entry.Profile != "reviewer" || entry.RegistryURL != "https://registry.example" || entry.Owner != "alice" || entry.Stack != "reviewer" || entry.Version != 2 || entry.Verdict != wantVerdicts[i] || entry.RecordedAt.IsZero() || entry.SharedAt != nil {
			t.Fatalf("entry = %#v", entry)
		}
	}
	profile := st.Profiles["reviewer"]
	profile.Registry.Version = 3
	profile.Registry.Owner = "changed"
	st.Profiles["reviewer"] = profile
	if err := st.Save(home); err != nil {
		t.Fatal(err)
	}
	st, _ = state.Load(home)
	if st.Trials[0].Owner != "alice" || st.Trials[0].Version != 2 {
		t.Fatalf("trial snapshot changed with profile: %#v", st.Trials[0])
	}
}

func TestTrialRecordRejectsInvalidProfilesVerdictsAndNotes(t *testing.T) {
	home := setupTrialHome(t, "https://registry.example")
	st, _ := state.Load(home)
	st.Profiles["direct"] = state.Profile{Name: "direct", Path: filepath.Join(home, "profiles", "direct"), Origin: "https://git.example/direct.git", Harness: "codex"}
	if err := st.Save(home); err != nil {
		t.Fatal(err)
	}
	tests := [][]string{
		{"trial", "record", "missing", "--verdict", "keep"},
		{"trial", "record", "mine", "--verdict", "keep"},
		{"trial", "record", "direct", "--verdict", "keep"},
		{"trial", "record", "reviewer", "--verdict", "maybe"},
		{"trial", "record", "reviewer", "--verdict", "keep_with_notes"},
		{"trial", "record", "reviewer", "--verdict", "keep", "--notes", strings.Repeat("x", maxTrialNotesBytes+1)},
		{"trial", "record", "reviewer", "--verdict", "keep", "--notes", string([]byte{0xff})},
	}
	for _, args := range tests {
		var out, errOut bytes.Buffer
		if code := Run(args, &out, &errOut); code == 0 {
			t.Fatalf("%v succeeded", args[:4])
		}
	}
	st, _ = state.Load(home)
	if len(st.Trials) != 0 {
		t.Fatalf("invalid record created trials: %#v", st.Trials)
	}
}

func TestTrialRecordParserNeverEchoesPrivateNotes(t *testing.T) {
	secret := "SECRET_NOTE_123"
	for _, args := range [][]string{
		{"reviewer", "--verdict", "keep", "--notes", secret, "extra"},
		{"reviewer", "--verdict", "keep", "--notes", secret, "--unknown"},
		{"reviewer", "--verdict", "keep", "--notes=" + secret, "--notes", "again"},
	} {
		_, err := parseTrialRecordArgs(args)
		if err == nil || strings.Contains(err.Error(), secret) {
			t.Fatalf("args error = %v", err)
		}
	}
}

func TestTrialListFiltersNewestFirstAndRendersTerminalSafeNotes(t *testing.T) {
	home := setupTrialHome(t, "https://registry.example")
	st, _ := state.Load(home)
	older := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	newer := older.Add(time.Hour)
	st.Trials = []state.TrialEntry{
		{ID: trialID(1), Profile: "reviewer", Owner: "alice", Stack: "reviewer", Version: 1, Verdict: "keep", Notes: "old", RecordedAt: older},
		{ID: trialID(2), Profile: "other", Owner: "bob", Stack: "builder", Version: 2, Verdict: "revert", Notes: "other", RecordedAt: newer},
		{ID: trialID(3), Profile: "reviewer", Owner: "alice", Stack: "reviewer", Version: 2, Verdict: "keep_with_notes", Notes: "safe\x1b[2J\nprivate", RecordedAt: newer},
	}
	if err := st.Save(home); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"trial", "list"}, &out, &errOut); code != 0 {
		t.Fatal(errOut.String())
	}
	text := out.String()
	if strings.Contains(text, "\x1b") || !strings.Contains(text, "notes: safe [2J private") {
		t.Fatalf("terminal output = %q", text)
	}
	if strings.Index(text, trialID(3)) > strings.Index(text, trialID(2)) || strings.Index(text, trialID(2)) > strings.Index(text, trialID(1)) {
		t.Fatalf("list order = %q", text)
	}
	out.Reset()
	if code := Run([]string{"trial", "list", "--profile", "reviewer"}, &out, &errOut); code != 0 {
		t.Fatal(errOut.String())
	}
	if strings.Contains(out.String(), trialID(2)) || !strings.Contains(out.String(), trialID(1)) || !strings.Contains(out.String(), trialID(3)) {
		t.Fatalf("filtered output = %q", out.String())
	}
}

func TestTrialShareSendsVerdictOnlyAndIsExplicitlyRepeatable(t *testing.T) {
	secret := "ghp_SECRET_NOTE_NEVER_SEND"
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/v1/me/trials/alice/reviewer/2" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer user-session" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	home := setupTrialHome(t, srv.URL)
	if err := saveRegistrySession(home, srv.URL, "user-session", "bob"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHERPA_REGISTRY_TOKEN", "admin-must-not-be-used")
	st, _ := state.Load(home)
	entry := state.TrialEntry{ID: trialID(4), Profile: "reviewer", RegistryURL: srv.URL, Owner: "alice", Stack: "reviewer", Version: 2, Verdict: "keep_with_notes", Notes: secret, RecordedAt: time.Now().UTC()}
	st.Trials = []state.TrialEntry{entry}
	if err := st.Save(home); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		var out, errOut bytes.Buffer
		if code := Run([]string{"trial", "share", entry.ID}, &out, &errOut); code != 0 {
			t.Fatalf("share: %s", errOut.String())
		}
		if strings.Contains(out.String(), secret) || strings.Contains(errOut.String(), secret) {
			t.Fatalf("share output disclosed notes: stdout=%q stderr=%q", out.String(), errOut.String())
		}
	}
	if len(bodies) != 2 {
		t.Fatalf("share requests = %d", len(bodies))
	}
	for _, body := range bodies {
		if body != `{"verdict":"keep_with_notes"}` || strings.Contains(body, secret) || strings.Contains(body, `"notes":`) {
			t.Fatalf("share body = %q", body)
		}
	}
	st, _ = state.Load(home)
	if st.Trials[0].SharedAt == nil {
		t.Fatal("successful share did not record SharedAt")
	}
}

func TestFailedTrialShareDoesNotDiscloseNotesOrSetSharedAt(t *testing.T) {
	secret := "PRIVATE_TRIAL_NOTE"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(secret))
	}))
	defer srv.Close()
	home := setupTrialHome(t, srv.URL)
	if err := saveRegistrySession(home, srv.URL, "user-session", "bob"); err != nil {
		t.Fatal(err)
	}
	st, _ := state.Load(home)
	entry := state.TrialEntry{ID: trialID(5), Profile: "reviewer", RegistryURL: srv.URL, Owner: "alice", Stack: "reviewer", Version: 2, Verdict: "revert", Notes: secret, RecordedAt: time.Now().UTC()}
	st.Trials = []state.TrialEntry{entry}
	_ = st.Save(home)
	var out, errOut bytes.Buffer
	ctx := &Ctx{Home: home, Stdout: &out, Stderr: &errOut, Stdin: strings.NewReader("")}
	err := cmdTrialShare(ctx, []string{entry.ID})
	if !errors.Is(err, errRegistryUnavailable) || strings.Contains(err.Error(), secret) || strings.Contains(out.String(), secret) || strings.Contains(errOut.String(), secret) {
		t.Fatalf("error=%v stdout=%q stderr=%q", err, out.String(), errOut.String())
	}
	st, _ = state.Load(home)
	if st.Trials[0].SharedAt != nil {
		t.Fatalf("failed share set SharedAt = %v", st.Trials[0].SharedAt)
	}
}

func TestTrialStateFileRemainsPrivateAndContainsNoSessionToken(t *testing.T) {
	home := setupTrialHome(t, "https://registry.example")
	if err := saveRegistrySession(home, "https://registry.example", "session-secret", "bob"); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"trial", "record", "reviewer", "--verdict", "keep"}, &out, &errOut); code != 0 {
		t.Fatal(errOut.String())
	}
	path := filepath.Join(home, "state.json")
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, err=%v", info.Mode().Perm(), err)
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "session-secret") || strings.Contains(string(b), "access_token") {
		t.Fatalf("state contains session data: %s", b)
	}
}

func setupTrialHome(t *testing.T, registryURL string) string {
	t.Helper()
	home := setupHome(t)
	st, err := state.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	st.Profiles["reviewer"] = state.Profile{
		Name: "reviewer", Path: filepath.Join(home, "profiles", "reviewer"), Origin: registryURL + "/reviewer.git", Harness: "codex",
		Registry: &state.RegistryOrigin{RegistryURL: registryURL, Owner: "alice", Stack: "reviewer", Version: 2},
	}
	if err := st.Save(home); err != nil {
		t.Fatal(err)
	}
	return home
}

func trialID(fill byte) string {
	return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{fill}, 16))
}

func TestTrialEntryJSONNeverDerivesNotesFromUnknownFields(t *testing.T) {
	var entry state.TrialEntry
	if err := json.Unmarshal([]byte(`{"id":"x","future":"ignored"}`), &entry); err != nil || entry.Notes != "" {
		t.Fatalf("entry = %#v, err=%v", entry, err)
	}
}
