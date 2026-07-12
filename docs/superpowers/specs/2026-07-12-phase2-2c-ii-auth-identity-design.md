# SherpA Phase 2 · Sub-project 2c-ii — Auth + Publisher Identity + Trust

**Status:** Design for approval
**Date:** 2026-07-12
**Parent specs:** `2026-07-11-phase2-2c-registry-design.md` (§2 decomposition, carry-ins); builds on 2c-i (merged).

## 1. Goal

Make the registry **multi-user**: a real person logs in with GitHub, and publishing is bound
to their identity — you can publish under `@<your-github-login>` and nothing else. This
replaces 2c-i's single static token (which authorized publishing under *any* owner) with
GitHub-authenticated sessions, keeps the static token as an env-gated admin/CI escape hatch,
and records a **trust tier** per published version. Also folds in the two 2c-i carry-in fixes.

## 2. Scope (one plan; async re-scan cut)

**In:** GitHub OAuth **device flow** (CLI login, no browser redirect), server-mediated so the
client secret stays server-side; **session tokens** (opaque, stored hashed); **publish
authorization** (owner must be the caller's login); **trust tier** on versions; `sherpa
login`/`logout`; the carry-in fixes.

**Cut (YAGNI):** **async re-scan workers.** 2c-i already scans synchronously and fail-closed
on every publish, so nothing unscanned is ever stored. Async re-scan only helps re-scan *old*
versions when detection rules improve — a real but future need with no current driver. Defer
to its own later sub-project; do not build worker infrastructure now.

**Deferred (noted, not built):** GitHub-**org** publishing (own-login only in 2c-ii);
`verified` trust tier automation (schema allows the value; blessing is a future manual step).

## 3. Decisions (2026-07-12)

- **GitHub access via a `GitHubClient` interface + fake** (same pattern as Store/ContentStore).
  All auth logic is tested against the fake; the real HTTP adapter is thin and separately
  smoke-tested. GitHub's endpoints are never hit in unit/integration tests.
- **Publish scope: own login only.** A session-authenticated user may publish under
  `@<login>`; any other owner → 403.
- **Static token kept as env-gated admin/CI token.** A configured `SHERPA_REGISTRY_TOKEN`
  still authorizes publishing under any owner (CI, seeding, the 2c-i e2e); its publications are
  `trust_tier=unreviewed`. GitHub sessions are the normal user path (`trust_tier=linked`).

## 4. Architecture

New package `internal/registry/auth` (the GitHubClient interface + real adapter + fake +
session logic). The API gains auth endpoints and an authorizer the publish handler consults.

```
sherpa login ──▶ POST /v1/auth/device/start ──▶ GitHubClient.StartDeviceFlow ──▶ GitHub
   (poll)   ──▶ POST /v1/auth/device/poll  ──▶ GitHubClient.PollToken/GetUser ──▶ GitHub
                     │ on success: upsert user (github_id, login), mint session,
                     │ store token_hash, return opaque session token
                     ▼
sherpa publish (Bearer <session|admin>) ──▶ authorize(owner) ──▶ publish gate (2c-i)
```

### 4.1 `GitHubClient` interface

```go
type DeviceCode struct { DeviceCode, UserCode, VerificationURI string; Interval, ExpiresIn int }
type GitHubUser struct { ID int64; Login string }
type GitHubClient interface {
    StartDeviceFlow(ctx) (DeviceCode, error)                 // POST github.com/login/device/code
    PollToken(ctx, deviceCode string) (accessToken string, err error) // ErrAuthPending / ErrSlowDown / ErrExpired
    GetUser(ctx, accessToken string) (GitHubUser, error)     // GET api.github.com/user
}
```
Real adapter: `NewGitHubClient(clientID string, httpBaseURLs...)` — base URLs overridable for a
smoke test; needs a registered GitHub OAuth App's **client id** (client secret is not required
for device flow's public-client model; if used, it stays server-side via env). Fake:
scripted transitions (pending → token → user) for tests.

### 4.2 Auth endpoints

- `POST /v1/auth/device/start` → `{device_code, user_code, verification_uri, interval, expires_in}`
  (server calls `StartDeviceFlow`, returns GitHub's codes; the CLI shows `user_code` +
  `verification_uri`).
- `POST /v1/auth/device/poll` `{device_code}` → `200 {access_token}` (a **SherpA** session
  token) once GitHub confirms; `202 {status:"pending"|"slow_down"}` while waiting; `410` if
  expired. On confirm: `GetUser`, `Store.UpsertUser(login, github_id)`, mint a session.

### 4.3 Sessions

Opaque 32-byte random token, returned to the CLI once; only its **SHA-256 hash** is stored.
`sessions(id, user_id, token_hash unique, created_at, last_used_at, expires_at)`; default TTL
(e.g. 90d). Auth on publish: hash the Bearer, look up an unexpired session → user.

### 4.4 Publish authorization (replaces 2c-i's any-owner static token)

`authorizePublish(r, owner) (userHandle string, tier string, err)`:
1. If Bearer == the configured admin static token (constant-time) → allow any owner,
   `tier="unreviewed"`, handle = the path owner.
2. Else hash the Bearer → session lookup → user; require `owner == user.login` → else **403**;
   `tier="linked"`.
3. No/blank/invalid Bearer → **401**.
The publish handler calls this before staging; the resolved `tier` is written on the version.

### 4.5 Trust tier

`stack_versions.trust_tier text` (`unreviewed` | `linked`; `verified` reserved). Search and
detail responses include it. Migration: `ALTER TABLE ... ADD COLUMN IF NOT EXISTS trust_tier`.

## 5. Data model additions (idempotent migrations appended to 2c-i's runner)

```sql
ALTER TABLE users ADD COLUMN IF NOT EXISTS github_id bigint UNIQUE;
CREATE TABLE IF NOT EXISTS sessions(
  id bigserial PRIMARY KEY, user_id bigint REFERENCES users(id),
  token_hash text UNIQUE NOT NULL, created_at timestamptz DEFAULT now(),
  last_used_at timestamptz, expires_at timestamptz NOT NULL);
ALTER TABLE stack_versions ADD COLUMN IF NOT EXISTS trust_tier text NOT NULL DEFAULT 'unreviewed';
```
`Store` gains: `UpsertUserGitHub(ctx, login string, githubID int64)(userID,err)`,
`CreateSession(ctx, userID, tokenHash string, ttl)(err)`, `SessionUser(ctx, tokenHash)(login string, err)` [ErrNotFound/expired], and `InsertVersion` carries `TrustTier`.

## 6. CLI changes

- `sherpa login`: POST `/v1/auth/device/start`; print `user_code` + open/instruct
  `verification_uri`; poll `/v1/auth/device/poll` at `interval`; on success save the session
  token to `$SHERPA_HOME/registry-session.json` (0600, machine-local, never in a stack/profile,
  so unaffected by the publish barrier). Prints the logged-in GitHub login.
- `sherpa logout`: delete the local session file (optionally call a revoke endpoint — v1 just
  deletes locally).
- `sherpa publish --registry`: use the saved session token if present; `SHERPA_REGISTRY_TOKEN`
  env still overrides (admin/CI). A 403 (wrong owner) prints a clear "you can only publish under
  @<your-login>" message.
- Session token is resolved by a small helper (env `SHERPA_REGISTRY_TOKEN` > session file).

## 7. Carry-in fixes (from 2c-i whole-branch review)

- **Orphan tag → 409 + reconcile:** in publish, if `content.Commit`'s tag already exists but no
  version row is present (the 2c-i orphan window), return **409** (not 500) with a clear
  message; and on a *matching* re-publish (same version, same scanned HEAD) adopt the orphan by
  inserting the missing version row instead of erroring. (Fail-closed unchanged: content was
  already scanned.)
- **Server-side 500 logging:** `log.Printf` on every 500 path (error detail server-side only;
  client still gets the generic message).

## 8. Error handling / invariants

- Client secret / GitHub tokens never returned to the CLI or logged; only the SherpA session
  token reaches the client, and only its hash is stored.
- Session tokens: constant-time compare on the admin token; sessions matched by hash; expired
  sessions rejected (treated as 401).
- Publish authorization is **deny-by-default**: unknown/expired/wrong-owner → 401/403, before
  staging or any write. The 2c-i fail-closed scan gate is unchanged and still runs after authz.
- Device poll respects GitHub's `slow_down`/`interval`; the server never busy-loops GitHub.

## 9. Testing strategy

- **auth package:** device-flow logic against the fake `GitHubClient` — start returns codes;
  poll transitions pending → (GetUser) → session minted + hash stored; expired → 410; the real
  adapter gets a focused httptest wire-format test (its base URLs overridden).
- **Store (real Postgres):** UpsertUserGitHub idempotent on github_id; CreateSession +
  SessionUser round-trip; expired session → ErrNotFound; trust_tier persisted on versions.
- **Publish authz (the security surface):** admin token → any owner (unreviewed); session user
  → only own login (403 on mismatch); no/expired/invalid Bearer → 401; **each rejection writes
  nothing** (spies). A session publish records `trust_tier=linked`.
- **Carry-in:** orphan-tag republish → 409 (or adoption on exact match); a 500 path logs.
- **e2e (real Postgres + fake GitHub):** `login` (via fake device flow) → `publish` under own
  login succeeds (`linked`) → `publish` under a *different* owner → 403; the existing
  admin-token e2e still passes unchanged.

## 10. Alternatives considered

- **OAuth web/redirect flow.** Rejected: the CLI has no redirect URI; device flow is the
  standard CLI auth and keeps the secret server-side.
- **JWTs instead of opaque sessions.** Rejected for v1: opaque + hashed-in-DB gives instant
  revocation and no key-management surface; JWTs add complexity for no current benefit.
- **Build async re-scan now.** Rejected (YAGNI, §2).

## 11. Open questions (settle in the plan)

1. **GitHub OAuth App registration** (client id) is a real prerequisite for the *real* adapter
   in production — a 2c-iii/deploy config item, not a 2c-ii build blocker (the fake covers the
   logic). Note it in the plan.
2. **`sherpa login` UX** — auto-open the browser (`open`/`xdg-open`) vs just printing the URL.
   Minor; the plan can print + best-effort open.
