# SherpA Phase 2 · Sub-project 2e — Follows, Updates, and Trial Feedback

**Status:** Reviewed and ready for implementation
**Date:** 2026-07-13
**Parent specs:** `2026-07-08-follow-the-expert-design.md` (§4.2–4.6); builds on 2c-i
(registry API), 2c-ii (GitHub identity and issuer-scoped CLI sessions), 2c-iii (Railway), and
2d (public discovery website).

## 1. Goal

Close the expert-following loop without weakening SherpA's safety model. A signed-in user can
follow a stack, see later immutable versions on the website and CLI, explicitly mark an update
reviewed, and keep a private local trial journal with an explicit verdict-only sharing option.
`sherpa clone @owner/name` attempts to follow the installed stack, but installation remains
successful and usable when the registry or login is unavailable.

Following never downloads, activates, merges, or applies an update. `sherpa update` remains the
only update path and retains its review-and-merge behavior.

2e also adds web sign-in. The website remains a separate stateless Railway service with no
database, content volume, GitHub credential, admin token, or deploy-time session secret. It acts
as a narrow backend-for-frontend (BFF): an opaque, web-scoped SherpA session is held in a secure
first-party cookie and forwarded only by typed server-side calls to the registry's private API.

## 2. Review amendments

Review against the implemented 2c/2d system and the broader Phase 2 sketch changes the initial
product outline in these substantive ways:

1. **Do not fan out during publish.** Per-follower writes make publish latency and its five-minute
   write budget grow with popularity. Publish instead inserts one durable `stack_published` event
   in the same Postgres transaction as version metadata. Feeds are derived with bounded indexed
   joins. Email delivery, if later justified, may consume the same event stream asynchronously.
2. **Browser sessions cannot publish.** Existing device-flow sessions are powerful CLI bearer
   credentials. 2e adds a `purpose` to sessions (`cli` or `web`). Both may use personal social
   endpoints, but publish explicitly accepts only `cli`; the optional admin token remains publish-
   only and cannot impersonate a user.
3. **No browser bearer storage or cross-origin API.** JavaScript/localStorage and cross-domain
   cookies would broaden the credential and CORS surface. GitHub's callback stays on the pinned
   registry origin, then a two-minute, one-use grant transfers identity server-to-server to the
   website. The browser receives only a host-only `HttpOnly` website cookie.
4. **The website does not need a cookie encryption key.** The cookie value is already a random,
   opaque registry token whose hash alone is stored in Postgres. A separate random CSRF cookie,
   exact pinned-Origin checks, `SameSite=Lax`, and POST-only mutations protect the BFF. Avoiding a
   custom encrypted-cookie envelope removes key distribution and rotation without exposing user
   data in the cookie.
5. **A new follow has no historical backlog.** Its `last_seen_version_id` starts at the stack's
   current latest version. Only versions published after the follow are pending. Seen state moves
   monotonically and listing a feed never silently marks it seen.
6. **Clone auto-follow is best effort and recoverable.** A registry-ref clone records a registry-
   scoped pending follow after the atomic install, tries it immediately, and retries during later
   online social commands. Failure never rolls back or corrupts the installed profile.
7. **Trial notes stay local.** The registry accepts only `keep`, `keep_with_notes`, or `revert`
   plus the immutable version identity. Notes, prompts, transcripts, paths, and repository data
   are never uploaded. Sharing is a separate explicit command/action; no aggregate is published
   until a later privacy and anti-gaming design sets a minimum cohort.
8. **Stars and a global leaderboard are removed from 2e.** A follow already expresses durable
   interest, and the parent design rejects context-free popularity ranking. Most-followed and
   context-fit ranking remain future read-model work, not a hidden 2e launch dependency.

## 3. Scope

**In:**

- Follows, durable publish events, follower counts, bounded update feeds, monotonic seen state,
  and verdict-only trial feedback in Postgres.
- Session purpose/revocation, registry personal APIs, GitHub OAuth web flow with PKCE, and a
  single-use website exchange grant.
- CLI `follow`, `unfollow`, and `updates`; pending updates in `status`; registry-ref clone
  auto-follow; update success advancing seen state; a local trial journal and explicit sharing.
- Website sign-in/out, follow controls, a private dashboard, public follower counts, and a
  minimal public expert page listing published stacks.
- Adversarial auth/CSRF/replay tests, deployment documentation, and a live staging acceptance
  gate after the existing 2c-iii and 2d gates.

**Cut:**

- Email digests, mobile/push delivery, background workers, and per-follower notification rows.
- Stars, trending/ranking algorithms, public keep-rate aggregates, badges, moderation, editable
  bios/avatars, and follower identity lists.
- Browser content trees, README rendering, version diffs, and fork-lineage graphs.
- MCP endpoints, automated compare, session-end hooks/prompts, and automatic update application.
- Counting raw installs/tries. Those require abuse-resistant event semantics before becoming
  useful product metrics.

## 4. Product invariants

1. `mine` and installed profiles keep working with no registry or website.
2. Following and feed reads never mutate a profile or Git worktree.
3. Listing updates does not mark them seen. A successful `sherpa update` may advance the matching
   follow; website and CLI also expose explicit mark-reviewed actions.
4. A failed install never creates a follow. A successful install is never rolled back because
   follow sync failed.
5. Web credentials cannot authorize publish. Admin credentials cannot access `/v1/me/**`.
6. No GitHub token, SherpA token, OAuth code/verifier/state, CSRF token, grant, trial note, query,
   or upstream error body is logged.
7. Publish remains fail-closed, version-immutable, owner-authorized, host-pinned, and capped at
   50 MiB. Its only 2e side effect is one bounded transactional event insert.
8. Every list is bounded and deterministically ordered. No endpoint loads all followers or all
   events into memory.

## 5. Data model

Additive migrations preserve current rollback safety:

```sql
alter table sessions
  add column if not exists purpose text not null default 'cli';
alter table sessions
  add constraint sessions_purpose_check check (purpose in ('cli', 'web'));

create table follows (
  user_id bigint not null references users(id) on delete cascade,
  stack_id bigint not null references stacks(id) on delete cascade,
  last_seen_version_id bigint references stack_versions(id),
  created_at timestamptz not null default now(),
  primary key (user_id, stack_id)
);

create table events (
  id bigserial primary key,
  type text not null check (type = 'stack_published'),
  stack_version_id bigint not null references stack_versions(id),
  created_at timestamptz not null default now(),
  unique (type, stack_version_id)
);

create table web_grants (
  token_hash text primary key,
  user_id bigint not null references users(id) on delete cascade,
  handoff_challenge text not null,
  expires_at timestamptz not null,
  created_at timestamptz not null default now()
);

create table trial_feedback (
  user_id bigint not null references users(id) on delete cascade,
  stack_version_id bigint not null references stack_versions(id) on delete cascade,
  verdict text not null check (verdict in ('keep', 'keep_with_notes', 'revert')),
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now(),
  primary key (user_id, stack_version_id)
);
```

Migration code must create named constraints idempotently through a guarded `DO` block; a plain
`add constraint` is not rerunnable. Required indexes cover `events(id)`,
`events(stack_version_id)`, `follows(user_id, stack_id)`, `follows(stack_id)`, and
`web_grants(expires_at)`. The primary/unique indexes already cover some of these; do not duplicate
them mechanically.

`InsertVersion` becomes a transaction: insert immutable metadata, then its event, then commit.
An existing version still returns `ErrVersionExists`. Git content is committed before metadata as
today; the consistency audit remains the recovery tool for interrupted cross-store publishes.

`FollowStack` locks/resolves the stack and its latest version and inserts the current latest ID as
`last_seen_version_id`. Concurrent duplicate calls converge. `MarkSeen` accepts a version from
the followed stack and updates only when its numeric version is newer than the currently seen
version. It cannot move backwards or mark a version from another stack.

Expired sessions and grants are rejected by query predicates. Deletion is opportunistic and
bounded; correctness never depends on cleanup timing.

## 6. Registry API

### 6.1 Authentication model

Replace login-only session lookup with a typed identity:

```go
type SessionIdentity struct {
    SessionID int64
    UserID    int64
    Login     string
    Purpose   SessionPurpose // cli | web
}
```

Personal endpoints accept either session purpose and reject the admin token. Publish accepts the
admin token as today or a same-owner `cli` identity; a `web` identity receives `403`. Authentication
uses the existing SHA-256 token hash and constant-time admin comparison. `DELETE /v1/me/session`
revokes only the presented session and is idempotent.

### 6.2 Personal endpoints

All responses are JSON, body-limited, unknown-field rejecting, and `Cache-Control: no-store`.

| Method and route | Contract |
|---|---|
| `GET /v1/me` | Login and session purpose |
| `DELETE /v1/me/session` | Revoke presented session; `204` |
| `PUT /v1/me/follows/{owner}/{name}` | Idempotently follow; returns current follow state |
| `DELETE /v1/me/follows/{owner}/{name}` | Idempotently unfollow; `204` |
| `GET /v1/me/follows?limit=&cursor=` | At most 50, owner/name ordered with opaque cursor |
| `GET /v1/me/updates?limit=&cursor=` | Events after each follow's seen version, newest first |
| `PUT /v1/me/follows/{owner}/{name}/seen` | Body `{\"version\":N}`; monotonic, idempotent |
| `PUT /v1/me/trials/{owner}/{name}/{version}` | Body `{\"verdict\":...}`; idempotent upsert |

Defaults are 25 rows; maximum is 50. Cursors are opaque base64url encodings of the deterministic
sort tuple and are strictly bounded before decode. Malformed cursors are `400`; nonexistent stacks
or versions are `404`; an authenticated user following nothing receives an empty `200` response.
Update rows contain ref, version, git tag, changelog, published time, trust tier, and follower's
seen version. They never contain manifests, scan excerpts, or repository URLs.

### 6.3 Public additions

- Search and stack detail add `follower_count` as a non-negative integer.
- `GET /v1/users/{handle}?limit=&offset=` returns the canonical handle and a bounded list of that
  user's published stacks. No email, GitHub token, GitHub numeric ID, sessions, follower identities,
  or private profile data is exposed.
- Public counts are presentation signals only. 2e does not sort or rank by them.

### 6.4 Web-auth endpoints

| Method and route | Contract |
|---|---|
| `GET /v1/auth/web/start?handoff_challenge=` | Validate website nonce challenge, set bounded OAuth state/PKCE cookie, and redirect to fixed GitHub authorize endpoint |
| `GET /v1/auth/web/callback` | Validate GitHub callback, create one-use grant, redirect to fixed website callback |
| `POST /v1/auth/web/exchange` | Consume `{\"grant\":\"...\",\"handoff_nonce\":\"...\"}` server-to-server and mint one `web` session |

The exchange endpoint is public only in the sense that it needs no prior SherpA session. The
unguessable one-use grant is its credential. It is body-, time-, and rate-bounded, does not enable
CORS, and returns `410` for expired, consumed, or unknown grants without distinguishing them.

## 7. Web OAuth and BFF session

GitHub documents separate device and web flows, recommends an unguessable `state`, recommends
PKCE, requires the client secret for a confidential web exchange, and requires identity
revalidation after every login. The implementation follows that sequence and requests no extra
scope: [Authorizing OAuth apps](https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/authorizing-oauth-apps).
The client secret remains a registry-only Railway secret, consistent with GitHub's
[OAuth app security guidance](https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/best-practices-for-creating-an-oauth-app).

### 7.1 Flow

1. Website `GET /login` generates a random 32-byte handoff nonce, stores it in a ten-minute
   `Secure; HttpOnly; SameSite=Lax; Path=/` `__Host-sherpa_login` cookie, and redirects to the
   registry's pinned public `GET /v1/auth/web/start` with only the base64url SHA-256 challenge.
   There is no caller-supplied return URL.
2. Registry generates 32-byte `state` and PKCE verifier values. It stores them only in one
   strictly decoded `Secure; HttpOnly; SameSite=Lax; Path=/` `__Host-sherpa_oauth` cookie together
   with the validated handoff challenge, then redirects to
   GitHub with `state`, `code_challenge`, `code_challenge_method=S256`, the exact pinned callback,
   and no requested scope.
3. Callback requires one value for `code` and `state`, constant-time matches state to the cookie,
   clears the cookie on every outcome, exchanges code plus verifier and client secret, then calls
   GitHub `/user` and applies existing identity conflict/rename rules.
4. Registry stores only a hash of a random two-minute web grant tied to the user and handoff
   challenge, then redirects to the fixed website `/auth/callback?grant=...`. Errors redirect with
   a fixed non-sensitive code. The grant may appear transiently in browser/edge metadata but is
   unusable without the website's nonce cookie.
5. Website callback requires and clears `__Host-sherpa_login`, then POSTs the grant and raw handoff
   nonce to `/v1/auth/web/exchange` server-to-server over the private registry URL. Registry hashes
   the nonce and constant-time matches the stored challenge. A single Postgres transaction then
   consumes the grant and creates a 30-day `web` session. Concurrent/replayed exchanges yield
   exactly one success.
6. Website sets the raw opaque token in `__Host-sherpa_session` with `Secure; HttpOnly;
   SameSite=Lax; Path=/; Max-Age=2592000`, sets a 32-byte `__Host-sherpa_csrf` cookie with
   `Secure; HttpOnly; SameSite=Lax; Path=/; Max-Age=2592000`, and redirects to `/dashboard` with no
   query. Both callbacks set `Referrer-Policy: no-referrer`; website application logs omit query.

Production requires HTTPS pinned bases. Local tests may explicitly allow loopback HTTP and use
non-Secure test-cookie helpers without changing production defaults. The GitHub OAuth App has one
callback URL, so staging and production require separate apps as already required by the Railway
runbook.

The registry public base remains the OAuth callback issuer; request `Host`, `Forwarded`, and
`X-Forwarded-Host` never influence a redirect. The website base configured in the registry is the
only grant redirect target. Grant/query values are redacted from access logs.

### 7.2 Configuration

- Registry `SHERPA_GITHUB_CLIENT_SECRET` and registry-side `SHERPA_WEB_PUBLIC_BASE_URL` are an
  all-or-nothing pair enabling web OAuth. Device flow remains available with only
  `SHERPA_GITHUB_CLIENT_ID`.
- Website `SHERPA_REGISTRY_PUBLIC_URL` is the pinned browser-visible registry origin used only for
  `/v1/auth/web/start`. `SHERPA_REGISTRY_API_URL` remains the private server-to-server origin.
- Staging and production use separate GitHub OAuth Apps because an OAuth App has
  [one configured callback URL](https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/creating-an-oauth-app).
  Each callback is the exact pinned registry public base plus
  `/v1/auth/web/callback`.
- Production bases are HTTPS. Explicit loopback HTTP is allowed only by test/local configuration.

### 7.3 Website request security

- Public GET pages remain anonymous and indexable. If a valid session cookie exists, the BFF may
  fetch `/v1/me` to render controls; an invalid/expired session is cleared and treated signed out.
- Every mutation is a website POST form with a CSRF field exactly matching the CSRF cookie and an
  `Origin` exactly matching the scheme/host/port origin derived from the configured website public
  base. Missing, duplicate, malformed, or foreign Origin/CSRF values are `403` before an upstream
  call.
- Authenticated registry calls use a dedicated typed client. It sends the cookie token only as an
  `Authorization` header to the fixed private origin, never follows redirects, and never forwards
  browser headers/cookies.
- `/dashboard`, auth callback, and every authenticated response use `Cache-Control: no-store` and
  `X-Robots-Tag: noindex`; the existing CSP remains external-script-free and `form-action 'self'`.
- Logout first attempts registry revocation, always expires the session, CSRF, and login-handoff
  cookies, and redirects to `/`. A registry outage cannot trap a user in a local session.

## 8. Website behavior

- Stack pages show follower count. Signed-out users get a sign-in action; signed-in users get an
  idempotent Follow/Following control. Forms preserve only a server-validated local return path.
- `/users/{handle}` is a public expert page: canonical handle, the sum of stack follows (labeled
  "stack follows", not unique people), and bounded stack rows. No speculative biography or
  GitHub-derived avatar is shown.
- `/dashboard` shows followed stacks, pending versions, explicit Mark reviewed actions, and local
  CLI guidance. The server does not claim to know locally installed profiles or private trial
  notes; those remain on the user's machine.
- An empty dashboard is a normal state. An unavailable registry produces the existing bounded
  degraded page and never exposes the session token or upstream cause.
- Existing home/search ordering remains recent-first. Follower counts do not silently redefine it
  as trending.

## 9. CLI and local state

Commands:

```text
sherpa follow @owner/name
sherpa unfollow @owner/name
sherpa updates [--limit N]
sherpa updates --seen @owner/name@vN
sherpa trial record <profile> --verdict keep|keep-with-notes|revert [--notes TEXT]
sherpa trial list [--profile NAME]
sherpa trial share <entry-id>
```

`follow`, `unfollow`, `updates`, and `trial share` require an issuer-matching CLI session. Admin
tokens are never used for personal commands. `status` always prints local profile state first;
when a matching session and registry URL exist, it prints a bounded pending-update summary. A
network/auth failure is a warning and does not make local status fail.

`state.json` gains backward-compatible registry-scoped data and a local journal:

```go
type RegistryState struct {
    PendingFollows []string  `json:"pending_follows,omitempty"`
    CachedUpdates  []Update  `json:"cached_updates,omitempty"`
    LastCheckedAt  time.Time `json:"last_checked_at,omitempty"`
}
type TrialEntry struct {
    ID, Profile, RegistryURL, Owner, Stack string
    Version int
    Verdict, Notes string
    RecordedAt time.Time
    SharedAt *time.Time
}
type RegistryOrigin struct {
    RegistryURL, Owner, Stack string
    Version int
}
```

`Profile` gains an optional `Registry *RegistryOrigin` so a registry-ref clone preserves identity
without reverse-engineering the resolved git URL; update success advances its version. Registry
maps use the same canonical issuer normalization as the session file. Tokens are never copied
into `state.json`. Values loaded from older files initialize to empty maps/slices. Journal
notes are length-bounded, stored mode `0600` through the existing atomic save, printed only by the
explicit `trial list`, and never included in logs or network requests.

A registry-ref clone preserves `{issuer, owner, name}` separately from the resolved git URL. Only
after install and state save succeed does it enqueue/attempt the follow. Direct URL clones do not
infer registry identity from a hostname. `status`, `follow`, and `updates` retry queued follows;
success removes the exact entry. Duplicate entries normalize away.

After `sherpa update <profile>` successfully commits a newer upstream version, it best-effort marks
that version seen when the profile has registry identity. Update success is never reversed by a
failed seen call. A warning plus future retry is sufficient. Feed output never runs git or invokes
a harness.

## 10. Resource and abuse bounds

- OAuth start and callback use separate per-IP limits under the existing proxy trust rules.
  Grant exchange is limited by IP and grant hash. Device-flow limits remain unchanged.
- Personal JSON bodies are at most 8 KiB. OAuth query values, cookies, refs, cursors, notes, and
  return paths have explicit byte limits before decoding or allocation.
- Feed/follow/profile queries cap at 50 and use indexed keyset pagination where data can grow;
  public expert stack pagination may retain the existing bounded offset convention.
- Publish adds constant database work independent of follower count. There are no email calls,
  webhooks, or follower scans in the handler.
- Website cookie headers stay below browser limits and are never reflected. Upstream response
  limits and timeouts from 2d apply to authenticated calls too.

## 11. Failure and concurrency semantics

| Failure | Result |
|---|---|
| Version insert or event insert fails | Metadata transaction rolls back; publish reports failure; audit/recovery handles committed Git content |
| Duplicate/concurrent follow | One row, both callers observe followed state |
| Follow races with publish | Row starts at a transactionally observed latest version; the publish is either baseline or pending, never partially represented |
| Mark-seen races/out of order | Greatest valid version wins; state never moves backward |
| Grant copied without handoff nonce | Exchange is `410`; no session is minted |
| Grant exchanged twice/concurrently | Exactly one web session is minted; all later exchanges are `410` |
| Website loses registry after cookie set | Public process stays healthy; authenticated pages degrade; cookie is not exposed |
| Logout while registry is down | Local cookies clear; server session expires naturally or can be revoked later |
| Auto-follow fails | Installed profile remains; pending follow retries later |
| Trial share fails | Local entry and notes remain; `SharedAt` is unchanged |

## 12. Verification and release gate

Automated gates include Postgres concurrency tests, event/version atomicity, cursor bounds, no
historical follow backlog, monotonic seen state, session-purpose auth matrices, grant replay,
OAuth state/PKCE/open-redirect adversarial tests, cookie attributes, Origin/CSRF rejection,
upstream credential containment, CLI offline behavior, state migration, clone failure semantics,
trial note non-disclosure, XSS tests, race tests, whole-suite tests, vet, builds, and both Docker
smokes.

Live 2e acceptance runs only after the exact registry and website commit passes the existing
2c-iii and 2d Railway gates. It must prove real GitHub web login on staging, callback/domain
pinning at Railway's edge, cookie flags, follow/update behavior across a real publish, web-session
publish rejection, CLI/website consistency, logout/replay rejection, registry-outage degradation,
and secret-free logs. Production promotion is blocked until those results are recorded.

## 13. Deferred follow-ups

- **2f:** content read API, rendered stack files, immutable version diffs, and fork lineage.
- **2g:** MCP read/stage surface using existing CLI sessions and explicit activation boundary.
- **2h:** compare automation, session-end trial prompts, privacy-thresholded context-fit ranking,
  and benchmark design.
- Email delivery is considered only after update-feed retention and engagement justify an
  asynchronous worker/provider. The 2e event table is its durable source; no schema rewrite is
  required.
