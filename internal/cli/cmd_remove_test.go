package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sherpa/internal/state"
)

// runRemove drives cmdRemove with an explicit context, matching how the other
// interactive commands are tested.
func runRemove(home, stdin string, args ...string) (string, string, error) {
	var out, errb bytes.Buffer
	ctx := &Ctx{Home: home, Stdout: &out, Stderr: &errb, Stdin: strings.NewReader(stdin)}
	err := cmdRemove(ctx, args)
	return out.String(), errb.String(), err
}

// removableHome sets up a home with an installed, non-active, non-baseline
// profile that has content on disk.
func removableHome(t *testing.T) string {
	t.Helper()
	home := setupHome(t)
	dir := filepath.Join(home, "profiles", "jane")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stack.yaml"), []byte("name: jane\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestRemoveDeletesProfileFromStateAndDisk(t *testing.T) {
	home := removableHome(t)
	_, _, err := runRemove(home, "yes\n", "jane")
	if err != nil {
		t.Fatalf("remove failed: %s", err)
	}
	st, _ := state.Load(home)
	if _, ok := st.Profiles["jane"]; ok {
		t.Fatal("profile still recorded in state")
	}
	if _, err := os.Stat(filepath.Join(home, "profiles", "jane")); !os.IsNotExist(err) {
		t.Fatalf("profile directory still present: %v", err)
	}
	if st.Active != "mine" {
		t.Fatalf("active profile changed to %q", st.Active)
	}
}

func TestRemoveRefusesTheBaselineProfile(t *testing.T) {
	home := removableHome(t)
	_, _, err := runRemove(home, "yes\n", "mine")
	if err == nil {
		t.Fatal("removing the baseline profile must fail")
	}
	st, _ := state.Load(home)
	if _, ok := st.Profiles["mine"]; !ok {
		t.Fatal("baseline profile was removed")
	}
}

func TestRemoveRefusesTheActiveProfile(t *testing.T) {
	home := removableHome(t)
	st, _ := state.Load(home)
	st.Active = "jane"
	if err := st.Save(home); err != nil {
		t.Fatal(err)
	}
	_, _, err := runRemove(home, "yes\n", "jane")
	if err == nil {
		t.Fatal("removing the active profile must fail")
	}
	reloaded, _ := state.Load(home)
	if _, ok := reloaded.Profiles["jane"]; !ok {
		t.Fatal("active profile was removed")
	}
	if _, err := os.Stat(filepath.Join(home, "profiles", "jane")); err != nil {
		t.Fatalf("active profile directory removed: %v", err)
	}
}

func TestRemoveKeepsEverythingWhenConfirmationIsDeclined(t *testing.T) {
	home := removableHome(t)
	_, _, err := runRemove(home, "no\n", "jane")
	if err == nil {
		t.Fatal("declined confirmation must not succeed")
	}
	st, _ := state.Load(home)
	if _, ok := st.Profiles["jane"]; !ok {
		t.Fatal("profile removed despite declined confirmation")
	}
	if _, err := os.Stat(filepath.Join(home, "profiles", "jane")); err != nil {
		t.Fatalf("profile directory removed despite declined confirmation: %v", err)
	}
}

func TestRemoveYesSkipsConfirmation(t *testing.T) {
	home := removableHome(t)
	// Empty stdin: the command must not block or fail waiting for input.
	_, _, err := runRemove(home, "", "jane", "--yes")
	if err != nil {
		t.Fatalf("remove --yes failed: %v", err)
	}
	st, _ := state.Load(home)
	if _, ok := st.Profiles["jane"]; ok {
		t.Fatal("profile still recorded in state")
	}
}

func TestRemoveRejectsUnknownProfile(t *testing.T) {
	home := removableHome(t)
	_, _, err := runRemove(home, "yes\n", "nope")
	if err == nil {
		t.Fatal("removing an unknown profile must fail")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Fatalf("error should name the profile: %v", err)
	}
}
