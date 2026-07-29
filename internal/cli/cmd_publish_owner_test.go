package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeStackManifest(t *testing.T, dir, contents string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "stack.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSetStackOwnerAddsTheKeyWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	path := writeStackManifest(t, dir, "name: reviewer\nversion: 1\nsummary: a stack\n")

	if err := setStackOwner(dir, "alice"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.Contains(got, "owner: alice") {
		t.Fatalf("owner not recorded:\n%s", got)
	}
	// The rest of the manifest must survive intact.
	for _, want := range []string{"name: reviewer", "version: 1", "summary: a stack"} {
		if !strings.Contains(got, want) {
			t.Errorf("manifest lost %q:\n%s", want, got)
		}
	}
}

func TestSetStackOwnerReplacesAnExistingValue(t *testing.T) {
	dir := t.TempDir()
	path := writeStackManifest(t, dir, "name: reviewer\nowner: stale\nversion: 2\n")

	if err := setStackOwner(dir, "alice"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	got := string(data)
	if strings.Contains(got, "stale") {
		t.Fatalf("previous owner retained:\n%s", got)
	}
	if !strings.Contains(got, "owner: alice") || !strings.Contains(got, "version: 2") {
		t.Fatalf("unexpected manifest:\n%s", got)
	}
}

func TestSetStackOwnerRejectsANonMappingManifest(t *testing.T) {
	dir := t.TempDir()
	writeStackManifest(t, dir, "- not\n- a mapping\n")
	if err := setStackOwner(dir, "alice"); err == nil {
		t.Fatal("a non-mapping manifest must be rejected rather than rewritten")
	}
}

func TestSetStackOwnerIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := writeStackManifest(t, dir, "name: reviewer\nversion: 1\n")

	if err := setStackOwner(dir, "alice"); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(path)
	if err := setStackOwner(dir, "alice"); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(path)
	if string(first) != string(second) {
		t.Fatalf("repeated writes changed the manifest:\n%s\n---\n%s", first, second)
	}
}
