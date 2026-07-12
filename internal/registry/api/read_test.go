package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"sherpa/internal/gitutil"
	registryauth "sherpa/internal/registry/auth"
	"sherpa/internal/registry/content"
	"sherpa/internal/registry/store"
)

func TestSearchReturnsMatchingStacks(t *testing.T) {
	st := newFakeStore()
	st.searchResults = []store.StackWithLatest{{
		Stack: store.Stack{
			Owner:      "alice",
			Name:       "reviewer",
			Summary:    "Review code with strict security checks",
			Harness:    "claude-code",
			ForkedFrom: "@origin/reviewer",
			Tags:       []string{"review", "security"},
		},
		Version:     2,
		PublishedAt: testTime(2),
	}}
	handler := New(st, content.NewBareGit(t.TempDir()), "", &registryauth.FakeGitHubClient{})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://registry.test/v1/search?q=sec&harness=claude-code&tag=review", nil)
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	if st.searchQ != "sec" || st.searchHarness != "claude-code" || st.searchTag != "review" {
		t.Fatalf("search args = (%q,%q,%q)", st.searchQ, st.searchHarness, st.searchTag)
	}

	var body struct {
		Stacks []struct {
			Ref        string   `json:"ref"`
			Name       string   `json:"name"`
			Owner      string   `json:"owner"`
			Summary    string   `json:"summary"`
			Tags       []string `json:"tags"`
			Harness    string   `json:"harness"`
			Version    int      `json:"version"`
			ForkedFrom string   `json:"forked_from"`
			RepoURL    string   `json:"repo_url"`
		} `json:"stacks"`
	}
	decodeJSON(t, rr, &body)
	if len(body.Stacks) != 1 {
		t.Fatalf("stacks len = %d, want 1: %#v", len(body.Stacks), body.Stacks)
	}
	got := body.Stacks[0]
	if got.Ref != "@alice/reviewer" || got.Name != "reviewer" || got.Owner != "alice" || got.Version != 2 {
		t.Fatalf("stack identity = %#v", got)
	}
	if got.Summary != "Review code with strict security checks" || got.Harness != "claude-code" {
		t.Fatalf("stack metadata = %#v", got)
	}
	if strings.Join(got.Tags, ",") != "review,security" {
		t.Fatalf("tags = %#v", got.Tags)
	}
	if got.ForkedFrom != "@origin/reviewer" {
		t.Fatalf("forked_from = %q", got.ForkedFrom)
	}
	if got.RepoURL != "http://registry.test/v1/stacks/alice/reviewer.git" {
		t.Fatalf("repo_url = %q", got.RepoURL)
	}
}

func TestStackDetailReturnsVersions(t *testing.T) {
	st := newFakeStore()
	st.stacks["alice/reviewer"] = store.Stack{
		Owner:      "alice",
		Name:       "reviewer",
		Summary:    "Review code",
		Harness:    "codex",
		ForkedFrom: "",
		Tags:       []string{"review"},
	}
	st.versions["alice/reviewer"] = []store.Version{
		{
			Version:     2,
			Changelog:   "Second release",
			ScanReport:  json.RawMessage(`{"findings":[{"path":"x"},{"path":"y"}]}`),
			PublishedAt: testTime(2),
		},
		{
			Version:     1,
			Changelog:   "Initial release",
			ScanReport:  json.RawMessage(`{"findings":[]}`),
			PublishedAt: testTime(1),
		},
	}
	handler := New(st, content.NewBareGit(t.TempDir()), "", &registryauth.FakeGitHubClient{})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://registry.test/v1/stacks/alice/reviewer", nil)
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Name       string   `json:"name"`
		Owner      string   `json:"owner"`
		Summary    string   `json:"summary"`
		Tags       []string `json:"tags"`
		Harness    string   `json:"harness"`
		ForkedFrom string   `json:"forked_from"`
		Versions   []struct {
			Version     int    `json:"version"`
			PublishedAt string `json:"published_at"`
			Changelog   string `json:"changelog"`
			ScanSummary string `json:"scan_summary"`
		} `json:"versions"`
	}
	decodeJSON(t, rr, &body)
	if body.Owner != "alice" || body.Name != "reviewer" || body.Summary != "Review code" || body.Harness != "codex" {
		t.Fatalf("stack detail = %#v", body)
	}
	if strings.Join(body.Tags, ",") != "review" {
		t.Fatalf("tags = %#v", body.Tags)
	}
	if len(body.Versions) != 2 {
		t.Fatalf("versions len = %d, want 2: %#v", len(body.Versions), body.Versions)
	}
	if body.Versions[0].Version != 2 || body.Versions[0].ScanSummary != "2 findings" {
		t.Fatalf("version 2 summary = %#v", body.Versions[0])
	}
	if body.Versions[1].Version != 1 || body.Versions[1].ScanSummary != "clean" {
		t.Fatalf("version 1 summary = %#v", body.Versions[1])
	}
}

func TestStackDetailMissingReturns404(t *testing.T) {
	handler := New(newFakeStore(), content.NewBareGit(t.TempDir()), "", &registryauth.FakeGitHubClient{})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://registry.test/v1/stacks/alice/missing", nil)
	handler.ServeHTTP(rr, req)

	assertJSONError(t, rr, http.StatusNotFound)
}

func TestVersionDetailReturnsManifestAndScanReport(t *testing.T) {
	st := newFakeStore()
	st.versionByKey["alice/reviewer/2"] = store.Version{
		Version:     2,
		GitTag:      "v2",
		Manifest:    json.RawMessage(`{"name":"reviewer","version":2}`),
		ScanReport:  json.RawMessage(`{"findings":[]}`),
		Changelog:   "Second release",
		PublishedAt: testTime(2),
	}
	handler := New(st, content.NewBareGit(t.TempDir()), "", &registryauth.FakeGitHubClient{})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://registry.test/v1/stacks/alice/reviewer/versions/2", nil)
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Version     int             `json:"version"`
		GitTag      string          `json:"git_tag"`
		Manifest    json.RawMessage `json:"manifest"`
		ScanReport  json.RawMessage `json:"scan_report"`
		Changelog   string          `json:"changelog"`
		PublishedAt string          `json:"published_at"`
	}
	decodeJSON(t, rr, &body)
	if body.Version != 2 || body.GitTag != "v2" || body.Changelog != "Second release" {
		t.Fatalf("version response = %#v", body)
	}
	if string(body.Manifest) != `{"name":"reviewer","version":2}` {
		t.Fatalf("manifest = %s", body.Manifest)
	}
	if string(body.ScanReport) != `{"findings":[]}` {
		t.Fatalf("scan_report = %s", body.ScanReport)
	}
}

func TestVersionDetailBadVersionReturns400(t *testing.T) {
	handler := New(newFakeStore(), content.NewBareGit(t.TempDir()), "", &registryauth.FakeGitHubClient{})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://registry.test/v1/stacks/alice/reviewer/versions/two", nil)
	handler.ServeHTTP(rr, req)

	assertJSONError(t, rr, http.StatusBadRequest)
}

func TestVersionDetailMissingReturns404(t *testing.T) {
	handler := New(newFakeStore(), content.NewBareGit(t.TempDir()), "", &registryauth.FakeGitHubClient{})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://registry.test/v1/stacks/alice/reviewer/versions/99", nil)
	handler.ServeHTTP(rr, req)

	assertJSONError(t, rr, http.StatusNotFound)
}

func TestGitRouteServesBareRepoInfoRefs(t *testing.T) {
	root := t.TempDir()
	cs := content.NewBareGit(filepath.Join(root, "content"))
	buildBareRepo(t, cs, root, "alice", "reviewer")
	handler := New(newFakeStore(), cs, "", &registryauth.FakeGitHubClient{})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://registry.test/v1/stacks/alice/reviewer.git/info/refs", nil)
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "refs/heads/main") {
		t.Fatalf("info/refs = %q, want refs/heads/main", rr.Body.String())
	}
}

func TestGitRouteMissingRepoReturns404(t *testing.T) {
	handler := New(newFakeStore(), content.NewBareGit(t.TempDir()), "", &registryauth.FakeGitHubClient{})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://registry.test/v1/stacks/alice/missing.git/info/refs", nil)
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body %s", rr.Code, rr.Body.String())
	}
}

type fakeStore struct {
	searchQ       string
	searchHarness string
	searchTag     string
	searchResults []store.StackWithLatest
	searchErr     error
	stacks        map[string]store.Stack
	versions      map[string][]store.Version
	versionByKey  map[string]store.Version
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		stacks:       map[string]store.Stack{},
		versions:     map[string][]store.Version{},
		versionByKey: map[string]store.Version{},
	}
}

func (f *fakeStore) UpsertUser(context.Context, string) (int64, error) {
	return 0, errors.New("not implemented")
}

func (f *fakeStore) UpsertUserGitHub(context.Context, string, int64) (int64, error) {
	return 0, store.ErrNotFound
}

func (f *fakeStore) CreateSession(context.Context, int64, string, time.Duration) error {
	return store.ErrNotFound
}

func (f *fakeStore) SessionUser(context.Context, string) (string, error) {
	return "", store.ErrNotFound
}

func (f *fakeStore) UpsertStack(context.Context, store.Stack) (int64, error) {
	return 0, errors.New("not implemented")
}

func (f *fakeStore) InsertVersion(context.Context, store.Version) error {
	return errors.New("not implemented")
}

func (f *fakeStore) Search(_ context.Context, q, harness, tag string) ([]store.StackWithLatest, error) {
	f.searchQ = q
	f.searchHarness = harness
	f.searchTag = tag
	return f.searchResults, f.searchErr
}

func (f *fakeStore) GetStack(_ context.Context, owner, name string) (store.Stack, []store.Version, error) {
	key := owner + "/" + name
	stack, ok := f.stacks[key]
	if !ok {
		return store.Stack{}, nil, store.ErrNotFound
	}
	return stack, f.versions[key], nil
}

func (f *fakeStore) GetVersion(_ context.Context, owner, name string, v int) (store.Version, error) {
	version, ok := f.versionByKey[owner+"/"+name+"/"+strconv.Itoa(v)]
	if !ok {
		return store.Version{}, store.ErrNotFound
	}
	return version, nil
}

func (f *fakeStore) Close() error {
	return nil
}

func buildBareRepo(t *testing.T, cs *content.BareGit, root, owner, name string) {
	t.Helper()

	src := filepath.Join(root, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatalf("mkdir source repo: %v", err)
	}
	runGit(t, src, "init", "-b", "main")
	runGit(t, src, "config", "user.email", "test@example.com")
	runGit(t, src, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(src, "stack.yaml"), []byte("name: reviewer\nversion: 1\n"), 0o644); err != nil {
		t.Fatalf("write stack.yaml: %v", err)
	}
	runGit(t, src, "add", "-A")
	runGit(t, src, "commit", "-m", "initial")

	repoPath := cs.RepoPath(owner, name)
	if err := os.MkdirAll(filepath.Dir(repoPath), 0o755); err != nil {
		t.Fatalf("mkdir bare parent: %v", err)
	}
	runGit(t, filepath.Dir(repoPath), "init", "--bare", repoPath)
	runGit(t, src, "remote", "add", "origin", repoPath)
	runGit(t, src, "push", "origin", "main")
	runGit(t, repoPath, "symbolic-ref", "HEAD", "refs/heads/main")
	runGit(t, repoPath, "update-server-info")
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if _, err := gitutil.Run(dir, args...); err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
}

func decodeJSON(t *testing.T, rr *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rr.Body.Bytes(), v); err != nil {
		t.Fatalf("decode JSON body %q: %v", rr.Body.String(), err)
	}
}

func assertJSONError(t *testing.T, rr *httptest.ResponseRecorder, status int) {
	t.Helper()
	if rr.Code != status {
		t.Fatalf("status = %d, want %d; body %s", rr.Code, status, rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	var body struct {
		Error string `json:"error"`
	}
	decodeJSON(t, rr, &body)
	if body.Error == "" {
		t.Fatalf("error body = %#v", body)
	}
}

func testTime(day int) time.Time {
	return time.Date(2026, 7, day, 12, 0, 0, 0, time.UTC)
}
