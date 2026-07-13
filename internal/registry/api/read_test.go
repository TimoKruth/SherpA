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
		TrustTier:   "linked",
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
	if st.searchMaxRows != 0 || st.searchOffset != 0 {
		t.Fatalf("search pagination = (%d,%d), want unbounded compatibility", st.searchMaxRows, st.searchOffset)
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
			TrustTier  string   `json:"trust_tier"`
			ForkedFrom string   `json:"forked_from"`
			RepoURL    string   `json:"repo_url"`
		} `json:"stacks"`
	}
	decodeJSON(t, rr, &body)
	if strings.Contains(rr.Body.String(), "next_offset") {
		t.Fatalf("unbounded response unexpectedly contains next_offset: %s", rr.Body.String())
	}
	if len(body.Stacks) != 1 {
		t.Fatalf("stacks len = %d, want 1: %#v", len(body.Stacks), body.Stacks)
	}
	got := body.Stacks[0]
	if got.Ref != "@alice/reviewer" || got.Name != "reviewer" || got.Owner != "alice" || got.Version != 2 || got.TrustTier != "linked" {
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

func TestSearchPaginationUsesSentinelAndNextOffset(t *testing.T) {
	st := newFakeStore()
	for _, name := range []string{"one", "two", "three"} {
		st.searchResults = append(st.searchResults, store.StackWithLatest{Stack: store.Stack{Owner: "alice", Name: name}})
	}
	handler := New(st, content.NewBareGit(t.TempDir()), "", &registryauth.FakeGitHubClient{})
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "http://registry.test/v1/search?limit=2&offset=1", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Stacks     []searchStackResponse `json:"stacks"`
		NextOffset *int                  `json:"next_offset"`
	}
	decodeJSON(t, rr, &body)
	if len(body.Stacks) != 2 || body.NextOffset == nil || *body.NextOffset != 3 {
		t.Fatalf("page = %#v, next = %v", body.Stacks, body.NextOffset)
	}
	if st.searchMaxRows != 3 || st.searchOffset != 1 {
		t.Fatalf("store pagination = (%d,%d), want (3,1)", st.searchMaxRows, st.searchOffset)
	}
}

func TestSearchPaginationAcceptsMaximumLimit(t *testing.T) {
	st := newFakeStore()
	handler := New(st, content.NewBareGit(t.TempDir()), "", &registryauth.FakeGitHubClient{})
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "http://registry.test/v1/search?limit=50", nil))
	if rr.Code != http.StatusOK || st.searchMaxRows != 51 {
		t.Fatalf("status = %d, store max rows = %d; want 200, 51", rr.Code, st.searchMaxRows)
	}
}

func TestReadPaginationRejectsInvalidParameters(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{"search zero limit", "/v1/search?limit=0"},
		{"search negative limit", "/v1/search?limit=-1"},
		{"search over max", "/v1/search?limit=51"},
		{"search non integer", "/v1/search?limit=two"},
		{"search negative offset", "/v1/search?limit=2&offset=-1"},
		{"search offset without limit", "/v1/search?offset=1"},
		{"versions zero limit", "/v1/stacks/alice/reviewer?versions_limit=0"},
		{"versions over max", "/v1/stacks/alice/reviewer?versions_limit=51"},
		{"versions non integer", "/v1/stacks/alice/reviewer?versions_limit=two"},
		{"versions negative offset", "/v1/stacks/alice/reviewer?versions_limit=2&versions_offset=-1"},
		{"versions offset without limit", "/v1/stacks/alice/reviewer?versions_offset=1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := New(newFakeStore(), content.NewBareGit(t.TempDir()), "", &registryauth.FakeGitHubClient{})
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "http://registry.test"+tc.path, nil))
			assertJSONError(t, rr, http.StatusBadRequest)
		})
	}
}

func TestSearchRepoURLUsesPinnedPublicBase(t *testing.T) {
	handler := searchHandlerWithOptions(t, Options{
		PublicBaseURL: "https://registry.example/prefix",
		TrustProxy:    true,
	})
	req := httptest.NewRequest(http.MethodGet, "http://evil.example/v1/search", nil)
	req.Host = "evil.example"
	req.Header.Set("X-Forwarded-Host", "attacker.example")
	req.Header.Set("X-Forwarded-Proto", "http")

	if got := searchRepoURL(t, handler, req); got != "https://registry.example/prefix/v1/stacks/alice/reviewer.git" {
		t.Fatalf("repo_url = %q", got)
	}
}

func TestSearchRepoURLFallbackProxyHandling(t *testing.T) {
	for _, tc := range []struct {
		name       string
		trustProxy bool
		forwarded  string
		requestURL string
		want       string
	}{
		{name: "trusted HTTPS scheme", trustProxy: true, forwarded: "https", requestURL: "http://registry.internal/v1/search", want: "https://registry.internal/v1/stacks/alice/reviewer.git"},
		{name: "untrusted forwarded scheme", trustProxy: false, forwarded: "https", requestURL: "http://registry.internal/v1/search", want: "http://registry.internal/v1/stacks/alice/reviewer.git"},
		{name: "invalid forwarded scheme", trustProxy: true, forwarded: "ftp", requestURL: "http://registry.internal/v1/search", want: "http://registry.internal/v1/stacks/alice/reviewer.git"},
		{name: "direct TLS wins without proxy trust", trustProxy: false, forwarded: "http", requestURL: "https://registry.internal/v1/search", want: "https://registry.internal/v1/stacks/alice/reviewer.git"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := searchHandlerWithOptions(t, Options{TrustProxy: tc.trustProxy})
			req := httptest.NewRequest(http.MethodGet, tc.requestURL, nil)
			req.Header.Set("X-Forwarded-Proto", tc.forwarded)
			req.Header.Set("X-Forwarded-Host", "attacker.example")
			if got := searchRepoURL(t, handler, req); got != tc.want {
				t.Fatalf("repo_url = %q, want %q", got, tc.want)
			}
		})
	}
}

func searchHandlerWithOptions(t *testing.T, options Options) http.Handler {
	t.Helper()
	st := newFakeStore()
	st.searchResults = []store.StackWithLatest{{
		Stack:   store.Stack{Owner: "alice", Name: "reviewer"},
		Version: 1,
	}}
	return NewWithOptions(st, content.NewBareGit(t.TempDir()), "", &registryauth.FakeGitHubClient{}, options)
}

func searchRepoURL(t *testing.T, handler http.Handler, req *http.Request) string {
	t.Helper()
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("search status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Stacks []struct {
			RepoURL string `json:"repo_url"`
		} `json:"stacks"`
	}
	decodeJSON(t, rr, &body)
	if len(body.Stacks) != 1 {
		t.Fatalf("stacks = %#v", body.Stacks)
	}
	return body.Stacks[0].RepoURL
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
			TrustTier:   "linked",
			Changelog:   "Second release",
			ScanReport:  json.RawMessage(`{"findings":[{"path":"x"},{"path":"y"}]}`),
			PublishedAt: testTime(2),
		},
		{
			Version:     1,
			TrustTier:   "linked",
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
			TrustTier   string `json:"trust_tier"`
		} `json:"versions"`
	}
	decodeJSON(t, rr, &body)
	if strings.Contains(rr.Body.String(), "next_versions_offset") {
		t.Fatalf("unbounded response unexpectedly contains next_versions_offset: %s", rr.Body.String())
	}
	if body.Owner != "alice" || body.Name != "reviewer" || body.Summary != "Review code" || body.Harness != "codex" {
		t.Fatalf("stack detail = %#v", body)
	}
	if strings.Join(body.Tags, ",") != "review" {
		t.Fatalf("tags = %#v", body.Tags)
	}
	if len(body.Versions) != 2 {
		t.Fatalf("versions len = %d, want 2: %#v", len(body.Versions), body.Versions)
	}
	if st.stackMaxVersions != 0 || st.stackOffset != 0 {
		t.Fatalf("stack pagination = (%d,%d), want unbounded compatibility", st.stackMaxVersions, st.stackOffset)
	}
	if body.Versions[0].Version != 2 || body.Versions[0].ScanSummary != "2 findings" || body.Versions[0].TrustTier != "linked" {
		t.Fatalf("version 2 summary = %#v", body.Versions[0])
	}
	if body.Versions[1].Version != 1 || body.Versions[1].ScanSummary != "clean" {
		t.Fatalf("version 1 summary = %#v", body.Versions[1])
	}
}

func TestStackVersionPaginationUsesSentinelAndNextOffset(t *testing.T) {
	st := newFakeStore()
	st.stacks["alice/reviewer"] = store.Stack{Owner: "alice", Name: "reviewer"}
	st.versions["alice/reviewer"] = []store.Version{{Version: 4}, {Version: 3}, {Version: 2}}
	handler := New(st, content.NewBareGit(t.TempDir()), "", &registryauth.FakeGitHubClient{})
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "http://registry.test/v1/stacks/alice/reviewer?versions_limit=2&versions_offset=1", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Versions           []versionSummary `json:"versions"`
		NextVersionsOffset *int             `json:"next_versions_offset"`
	}
	decodeJSON(t, rr, &body)
	if len(body.Versions) != 2 || body.NextVersionsOffset == nil || *body.NextVersionsOffset != 3 {
		t.Fatalf("versions = %#v, next = %v", body.Versions, body.NextVersionsOffset)
	}
	if st.stackMaxVersions != 3 || st.stackOffset != 1 {
		t.Fatalf("store pagination = (%d,%d), want (3,1)", st.stackMaxVersions, st.stackOffset)
	}
}

func TestDetailRepoURLsUsePinnedPublicBase(t *testing.T) {
	st := newFakeStore()
	st.stacks["alice/reviewer"] = store.Stack{Owner: "alice", Name: "reviewer"}
	st.versionByKey["alice/reviewer/2"] = store.Version{Version: 2}
	handler := NewWithOptions(st, content.NewBareGit(t.TempDir()), "", &registryauth.FakeGitHubClient{}, Options{PublicBaseURL: "https://registry.example/prefix", TrustProxy: true})
	for _, path := range []string{"/v1/stacks/alice/reviewer", "/v1/stacks/alice/reviewer/versions/2"} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "http://evil.example"+path, nil)
		req.Host = "evil.example"
		req.Header.Set("X-Forwarded-Host", "attacker.example")
		req.Header.Set("X-Forwarded-Proto", "http")
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s status = %d, body %s", path, rr.Code, rr.Body.String())
		}
		var body struct {
			RepoURL string `json:"repo_url"`
		}
		decodeJSON(t, rr, &body)
		if body.RepoURL != "https://registry.example/prefix/v1/stacks/alice/reviewer.git" {
			t.Fatalf("%s repo_url = %q", path, body.RepoURL)
		}
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
		TrustTier:   "linked",
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
		TrustTier   string          `json:"trust_tier"`
	}
	decodeJSON(t, rr, &body)
	if body.Version != 2 || body.GitTag != "v2" || body.Changelog != "Second release" || body.TrustTier != "linked" {
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
	searchQ          string
	searchHarness    string
	searchTag        string
	searchResults    []store.StackWithLatest
	searchErr        error
	searchMaxRows    int
	searchOffset     int
	stackMaxVersions int
	stackOffset      int
	stacks           map[string]store.Stack
	versions         map[string][]store.Version
	versionByKey     map[string]store.Version
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

func (f *fakeStore) Search(_ context.Context, q, harness, tag string, maxRows, offset int) ([]store.StackWithLatest, error) {
	f.searchQ = q
	f.searchHarness = harness
	f.searchTag = tag
	f.searchMaxRows = maxRows
	f.searchOffset = offset
	return f.searchResults, f.searchErr
}

func (f *fakeStore) GetStack(_ context.Context, owner, name string, maxVersions, offset int) (store.Stack, []store.Version, error) {
	f.stackMaxVersions = maxVersions
	f.stackOffset = offset
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

func (f *fakeStore) AllVersionRefs(context.Context) ([]store.VersionRef, error) {
	return nil, nil
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
