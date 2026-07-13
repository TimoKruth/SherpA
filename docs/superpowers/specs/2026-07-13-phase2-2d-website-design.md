# SherpA Phase 2 · Sub-project 2d — Discovery Website

**Status:** Design for approval
**Date:** 2026-07-13
**Parent specs:** `2026-07-08-follow-the-expert-design.md` (§4.2 registry, website); builds on 2c-i
(registry API core), 2c-ii (auth + trust tier), 2c-iii (Railway deployment) — all merged.

## 1. Goal

Give SherpA a **public website** where anyone can discover expert stacks in a browser: search,
read a stack's summary/tags/trust tier, inspect a version's manifest and scan report, and copy
the exact CLI command to try or clone it. The website is the discovery surface; the **CLI stays
the action surface** (clone / try / publish). No account, no login, no writes on the website in
2d.

The overriding architectural constraint (user decision, 2026-07-13): the website is a
**separate service, deployed independently on Railway, so that either service staying up does
not depend on the other.** The registry can be down and the website still serves (a degraded
page); the website can be down and the registry/CLI are wholly unaffected.

## 2. Scope

**In:**
- A new binary `cmd/web`: a server-rendered Go HTTP site (stdlib `net/http` + `html/template`),
  its own Dockerfile and `railway.json`, deployed as a **second Railway service**.
- Pages: **home** (search box + a broad/recent stack list), **search results**, **stack
  detail** (versions, trust tier, tags, "try it" CLI commands), **version detail** (manifest
  snapshot, scan report, changelog, clone/try commands).
- A thin, resilient **registry API client** (bounded timeout, context-cancellable) that the
  website uses to read data. The website holds **no database and no secrets**.
- `/healthz` for Railway, deliberately **independent of the registry's health**.

**Cut (YAGNI / wrong sub-project):**
- **Accounts, login, following, trial feedback** on the website. "Follow the expert" social
  features are **2e**; putting them here would require the website to hold sessions/secrets and
  couple it to auth, defeating the fault-isolation goal. The website is read-only in 2d.
- **In-browser git file browsing** (rendering the stack's actual CLAUDE.md/skills/hooks trees
  in the page). The parent spec keeps this swappable behind `ContentStore` for later; it needs a
  file-tree read path the registry does not yet expose (or a Forgejo swap). 2d shows metadata +
  "clone with the CLI"; file browsing is a later sub-project.
- **Any change to the registry service.** The website is a pure consumer of the existing 2c-i/ii
  public read API. If a home-page ordering need can't be met by the existing endpoints, that's a
  minor registry follow-up, explicitly out of 2d's scope (see §5, home page).

## 3. Decisions (2026-07-13)

- **Fault isolation via the public HTTP API, not a shared database.** The website talks to the
  registry only through its existing public read endpoints (`/v1/search`,
  `/v1/stacks/{owner}/{name}`, `/v1/stacks/{owner}/{name}/versions/{v}`) — the same JSON contract
  the CLI consumes. It does **not** open Postgres and does **not** share the registry's
  connection pool or schema. This is what makes the two services independently deployable and
  independently failing: a stateless renderer over a public API. A registry outage degrades the
  website to an "unavailable" page; it never crashes it. **Rejected alternative:** website reads
  Postgres directly — faster, but re-couples the services (shared schema, shared pool, shared
  failure) and re-implements query/permission logic, so it loses the exact property the user
  asked for.
- **Server-rendered, `html/template`, minimal client JS.** Pages are rendered server-side and
  work with JavaScript disabled. Any JS is progressive enhancement only. This matches the "server
  rendered go" decision and keeps the security surface small.
- **Read-only, unauthenticated.** Every page is public (read endpoints are already public per
  2c-i §3). No sessions, no cookies carrying identity, no CSRF surface, no secrets in the web
  service's environment.
- **Reuse the 2c-iii deployment patterns.** Own multi-stage non-root Dockerfile, own
  `railway.json` with `healthcheckPath: /healthz`, `numReplicas: 1`, graceful shutdown, bounded
  read/write timeouts — the same hardening the registry got, minus everything DB/secret-related
  (which the website doesn't have).

## 4. Architecture

New binary + one internal package. Nothing in the registry changes.

```
  browser ──HTTP──▶ cmd/web (Railway service #2, server-rendered) ──HTTP──▶ registry API (service #1)
                     │  net/http router → handlers → html/template          /v1/search
                     │  internal/web/client (bounded, cancellable)          /v1/stacks/{o}/{n}
                     │  /healthz  ← independent of registry                 /v1/stacks/{o}/{n}/versions/{v}
                     └─ no DB, no secrets, stateless
```

- **`cmd/web`** — main: load config from env, build the registry client, mount the router,
  serve with a hardened `http.Server`, graceful shutdown (mirrors `cmd/registry/main.go`
  structure).
- **`internal/web/client`** — the registry API client. Typed methods `Search`, `GetStack`,
  `GetVersion` returning Go structs that mirror the registry's JSON (`searchStackResponse`,
  `stackResponse`, `versionResponse` shapes). Uses a shared `*http.Client` with a bounded
  timeout; every call takes the request `context.Context` so a slow registry cannot pin a web
  request open. Maps registry `404 → ErrNotFound`, any non-2xx / transport error →
  `ErrUpstream` (so handlers can render 404 vs. degraded distinctly).
- **`internal/web`** — HTTP handlers + routing + the `html/template` set (embedded via
  `//go:embed`). One handler per page. Handlers are thin: parse request → call client → render
  template (or render the degraded/`404` page).
- **Templates + CSS** — `html/template` with a base layout; CSS is a single stylesheet served
  from an embedded file at `/static/app.css` (embedded via `//go:embed`, no external CDN). A
  strict `Content-Security-Policy` with no external origins.

### 4.1 Routes (website URLs)

| Route | Renders |
|---|---|
| `GET /` | Home: search form + a broad stack list (calls `/v1/search?q=`) |
| `GET /search?q=&harness=&tag=` | Search results (calls `/v1/search` with the same params) |
| `GET /stacks/{owner}/{name}` | Stack detail (calls `/v1/stacks/{owner}/{name}`) |
| `GET /stacks/{owner}/{name}/v/{version}` | Version detail (calls `…/versions/{version}`) |
| `GET /healthz` | `200 ok` iff the web process is up — **does not call the registry** |
| `GET /static/app.css` | Embedded stylesheet |

Canonical stack paths mirror the API (`/stacks/owner/name`). The `@owner/name` form is the CLI
ref and is shown *on* the pages (in copy-paste commands), not used as a URL path.

## 5. Pages

**Home (`/`).** Title, one-line pitch, a search form (text `q`, optional `harness` and `tag`),
and a list of stacks. The list is populated by `GET /v1/search?q=` (empty query → the registry's
default result set). Each row: ref (`@owner/name`), summary, harness, tags, trust-tier badge,
latest version, and a link to the stack page. *Ordering note:* the list shows whatever order the
registry returns for an empty query; a dedicated "most recent / most followed" ordering would be
a small registry-side `sort` addition and is **out of 2d scope** (the website stays a pure
consumer). If empty-query search returns nothing useful, the home list simply renders empty with
a prompt to search — no website change needed.

**Search results (`/search`).** Re-renders the search form with the current values, then the
result rows (same row shape as home). Empty result → a clear "no stacks match" message. The
form submits via `GET` so results are linkable/bookmarkable.

**Stack detail (`/stacks/{owner}/{name}`).** Header: `@owner/name`, summary, harness, trust-tier
badge, tags, and `forked_from` provenance (linked to the parent stack page if present). A
**"Try this stack"** block with copy-paste CLI commands built from the API's `repo_url` (see
§7): e.g. `sherpa try @owner/name` and `sherpa clone @owner/name`. A **versions table**: version
number (linked to the version page), published-at, changelog, scan summary (`clean` / `N
findings`), trust tier. Registry `404 → the website's 404 page`.

**Version detail (`/stacks/{owner}/{name}/v/{version}`).** The version's `git_tag`, published-at,
trust tier, changelog, the **manifest snapshot** (rendered read-only, e.g. pretty-printed and
escaped), and the **scan report** (findings list or "clean"). The same "try it" commands, pinned
to this version where the CLI supports it. Registry `404 → 404 page`.

## 6. Configuration

Env vars (website service only), mirroring the registry's naming style:

- `SHERPA_WEB_ADDR` — listen address (default `:8080`; Railway sets `PORT`, honored if present).
- `SHERPA_REGISTRY_API_URL` — base URL of the registry API the website reads. In production this
  is Railway's **private-network** address of the registry service (e.g.
  `http://registry.railway.internal:PORT`) so web→registry traffic never leaves the project; it
  may be the public URL for local/dev. Required; startup fails fast if unset/invalid.
- `SHERPA_WEB_PUBLIC_BASE_URL` — the website's own public URL, used only to build absolute links
  in the website's own pages (canonical/OG). Optional; when unset, links are relative.
- `SHERPA_WEB_UPSTREAM_TIMEOUT` — per-registry-call timeout (default e.g. `5s`).

No `DATABASE_URL`, no tokens, no GitHub client secret. The website's environment carries no
secret; this is a deliberate property, not an omission.

## 7. Security / invariants

- **Stored-XSS prevention is the primary concern.** Every field the website renders originates
  from a publisher (summaries, changelogs, tags, `forked_from`, manifest JSON, scan findings) and
  is untrusted. All of it is rendered through `html/template`'s contextual auto-escaping;
  publisher-supplied data is **never** passed through `template.HTML`, `template.JS`,
  `template.URL`, or any unescaped construct. Manifest/scan JSON is displayed as escaped text
  (pretty-printed string), not injected as markup.
- **Clone/try URLs come from the API's `repo_url`, verbatim.** The registry already pins
  `repo_url` to its hardened `SHERPA_PUBLIC_BASE_URL` (2c-iii host-header hardening). The website
  displays that value; it does **not** reconstruct a git URL from its own request host or any
  forwarded header. No host-header reflection anywhere on the website.
- **Strict CSP, self-contained assets.** `Content-Security-Policy` with `default-src 'self'`
  (no external scripts/styles/fonts/images), plus standard hardening headers
  (`X-Content-Type-Options: nosniff`, a sensible `Referrer-Policy`, frame-deny). All CSS/JS are
  served from the website's own embedded files.
- **No open redirects, no reflected request data as markup.** Query params (`q`, `harness`,
  `tag`) are only ever echoed back through auto-escaped template output, never into a redirect
  target or raw HTML.
- **Path validation.** `{owner}`/`{name}`/`{version}` are validated/normalized before building
  the upstream API path (reuse the same segment rules the registry uses), so a crafted path can't
  make the website call an unexpected upstream URL.

## 8. Fault isolation / resilience (the core requirement)

- **`/healthz` is independent of the registry.** It returns `200` as long as the web process is
  alive. It does **not** probe the registry — otherwise a registry outage would fail the
  website's Railway health check and take the website down too, which is exactly what the
  separate-service design exists to prevent.
- **Bounded, cancellable upstream calls.** Every registry call uses the shared client's timeout
  and the inbound request's context. A hung registry produces a fast degraded response, never a
  pile-up of stuck web requests.
- **Graceful degradation, not a crash.** When a registry call fails (timeout, connection
  refused, 5xx → `ErrUpstream`), the handler renders a friendly **"the registry is temporarily
  unavailable"** page (HTTP `503`, so crawlers retry) with the site chrome intact. A registry
  `404` (`ErrNotFound`) renders the website's normal **404** page. The two are distinct and never
  conflated.
- **No shared state to corrupt.** The website is stateless; a registry restart, migration, or DR
  export has zero effect on in-flight web requests beyond a transient degraded page.
- **Graceful shutdown.** On `SIGTERM` the server stops accepting, drains in-flight requests
  within a bounded timeout, then exits (mirrors `cmd/registry`).

*(A short-TTL in-process cache of search/detail responses — to smooth registry blips and cut
load — is a reasonable future enhancement but is **cut from v1** as YAGNI; v1 is direct
pass-through with a bounded timeout.)*

## 9. Testing strategy

- **Registry client (`internal/web/client`):** against an `httptest.Server` returning canned
  JSON — `Search`/`GetStack`/`GetVersion` decode correctly; `404 → ErrNotFound`; `500` and a
  forced transport error / timeout → `ErrUpstream`; the request context cancels an in-flight
  call.
- **Handlers/rendering:** against a fake registry (httptest) — each page renders expected data;
  a stack `404` renders the 404 page; an upstream failure renders the degraded `503` page with
  `/healthz` still returning `200` in the same test process (**the fault-isolation proof**).
- **XSS escaping (the security surface):** feed a stack whose summary/changelog/tag/manifest
  contains `<script>…</script>` and `"><img onerror>` payloads through the fake registry; assert
  the rendered HTML contains the escaped forms and **never** the live markup. One test per
  untrusted field family.
- **`repo_url` fidelity:** the "try it" block renders exactly the API-provided `repo_url`; a
  test with an attacker-style `Host`/`X-Forwarded-Host` on the *website* request asserts the
  displayed clone URL is unchanged (website never reflects its own host into the command).
- **Config:** required `SHERPA_REGISTRY_API_URL` missing/invalid → startup error; defaults
  applied for optional vars.
- **Deploy smoke (non-CI/manual, documented):** `docker build` the web image, run it against a
  local registry, confirm `/healthz` `200`, a page renders, and stopping the registry flips pages
  to the degraded state while `/healthz` stays `200`.

## 10. Deployment (Railway, second service)

- **`cmd/web` gets its own Dockerfile** (multi-stage build → minimal non-root runtime image; no
  Postgres/git tooling needed — it's a pure HTTP renderer, so a distroless/`scratch`-class final
  image with just the binary + CA certs + embedded assets).
- **Its own `railway.json`**: `healthcheckPath: /healthz`, `numReplicas: 1`,
  `restartPolicyType: ON_FAILURE`, mirroring the registry's.
- **Separate Railway service** in the same project, so it can reach the registry over private
  networking (`SHERPA_REGISTRY_API_URL = http://<registry>.railway.internal:PORT`) while being
  deployed, scaled, restarted, and failing **independently**. A deploy or crash of one service
  does not restart or block the other.
- The registry's `SHERPA_PUBLIC_BASE_URL` remains the registry's own public host (for `repo_url`
  / git clone). The website gets its own public domain. Runbook additions documented alongside
  `docs/deployment/railway.md`.

## 11. Open questions (settle in the plan)

1. **Home-page list source.** Confirm the registry's empty-query `/v1/search` returns a usable
   default set and its ordering. If ordering is unsuitable for a "recent" feel, the plan notes a
   minor registry `sort` follow-up as explicitly deferred — the website ships consuming what
   exists.
2. **Version-pinned try command.** Whether `sherpa try @owner/name` can pin a specific version
   for the version-detail page, or only latest — confirm the CLI's current capability and render
   accordingly (fall back to latest-only if unsupported).
3. **Distroless vs. minimal Debian final image.** Both satisfy "non-root, no secrets, no DB
   tooling"; the plan picks one (leaning distroless/`scratch` since the website shells out to
   nothing).
