package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime/multipart"
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

func TestPublishBundleLimitRemains50MiB(t *testing.T) {
	if maxPublishBundleBytes != 50<<20 {
		t.Fatalf("maxPublishBundleBytes = %d, want %d", maxPublishBundleBytes, 50<<20)
	}
}

func TestPublishRequiresBearerTokenBeforeWrites(t *testing.T) {
	tests := []struct {
		name   string
		header string
	}{
		{name: "missing"},
		{name: "blank bearer", header: "Bearer "},
		{name: "wrong bearer", header: "Bearer wrong"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := newPublishSpyStore()
			cs := newPublishSpyContent(t)
			handler := New(st, cs, "registry-token", &registryauth.FakeGitHubClient{})

			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "http://registry.test/v1/stacks/o/n/versions", nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			handler.ServeHTTP(rr, req)

			assertJSONError(t, rr, http.StatusUnauthorized)
			assertNoPublishWrites(t, st, cs)
		})
	}
}

func TestPublishAuthorizationMatrixBeforeStaging(t *testing.T) {
	valid := "session-token"
	for _, tc := range []struct {
		name   string
		owner  string
		token  string
		status int
	}{
		{"missing", "alice", "", http.StatusUnauthorized},
		{"invalid", "alice", "bad", http.StatusUnauthorized},
		{"expired", "alice", "expired", http.StatusUnauthorized},
		{"wrong owner", "bob", valid, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newPublishSpyStore()
			st.sessionUsers[registryauth.HashToken(valid)] = "alice"
			cs := newPublishSpyContent(t)
			h := New(st, cs, "admin", &registryauth.FakeGitHubClient{})
			rr := postBundle(t, h, tc.owner, "n", []byte("not staged"), tc.token)
			if rr.Code != tc.status {
				t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
			}
			assertNoPublishWrites(t, st, cs)
			if cs.stageCalls != 0 {
				t.Fatalf("StageBundle calls = %d", cs.stageCalls)
			}
		})
	}
}

func TestPublishTrustTierByCredential(t *testing.T) {
	for _, tc := range []struct {
		name  string
		owner string
		token string
		tier  string
	}{
		{"admin any owner", "org", "admin", "unreviewed"},
		{"session own login", "alice", "session-token", "linked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newPublishSpyStore()
			st.sessionUsers[registryauth.HashToken("session-token")] = "alice"
			h := New(st, newPublishSpyContent(t), "admin", &registryauth.FakeGitHubClient{})
			bundle := buildPublishBundle(t, map[string]string{
				"stack.yaml": "name: n\nowner: " + tc.owner + "\nversion: 1\nharness: codex\n",
				"README.md":  "clean\n",
			})
			rr := postBundle(t, h, tc.owner, "n", bundle, tc.token)
			if rr.Code != http.StatusCreated {
				t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
			}
			if got := st.insertedVersions[0].TrustTier; got != tc.tier {
				t.Fatalf("TrustTier = %q", got)
			}
		})
	}
}

func TestMatchingOrphanTagIsAdopted(t *testing.T) {
	st := newPublishSpyStore()
	cs := newPublishSpyContent(t)
	h := New(st, cs, "admin", &registryauth.FakeGitHubClient{})
	bundle := buildPublishBundle(t, map[string]string{
		"stack.yaml": "name: n\nowner: o\nversion: 1\nharness: codex\n",
		"README.md":  "clean\n",
	})
	first := postBundle(t, h, "o", "n", bundle, "admin")
	if first.Code != http.StatusCreated {
		t.Fatalf("first = %d %s", first.Code, first.Body.String())
	}
	delete(st.versionByKey, "o/n/1")
	st.versions["o/n"] = nil
	st.insertVersionCalls = 0
	st.insertedVersions = nil
	second := postBundle(t, h, "o", "n", bundle, "admin")
	if second.Code != http.StatusCreated {
		t.Fatalf("adopt = %d %s", second.Code, second.Body.String())
	}
	if cs.commitCalls != 1 || st.insertVersionCalls != 1 {
		t.Fatalf("commit=%d insert=%d", cs.commitCalls, st.insertVersionCalls)
	}
}

func TestConflictingOrphanTagReturns409(t *testing.T) {
	st := newPublishSpyStore()
	cs := newPublishSpyContent(t)
	cs.tagCommits["o/n/v1"] = "different"
	h := New(st, cs, "admin", &registryauth.FakeGitHubClient{})
	bundle := buildPublishBundle(t, map[string]string{
		"stack.yaml": "name: n\nowner: o\nversion: 1\nharness: codex\n",
		"README.md":  "clean\n",
	})
	rr := postBundle(t, h, "o", "n", bundle, "admin")
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "orphan tag") {
		t.Fatalf("response = %d %s", rr.Code, rr.Body.String())
	}
	assertNoPublishWrites(t, st, cs)
}

func TestPublishInternalErrorIsLogged(t *testing.T) {
	st := newPublishSpyStore()
	st.getVersionErr = errors.New("database unavailable")
	var logs bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(old) })
	h := New(st, newPublishSpyContent(t), "admin", &registryauth.FakeGitHubClient{})
	bundle := buildPublishBundle(t, map[string]string{"stack.yaml": "name: n\nowner: o\nversion: 1\nharness: codex\n"})
	rr := postBundle(t, h, "o", "n", bundle, "admin")
	if rr.Code != http.StatusInternalServerError || !strings.Contains(logs.String(), "database unavailable") {
		t.Fatalf("response=%d logs=%q", rr.Code, logs.String())
	}
	if strings.Contains(rr.Body.String(), "database unavailable") {
		t.Fatalf("client leaked internal error: %s", rr.Body.String())
	}
}

func TestPublishBlocksTrackedSecretFailClosed(t *testing.T) {
	st := newPublishSpyStore()
	cs := newPublishSpyContent(t)
	handler := New(st, cs, "registry-token", &registryauth.FakeGitHubClient{})

	bundle := buildPublishBundle(t, map[string]string{
		"stack.yaml":    "name: n\nowner: o\nversion: 1\nharness: codex\nsummary: clean summary\n",
		"settings.json": `{"token":"ghp_` + strings.Repeat("x", 36) + `"}` + "\n",
	})
	rr := postBundle(t, handler, "o", "n", bundle, "registry-token")

	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Findings []publishFinding `json:"findings"`
	}
	decodeJSON(t, rr, &body)
	if len(body.Findings) == 0 {
		t.Fatalf("findings = %#v, want at least one", body.Findings)
	}
	assertFinding(t, body.Findings, "settings.json", "secret")
	assertNoPublishWrites(t, st, cs)
}

func TestPublishBlocksCodexSetupStateFailClosed(t *testing.T) {
	st := newPublishSpyStore()
	cs := newPublishSpyContent(t)
	handler := New(st, cs, "registry-token", &registryauth.FakeGitHubClient{})

	bundle := buildPublishBundle(t, map[string]string{
		"stack.yaml": "name: n\nowner: o\nversion: 1\nharness: codex\nsummary: clean summary\n",
		"auth.json":  `{"access_token":"local-token"}` + "\n",
	})
	rr := postBundle(t, handler, "o", "n", bundle, "registry-token")

	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Findings []publishFinding `json:"findings"`
	}
	decodeJSON(t, rr, &body)
	if len(body.Findings) == 0 {
		t.Fatalf("findings = %#v, want at least one", body.Findings)
	}
	assertFinding(t, body.Findings, "auth.json", "setup-state")
	assertNoPublishWrites(t, st, cs)
}

func TestPublishRejectsVersionTagThatDoesNotPointAtPublishedHeadBeforeWrites(t *testing.T) {
	st := newPublishSpyStore()
	cs := newPublishSpyContent(t)
	handler := New(st, cs, "registry-token", &registryauth.FakeGitHubClient{})

	bundle := buildPublishBundleWithMismatchedVersionTag(t)
	rr := postBundle(t, handler, "o", "n", bundle, "registry-token")

	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "version tag v1 does not point at the published HEAD") {
		t.Fatalf("body = %s, want tag mismatch validation", rr.Body.String())
	}
	assertNoPublishWrites(t, st, cs)
}

func TestPublishRejectsExistingVersionBeforeWrites(t *testing.T) {
	st := newPublishSpyStore()
	st.versionByKey["o/n/1"] = store.Version{Version: 1, GitTag: "v1"}
	cs := newPublishSpyContent(t)
	handler := New(st, cs, "registry-token", &registryauth.FakeGitHubClient{})

	bundle := buildPublishBundle(t, map[string]string{
		"stack.yaml": "name: n\nowner: o\nversion: 1\nharness: codex\nsummary: clean summary\n",
		"README.md":  "clean\n",
	})
	rr := postBundle(t, handler, "o", "n", bundle, "registry-token")

	assertJSONError(t, rr, http.StatusConflict)
	assertNoPublishWrites(t, st, cs)
}

func TestPublishCleanBundleCommitsMetadataAndAppearsInDetail(t *testing.T) {
	st := newPublishSpyStore()
	cs := newPublishSpyContent(t)
	handler := New(st, cs, "registry-token", &registryauth.FakeGitHubClient{})

	bundle := buildPublishBundle(t, map[string]string{
		"stack.yaml": "name: n\nowner: o\nversion: 1\nharness: codex\nsummary: clean summary\ntags:\n  - review\n",
		"README.md":  "clean\n",
	})
	rr := postBundle(t, handler, "o", "n", bundle, "registry-token")

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", rr.Code, rr.Body.String())
	}
	if cs.commitCalls != 1 {
		t.Fatalf("ContentStore.Commit calls = %d, want 1", cs.commitCalls)
	}
	if st.insertVersionCalls != 1 {
		t.Fatalf("Store.InsertVersion calls = %d, want 1", st.insertVersionCalls)
	}
	inserted := st.insertedVersions[0]
	if inserted.Version != 1 || inserted.GitTag != "v1" {
		t.Fatalf("inserted version = %#v", inserted)
	}
	if len(inserted.Manifest) == 0 {
		t.Fatal("inserted manifest is empty")
	}
	var manifest map[string]any
	if err := json.Unmarshal(inserted.Manifest, &manifest); err != nil {
		t.Fatalf("manifest JSON = %s: %v", inserted.Manifest, err)
	}
	if manifest["name"] != "n" {
		t.Fatalf("manifest snapshot = %#v, want name n", manifest)
	}
	var report struct {
		Findings []json.RawMessage `json:"findings"`
	}
	if err := json.Unmarshal(inserted.ScanReport, &report); err != nil {
		t.Fatalf("scan report JSON = %s: %v", inserted.ScanReport, err)
	}
	if len(report.Findings) != 0 {
		t.Fatalf("scan report findings = %#v, want clean", report.Findings)
	}

	var publishBody struct {
		Version int    `json:"version"`
		GitTag  string `json:"git_tag"`
	}
	decodeJSON(t, rr, &publishBody)
	if publishBody.Version != 1 || publishBody.GitTag != "v1" {
		t.Fatalf("publish response = %#v", publishBody)
	}

	detail := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://registry.test/v1/stacks/o/n", nil)
	handler.ServeHTTP(detail, req)
	if detail.Code != http.StatusOK {
		t.Fatalf("detail status = %d, body %s", detail.Code, detail.Body.String())
	}
	var detailBody struct {
		Owner    string `json:"owner"`
		Name     string `json:"name"`
		Versions []struct {
			Version     int    `json:"version"`
			ScanSummary string `json:"scan_summary"`
		} `json:"versions"`
	}
	decodeJSON(t, detail, &detailBody)
	if detailBody.Owner != "o" || detailBody.Name != "n" || len(detailBody.Versions) != 1 {
		t.Fatalf("detail body = %#v", detailBody)
	}
	if detailBody.Versions[0].Version != 1 || detailBody.Versions[0].ScanSummary != "clean" {
		t.Fatalf("detail version = %#v", detailBody.Versions[0])
	}
}

func TestPublishResponseRepoURLUsesPinnedPublicBase(t *testing.T) {
	st := newPublishSpyStore()
	handler := NewWithOptions(st, newPublishSpyContent(t), "admin", &registryauth.FakeGitHubClient{}, Options{
		PublicBaseURL: "https://registry.example/prefix",
		TrustProxy:    true,
	})
	bundle := buildPublishBundle(t, map[string]string{
		"stack.yaml": "name: n\nowner: o\nversion: 1\nharness: codex\n",
		"README.md":  "clean\n",
	})
	body, contentType := multipartBundle(t, bundle)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://evil.example/v1/stacks/o/n/versions", body)
	req.Host = "evil.example"
	req.Header.Set("X-Forwarded-Host", "attacker.example")
	req.Header.Set("X-Forwarded-Proto", "http")
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer admin")
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	var response struct {
		RepoURL string `json:"repo_url"`
	}
	decodeJSON(t, rr, &response)
	if response.RepoURL != "https://registry.example/prefix/v1/stacks/o/n.git" {
		t.Fatalf("repo_url = %q", response.RepoURL)
	}
}

type publishSpyContent struct {
	inner       *content.BareGit
	stageCalls  int
	commitCalls int
	tagCommits  map[string]string
}

func newPublishSpyContent(t *testing.T) *publishSpyContent {
	t.Helper()
	return &publishSpyContent{
		inner:      content.NewBareGit(filepath.Join(t.TempDir(), "content")),
		tagCommits: map[string]string{},
	}
}

func (p *publishSpyContent) EnsureRepo(owner, name string) error {
	return p.inner.EnsureRepo(owner, name)
}

func (p *publishSpyContent) StageBundle(bundle []byte, gitTag string) (string, string, func(), error) {
	p.stageCalls++
	return p.inner.StageBundle(bundle, gitTag)
}

func (p *publishSpyContent) Commit(owner, name, stageDir string) error {
	p.commitCalls++
	if err := p.inner.Commit(owner, name, stageDir); err != nil {
		return err
	}
	if commit, err := p.inner.TagCommit(owner, name, "v1"); err == nil {
		p.tagCommits[owner+"/"+name+"/v1"] = commit
	}
	return nil
}

func (p *publishSpyContent) TagCommit(owner, name, tag string) (string, error) {
	if commit, ok := p.tagCommits[owner+"/"+name+"/"+tag]; ok {
		return commit, nil
	}
	return p.inner.TagCommit(owner, name, tag)
}

func (p *publishSpyContent) ListRepositories() ([]content.RepositoryRef, error) {
	return p.inner.ListRepositories()
}

func (p *publishSpyContent) ListTags(owner, name string) ([]string, error) {
	return p.inner.ListTags(owner, name)
}

func (p *publishSpyContent) RepoPath(owner, name string) string {
	return p.inner.RepoPath(owner, name)
}

type publishSpyStore struct {
	upsertUserCalls    int
	upsertStackCalls   int
	insertVersionCalls int
	nextUserID         int64
	nextStackID        int64
	stackIDs           map[string]int64
	stacks             map[string]store.Stack
	versions           map[string][]store.Version
	versionByKey       map[string]store.Version
	insertedVersions   []store.Version
	insertErr          error
	getVersionErr      error
	githubUserID       int64
	githubLogin        string
	createdSessionHash string
	createdSessionTTL  time.Duration
	sessionUsers       map[string]string
}

func newPublishSpyStore() *publishSpyStore {
	return &publishSpyStore{
		nextUserID:   1,
		nextStackID:  100,
		stackIDs:     map[string]int64{},
		stacks:       map[string]store.Stack{},
		versions:     map[string][]store.Version{},
		versionByKey: map[string]store.Version{},
		sessionUsers: map[string]string{},
	}
}

func (p *publishSpyStore) UpsertUser(_ context.Context, handle string) (int64, error) {
	p.upsertUserCalls++
	p.nextUserID++
	return p.nextUserID - 1, nil
}

func (p *publishSpyStore) UpsertUserGitHub(_ context.Context, login string, githubID int64) (int64, error) {
	p.githubLogin, p.githubUserID = login, githubID
	return 7, nil
}

func (p *publishSpyStore) CreateSession(_ context.Context, _ int64, hash string, ttl time.Duration) error {
	p.createdSessionHash, p.createdSessionTTL = hash, ttl
	return nil
}

func (p *publishSpyStore) SessionUser(_ context.Context, hash string) (string, error) {
	login, ok := p.sessionUsers[hash]
	if !ok {
		return "", store.ErrNotFound
	}
	return login, nil
}

func (p *publishSpyStore) AllVersionRefs(context.Context) ([]store.VersionRef, error) {
	var refs []store.VersionRef
	for key, versions := range p.versions {
		owner, name, _ := strings.Cut(key, "/")
		for _, version := range versions {
			refs = append(refs, store.VersionRef{Owner: owner, Name: name, Version: version.Version, GitTag: version.GitTag})
		}
	}
	return refs, nil
}

func (p *publishSpyStore) UpsertStack(_ context.Context, s store.Stack) (int64, error) {
	p.upsertStackCalls++
	key := s.Owner + "/" + s.Name
	id, ok := p.stackIDs[key]
	if !ok {
		id = p.nextStackID
		p.nextStackID++
		p.stackIDs[key] = id
	}
	s.ID = id
	p.stacks[key] = s
	return id, nil
}

func (p *publishSpyStore) InsertVersion(_ context.Context, v store.Version) error {
	p.insertVersionCalls++
	if p.insertErr != nil {
		return p.insertErr
	}
	var stackKey string
	for key, id := range p.stackIDs {
		if id == v.StackID {
			stackKey = key
			break
		}
	}
	if stackKey == "" {
		return errors.New("unknown stack ID")
	}
	key := stackKey + "/" + strconv.Itoa(v.Version)
	if _, ok := p.versionByKey[key]; ok {
		return store.ErrVersionExists
	}
	v.ID = int64(len(p.insertedVersions) + 1)
	v.PublishedAt = time.Now().UTC()
	p.versionByKey[key] = v
	p.versions[stackKey] = append([]store.Version{v}, p.versions[stackKey]...)
	p.insertedVersions = append(p.insertedVersions, v)
	return nil
}

func (p *publishSpyStore) Search(context.Context, string, string, string, int, int) ([]store.StackWithLatest, error) {
	return nil, nil
}

func (p *publishSpyStore) GetStack(_ context.Context, owner, name string, _, _ int) (store.Stack, []store.Version, error) {
	key := owner + "/" + name
	stack, ok := p.stacks[key]
	if !ok {
		return store.Stack{}, nil, store.ErrNotFound
	}
	return stack, p.versions[key], nil
}

func (p *publishSpyStore) GetVersion(_ context.Context, owner, name string, v int) (store.Version, error) {
	if p.getVersionErr != nil {
		return store.Version{}, p.getVersionErr
	}
	version, ok := p.versionByKey[owner+"/"+name+"/"+strconv.Itoa(v)]
	if !ok {
		return store.Version{}, store.ErrNotFound
	}
	return version, nil
}

func (p *publishSpyStore) Close() error {
	return nil
}

func assertNoPublishWrites(t *testing.T, st *publishSpyStore, cs *publishSpyContent) {
	t.Helper()
	if cs.commitCalls != 0 {
		t.Fatalf("ContentStore.Commit calls = %d, want 0", cs.commitCalls)
	}
	if st.insertVersionCalls != 0 {
		t.Fatalf("Store.InsertVersion calls = %d, want 0", st.insertVersionCalls)
	}
	if st.upsertStackCalls != 0 || st.upsertUserCalls != 0 {
		t.Fatalf("store upsert calls user=%d stack=%d, want 0", st.upsertUserCalls, st.upsertStackCalls)
	}
}

type publishFinding struct {
	File string `json:"file"`
	Kind string `json:"kind"`
}

func assertFinding(t *testing.T, findings []publishFinding, file, kind string) {
	t.Helper()
	for _, finding := range findings {
		if finding.File == file && finding.Kind == kind {
			return
		}
	}
	t.Fatalf("findings = %#v, want %s in %s", findings, kind, file)
}

func postBundle(t *testing.T, handler http.Handler, owner, name string, bundle []byte, token string) *httptest.ResponseRecorder {
	t.Helper()
	body, contentType := multipartBundle(t, bundle)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://registry.test/v1/stacks/"+owner+"/"+name+"/versions", body)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+token)
	handler.ServeHTTP(rr, req)
	return rr
}

func multipartBundle(t *testing.T, bundle []byte) (io.Reader, string) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("bundle", "stack.bundle")
	if err != nil {
		t.Fatalf("create bundle part: %v", err)
	}
	if _, err := part.Write(bundle); err != nil {
		t.Fatalf("write bundle part: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	return &body, writer.FormDataContentType()
}

func buildPublishBundle(t *testing.T, files map[string]string) []byte {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "src")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	runGit(t, repo, "init", "-b", "main")
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "Test User")
	runGit(t, repo, "config", "commit.gpgsign", "false")
	for name, content := range files {
		path := filepath.Join(repo, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s parent: %v", name, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-m", "publish fixture")
	runGit(t, repo, "tag", "v1")

	path := filepath.Join(root, "stack.bundle")
	if _, err := gitutil.Run(repo, "bundle", "create", path, "--all"); err != nil {
		t.Fatalf("git bundle create: %v", err)
	}
	bundle, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read bundle: %v", err)
	}
	return bundle
}

func buildPublishBundleWithMismatchedVersionTag(t *testing.T) []byte {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "src")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	runGit(t, repo, "init", "-b", "main")
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "Test User")
	runGit(t, repo, "config", "commit.gpgsign", "false")
	writeBundleFile(t, repo, "stack.yaml", "name: n\nowner: o\nversion: 1\nharness: codex\nsummary: clean summary\n")
	writeBundleFile(t, repo, "README.md", "clean\n")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-m", "clean head")

	runGit(t, repo, "checkout", "-b", "tagged-secret")
	writeBundleFile(t, repo, "settings.json", `{"token":"ghp_`+strings.Repeat("x", 36)+`"}`+"\n")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-m", "secret tag target")
	runGit(t, repo, "tag", "v1")
	runGit(t, repo, "checkout", "main")

	path := filepath.Join(root, "stack.bundle")
	if _, err := gitutil.Run(repo, "bundle", "create", path, "--all"); err != nil {
		t.Fatalf("git bundle create: %v", err)
	}
	bundle, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read bundle: %v", err)
	}
	return bundle
}

func writeBundleFile(t *testing.T, repo, name, content string) {
	t.Helper()
	path := filepath.Join(repo, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s parent: %v", name, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}
