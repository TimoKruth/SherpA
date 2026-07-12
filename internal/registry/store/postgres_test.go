package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestPostgresStoreContract(t *testing.T) {
	dsn := StartPostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	store, err := OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatalf("open postgres store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("close store: %v", err)
		}
	})

	userID, err := store.UpsertUser(ctx, "alice")
	if err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	if userID == 0 {
		t.Fatalf("user id = 0")
	}

	stackID, err := store.UpsertStack(ctx, Stack{
		Owner:      "alice",
		Name:       "reviewer",
		Summary:    "Review code with strict security checks",
		Harness:    "claude-code",
		ForkedFrom: "origin/reviewer",
		Tags:       []string{"review", "security"},
	})
	if err != nil {
		t.Fatalf("upsert stack: %v", err)
	}
	if stackID == 0 {
		t.Fatalf("stack id = 0")
	}

	if err := store.InsertVersion(ctx, Version{
		StackID:    stackID,
		Version:    1,
		GitTag:     "v1",
		Manifest:   json.RawMessage(`{"name":"reviewer","version":1}`),
		ScanReport: json.RawMessage(`{"findings":[]}`),
		Changelog:  "Initial release",
	}); err != nil {
		t.Fatalf("insert version 1: %v", err)
	}
	if err := store.InsertVersion(ctx, Version{
		StackID:    stackID,
		Version:    2,
		GitTag:     "v2",
		Manifest:   json.RawMessage(`{"name":"reviewer","version":2}`),
		ScanReport: json.RawMessage(`{"findings":[]}`),
		Changelog:  "Second release",
	}); err != nil {
		t.Fatalf("insert version 2: %v", err)
	}
	if err := store.InsertVersion(ctx, Version{
		StackID:    stackID,
		Version:    2,
		GitTag:     "v2",
		Manifest:   json.RawMessage(`{"name":"reviewer","version":2}`),
		ScanReport: json.RawMessage(`{"findings":[]}`),
	}); !errors.Is(err, ErrVersionExists) {
		t.Fatalf("duplicate insert error = %v, want %v", err, ErrVersionExists)
	}

	matches, err := store.Search(ctx, "SECUR", "CLAUDE-CODE", "")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("search returned %d matches, want 1: %#v", len(matches), matches)
	}
	if matches[0].Owner != "alice" || matches[0].Name != "reviewer" || matches[0].Version != 2 {
		t.Fatalf("search match = %#v, want alice/reviewer latest v2", matches[0])
	}

	tagMatches, err := store.Search(ctx, "", "", "review")
	if err != nil {
		t.Fatalf("tag search: %v", err)
	}
	if len(tagMatches) != 1 || tagMatches[0].Version != 2 {
		t.Fatalf("tag search = %#v, want one latest v2 match", tagMatches)
	}

	noMatches, err := store.Search(ctx, "security", "codex", "")
	if err != nil {
		t.Fatalf("filtered search: %v", err)
	}
	if len(noMatches) != 0 {
		t.Fatalf("filtered search returned %#v, want none", noMatches)
	}

	stack, versions, err := store.GetStack(ctx, "alice", "reviewer")
	if err != nil {
		t.Fatalf("get stack: %v", err)
	}
	if stack.ID != stackID || stack.Owner != "alice" || stack.Name != "reviewer" {
		t.Fatalf("stack = %#v, want alice/reviewer id %d", stack, stackID)
	}
	if len(versions) != 2 || versions[0].Version != 2 || versions[1].Version != 1 {
		t.Fatalf("versions = %#v, want newest-first v2,v1", versions)
	}

	version, err := store.GetVersion(ctx, "alice", "reviewer", 1)
	if err != nil {
		t.Fatalf("get version: %v", err)
	}
	if version.StackID != stackID || version.Version != 1 || version.GitTag != "v1" {
		t.Fatalf("version = %#v, want stack %d v1 tag v1", version, stackID)
	}

	if _, _, err := store.GetStack(ctx, "alice", "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing stack error = %v, want %v", err, ErrNotFound)
	}
}
