# Phase 2 · 2e — Follows, Updates, and Trial Feedback Implementation Plan

> Keep checkbox state current and commit after each green task. Use GPT-5.6 Terra for
> implementation workers when available, with a separate adversarial/Fable-class review for the
> session, OAuth/grant, feed-concurrency, and CSRF boundaries.

**Status:** In progress; Tasks 1-4 implemented and verified

**Implementation record (2026-07-13):** Task 1 adds additive social tables, explicit CLI/web
session purposes, revocation and nonce-bound one-use grant primitives, transactional version/event
inserts, and fail-closed web-session publish rejection. Focused race tests, the uncached whole
suite, vet, command builds, and whitespace checks are green. Task 2 adds idempotent follows,
derived keyset-paged update feeds, monotonic seen state, verdict-only feedback, scoped personal
APIs, public follower counts, and bounded expert data with the same verification gates green.
Task 3 adds canonical registry issuers shared by session and non-secret state, backward-compatible
state migration, atomic `0600` saves, an issuer-scoped user-session lookup, and a typed bounded
social client that rejects redirects and redacts all remote details from errors. Its focused race
suite and the repository-wide verification gates are green.
Task 4 adds strict follow/unfollow/update commands, bounded terminal-safe update output, offline-
safe status caching, registry identity on registry-ref clones, recoverable auto-follow, and
best-effort monotonic seen synchronization after successful local updates. Real Git-over-HTTP
clone tests, focused race tests, and the repository-wide verification gates are green.

**Goal:** Add stack follows, bounded pending-update feeds, scoped website sign-in, CLI update
awareness, and privacy-preserving trial feedback while keeping publish constant-time in follower
count and never applying updates automatically.

**Architecture:** Postgres stores one event per immutable version and derives each user's feed
through indexed follows rather than publish-time fan-out. Existing device sessions become
explicit `cli` sessions; web OAuth exchanges a short-lived one-use grant for a restricted `web`
session held by the stateless website BFF. CLI state caches only non-secret follow/update data and
keeps trial notes local.

**Design:** `docs/superpowers/specs/2026-07-13-phase2-2e-follows-notifications-design.md`

**Tech stack:** Go 1.26 stdlib, pgx/Postgres 16 test harness, server-rendered `html/template`,
existing embedded CSS/JS, GitHub OAuth web flow with PKCE, Docker, Railway. No web framework,
SPA/CORS API, email provider, queue, or new Go dependency is required.

## Global constraints

- Preserve `mine`, profile atomicity, publish fail-closed scan/review, own-login/admin authz,
  immutable versions, pinned clone URLs, and the 50 MiB publish cap.
- Publish performs one version insert and one event insert in one DB transaction, independent of
  follower count. It must not query follows or call external notification services.
- A `web` session can use social APIs but cannot publish. The admin bearer can publish but cannot
  use `/v1/me/**` or impersonate a user.
- No feed read marks data seen. Seen state moves only through an explicit action or after a
  successful profile update and never moves backward.
- The website has no DB, content volume, GitHub secret, admin token, or deploy-time session key.
  It may hold a user's opaque web token only in a host-only secure cookie/request memory.
- Keep GitHub/SherpA tokens, OAuth code/state/verifier, grant, cookies, CSRF, trial notes, query
  text, and upstream bodies out of application logs and rendered errors.
- Keep all lists/body reads/cursors/cookies bounded. Authenticated web calls retain 2d's fixed
  upstream, redirect rejection, timeouts, and response-size cap.
- Every task ends with focused tests plus `git diff --check`. Run the uncached whole suite, vet,
  command builds, and both Docker smokes at the final automated gate. With an offline module
  cache use `GOFLAGS=-mod=mod GOPROXY=off GOCACHE=/private/tmp/sherpa-go-build`.
- Do not operate the user's Railway services. Tasks 1–9 are local. Task 10 documents the gate;
  its live steps are operator-owned after the current 2c-iii and 2d gates pass.

---

### Task 1: Add session purpose, atomic publish events, and grant primitives

**Files:**
- Modify: `internal/registry/store/migrations.go`
- Modify: `internal/registry/store/store.go`
- Modify: `internal/registry/store/postgres.go`
- Modify: `internal/registry/store/postgres_test.go`
- Modify: `internal/registry/store/testharness.go`
- Modify: `internal/registry/api/authorize.go`
- Modify: `internal/registry/api/authorize_test.go` (create if authorization tests currently live elsewhere)
- Modify affected Store fakes in `internal/registry/api/*_test.go`

**Interfaces:**

```go
type SessionPurpose string
const (
    SessionCLI SessionPurpose = "cli"
    SessionWeb SessionPurpose = "web"
)
type SessionIdentity struct {
    SessionID int64
    UserID    int64
    Login     string
    Purpose   SessionPurpose
}

CreateSession(context.Context, int64, string, SessionPurpose, time.Duration) error
SessionIdentity(context.Context, string) (SessionIdentity, error)
RevokeSession(context.Context, int64) error
CreateWebGrant(context.Context, userID int64, grantHash, handoffChallenge string, ttl time.Duration) error
ExchangeWebGrant(context.Context, grantHash, handoffChallenge, sessionHash string, ttl time.Duration) (SessionIdentity, error)
```

`ExchangeWebGrant` is one Postgres transaction: delete/return the unexpired grant, insert the
`web` session hash, and return identity. Generate the raw session token in the API layer before
calling it. No method returns or stores a raw token.

- [x] **Step 1: Write migration tests.** Assert fresh and repeated migrations; existing session
  rows become `cli`; named purpose check is created once; tables/foreign keys/checks/indexes match
  the design; additive migrations preserve old data.
- [x] **Step 2: Write session/grant tests.** Cover CLI/web identity, last-used update, expiry,
  idempotent revoke, expired grant, wrong hash, wrong handoff challenge, one-use exchange, and
  concurrent exchange with exactly one success/session. Assert errors never contain hashes.
- [x] **Step 3: Write publish transaction tests.** `InsertVersion` inserts exactly one
  `stack_published` event. Duplicate versions remain `ErrVersionExists`; forced event failure
  leaves no version; normal search/detail results are unchanged.
- [x] **Step 4: Implement migrations and store methods.** Use guarded idempotent constraints and
  existing pgx transactions. Delete expired grants opportunistically in a bounded statement, not
  as a correctness dependency.
- [x] **Step 5: Harden publish auth.** Refactor it to typed identity. Admin and same-owner CLI
  behavior remain exact; web is `403`; expired/revoked/unknown bearer is `401`; cross-owner CLI
  remains `403`.
- [x] **Step 6: Verify and commit.** Run:
  `go test -race ./internal/registry/store ./internal/registry/api`
  `git commit -m "feat(registry): add scoped sessions and publish events"`

---

### Task 2: Implement follow, update-feed, seen, feedback, and public social queries

**Files:**
- Modify: `internal/registry/store/store.go`
- Modify: `internal/registry/store/postgres.go`
- Modify: `internal/registry/store/postgres_test.go`
- Create: `internal/registry/api/social.go`
- Create: `internal/registry/api/social_test.go`
- Modify: `internal/registry/api/read.go`
- Modify: `internal/registry/api/read_test.go`
- Modify: `internal/registry/api/router.go`

**Store methods:**

```go
FollowStack(ctx context.Context, userID int64, owner, name string) (Follow, error)
UnfollowStack(ctx context.Context, userID int64, owner, name string) error
ListFollows(ctx context.Context, userID int64, page FollowPage) (FollowResult, error)
ListUpdates(ctx context.Context, userID int64, page UpdatePage) (UpdateResult, error)
MarkSeen(ctx context.Context, userID int64, owner, name string, version int) (Follow, error)
PutTrialFeedback(ctx context.Context, userID int64, owner, name string, version int, verdict Verdict) error
GetUser(ctx context.Context, handle string, maxRows, offset int) (UserProfile, []StackWithLatest, error)
```

Extend `StackWithLatest` and stack detail projections with `FollowerCount int`. Pagination structs
own opaque cursor encode/decode; API handlers never accept raw SQL keys.

- [x] **Step 1: Write Postgres behavior tests.** Cover first follow baselining current latest,
  no historical backlog, a later version becoming pending, duplicate/concurrent follow,
  unfollow idempotence, follow/publish race outcomes, independent users, deterministic keyset
  pages, maximum 50, mark-seen monotonicity, wrong-stack version rejection, and feedback upsert.
- [x] **Step 2: Prove query/resource bounds.** Seed more than one page, traverse without
  duplicates/gaps on a stable dataset, reject malformed/oversized cursors before querying, and
  use `EXPLAIN` assertions or explicit index inspection for the feed/follower hot paths.
- [x] **Step 3: Write the personal API auth matrix.** Missing/bad/expired/revoked sessions are
  `401`; CLI and web are allowed; admin bearer is `401`; unknown stack/version is `404`; malformed
  refs/body/cursor are `400`; over-limit body is `413`; all personal responses are `no-store`.
- [x] **Step 4: Implement strict handlers.** Register exact methods/routes from spec §6.2. Use
  `http.MaxBytesReader`, `DisallowUnknownFields`, one JSON value only, explicit content type, and
  the shared session middleware/helper. Keep error strings static and non-enumerating where auth
  has not succeeded.
- [x] **Step 5: Add public follower counts and expert endpoint.** Counts appear on search/stack
  JSON without changing order. `/v1/users/{handle}` exposes only canonical public fields and a
  bounded stack page. Add XSS-shaped values to response tests even though JSON escaping is not
  the website's final boundary.
- [x] **Step 6: Verify and commit.** Run:
  `go test -race ./internal/registry/store ./internal/registry/api`
  `git commit -m "feat(registry): add follows and bounded update feeds"`

---

### Task 3: Add the typed CLI social client and backward-compatible local state

**Files:**
- Modify: `internal/state/state.go`
- Modify: `internal/state/state_test.go`
- Refactor: `internal/cli/registryclient.go`
- Create: `internal/cli/registrysocial.go`
- Create: `internal/cli/registrysocial_test.go`
- Modify: `internal/cli/cmd_auth.go`
- Modify: `internal/cli/cmd_auth_test.go`

**State additions:**

```go
type RegistryOrigin struct { RegistryURL, Owner, Stack string; Version int }
type RegistryState struct {
    PendingFollows []string
    CachedUpdates  []UpdateSummary
    LastCheckedAt  time.Time
}
type State struct {
    // existing fields
    Registries map[string]RegistryState
    Trials     []TrialEntry
}
```

Add `Registry *RegistryOrigin` to `state.Profile`. JSON fields use `omitempty`; old state remains
readable. Canonical registry keys reuse the session issuer normalizer rather than inventing a
second URL equivalence rule.

- [x] **Step 1: Write state migration/permission tests.** Load old fixtures, nil maps, unknown
  additive fields, duplicate pending refs, and registry URLs with equivalent trailing slash/path
  forms. Save remains atomic and mode `0600`; no token enters serialized state.
- [x] **Step 2: Write the bounded client tests.** Follow/unfollow/list/seen/trial calls build fixed
  URLs, send the issuer-matched bearer only, reject redirects, cap JSON, honor context/timeouts,
  classify `401/403/404/410/429/5xx`, and redact bearer/body/base URL from errors.
- [x] **Step 3: Refactor auth lookup once.** Expose an internal helper that loads the registry-
  scoped session currently used by publish. Preserve `SHERPA_REGISTRY_TOKEN` precedence only for
  publish; personal calls explicitly reject/ignore it and require a saved user session.
- [x] **Step 4: Implement state/client.** Keep public search behavior compatible while consolidating
  endpoint/client construction enough to avoid unbounded `http.Get` in new paths. Do not broaden
  this task into an unrelated full client rewrite.
- [x] **Step 5: Verify and commit.** Run:
  `go test -race ./internal/state ./internal/cli`
  `git commit -m "feat(cli): add registry-scoped social state and client"`

---

### Task 4: Implement CLI follow commands, auto-follow, status, and update acknowledgement

**Files:**
- Create: `internal/cli/cmd_follow.go`
- Create: `internal/cli/cmd_follow_test.go`
- Create: `internal/cli/cmd_updates.go`
- Create: `internal/cli/cmd_updates_test.go`
- Modify: `internal/cli/cmd_clone.go`
- Modify: `internal/cli/cmd_clone_test.go`
- Modify: `internal/cli/cmd_switch.go`
- Modify: `internal/cli/cmd_switch_test.go`
- Modify: `internal/cli/cmd_update.go`
- Modify: `internal/cli/cmd_update_test.go`
- Modify: `internal/cli/cmd_run.go` only if command registration/help is centralized there

- [x] **Step 1: Write command parsing tests.** Strictly accept `@owner/name`, bounded `--limit`,
  and `@owner/name@vN` only for `--seen`. Reject URL refs, extra args, zero/negative versions,
  unknown flags, missing issuer, issuer mismatch, and admin-only auth.
- [x] **Step 2: Write offline/failure semantics.** `status` prints local data and exits success
  during timeout/401/5xx with one bounded warning. Feed/follow commands fail clearly without
  changing profiles. Listing updates never calls git, harness launch, mark-seen, or state save
  except to refresh the non-secret cache after a successful response.
- [x] **Step 3: Implement commands.** `follow`/`unfollow` are idempotent. `updates` presents ref,
  seen→new version, changelog, time, and `sherpa update` guidance. `--seen` is an explicit network
  mutation. Output is terminal-safe: remove control characters and bound server text.
- [x] **Step 4: Implement recoverable clone auto-follow.** Parse registry identity before URL
  resolution; write it to the installed profile; only after atomic install succeeds enqueue and
  attempt follow. Direct URL clones do not infer identity. Login/network failure warns and leaves
  one deduplicated pending entry; later social/status calls retry it.
- [x] **Step 5: Integrate successful update.** Track/update the profile's immutable registry
  version after merge commit succeeds, then best-effort mark it seen. A seen failure warns and is
  retryable; it never rolls back the merge or changes active-profile safety.
- [x] **Step 6: Verify and commit.** Run:
  `go test -race ./internal/cli ./internal/state ./internal/update`
  `git commit -m "feat(cli): add follows and pending update awareness"`

---

### Task 5: Add the private trial journal and explicit verdict sharing

**Files:**
- Modify: `internal/state/state.go`
- Modify: `internal/state/state_test.go`
- Create: `internal/cli/cmd_trial.go`
- Create: `internal/cli/cmd_trial_test.go`

**Behavior:** `record` requires an installed non-`mine` profile with immutable registry identity,
unless a later explicit `--local-only` design is approved. Entry IDs are random 128-bit base64url
values. Verdict is normalized to the API enum. Notes are UTF-8, at most 4 KiB, local only.

- [ ] **Step 1: Write journal tests.** Cover all verdicts, immutable owner/stack/version snapshot,
  random unique IDs, notes bound, old state, missing/non-registry profile, `mine` rejection,
  filtering, deterministic newest-first list, and terminal-safe rendering.
- [ ] **Step 2: Prove non-disclosure.** Plant secret-looking notes and assert `share` sends only
  verdict and route version; client errors/log/output do not echo notes; failed sharing does not
  set `SharedAt`; repeat sharing is idempotent and explicit.
- [ ] **Step 3: Implement subcommands.** Do not add session-end prompts or modify harness launch.
  `SharedAt` is recorded only after registry success. Re-recording creates a new local observation;
  the registry's one-row-per-user/version value becomes the latest explicitly shared verdict.
- [ ] **Step 4: Verify and commit.** Run:
  `go test -race ./internal/state ./internal/cli`
  `git commit -m "feat(cli): add private trial journal"`

---

### Task 6: Implement registry GitHub web OAuth and one-use website grants

**Files:**
- Modify: `internal/registry/config.go`
- Modify: `internal/registry/config_test.go`
- Extend: `internal/registry/auth/github.go`
- Modify: `internal/registry/auth/httpgithub.go`
- Modify: `internal/registry/auth/httpgithub_test.go`
- Create: `internal/registry/auth/weboauth.go`
- Create: `internal/registry/auth/weboauth_test.go`
- Create: `internal/registry/api/webauth.go`
- Create: `internal/registry/api/webauth_test.go`
- Modify: `internal/registry/api/abuse.go`
- Modify: `internal/registry/api/router.go`
- Modify: `cmd/registry/main.go`
- Modify: `cmd/registry/main_test.go`

**Configuration:**

| Variable | Registry behavior |
|---|---|
| `SHERPA_GITHUB_CLIENT_SECRET` | Required only when web OAuth is enabled; never logged |
| `SHERPA_WEB_PUBLIC_BASE_URL` | Fixed website HTTPS origin and sole grant redirect target |

Web OAuth is enabled only when both are present. In production `SHERPA_PUBLIC_BASE_URL` and web
base must be HTTPS. Device flow remains usable with only the client ID. Website callback path is
fixed `/auth/callback`; registry callback is fixed `/v1/auth/web/callback`.

- [ ] **Step 1: Write config validation tests.** Reject partial web config, credentials/query/
  fragment, non-HTTPS production origins, equal registry/website origins if cookie isolation would
  be ambiguous, and request-host influence. Preserve current local/device-only configurations.
- [ ] **Step 2: Write GitHub client tests.** Assert authorize URL contains exact client ID,
  callback, random state, S256 challenge, no broadened scope, and no secret. Token exchange sends
  secret/code/verifier only to fixed GitHub HTTPS endpoints with JSON accept, rejects redirects,
  caps bodies, validates token type, and re-fetches `/user` every login.
- [ ] **Step 3: Write adversarial handler tests.** Cover missing/duplicate/oversized code/state/
  handoff challenge, absent/tampered/mismatched OAuth cookie, GitHub denial, login rename/conflict,
  fixed redirect, Host/forwarded-host spoofing, newline values, callback replay, rate limits, and
  no sensitive logging. Assert OAuth cookie is
  `__Host-sherpa_oauth; Secure; HttpOnly; SameSite=Lax; Path=/` and is expired on every callback
  outcome.
- [ ] **Step 4: Write grant exchange tests.** Body-limit and auth-free fixed
  `POST /v1/auth/web/exchange` endpoint; its body requires grant plus handoff nonce. A valid pair
  returns a raw `web` session/login once; a copied grant with the wrong/missing nonce is `410`;
  concurrent replay gets one `200` and one `410`; expired or unknown grants are `410`; responses
  are no-store and never include grant/hash/nonce.
- [ ] **Step 5: Implement flow and separate rate-limit buckets.** Use `crypto/rand`, SHA-256 PKCE,
  constant-time state comparison, fixed error codes, two-minute grants, 30-day web sessions, and
  existing proxy-safe client IP logic. Never accept `return_to` from a request.
- [ ] **Step 6: Fable-class review and commit.** Review OAuth CSRF/login-CSRF, PKCE, cookie-prefix,
  redirect, replay, session-purpose, log, and host-header attacks before:
  `git commit -m "feat(registry): add scoped GitHub web sign-in"`

---

### Task 7: Add website session/CSRF boundary and authenticated registry client

**Files:**
- Modify: `internal/web/config.go`
- Modify: `internal/web/config_test.go`
- Extend: `internal/web/registryclient/client.go`
- Extend: `internal/web/registryclient/types.go`
- Modify: `internal/web/registryclient/client_test.go`
- Create: `internal/web/session.go`
- Create: `internal/web/session_test.go`
- Create: `internal/web/auth.go`
- Create: `internal/web/auth_test.go`
- Modify: `internal/web/web.go`
- Modify: `internal/web/middleware.go`
- Modify: `cmd/web/main.go`
- Modify: `cmd/web/main_test.go`

**Website configuration:** add required production `SHERPA_REGISTRY_PUBLIC_URL`, distinct from
the private `SHERPA_REGISTRY_API_URL`. It is used only for the `/v1/auth/web/start` browser
redirect. Both it and `SHERPA_WEB_PUBLIC_BASE_URL` are pinned absolute origins; Host headers never
construct either.

- [ ] **Step 1: Write public/private URL tests.** Validate path-prefix handling, HTTPS production,
  loopback-only HTTP test mode, no userinfo/query/fragment, no accidental private URL in rendered
  links, and no Host/forwarded-host influence.
- [ ] **Step 2: Write authenticated client tests.** Add typed `Me`, exchange, revoke, follow,
  unfollow, follows, updates, seen, and trial methods. The bearer is attached only to these fixed
  calls, never anonymous search/detail, redirects, error strings, or logs. Retain 1 MiB/timeout
  bounds and map grant `410` separately.
- [ ] **Step 3: Write cookie tests.** Exact `__Host-sherpa_login`, `__Host-sherpa_session`, and
  `__Host-sherpa_csrf` attributes, expiry/deletion, malformed/duplicate/oversized cookie rejection,
  and no reflection. All are `Secure; HttpOnly; SameSite=Lax; Path=/`; login handoff is ten minutes,
  session and CSRF are 30 days. Treat invalid/expired registry identity as signed out and clear
  local cookies.
- [ ] **Step 4: Write CSRF/Origin tests.** Every mutation requires POST, one form token matching
  one cookie, and one exact scheme/host/port Origin derived from the pinned public base. Reject
  absent/foreign/opaque/malformed/duplicate values before fake-registry calls. Local test helpers
  must not weaken production configuration.
- [ ] **Step 5: Implement sign-in/callback/logout.** `/login` creates a random handoff nonce cookie
  and issues a `303` to the pinned public registry start with only its SHA-256 challenge. Callback
  requires/clears that cookie and exchanges nonce plus one bounded grant server-side, sets session
  and CSRF cookies, and clears its query via `303 /dashboard`. Both callback responses use
  `Referrer-Policy: no-referrer`. Logout attempts revoke but always clears all three cookies.
- [ ] **Step 6: Fable-class review and commit.** Confirm browser token cannot reach publisher
  templates, JS, logs, URLs, anonymous upstream calls, or cross-origin responses before:
  `git commit -m "feat(web): add secure registry-backed sessions"`

---

### Task 8: Build follow-aware pages, dashboard, and expert profile

**Files:**
- Modify: `internal/web/registryclient/types.go`
- Modify: `internal/web/search.go`
- Modify: `internal/web/search_test.go`
- Modify: `internal/web/detail.go`
- Modify: `internal/web/detail_test.go`
- Create: `internal/web/dashboard.go`
- Create: `internal/web/dashboard_test.go`
- Create: `internal/web/profile.go`
- Create: `internal/web/profile_test.go`
- Modify: `internal/web/routes.go`
- Modify: `internal/web/render.go`
- Modify: `internal/web/templates/base.html`
- Modify: `internal/web/templates/stack.html`
- Modify: `internal/web/templates/stack_rows.html`
- Create: `internal/web/templates/dashboard.html`
- Create: `internal/web/templates/profile.html`
- Create: `internal/web/templates/auth_error.html`
- Modify: `internal/web/static/app.css`
- Modify: `internal/web/static/app.js` only if progressive status text needs an existing-safe extension
- Modify: `internal/web/presentation_test.go`

- [ ] **Step 1: Write signed-out/signed-in rendering tests.** Header sign-in/account state,
  follower count, Follow/Following forms, CSRF field, local return path, expired-session fallback,
  and no token/cookie in HTML. Anonymous pages preserve 2d canonical/indexing behavior.
- [ ] **Step 2: Write dashboard tests.** Empty/followed/pending/seen states; explicit mark reviewed;
  bounded navigation; no implicit seen call on GET; noindex/no-store; `401` clears session;
  `5xx` maps to safe degraded output without clearing a valid cookie.
- [ ] **Step 3: Write expert-page tests.** Bounded public stack list, sum labeled "stack follows",
  exact canonical URL, unknown handle 404, pagination, and adversarial publisher values. Do not invent
  bio/avatar fields or make counts into ranking claims.
- [ ] **Step 4: Implement SSR routes/forms.** Add `GET /users/{handle}`, `GET /dashboard`, and
  POST-only follow/unfollow/seen routes. Validate return paths as exact local routes, not arbitrary
  URLs. Use `303` after successful mutations to prevent resubmission.
- [ ] **Step 5: Extend Alpine styling accessibly.** Reuse established compact surfaces/badges;
  add clear signed-in navigation, follower text, update rows, and standard form buttons. No nested
  cards, inline script/style, external asset, layout shift, or color-only state.
- [ ] **Step 6: Verify templates and commit.** Run:
  `go test -race ./internal/web/... ./cmd/web`
  `git commit -m "feat(web): add follows dashboard and expert profiles"`

---

### Task 9: Run integrated adversarial and regression verification

**Files:**
- Modify: `internal/integration/registry_e2e_test.go`
- Create: `internal/integration/social_e2e_test.go`
- Modify focused tests from Tasks 1–8 as findings require
- Update: `docs/superpowers/specs/2026-07-13-phase2-2e-follows-notifications-design.md` only for
  verified contract corrections

- [ ] **Step 1: Add an end-to-end lifecycle.** Device login Alice, publish v1, Bob follows with
  CLI, confirm no backlog, Alice publishes v2, Bob sees v2 in API/CLI/web, feed GET leaves pending,
  successful update/explicit seen clears it, unfollow prevents later v3 from appearing.
- [ ] **Step 2: Add web-session separation.** Fake GitHub web login/exchange, follow through BFF,
  reject same token on publish, accept CLI same-owner publish, reject admin on `/v1/me`, revoke/
  logout, and reject grant replay.
- [ ] **Step 3: Run adversarial corpus.** Host/forwarded-host injection, redirect tricks, duplicate
  headers/query/cookies, multi-line proxy headers, malformed cursors/JSON, CSRF/login-CSRF, XSS,
  terminal controls, slow/large upstream bodies, concurrent follows/seen/grants/publishes, and
  planted credentials/notes in log assertions.
- [ ] **Step 4: Confirm old invariants.** Explicitly rerun publish cap/fail-closed/authz, clone
  atomicity, `mine`/offline status, update abort, session issuer collision, website CSP/canonical,
  registry audit/export, and 2d anonymous browser presentation tests.
- [ ] **Step 5: Run full automated gates.** From a clean environment:

  ```sh
  go test -count=1 ./...
  go test -race ./internal/registry/... ./internal/web/... ./internal/cli ./internal/state ./internal/integration
  go vet ./...
  go build ./cmd/...
  bash deploy/smoke_build.sh
  bash deploy/web/smoke_build.sh
  git diff --check
  ```

- [ ] **Step 6: Review and commit fixes.** A separate Fable-class pass must report no Important or
  Critical finding in auth, publish, social query, browser session, or privacy boundaries.
  `git commit -m "test: cover social lifecycle and auth boundaries"`

---

### Task 10: Update deployment/runbook and define the live 2e gate

**Files:**
- Modify: `docs/deployment/railway.md`
- Modify: `README.md` command/config sections if present
- Modify: `docs/superpowers/plans/2026-07-13-phase2-2e-follows-notifications.md` status after
  automated implementation only; do not mark the live gate complete without operator evidence

- [ ] **Step 1: Amend registry variables.** Document registry-only
  `SHERPA_GITHUB_CLIENT_SECRET`, registry-side `SHERPA_WEB_PUBLIC_BASE_URL`, separate staging/prod
  OAuth Apps and exact callback URLs, minimal scopes, secret rotation/revocation, and unchanged
  device-flow requirements. Never put values in docs or build args.
- [ ] **Step 2: Amend website separation truthfully.** Add public registry origin, browser cookies,
  and transient user session credentials. Keep no DB/volume/admin/GitHub secret/deploy-time
  session key. Document that auth availability now depends on the registry while public process
  health remains local.
- [ ] **Step 3: Add migration/rollback notes.** Migrations remain additive. Old images ignore new
  tables/columns and rollback safely; once web sessions exist, an old image treats them as normal
  sessions unless publish checks are in that old image. Therefore rollback below the 2e boundary
  requires first revoking/deleting `purpose='web'` sessions or disabling web login, then deploying
  the old image. This is a security-critical exception to the earlier generic additive rollback.
- [ ] **Step 4: Add live staging gate.** Record exact commits/domains/operator/times without
  secrets. Required checks:
  1. Existing 2c-iii and 2d gates passed for the candidate.
  2. Real GitHub website login uses the staging OAuth App and pinned callback/domain.
  3. OAuth/grant callback replay, copied grant without its handoff nonce, and tampered state fail;
     cookies show exact Secure/HttpOnly/SameSite/Path attributes; history is cleared to
     `/dashboard`.
  4. Web session follows but cannot publish; CLI session follows and same-owner publishes; admin
     cannot access personal endpoints.
  5. Follow current v1, publish v2, and observe identical pending state on dashboard and CLI;
     reading alone does not clear it.
  6. Successful CLI update or explicit web mark-reviewed clears pending without auto-activation;
     out-of-order seen requests do not restore old pending data.
  7. Registry-ref clone auto-follows online; registry outage still installs and queues; recovery
     syncs once without duplicates.
  8. Trial notes stay only in local `0600` state; explicit share sends verdict/version only.
  9. Spoofed Host/forwarded headers, foreign Origin, CSRF, open-return URL, and repeated grant are
     rejected at the real Railway edge.
  10. Registry outage keeps website `/healthz` up, degrades authenticated pages safely, and local
      CLI/status/profiles keep working; logout still clears website cookies.
  11. Railway logs and off-site export inspection expose no OAuth/session/grant/CSRF/trial-note
      values; export/restore includes follows/events/feedback and grant/session hashes only.
  12. Separate registry-only and website-only rollback drills preserve publish authorization and
      public discovery; pre-2e registry rollback follows the web-session revocation procedure.
- [ ] **Step 5: Verify docs and commit.** Run `git diff --check`, validate any changed JSON/shell,
  and commit:
  `git commit -m "docs: add 2e deployment and staging gate"`

## Completion criterion

Local implementation is complete when Tasks 1–10 code/docs and all automated gates are green,
with the plan status reading **Implemented; automated gates green, live Railway acceptance
pending**. Production readiness is a separate state and requires the operator-recorded 12-step
2e gate for the exact deployed commits after the existing registry and website gates.
