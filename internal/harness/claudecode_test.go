package harness

import (
	"encoding/json"
	"testing"
)

const rawSetup = `{
  "hasCompletedOnboarding": true,
  "oauthAccount": {"emailAddress": "me@example.com"},
  "userID": "u1",
  "installMethod": "native",
  "theme": "dark",
  "sonnet45MigrationComplete": true,
  "effortCalloutDismissed": true,
  "projects": {"/Users/me/x": {"hasTrustDialogAccepted": true, "mcpServers": {"evil": {}}}},
  "modelAccessCache": {"secretish": 1},
  "someFutureUnknownKey": {"nested": true}
}`

func TestSeedKeepsIdentityStripsProjectsAndCaches(t *testing.T) {
	rel, content, err := Default().Seed([]byte(rawSetup))
	if err != nil {
		t.Fatal(err)
	}
	if rel != ".claude.json" {
		t.Fatalf("target rel = %q, want .claude.json", rel)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(content, &m); err != nil {
		t.Fatal(err)
	}
	for _, keep := range []string{"hasCompletedOnboarding", "oauthAccount", "userID", "installMethod", "theme", "sonnet45MigrationComplete", "effortCalloutDismissed"} {
		if _, ok := m[keep]; !ok {
			t.Errorf("expected key %q kept", keep)
		}
	}
	for _, drop := range []string{"projects", "modelAccessCache", "someFutureUnknownKey"} {
		if _, ok := m[drop]; ok {
			t.Errorf("expected key %q dropped (whitelist fails safe)", drop)
		}
	}
}

func TestSeedDropsStructuredFamilyKeys(t *testing.T) {
	raw := []byte(`{
	  "mcpMigrationBackup": {"x": 1},
	  "fooMigrationComplete": true,
	  "oauthAccount": {"emailAddress": "me@example.com"},
	  "hasSeenTasksHint": true
	}`)
	_, content, err := Default().Seed(raw)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(content, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["mcpMigrationBackup"]; ok {
		t.Fatal("expected structured family key mcpMigrationBackup dropped")
	}
	for _, keep := range []string{"fooMigrationComplete", "oauthAccount", "hasSeenTasksHint"} {
		if _, ok := m[keep]; !ok {
			t.Errorf("expected key %q kept", keep)
		}
	}
}

func TestSeedRejectsInvalidJSON(t *testing.T) {
	if _, _, err := Default().Seed([]byte("{not json")); err == nil {
		t.Fatal("want error on invalid setup json")
	}
}

func TestSetupStateSourcesDerivedFromConfigDir(t *testing.T) {
	got := Default().SetupStateSources("/tmp/x/.claude")
	if len(got) != 1 || got[0] != "/tmp/x/.claude.json" {
		t.Fatalf("sources = %v, want [/tmp/x/.claude.json]", got)
	}
}
