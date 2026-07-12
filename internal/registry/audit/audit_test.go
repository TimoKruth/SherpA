package audit

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"sherpa/internal/gitutil"
	"sherpa/internal/registry/content"
	"sherpa/internal/registry/store"
)

func TestRunWithPostgresAndBareGit(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenPostgres(ctx, store.StartPostgres(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	stackID, err := st.UpsertStack(ctx, store.Stack{Owner: "alice", Name: "reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.InsertVersion(ctx, store.Version{StackID: stackID, Version: 1, GitTag: "v1"}); err != nil {
		t.Fatal(err)
	}
	missingID, err := st.UpsertStack(ctx, store.Stack{Owner: "zoe", Name: "writer"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.InsertVersion(ctx, store.Version{StackID: missingID, Version: 2, GitTag: "v2"}); err != nil {
		t.Fatal(err)
	}

	cs := content.NewBareGit(t.TempDir())
	pushFixtureTag(t, cs, "alice", "reviewer", "v1")
	pushFixtureTag(t, cs, "orphan", "standalone", "v9")

	report, err := Run(ctx, st, cs)
	if err != nil {
		t.Fatal(err)
	}
	wantMissing := []store.VersionRef{{Owner: "zoe", Name: "writer", Version: 2, GitTag: "v2"}}
	if !reflect.DeepEqual(report.MissingContent, wantMissing) {
		t.Fatalf("MissingContent = %#v, want %#v", report.MissingContent, wantMissing)
	}
	if !reflect.DeepEqual(report.ExtraTags, []string{"orphan/standalone:v9"}) {
		t.Fatalf("ExtraTags = %#v, want orphan/standalone:v9", report.ExtraTags)
	}
}

func pushFixtureTag(t *testing.T, cs *content.BareGit, owner, name, tag string) {
	t.Helper()
	src := t.TempDir()
	runGit(t, src, "init", "-b", "main")
	runGit(t, src, "config", "user.email", "test@example.com")
	runGit(t, src, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(src, "fixture"), []byte(owner+name+tag), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, src, "add", "fixture")
	runGit(t, src, "commit", "-m", "fixture")
	runGit(t, src, "tag", tag)
	if err := cs.EnsureRepo(owner, name); err != nil {
		t.Fatal(err)
	}
	runGit(t, src, "push", cs.RepoPath(owner, name), "main", tag)
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if _, err := gitutil.Run(dir, args...); err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
}

func TestRunReportsMissingContentAndExtraTagsDeterministically(t *testing.T) {
	st := fakeStore{refs: []store.VersionRef{
		{Owner: "zoe", Name: "writer", Version: 2, GitTag: "v2"},
		{Owner: "alice", Name: "reviewer", Version: 1, GitTag: "v1"},
	}}
	cs := fakeContent{
		commits: map[string]string{"alice/reviewer:v1": "abc"},
		repos: []content.RepositoryRef{
			{Owner: "orphan", Name: "standalone"},
			{Owner: "alice", Name: "reviewer"},
		},
		tags: map[string][]string{
			"alice/reviewer":    {"v1", "untracked"},
			"orphan/standalone": {"v9"},
		},
	}

	report, err := Run(context.Background(), st, cs)
	if err != nil {
		t.Fatal(err)
	}
	wantMissing := []store.VersionRef{{Owner: "zoe", Name: "writer", Version: 2, GitTag: "v2"}}
	if !reflect.DeepEqual(report.MissingContent, wantMissing) {
		t.Fatalf("MissingContent = %#v, want %#v", report.MissingContent, wantMissing)
	}
	wantExtra := []string{"alice/reviewer:untracked", "orphan/standalone:v9"}
	if !reflect.DeepEqual(report.ExtraTags, wantExtra) {
		t.Fatalf("ExtraTags = %#v, want %#v", report.ExtraTags, wantExtra)
	}
}

func TestRunConsistent(t *testing.T) {
	ref := store.VersionRef{Owner: "alice", Name: "reviewer", Version: 1, GitTag: "v1"}
	report, err := Run(context.Background(), fakeStore{refs: []store.VersionRef{ref}}, fakeContent{
		commits: map[string]string{"alice/reviewer:v1": "abc"},
		repos:   []content.RepositoryRef{{Owner: "alice", Name: "reviewer"}},
		tags:    map[string][]string{"alice/reviewer": {"v1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.MissingContent) != 0 || len(report.ExtraTags) != 0 {
		t.Fatalf("report = %#v, want empty", report)
	}
}

func TestRunPropagatesOperationalContentError(t *testing.T) {
	ref := store.VersionRef{Owner: "alice", Name: "reviewer", Version: 1, GitTag: "v1"}
	_, err := Run(context.Background(), fakeStore{refs: []store.VersionRef{ref}}, fakeContent{commitErr: errors.New("disk failure")})
	if err == nil {
		t.Fatal("Run error = nil")
	}
}

type fakeStore struct {
	refs []store.VersionRef
	err  error
}

func (f fakeStore) AllVersionRefs(context.Context) ([]store.VersionRef, error) {
	return f.refs, f.err
}

type fakeContent struct {
	commits   map[string]string
	repos     []content.RepositoryRef
	tags      map[string][]string
	commitErr error
}

func (f fakeContent) TagCommit(owner, name, tag string) (string, error) {
	if f.commitErr != nil {
		return "", f.commitErr
	}
	commit, ok := f.commits[owner+"/"+name+":"+tag]
	if !ok {
		return "", content.ErrNotFound
	}
	return commit, nil
}

func (f fakeContent) ListRepositories() ([]content.RepositoryRef, error) { return f.repos, nil }
func (f fakeContent) ListTags(owner, name string) ([]string, error) {
	return f.tags[owner+"/"+name], nil
}
