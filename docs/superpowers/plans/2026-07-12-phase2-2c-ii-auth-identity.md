# Phase 2 · 2c-ii — Auth + Publisher Identity Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the registry multi-user with GitHub device-flow login, opaque hashed sessions, own-login-only publishing, per-version trust tiers, and the two 2c-i carry-in fixes.

**Architecture:** Add `internal/registry/auth` for the GitHub device-flow boundary, its real HTTP adapter and deterministic fake, and session token hashing/minting. Extend the existing Postgres-backed `store.Store`, inject `auth.GitHubClient` into the existing HTTP server, authorize before bundle staging, and keep the existing synchronous fail-closed scan gate unchanged after authorization. The CLI mediates login through the registry and stores only the SherpA session token in a mode-0600 file.

**Tech Stack:** Go ≥1.22 stdlib (`net/http`, `crypto/rand`, `crypto/sha256`, `crypto/subtle`), existing `github.com/jackc/pgx/v5`, existing `gopkg.in/yaml.v3`, system `git`, Postgres 16 through the existing Docker test harness; no web framework or ORM.

## Global Constraints

- fail-closed publish unchanged
- deny-by-default authz
- client secret/GitHub tokens never returned/logged
- sessions stored as sha256 hash
- static admin token kept env-gated
- own-login-only publish scope
- GitHubClient interface + fake, GitHub never hit in tests
- Postgres via the existing docker StartPostgres harness
- module sherpa
- gofmt/vet clean
- new deps minimal

## Codex Delegation

Codex implements via `~/.claude/skills/codex-call/codex-run.sh --mode workspace-write --cwd /Users/timokruth/Projekte/feat --timeout 700 --prompt-file <task>`; the controller verifies and commits because Codex cannot write `.git`. Use the offline environment `GOFLAGS=-mod=mod GOPROXY=off GOCACHE=/private/tmp/sherpa-go-build`. Reviews use Opus generally and Fable for Tasks 3 and 4, the auth/authz security surface.

---

### Task 1: GitHub device-flow boundary, deterministic fake, and real HTTP adapter

**Files:**
- Create: `internal/registry/auth/github.go`
- Create: `internal/registry/auth/fake.go`
- Create: `internal/registry/auth/httpgithub.go`
- Test: `internal/registry/auth/httpgithub_test.go`

**Interfaces:**
- Consumes: stdlib `context`, `net/http`, `net/url`, `encoding/json`.
- Produces (consumed by Tasks 3 and 7):

```go
type DeviceCode struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	Interval        int    `json:"interval"`
	ExpiresIn       int    `json:"expires_in"`
}
type GitHubUser struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}
type GitHubClient interface {
	StartDeviceFlow(ctx context.Context) (DeviceCode, error)
	PollToken(ctx context.Context, deviceCode string) (accessToken string, err error)
	GetUser(ctx context.Context, accessToken string) (GitHubUser, error)
}
var ErrAuthPending = errors.New("authorization pending")
var ErrSlowDown = errors.New("slow down")
var ErrExpired = errors.New("device code expired")
func NewGitHubClient(clientID string, httpBaseURLs ...string) *HTTPGitHubClient
```

- [ ] **Step 1: Write the failing wire-format test** in `internal/registry/auth/httpgithub_test.go`:

```go
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPGitHubDeviceFlowWireFormat(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login/device/code", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.Form.Get("client_id") != "client-id" {
			t.Fatalf("start form = %#v, err=%v", r.Form, err)
		}
		_ = json.NewEncoder(w).Encode(DeviceCode{DeviceCode: "device", UserCode: "ABCD-EFGH", VerificationURI: "https://github.com/login/device", Interval: 5, ExpiresIn: 900})
	})
	polls := 0
	mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		polls++
		if err := r.ParseForm(); err != nil || r.Form.Get("device_code") != "device" || r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" {
			t.Fatalf("poll form = %#v, err=%v", r.Form, err)
		}
		if polls == 1 {
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "github-token", "token_type": "bearer"})
	})
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer github-token" {
			t.Fatalf("Authorization = %q", r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(GitHubUser{ID: 42, Login: "alice"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := NewGitHubClient("client-id", srv.URL, srv.URL)
	ctx := context.Background()
	code, err := client.StartDeviceFlow(ctx)
	if err != nil || code.DeviceCode != "device" || code.Interval != 5 {
		t.Fatalf("StartDeviceFlow = %#v, %v", code, err)
	}
	if _, err := client.PollToken(ctx, code.DeviceCode); !errors.Is(err, ErrAuthPending) {
		t.Fatalf("first PollToken error = %v", err)
	}
	token, err := client.PollToken(ctx, code.DeviceCode)
	if err != nil || token != "github-token" {
		t.Fatalf("second PollToken = %q, %v", token, err)
	}
	user, err := client.GetUser(ctx, token)
	if err != nil || user != (GitHubUser{ID: 42, Login: "alice"}) {
		t.Fatalf("GetUser = %#v, %v", user, err)
	}
}
```

- [ ] **Step 2: Verify it fails** — `GOFLAGS=-mod=mod GOPROXY=off GOCACHE=/private/tmp/sherpa-go-build go test ./internal/registry/auth/` → FAIL with `package sherpa/internal/registry/auth is not in std` or undefined auth symbols.

- [ ] **Step 3: Implement the complete auth boundary.** Create `github.go`:

```go
package auth

import (
	"context"
	"errors"
)

type DeviceCode struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	Interval        int    `json:"interval"`
	ExpiresIn       int    `json:"expires_in"`
}

type GitHubUser struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}

type GitHubClient interface {
	StartDeviceFlow(ctx context.Context) (DeviceCode, error)
	PollToken(ctx context.Context, deviceCode string) (accessToken string, err error)
	GetUser(ctx context.Context, accessToken string) (GitHubUser, error)
}

var ErrAuthPending = errors.New("authorization pending")
var ErrSlowDown = errors.New("slow down")
var ErrExpired = errors.New("device code expired")
```

Create `fake.go`:

```go
package auth

import (
	"context"
	"errors"
	"sync"
)

type PollResult struct {
	AccessToken string
	Err         error
}

type FakeGitHubClient struct {
	mu          sync.Mutex
	Device      DeviceCode
	StartErr    error
	PollResults []PollResult
	Users       map[string]GitHubUser
	UserErr     error
	StartCalls  int
	PollCalls   int
	GetUserCalls int
}

func (f *FakeGitHubClient) StartDeviceFlow(context.Context) (DeviceCode, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.StartCalls++
	return f.Device, f.StartErr
}

func (f *FakeGitHubClient) PollToken(context.Context, string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.PollCalls++
	if len(f.PollResults) == 0 {
		return "", ErrAuthPending
	}
	result := f.PollResults[0]
	f.PollResults = f.PollResults[1:]
	return result.AccessToken, result.Err
}

func (f *FakeGitHubClient) GetUser(_ context.Context, accessToken string) (GitHubUser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.GetUserCalls++
	if f.UserErr != nil {
		return GitHubUser{}, f.UserErr
	}
	user, ok := f.Users[accessToken]
	if !ok {
		return GitHubUser{}, errors.New("fake GitHub user not scripted")
	}
	return user, nil
}
```

Create `httpgithub.go`:

```go
package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

type HTTPGitHubClient struct {
	clientID string
	loginURL string
	apiURL   string
	client   *http.Client
}

func NewGitHubClient(clientID string, httpBaseURLs ...string) *HTTPGitHubClient {
	loginURL := "https://github.com"
	apiURL := "https://api.github.com"
	if len(httpBaseURLs) > 0 {
		loginURL = strings.TrimRight(httpBaseURLs[0], "/")
	}
	if len(httpBaseURLs) > 1 {
		apiURL = strings.TrimRight(httpBaseURLs[1], "/")
	}
	return &HTTPGitHubClient{clientID: clientID, loginURL: loginURL, apiURL: apiURL, client: http.DefaultClient}
}

func (g *HTTPGitHubClient) StartDeviceFlow(ctx context.Context) (DeviceCode, error) {
	if strings.TrimSpace(g.clientID) == "" {
		return DeviceCode{}, fmt.Errorf("SHERPA_GITHUB_CLIENT_ID is required for login")
	}
	var out DeviceCode
	err := g.postForm(ctx, g.loginURL+"/login/device/code", url.Values{"client_id": {g.clientID}}, &out)
	return out, err
}

func (g *HTTPGitHubClient) PollToken(ctx context.Context, deviceCode string) (string, error) {
	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	err := g.postForm(ctx, g.loginURL+"/login/oauth/access_token", url.Values{
		"client_id":   {g.clientID},
		"device_code": {deviceCode},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
	}, &out)
	if err != nil {
		return "", err
	}
	switch out.Error {
	case "authorization_pending":
		return "", ErrAuthPending
	case "slow_down":
		return "", ErrSlowDown
	case "expired_token":
		return "", ErrExpired
	case "":
		if out.AccessToken == "" {
			return "", fmt.Errorf("GitHub token response omitted access_token")
		}
		return out.AccessToken, nil
	default:
		return "", fmt.Errorf("GitHub device flow: %s", out.Error)
	}
}

func (g *HTTPGitHubClient) GetUser(ctx context.Context, accessToken string) (GitHubUser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.apiURL+"/user", nil)
	if err != nil {
		return GitHubUser{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := g.client.Do(req)
	if err != nil {
		return GitHubUser{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return GitHubUser{}, fmt.Errorf("GitHub user endpoint: HTTP %d", resp.StatusCode)
	}
	var user GitHubUser
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		return GitHubUser{}, err
	}
	if user.ID == 0 || strings.TrimSpace(user.Login) == "" {
		return GitHubUser{}, fmt.Errorf("GitHub user response omitted id or login")
	}
	return user, nil
}

func (g *HTTPGitHubClient) postForm(ctx context.Context, endpoint string, values url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("GitHub endpoint: HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
```

- [ ] **Step 4: Verify** — `gofmt -w internal/registry/auth && GOFLAGS=-mod=mod GOPROXY=off GOCACHE=/private/tmp/sherpa-go-build go test ./internal/registry/auth/` → PASS; the test contacts only `httptest.Server`.

- [ ] **Step 5: Commit** — `git add internal/registry/auth && git commit -m "feat: add GitHub device-flow client boundary"`

---

### Task 2: Postgres identity, hashed sessions, and trust-tier persistence

**Files:**
- Modify: `internal/registry/store/store.go`
- Modify: `internal/registry/store/migrations.go`
- Modify: `internal/registry/store/postgres.go`
- Test: `internal/registry/store/postgres_test.go`

**Interfaces:**
- Consumes: existing `store.StartPostgres(t) string`, existing `PostgresStore`, `time.Duration`.
- Produces (consumed by Tasks 3–7):

```go
type Version struct {
	ID int64; StackID int64; Version int; GitTag string; Manifest json.RawMessage
	ScanReport json.RawMessage; Changelog string; TrustTier string; PublishedAt time.Time
}
type StackWithLatest struct { Stack; Version int; GitTag, TrustTier string; PublishedAt time.Time }
type Store interface {
	UpsertUser(ctx context.Context, handle string) (userID int64, err error)
	UpsertUserGitHub(ctx context.Context, login string, githubID int64) (userID int64, err error)
	CreateSession(ctx context.Context, userID int64, tokenHash string, ttl time.Duration) error
	SessionUser(ctx context.Context, tokenHash string) (login string, err error)
	UpsertStack(ctx context.Context, s Stack) (stackID int64, err error)
	InsertVersion(ctx context.Context, v Version) error
	Search(ctx context.Context, q, harness, tag string) ([]StackWithLatest, error)
	GetStack(ctx context.Context, owner, name string) (Stack, []Version, error)
	GetVersion(ctx context.Context, owner, name string, v int) (Version, error)
	Close() error
}
```

- [ ] **Step 1: Append the failing real-Postgres test** to `postgres_test.go`:

```go
func TestPostgresGitHubSessionsAndTrustTier(t *testing.T) {
	dsn := StartPostgres(t)
	ctx := context.Background()
	st, err := OpenPostgres(ctx, dsn)
	if err != nil { t.Fatal(err) }
	t.Cleanup(func() { _ = st.Close() })

	first, err := st.UpsertUserGitHub(ctx, "alice", 42)
	if err != nil { t.Fatal(err) }
	second, err := st.UpsertUserGitHub(ctx, "alice-renamed", 42)
	if err != nil { t.Fatal(err) }
	if first != second { t.Fatalf("user IDs = %d and %d", first, second) }
	if err := st.CreateSession(ctx, first, "valid-hash", time.Hour); err != nil { t.Fatal(err) }
	if got, err := st.SessionUser(ctx, "valid-hash"); err != nil || got != "alice-renamed" {
		t.Fatalf("SessionUser = %q, %v", got, err)
	}
	if err := st.CreateSession(ctx, first, "expired-hash", -time.Second); err != nil { t.Fatal(err) }
	if _, err := st.SessionUser(ctx, "expired-hash"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired SessionUser error = %v", err)
	}
	stackID, err := st.UpsertStack(ctx, Stack{Owner: "alice-renamed", Name: "trusted"})
	if err != nil { t.Fatal(err) }
	if err := st.InsertVersion(ctx, Version{StackID: stackID, Version: 1, GitTag: "v1", TrustTier: "linked"}); err != nil { t.Fatal(err) }
	v, err := st.GetVersion(ctx, "alice-renamed", "trusted", 1)
	if err != nil || v.TrustTier != "linked" { t.Fatalf("version = %#v, %v", v, err) }
	matches, err := st.Search(ctx, "trusted", "", "")
	if err != nil || len(matches) != 1 || matches[0].TrustTier != "linked" { t.Fatalf("search = %#v, %v", matches, err) }
}
```

- [ ] **Step 2: Verify it fails** — `GOFLAGS=-mod=mod GOPROXY=off GOCACHE=/private/tmp/sherpa-go-build go test ./internal/registry/store/ -run TestPostgresGitHubSessionsAndTrustTier -count=1` → FAIL with undefined store methods and fields.

- [ ] **Step 3: Implement the exact schema and store changes.** Add these fields and methods to `store.go`:

```go
package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

type Stack struct {
	ID int64
	Owner, Name, Summary, Harness, ForkedFrom string
	Tags []string
	CreatedAt time.Time
}

type Version struct {
	ID int64
	StackID int64
	Version int
	GitTag string
	Manifest json.RawMessage
	ScanReport json.RawMessage
	Changelog string
	TrustTier string
	PublishedAt time.Time
}

type StackWithLatest struct {
	Stack
	Version int
	GitTag string
	TrustTier string
	PublishedAt time.Time
}

type Store interface {
	UpsertUser(ctx context.Context, handle string) (userID int64, err error)
	UpsertUserGitHub(ctx context.Context, login string, githubID int64) (userID int64, err error)
	CreateSession(ctx context.Context, userID int64, tokenHash string, ttl time.Duration) error
	SessionUser(ctx context.Context, tokenHash string) (login string, err error)
	UpsertStack(ctx context.Context, s Stack) (stackID int64, err error)
	InsertVersion(ctx context.Context, v Version) error
	Search(ctx context.Context, q, harness, tag string) ([]StackWithLatest, error)
	GetStack(ctx context.Context, owner, name string) (Stack, []Version, error)
	GetVersion(ctx context.Context, owner, name string, v int) (Version, error)
	Close() error
}

var ErrVersionExists = errors.New("version already exists")
var ErrNotFound = errors.New("not found")
```

Append these exact entries to `migrations` in `migrations.go`:

```go
`alter table users add column if not exists github_id bigint unique`,
`create table if not exists sessions (
	id bigserial primary key,
	user_id bigint references users(id),
	token_hash text unique not null,
	created_at timestamptz default now(),
	last_used_at timestamptz,
	expires_at timestamptz not null
)`,
`alter table stack_versions add column if not exists trust_tier text not null default 'unreviewed'`,
```

Add to `postgres.go` after `UpsertUser`:

```go
func (s *PostgresStore) UpsertUserGitHub(ctx context.Context, login string, githubID int64) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `
		with updated as (
			update users set handle = $1 where github_id = $2 returning id
		), inserted as (
			insert into users(handle, github_id)
			select $1, $2 where not exists (select 1 from updated)
			on conflict (handle) do update set github_id = excluded.github_id
			returning id
		)
		select id from updated union all select id from inserted limit 1
	`, login, githubID).Scan(&id)
	return id, err
}

func (s *PostgresStore) CreateSession(ctx context.Context, userID int64, tokenHash string, ttl time.Duration) error {
	_, err := s.pool.Exec(ctx, `
		insert into sessions(user_id, token_hash, expires_at)
		values ($1, $2, now() + $3::interval)
	`, userID, tokenHash, pgInterval(ttl))
	return err
}

func (s *PostgresStore) SessionUser(ctx context.Context, tokenHash string) (string, error) {
	var login string
	err := s.pool.QueryRow(ctx, `
		update sessions se set last_used_at = now()
		from users u
		where se.user_id = u.id and se.token_hash = $1 and se.expires_at > now()
		returning u.handle
	`, tokenHash).Scan(&login)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return login, err
}

func pgInterval(ttl time.Duration) string {
	return fmt.Sprintf("%f seconds", ttl.Seconds())
}
```

Add `fmt` and `time` to the imports. Change `InsertVersion` to:

```go
func (s *PostgresStore) InsertVersion(ctx context.Context, version Version) error {
	_, err := s.pool.Exec(ctx, `
		insert into stack_versions(stack_id, version, git_tag, manifest, scan_report, changelog, trust_tier)
		values ($1, $2, $3, $4, $5, $6, $7)
	`, version.StackID, version.Version, version.GitTag, jsonOrNil(version.Manifest), jsonOrNil(version.ScanReport), version.Changelog, version.TrustTier)
	if isUniqueViolation(err) { return ErrVersionExists }
	return err
}
```

Change the lateral select in `Search` to select `version, git_tag, trust_tier, published_at`, add `coalesce(v.trust_tier, 'unreviewed')` before `v.published_at` in the outer select, and scan `&match.TrustTier` before `&match.PublishedAt`. Change all version selects (`GetVersion`, `versionsForStack`) to select `coalesce(trust_tier, 'unreviewed')` before `published_at` and scan `&version.TrustTier` before `&version.PublishedAt`.

- [ ] **Step 4: Verify** — `gofmt -w internal/registry/store && GOFLAGS=-mod=mod GOPROXY=off GOCACHE=/private/tmp/sherpa-go-build go test ./internal/registry/store/ -count=1` → PASS against real Postgres.

- [ ] **Step 5: Commit** — `git add internal/registry/store && git commit -m "feat: persist GitHub identities hashed sessions and trust tiers"`

---

### Task 3: Device start/poll API and session minting

**Files:**
- Create: `internal/registry/auth/session.go`
- Create: `internal/registry/auth/session_test.go`
- Create: `internal/registry/api/auth.go`
- Test: `internal/registry/api/auth_test.go`
- Modify: `internal/registry/api/router.go`
- Modify: `internal/registry/config.go`
- Modify: `cmd/registry/main.go`
- Test: `cmd/registry/main_test.go`

**Interfaces:**
- Consumes: `auth.GitHubClient`, `store.Store.UpsertUserGitHub`, `store.Store.CreateSession`.
- Produces:

```go
const SessionTTL = 90 * 24 * time.Hour
func NewSessionToken() (token string, tokenHash string, err error)
func HashToken(token string) string
func New(st store.Store, cs content.ContentStore, adminToken string, github auth.GitHubClient) http.Handler
// POST /v1/auth/device/start
// POST /v1/auth/device/poll {"device_code":"..."}
```

- [ ] **Step 1: Write the failing tests.** Create `session_test.go`:

```go
package auth

import "testing"

func TestNewSessionTokenStoresOnlyHash(t *testing.T) {
	token, hash, err := NewSessionToken()
	if err != nil { t.Fatal(err) }
	if len(token) != 64 || len(hash) != 64 || token == hash { t.Fatalf("token/hash lengths or equality: %q %q", token, hash) }
	if HashToken(token) != hash { t.Fatalf("HashToken did not reproduce hash") }
}
```

Create `api/auth_test.go`:

```go
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	registryauth "sherpa/internal/registry/auth"
)

func TestDeviceFlowMintsHashedSherpaSession(t *testing.T) {
	st := newPublishSpyStore()
	gh := &registryauth.FakeGitHubClient{
		Device: registryauth.DeviceCode{DeviceCode: "device", UserCode: "ABCD", VerificationURI: "https://github.com/login/device", Interval: 5, ExpiresIn: 900},
		PollResults: []registryauth.PollResult{{Err: registryauth.ErrAuthPending}, {AccessToken: "github-token"}},
		Users: map[string]registryauth.GitHubUser{"github-token": {ID: 42, Login: "alice"}},
	}
	h := New(st, newPublishSpyContent(t), "admin", gh)
	start := httptest.NewRecorder()
	h.ServeHTTP(start, httptest.NewRequest(http.MethodPost, "/v1/auth/device/start", nil))
	if start.Code != http.StatusOK || !strings.Contains(start.Body.String(), `"user_code":"ABCD"`) { t.Fatalf("start = %d %s", start.Code, start.Body.String()) }

	pending := httptest.NewRecorder()
	h.ServeHTTP(pending, httptest.NewRequest(http.MethodPost, "/v1/auth/device/poll", strings.NewReader(`{"device_code":"device"}`)))
	if pending.Code != http.StatusAccepted || !strings.Contains(pending.Body.String(), `"status":"pending"`) { t.Fatalf("pending = %d %s", pending.Code, pending.Body.String()) }

	done := httptest.NewRecorder()
	h.ServeHTTP(done, httptest.NewRequest(http.MethodPost, "/v1/auth/device/poll", strings.NewReader(`{"device_code":"device"}`)))
	if done.Code != http.StatusOK { t.Fatalf("done = %d %s", done.Code, done.Body.String()) }
	var body struct { AccessToken string `json:"access_token"`; Login string `json:"login"` }
	if err := json.Unmarshal(done.Body.Bytes(), &body); err != nil { t.Fatal(err) }
	if body.AccessToken == "" || body.Login != "alice" || strings.Contains(done.Body.String(), "github-token") { t.Fatalf("response = %s", done.Body.String()) }
	if st.createdSessionHash == "" || st.createdSessionHash == body.AccessToken || st.createdSessionHash != registryauth.HashToken(body.AccessToken) { t.Fatalf("stored hash = %q", st.createdSessionHash) }
	if st.createdSessionTTL != registryauth.SessionTTL { t.Fatalf("TTL = %v", st.createdSessionTTL) }
}

func TestDevicePollStatusMapping(t *testing.T) {
	for _, tc := range []struct { name string; err error; status int; body string }{
		{"slow", registryauth.ErrSlowDown, http.StatusAccepted, `"status":"slow_down"`},
		{"expired", registryauth.ErrExpired, http.StatusGone, `"error":"device code expired"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newPublishSpyStore()
			gh := &registryauth.FakeGitHubClient{PollResults: []registryauth.PollResult{{Err: tc.err}}}
			h := New(st, newPublishSpyContent(t), "", gh)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/auth/device/poll", strings.NewReader(`{"device_code":"device"}`)))
			if rr.Code != tc.status || !strings.Contains(rr.Body.String(), tc.body) { t.Fatalf("response = %d %s", rr.Code, rr.Body.String()) }
		})
	}
}
```

Add these exact methods/fields to `publishSpyStore` in `publish_test.go`:

```go
githubUserID       int64
githubLogin        string
createdSessionHash string
createdSessionTTL  time.Duration
sessionUsers       map[string]string

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
	if !ok { return "", store.ErrNotFound }
	return login, nil
}
```

Initialize `sessionUsers: map[string]string{}` in `newPublishSpyStore`. Add these exact interface methods to `fakeStore` in `read_test.go`:

```go
func (f *fakeStore) UpsertUserGitHub(context.Context, string, int64) (int64, error) { return 0, store.ErrNotFound }
func (f *fakeStore) CreateSession(context.Context, int64, string, time.Duration) error { return store.ErrNotFound }
func (f *fakeStore) SessionUser(context.Context, string) (string, error) { return "", store.ErrNotFound }
```

- [ ] **Step 2: Verify failure** — `GOFLAGS=-mod=mod GOPROXY=off GOCACHE=/private/tmp/sherpa-go-build go test ./internal/registry/auth/ ./internal/registry/api/` → FAIL with undefined session and router symbols.

- [ ] **Step 3: Implement session minting and endpoints.** Create `session.go`:

```go
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

const SessionTTL = 90 * 24 * time.Hour

func NewSessionToken() (string, string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil { return "", "", err }
	token := hex.EncodeToString(raw)
	return token, HashToken(token), nil
}

func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
```

Create `api/auth.go`:

```go
package api

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	registryauth "sherpa/internal/registry/auth"
)

func (s *server) handleDeviceStart(w http.ResponseWriter, r *http.Request) {
	code, err := s.github.StartDeviceFlow(r.Context())
	if err != nil { log.Printf("auth device start: %v", err); writeError(w, http.StatusInternalServerError, "internal server error"); return }
	writeJSON(w, http.StatusOK, code)
}

func (s *server) handleDevicePoll(w http.ResponseWriter, r *http.Request) {
	var req struct { DeviceCode string `json:"device_code"` }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.DeviceCode) == "" {
		writeError(w, http.StatusBadRequest, "device_code is required")
		return
	}
	githubToken, err := s.github.PollToken(r.Context(), req.DeviceCode)
	switch {
	case errors.Is(err, registryauth.ErrAuthPending):
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "pending"}); return
	case errors.Is(err, registryauth.ErrSlowDown):
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "slow_down"}); return
	case errors.Is(err, registryauth.ErrExpired):
		writeError(w, http.StatusGone, "device code expired"); return
	case err != nil:
		log.Printf("auth device poll: %v", err); writeError(w, http.StatusInternalServerError, "internal server error"); return
	}
	user, err := s.github.GetUser(r.Context(), githubToken)
	if err != nil { log.Printf("auth GitHub user: %v", err); writeError(w, http.StatusInternalServerError, "internal server error"); return }
	userID, err := s.store.UpsertUserGitHub(r.Context(), user.Login, user.ID)
	if err != nil { log.Printf("auth upsert user: %v", err); writeError(w, http.StatusInternalServerError, "internal server error"); return }
	token, hash, err := registryauth.NewSessionToken()
	if err != nil { log.Printf("auth mint session: %v", err); writeError(w, http.StatusInternalServerError, "internal server error"); return }
	if err := s.store.CreateSession(r.Context(), userID, hash, registryauth.SessionTTL); err != nil {
		log.Printf("auth create session: %v", err); writeError(w, http.StatusInternalServerError, "internal server error"); return
	}
	writeJSON(w, http.StatusOK, map[string]string{"access_token": token, "login": user.Login})
}
```

Replace `server` and `New` in `router.go` with:

```go
type server struct {
	store store.Store
	content content.ContentStore
	adminToken string
	github registryauth.GitHubClient
}

func New(st store.Store, cs content.ContentStore, adminToken string, github registryauth.GitHubClient) http.Handler {
	s := &server{store: st, content: cs, adminToken: adminToken, github: github}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/auth/device/start", s.handleDeviceStart)
	mux.HandleFunc("POST /v1/auth/device/poll", s.handleDevicePoll)
	mux.HandleFunc("GET /v1/search", s.handleSearch)
	mux.HandleFunc("GET /v1/stacks/{owner}/{repo}/", s.handleGit)
	mux.HandleFunc("GET /v1/stacks/{owner}/{name}", s.handleStack)
	mux.HandleFunc("GET /v1/stacks/{owner}/{name}/versions/{version}", s.handleVersion)
	mux.HandleFunc("POST /v1/stacks/{owner}/{name}/versions", s.handlePublish)
	return mux
}
```

Import `registryauth "sherpa/internal/registry/auth"` in `router.go`, `cmd/registry/main.go`, `api/publish_test.go`, `api/read_test.go`, and `internal/integration/registry_e2e_test.go`. Apply these exact constructor changes:

```go
// cmd/registry/main.go
github := registryauth.NewGitHubClient(cfg.GitHubClientID)
return api.New(st, cs, cfg.Token, github), cleanup, nil

// internal/integration/registry_e2e_test.go
handler := api.New(st, content.NewBareGit(t.TempDir()), "test-token", &registryauth.FakeGitHubClient{})

// internal/registry/api/publish_test.go: each existing three-argument New call
handler := New(st, cs, "registry-token", &registryauth.FakeGitHubClient{})

// internal/registry/api/read_test.go: each existing three-argument New call
handler := New(st, content.NewBareGit(t.TempDir()), "", &registryauth.FakeGitHubClient{})
handler := New(newFakeStore(), content.NewBareGit(t.TempDir()), "", &registryauth.FakeGitHubClient{})
handler := New(newFakeStore(), cs, "", &registryauth.FakeGitHubClient{})
```

Replace `Config` and the token validation portion in `internal/registry/config.go` with:

```go
type Config struct {
	Port string
	DatabaseURL string
	Token string
	ContentDir string
	GitHubClientID string
}

func LoadConfig() (Config, error) {
	cfg := Config{
		Port: valueOrDefault("PORT", "8080"),
		DatabaseURL: strings.TrimSpace(os.Getenv("DATABASE_URL")),
		Token: strings.TrimSpace(os.Getenv("SHERPA_REGISTRY_TOKEN")),
		ContentDir: valueOrDefault("SHERPA_CONTENT_DIR", "./registry-content"),
		GitHubClientID: strings.TrimSpace(os.Getenv("SHERPA_GITHUB_CLIENT_ID")),
	}
	if cfg.DatabaseURL == "" { return Config{}, fmt.Errorf("DATABASE_URL is required") }
	return cfg, nil
}
```

This keeps the admin escape hatch disabled when its env variable is absent and leaves GitHub OAuth App registration as a deploy prerequisite, not a build blocker. Replace `TestLoadConfigRequiresToken` with a test asserting that an empty token loads successfully.

```go
func TestLoadConfigAllowsDisabledAdminToken(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://example/sherpa")
	t.Setenv("SHERPA_REGISTRY_TOKEN", "")
	t.Setenv("SHERPA_GITHUB_CLIENT_ID", "github-client")
	cfg, err := LoadConfig()
	if err != nil { t.Fatalf("LoadConfig: %v", err) }
	if cfg.Token != "" { t.Fatalf("Token = %q", cfg.Token) }
	if cfg.GitHubClientID != "github-client" { t.Fatalf("GitHubClientID = %q", cfg.GitHubClientID) }
}
```

- [ ] **Step 4: Verify** — `gofmt -w internal/registry/auth internal/registry/api internal/registry/config.go cmd/registry && GOFLAGS=-mod=mod GOPROXY=off GOCACHE=/private/tmp/sherpa-go-build go test ./internal/registry/auth/ ./internal/registry/api/ ./cmd/registry/` → PASS.

- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: add registry GitHub device login endpoints"`

---

### Task 4: Deny-by-default publish authorization and trust-tier responses

**Files:**
- Create: `internal/registry/api/authorize.go`
- Test: `internal/registry/api/publish_test.go`
- Modify: `internal/registry/api/publish.go`
- Modify: `internal/registry/api/read.go`
- Test: `internal/registry/api/read_test.go`

**Interfaces:**
- Consumes: `store.Store.SessionUser`, `auth.HashToken`, configured optional admin token.
- Produces:

```go
func (s *server) authorizePublish(r *http.Request, owner string) (userHandle string, tier string, status int, err error)
// Admin bearer: any owner, owner/unreviewed.
// Session bearer: exact own login, login/linked.
```

- [ ] **Step 1: Add the failing security tests** to `publish_test.go`:

```go
func TestPublishAuthorizationMatrixBeforeStaging(t *testing.T) {
	valid := "session-token"
	for _, tc := range []struct { name, owner, token string; status int }{
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
			if rr.Code != tc.status { t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String()) }
			assertNoPublishWrites(t, st, cs)
			if cs.stageCalls != 0 { t.Fatalf("StageBundle calls = %d", cs.stageCalls) }
		})
	}
}

func TestPublishTrustTierByCredential(t *testing.T) {
	for _, tc := range []struct { name, owner, token, tier string }{
		{"admin any owner", "org", "admin", "unreviewed"},
		{"session own login", "alice", "session-token", "linked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newPublishSpyStore()
			st.sessionUsers[registryauth.HashToken("session-token")] = "alice"
			h := New(st, newPublishSpyContent(t), "admin", &registryauth.FakeGitHubClient{})
			bundle := buildPublishBundle(t, map[string]string{"stack.yaml": "name: n\nowner: "+tc.owner+"\nversion: 1\nharness: codex\n", "README.md": "clean\n"})
			rr := postBundle(t, h, tc.owner, "n", bundle, tc.token)
			if rr.Code != http.StatusCreated { t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String()) }
			if got := st.insertedVersions[0].TrustTier; got != tc.tier { t.Fatalf("TrustTier = %q", got) }
		})
	}
}
```

Import `registryauth "sherpa/internal/registry/auth"`, add `stageCalls int` to `publishSpyContent`, and increment it at the start of its existing `StageBundle` method. Extend the read tests' response structs and fixtures with `TrustTier string` and assert `linked` in search, stack version summary, and version detail.

- [ ] **Step 2: Verify failure** — `GOFLAGS=-mod=mod GOPROXY=off GOCACHE=/private/tmp/sherpa-go-build go test ./internal/registry/api/ -run 'TestPublishAuthorizationMatrix|TestPublishTrustTier'` → FAIL because session authorization is not active and trust tier is not written.

- [ ] **Step 3: Implement authorization.** Create `authorize.go`:

```go
package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"

	registryauth "sherpa/internal/registry/auth"
	"sherpa/internal/registry/store"
)

var errUnauthorized = errors.New("unauthorized")
var errWrongOwner = errors.New("you can only publish under your GitHub login")

func (s *server) authorizePublish(r *http.Request, owner string) (string, string, int, error) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || strings.TrimSpace(token) == "" { return "", "", http.StatusUnauthorized, errUnauthorized }
	if s.adminToken != "" {
		got, want := sha256.Sum256([]byte(token)), sha256.Sum256([]byte(s.adminToken))
		if subtle.ConstantTimeCompare(got[:], want[:]) == 1 { return owner, "unreviewed", 0, nil }
	}
	login, err := s.store.SessionUser(r.Context(), registryauth.HashToken(token))
	if errors.Is(err, store.ErrNotFound) { return "", "", http.StatusUnauthorized, errUnauthorized }
	if err != nil { return "", "", http.StatusInternalServerError, err }
	if owner != login { return login, "", http.StatusForbidden, errWrongOwner }
	return login, "linked", 0, nil
}
```

In `handlePublish`, replace the current `authorized` check with:

```go
	_, trustTier, authStatus, err := s.authorizePublish(r, owner)
	if err != nil {
		if authStatus == http.StatusInternalServerError { log.Printf("authorize publish: %v", err); writeError(w, authStatus, "internal server error") } else { writeError(w, authStatus, err.Error()) }
		return
	}
```

Delete the old `authorized` method and its `crypto/sha256` and `crypto/subtle` imports. Add `TrustTier: trustTier` to the inserted `store.Version` and every `versionResponse`. In `read.go`, add this exact field to `searchStackResponse`, `versionSummary`, and `versionResponse`, then map it from `match.TrustTier` and `version.TrustTier` in all three handlers:

```go
TrustTier string `json:"trust_tier"`
```

- [ ] **Step 4: Verify** — `gofmt -w internal/registry/api && GOFLAGS=-mod=mod GOPROXY=off GOCACHE=/private/tmp/sherpa-go-build go test ./internal/registry/api/ -count=1` → PASS, including nothing-written and zero-stage assertions for every rejection.

- [ ] **Step 5: Commit** — `git add internal/registry/api && git commit -m "feat: enforce publisher identity and expose trust tiers"`

---

### Task 5: Orphan-tag reconciliation and server-side 500 logging

**Files:**
- Modify: `internal/registry/content/content.go`
- Modify: `internal/registry/content/baregit.go`
- Test: `internal/registry/content/baregit_test.go`
- Modify: `internal/registry/api/publish.go`
- Modify: `internal/registry/api/read.go`
- Test: `internal/registry/api/publish_test.go`

**Interfaces:**
- Consumes: staged git tag and existing bare-repo tag.
- Produces:

```go
type ContentStore interface {
	EnsureRepo(owner, name string) error
	StageBundle(bundle []byte, gitTag string) (stageDir, worktreeDir string, cleanup func(), err error)
	Commit(owner, name, stageDir string) error
	TagCommit(owner, name, tag string) (commit string, err error)
	RepoPath(owner, name string) string
}
var ErrNotFound = errors.New("content not found")
```

- [ ] **Step 1: Write the failing carry-in tests.** Add to `baregit_test.go`:

```go
func TestTagCommit(t *testing.T) {
	root := t.TempDir()
	cs := NewBareGit(root)
	src := buildStackRepo(t, root, "v1", map[string]string{"stack.yaml": "name: n\nversion: 1\n"})
	bundle := createBundle(t, src, root, "tag-commit.bundle")
	stage, _, cleanup, err := cs.StageBundle(bundle, "v1")
	if err != nil { t.Fatal(err) }
	defer cleanup()
	if err := cs.Commit("alice", "n", stage); err != nil { t.Fatal(err) }
	got, err := cs.TagCommit("alice", "n", "v1")
	if err != nil || len(got) != 40 { t.Fatalf("TagCommit = %q, %v", got, err) }
	if _, err := cs.TagCommit("alice", "n", "v2"); !errors.Is(err, ErrNotFound) { t.Fatalf("missing tag error = %v", err) }
}
```

Add imports `errors`. Add to `publish_test.go`:

```go
func TestMatchingOrphanTagIsAdopted(t *testing.T) {
	st := newPublishSpyStore()
	cs := newPublishSpyContent(t)
	h := New(st, cs, "admin", &registryauth.FakeGitHubClient{})
	bundle := buildPublishBundle(t, map[string]string{"stack.yaml": "name: n\nowner: o\nversion: 1\nharness: codex\n", "README.md": "clean\n"})
	first := postBundle(t, h, "o", "n", bundle, "admin")
	if first.Code != http.StatusCreated { t.Fatalf("first = %d %s", first.Code, first.Body.String()) }
	delete(st.versionByKey, "o/n/1")
	st.versions["o/n"] = nil
	st.insertVersionCalls = 0
	st.insertedVersions = nil
	second := postBundle(t, h, "o", "n", bundle, "admin")
	if second.Code != http.StatusCreated { t.Fatalf("adopt = %d %s", second.Code, second.Body.String()) }
	if cs.commitCalls != 1 || st.insertVersionCalls != 1 { t.Fatalf("commit=%d insert=%d", cs.commitCalls, st.insertVersionCalls) }
}

func TestConflictingOrphanTagReturns409(t *testing.T) {
	st := newPublishSpyStore()
	cs := newPublishSpyContent(t)
	cs.tagCommits["o/n/v1"] = "different"
	h := New(st, cs, "admin", &registryauth.FakeGitHubClient{})
	bundle := buildPublishBundle(t, map[string]string{"stack.yaml": "name: n\nowner: o\nversion: 1\nharness: codex\n", "README.md": "clean\n"})
	rr := postBundle(t, h, "o", "n", bundle, "admin")
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "orphan tag") { t.Fatalf("response = %d %s", rr.Code, rr.Body.String()) }
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
	if rr.Code != http.StatusInternalServerError || !strings.Contains(logs.String(), "database unavailable") { t.Fatalf("response=%d logs=%q", rr.Code, logs.String()) }
	if strings.Contains(rr.Body.String(), "database unavailable") { t.Fatalf("client leaked internal error: %s", rr.Body.String()) }
}
```

Add `getVersionErr error` to `publishSpyStore`, return it first in `GetVersion`, and add `tagCommits map[string]string` plus this method to `publishSpyContent`:

```go
func (p *publishSpyContent) TagCommit(owner, name, tag string) (string, error) {
	if commit, ok := p.tagCommits[owner+"/"+name+"/"+tag]; ok { return commit, nil }
	return p.inner.TagCommit(owner, name, tag)
}
```

Initialize `tagCommits`; after a successful spy `Commit`, resolve `v1` with the inner store and cache it. Add delegating `TagCommit` methods to every other content fake.

- [ ] **Step 2: Verify failure** — `GOFLAGS=-mod=mod GOPROXY=off GOCACHE=/private/tmp/sherpa-go-build go test ./internal/registry/content/ ./internal/registry/api/ -run 'TestTagCommit|TestMatchingOrphan|TestConflictingOrphan|TestPublishInternalError'` → FAIL with undefined content capability and 500 logging behavior.

- [ ] **Step 3: Implement reconciliation and logging.** In `content.go`, add `errors`, declare `ErrNotFound`, and add `TagCommit` to the interface. Add to `baregit.go`:

```go
func (b *BareGit) TagCommit(owner, name, tag string) (string, error) {
	if err := validateOwnerName(owner, name); err != nil { return "", err }
	out, err := gitutil.Run(b.RepoPath(owner, name), "rev-parse", "--verify", tag+"^{commit}")
	if err != nil { return "", ErrNotFound }
	return strings.TrimSpace(out), nil
}
```

In `handlePublish`, after `GetVersion` returns `store.ErrNotFound`, calculate:

```go
	stagedCommit, err := gitutil.Run(stageDir, "rev-parse", gitTag+"^{commit}")
	if err != nil { log.Printf("resolve staged tag: %v", err); writeError(w, http.StatusInternalServerError, "internal server error"); return }
	orphanCommit, orphanErr := s.content.TagCommit(owner, name, gitTag)
	adoptOrphan := false
	switch {
	case orphanErr == nil && strings.TrimSpace(orphanCommit) == strings.TrimSpace(stagedCommit):
		adoptOrphan = true
	case orphanErr == nil:
		writeError(w, http.StatusConflict, "orphan tag exists with different content; reconcile before publishing")
		return
	case !errors.Is(orphanErr, content.ErrNotFound):
		log.Printf("inspect orphan tag: %v", orphanErr); writeError(w, http.StatusInternalServerError, "internal server error"); return
	}
```

Import `log` and `sherpa/internal/registry/content`. Replace the existing content commit block with:

```go
	if !adoptOrphan {
		if err := s.content.Commit(owner, name, stageDir); err != nil {
			log.Printf("commit published content: %v", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
			return
		}
	}
```

For the remaining 500 branches use this exact helper, added to `router.go`, and pass the actual underlying error at each call:

```go
func internalServerError(w http.ResponseWriter, operation string, err error) {
	log.Printf("%s: %v", operation, err)
	writeError(w, http.StatusInternalServerError, "internal server error")
}
```

Import `log` in `router.go`. Replace the publish 500 branches with `internalServerError` operations `resolve staged tag`, `scan published content`, `lookup version`, `snapshot manifest`, `encode scan report`, `upsert owner`, `upsert stack`, and `insert version`. Replace the read 500 branches with operations `search stacks`, `get stack`, and `get version`. Client bodies remain exactly `{"error":"internal server error"}`.

- [ ] **Step 4: Verify** — `gofmt -w internal/registry/content internal/registry/api && GOFLAGS=-mod=mod GOPROXY=off GOCACHE=/private/tmp/sherpa-go-build go test ./internal/registry/content/ ./internal/registry/api/ -count=1` → PASS.

- [ ] **Step 5: Commit** — `git add internal/registry/content internal/registry/api && git commit -m "fix: reconcile orphan tags and log registry 500s"`

---

### Task 6: CLI login/logout, mode-0600 session file, and publish token resolution

**Files:**
- Create: `internal/cli/cmd_auth.go`
- Create: `internal/cli/cmd_auth_test.go`
- Modify: `internal/cli/cmd_publish.go`

**Interfaces:**
- Consumes: `/v1/auth/device/start`, `/v1/auth/device/poll`, `Ctx.Home`, `SHERPA_REGISTRY_URL`, `SHERPA_REGISTRY_TOKEN`.
- Produces:

```go
func registrySessionPath(home string) string
func saveRegistrySession(home, token, login string) error
func loadRegistrySession(home string) (token, login string, err error)
func registryToken(home string) (string, error) // env admin token > session file
func cmdLogin(ctx *Ctx, args []string) error
func cmdLogout(ctx *Ctx, args []string) error
```

- [ ] **Step 1: Write the failing CLI test** in `cmd_auth_test.go`:

```go
package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoginLogoutAndTokenPrecedence(t *testing.T) {
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/auth/device/start":
			_ = json.NewEncoder(w).Encode(map[string]any{"device_code": "device", "user_code": "ABCD", "verification_uri": "https://github.com/login/device", "interval": 0, "expires_in": 900})
		case "/v1/auth/device/poll":
			polls++
			if polls == 1 { w.WriteHeader(http.StatusAccepted); _ = json.NewEncoder(w).Encode(map[string]string{"status": "pending"}); return }
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "sherpa-session", "login": "alice"})
		default: http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	home := t.TempDir()
	t.Setenv("SHERPA_REGISTRY_URL", srv.URL)
	t.Setenv("SHERPA_REGISTRY_TOKEN", "")
	var out bytes.Buffer
	ctx := &Ctx{Home: home, Stdout: &out, Stderr: &out, Stdin: strings.NewReader("")}
	if err := cmdLogin(ctx, nil); err != nil { t.Fatal(err) }
	if !strings.Contains(out.String(), "ABCD") || !strings.Contains(out.String(), "alice") { t.Fatalf("output = %q", out.String()) }
	info, err := os.Stat(filepath.Join(home, "registry-session.json"))
	if err != nil || info.Mode().Perm() != 0o600 { t.Fatalf("session mode = %v, err=%v", info.Mode().Perm(), err) }
	if token, err := registryToken(home); err != nil || token != "sherpa-session" { t.Fatalf("token = %q, %v", token, err) }
	t.Setenv("SHERPA_REGISTRY_TOKEN", "admin")
	if token, err := registryToken(home); err != nil || token != "admin" { t.Fatalf("admin precedence = %q, %v", token, err) }
	if err := cmdLogout(ctx, nil); err != nil { t.Fatal(err) }
	if _, err := os.Stat(filepath.Join(home, "registry-session.json")); !os.IsNotExist(err) { t.Fatalf("session still exists: %v", err) }
}
```

- [ ] **Step 2: Verify failure** — `GOFLAGS=-mod=mod GOPROXY=off GOCACHE=/private/tmp/sherpa-go-build go test ./internal/cli/ -run TestLoginLogoutAndTokenPrecedence` → FAIL with undefined CLI auth symbols.

- [ ] **Step 3: Implement CLI auth.** Create `cmd_auth.go`:

```go
package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type registrySession struct { AccessToken string `json:"access_token"`; Login string `json:"login"` }
type deviceStart struct { DeviceCode string `json:"device_code"`; UserCode string `json:"user_code"`; VerificationURI string `json:"verification_uri"`; Interval int `json:"interval"`; ExpiresIn int `json:"expires_in"` }

func init() { register("login", cmdLogin); register("logout", cmdLogout) }
func registrySessionPath(home string) string { return filepath.Join(home, "registry-session.json") }

func saveRegistrySession(home, token, login string) error {
	if err := os.MkdirAll(home, 0o700); err != nil { return err }
	b, err := json.Marshal(registrySession{AccessToken: token, Login: login})
	if err != nil { return err }
	tmp, err := os.CreateTemp(home, ".registry-session-*")
	if err != nil { return err }
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil { tmp.Close(); return err }
	if _, err := tmp.Write(append(b, '\n')); err != nil { tmp.Close(); return err }
	if err := tmp.Close(); err != nil { return err }
	return os.Rename(tmpName, registrySessionPath(home))
}

func loadRegistrySession(home string) (string, string, error) {
	b, err := os.ReadFile(registrySessionPath(home))
	if err != nil { return "", "", err }
	var session registrySession
	if err := json.Unmarshal(b, &session); err != nil { return "", "", err }
	if session.AccessToken == "" { return "", "", errors.New("registry session has no access token") }
	return session.AccessToken, session.Login, nil
}

func registryToken(home string) (string, error) {
	if token := strings.TrimSpace(os.Getenv("SHERPA_REGISTRY_TOKEN")); token != "" { return token, nil }
	token, _, err := loadRegistrySession(home)
	if os.IsNotExist(err) { return "", nil }
	return token, err
}

func cmdLogin(ctx *Ctx, args []string) error {
	if len(args) != 0 { return errors.New("usage: sherpa login") }
	base := strings.TrimSpace(os.Getenv("SHERPA_REGISTRY_URL"))
	startURL, err := registryEndpoint(base, "v1", "auth", "device", "start")
	if err != nil { return err }
	startResp, err := http.Post(startURL, "application/json", bytes.NewReader(nil))
	if err != nil { return err }
	defer startResp.Body.Close()
	if startResp.StatusCode != http.StatusOK { return fmt.Errorf("registry login start: %s", startResp.Status) }
	var start deviceStart
	if err := json.NewDecoder(startResp.Body).Decode(&start); err != nil { return err }
	fmt.Fprintf(ctx.Stdout, "Open %s and enter code %s\n", start.VerificationURI, start.UserCode)
	interval := time.Duration(start.Interval) * time.Second
	pollURL, err := registryEndpoint(base, "v1", "auth", "device", "poll")
	if err != nil { return err }
	deadline := time.Now().Add(time.Duration(start.ExpiresIn) * time.Second)
	for time.Now().Before(deadline) {
		if interval > 0 { time.Sleep(interval) }
		body, _ := json.Marshal(map[string]string{"device_code": start.DeviceCode})
		resp, err := http.Post(pollURL, "application/json", bytes.NewReader(body))
		if err != nil { return err }
		responseBody, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil { return readErr }
		if resp.StatusCode == http.StatusAccepted {
			var waiting struct { Status string `json:"status"` }
			if err := json.Unmarshal(responseBody, &waiting); err != nil { return err }
			if waiting.Status == "slow_down" { interval += 5 * time.Second }
			continue
		}
		if resp.StatusCode == http.StatusGone { return errors.New("GitHub device code expired") }
		if resp.StatusCode != http.StatusOK { return fmt.Errorf("registry login poll: HTTP %d", resp.StatusCode) }
		var session registrySession
		if err := json.Unmarshal(responseBody, &session); err != nil { return err }
		if err := saveRegistrySession(ctx.Home, session.AccessToken, session.Login); err != nil { return err }
		fmt.Fprintf(ctx.Stdout, "logged in as %s\n", session.Login)
		return nil
	}
	return errors.New("GitHub device code expired")
}

func cmdLogout(ctx *Ctx, args []string) error {
	if len(args) != 0 { return errors.New("usage: sherpa logout") }
	err := os.Remove(registrySessionPath(ctx.Home))
	if err != nil && !os.IsNotExist(err) { return err }
	fmt.Fprintln(ctx.Stdout, "logged out")
	return nil
}
```

The command prints the verification URL; browser auto-open is intentionally omitted because cross-platform process spawning is not required for correct device flow. In `publishRegistryVersion`, replace `os.Getenv("SHERPA_REGISTRY_TOKEN")` with:

```go
	token, err := registryToken(ctx.Home)
	if err != nil { return fmt.Errorf("load registry session: %w", err) }
	status, body, err := publishToRegistry(registryURL, token, owner, m.Name, bundlePath)
```

In `printRegistryPublishError`, before generic JSON printing, add:

```go
	if status == http.StatusForbidden {
		fmt.Fprintln(w, "you can only publish under @<your-github-login>")
		return
	}
```

- [ ] **Step 4: Verify** — `gofmt -w internal/cli && GOFLAGS=-mod=mod GOPROXY=off GOCACHE=/private/tmp/sherpa-go-build go test ./internal/cli/ -count=1` → PASS. Manually inspect `registry-session.json`: it contains only the SherpA token/login, never a GitHub token, and mode is 0600.

- [ ] **Step 5: Commit** — `git add internal/cli && git commit -m "feat: add CLI registry login logout and session auth"`

---

### Task 7: Fake-GitHub end-to-end identity flow and final security verification

**Files:**
- Modify: `internal/integration/registry_e2e_test.go`
- Modify: `cmd/registry/main_test.go`

**Interfaces:**
- Consumes: real Postgres via `store.StartPostgres`, real bare-git content, fake GitHub, CLI `login`/`publish`.
- Produces: end-to-end proof of login → own-login publish (`linked`) → other-owner 403, plus unchanged admin-token publishing (`unreviewed`).

- [ ] **Step 1: Replace the registry server helper and add the e2e test** in `registry_e2e_test.go`:

```go
func startRegistryServerWithGitHub(t *testing.T, gh auth.GitHubClient, adminToken string) (string, *store.PostgresStore) {
	t.Helper()
	dsn := store.StartPostgres(t)
	st, err := store.OpenPostgres(context.Background(), dsn)
	if err != nil { t.Fatalf("open postgres: %v", err) }
	t.Cleanup(func() { _ = st.Close() })
	handler := api.New(st, content.NewBareGit(t.TempDir()), adminToken, gh)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv.URL, st
}

func TestRegistryIdentityEndToEnd(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	t.Setenv("SHERPA_HOME", home)
	t.Setenv("SHERPA_REGISTRY_TOKEN", "")
	gh := &auth.FakeGitHubClient{
		Device: auth.DeviceCode{DeviceCode: "device", UserCode: "ABCD", VerificationURI: "https://github.com/login/device", Interval: 0, ExpiresIn: 60},
		PollResults: []auth.PollResult{{AccessToken: "github-token"}},
		Users: map[string]auth.GitHubUser{"github-token": {ID: 42, Login: "alice"}},
	}
	serverURL, st := startRegistryServerWithGitHub(t, gh, "admin-token")
	t.Setenv("SHERPA_REGISTRY_URL", serverURL)
	if out, errb, code := runCLI(t, nil, "login"); code != 0 || !strings.Contains(out, "alice") { t.Fatalf("login code=%d out=%s err=%s", code, out, errb) }
	if out, errb, code := runCLI(t, nil, "init"); code != 0 { t.Fatalf("init: %s %s", out, errb) }

	own := makeRegistryFixtureRepo(t, root, "alice", "own-stack", "Own stack", nil)
	if out, errb, code := runCLI(t, nil, "clone", own, "--name", "own-stack"); code != 0 { t.Fatalf("clone: %s %s", out, errb) }
	if _, errb, code := runCLI(t, nil, "use", "own-stack"); code != 0 { t.Fatal(errb) }
	if out, errb, code := runCLI(t, "yes\n", "publish", "--registry", serverURL); code != 0 { t.Fatalf("publish: %s %s", out, errb) }
	v, err := st.GetVersion(context.Background(), "alice", "own-stack", 2)
	if err != nil || v.TrustTier != "linked" { t.Fatalf("linked version = %#v, %v", v, err) }

	other := makeRegistryFixtureRepo(t, root, "bob", "other-stack", "Other stack", nil)
	if out, errb, code := runCLI(t, nil, "clone", other, "--name", "other-stack"); code != 0 { t.Fatalf("clone: %s %s", out, errb) }
	if _, errb, code := runCLI(t, nil, "use", "other-stack"); code != 0 { t.Fatal(errb) }
	if out, errb, code := runCLI(t, "yes\n", "publish", "--registry", serverURL); code == 0 || !strings.Contains(errb, "only publish under") { t.Fatalf("wrong-owner code=%d out=%s err=%s", code, out, errb) }
	if _, err := st.GetVersion(context.Background(), "bob", "other-stack", 2); !errors.Is(err, store.ErrNotFound) { t.Fatalf("wrong-owner write: %v", err) }
}
```

Add imports `errors` and `auth "sherpa/internal/registry/auth"`. Keep `startRegistryServer(t)` as:

```go
func startRegistryServer(t *testing.T) string {
	t.Helper()
	url, _ := startRegistryServerWithGitHub(t, &auth.FakeGitHubClient{}, "test-token")
	return url
}
```

Thus the existing admin-token e2e remains unchanged and must still pass. Update `cmd/registry/main_test.go` fixtures to include `GitHubClientID: "test-client"`; config tests assert the env is loaded, while an empty admin token is now valid.

- [ ] **Step 2: Verify the new test fails before final integration corrections** — `GOFLAGS=-mod=mod GOPROXY=off GOCACHE=/private/tmp/sherpa-go-build go test ./internal/integration/ -run TestRegistryIdentityEndToEnd -count=1` → FAIL if any router, fake, session-file, owner check, or trust-tier mapping is incomplete.

- [ ] **Step 3: Add the unchanged-admin assertion** to the existing admin e2e after its successful publish:

```go
	adminVersion, err := adminStore.GetVersion(context.Background(), "registryowner", "registry-clean", 2)
	if err != nil { t.Fatalf("get admin version: %v", err) }
	if adminVersion.TrustTier != "unreviewed" { t.Fatalf("admin trust tier = %q", adminVersion.TrustTier) }
```

Change the existing call at the top of `TestRegistryCLIEndToEnd` to:

```go
	serverURL, adminStore := startRegistryServerWithGitHub(t, &auth.FakeGitHubClient{}, "test-token")
```

Delete the one-result `startRegistryServer(t)` wrapper because both integration tests now call `startRegistryServerWithGitHub` directly. All constructor and test-double interface changes are already explicit in Tasks 3 and 5; this task introduces no production behavior.

- [ ] **Step 4: Run the complete verification matrix**:

```bash
gofmt -w cmd internal
test -z "$(gofmt -l cmd internal)"
GOFLAGS=-mod=mod GOPROXY=off GOCACHE=/private/tmp/sherpa-go-build go test ./... -count=1
GOFLAGS=-mod=mod GOPROXY=off GOCACHE=/private/tmp/sherpa-go-build go vet ./...
```

Expected: formatting check empty; all packages PASS (Docker-backed tests may skip only when Docker is unavailable); vet exits 0. Confirm test logs and HTTP bodies contain neither `github-token` nor any session token. Confirm the production GitHub adapter was exercised only through its `httptest.Server` test.

- [ ] **Step 5: Commit** — `git add -A && git commit -m "test: cover registry identity flow end to end"`

---

## Execution order & dependencies

Task 1 → Task 2 → Task 3 → Task 4 → Task 5 → Task 6 → Task 7. Tasks 1–2 establish boundaries and persistence; Task 3 depends on both; Task 4 depends on hashed session lookup; Task 5 depends on the authorized publish pipeline; Task 6 depends on stable endpoint JSON; Task 7 proves the whole path. Review Tasks 3 and 4 with Fable and all other tasks with Opus.

## Deliverables checklist (spec §4-9)

- [ ] §4.1 GitHubClient interface, scripted fake, error sentinels, and overridable real adapter (Task 1).
- [ ] §4.2 device start/poll status mapping, GitHub user lookup, and SherpA-only token response (Task 3).
- [ ] §4.3 opaque 32-byte session token, SHA-256-only storage, 90-day TTL, expired rejection (Tasks 2–3).
- [ ] §4.4 admin constant-time any-owner/unreviewed and session own-login-only/linked authorization before staging (Task 4).
- [ ] §4.5 trust tier persisted and surfaced in search, stack detail, and version detail (Tasks 2 and 4).
- [ ] §5 idempotent `github_id`, sessions schema, and `trust_tier` migration through existing runner (Task 2).
- [ ] §6 `sherpa login`/`logout`, URL/code display, interval/slow-down polling, mode-0600 session file, env-token precedence, clear 403 text (Task 6).
- [ ] §7 matching orphan adoption, conflicting orphan 409, and every 500 logged without client detail (Task 5).
- [ ] §8 deny-by-default 401/403, nothing-written and zero-stage proofs, unchanged fail-closed scan ordering, no token logging (Tasks 3–5).
- [ ] §9 fake-only auth tests, real-adapter `httptest`, real-Postgres store tests, authz matrix, admin e2e, and login/own/wrong-owner e2e (Tasks 1–7).
- [ ] Deferred exactly as specified: async re-scan workers, GitHub-org publishing, and automated `verified` blessing are not built.
- [ ] Production prerequisite recorded: register a GitHub OAuth App and set `SHERPA_GITHUB_CLIENT_ID`; no client secret is needed for the public-client device flow.
