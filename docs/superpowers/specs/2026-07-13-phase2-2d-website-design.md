# SherpA Phase 2 · Sub-project 2d — Discovery Website

**Status:** Implemented; automated gates green, live Railway acceptance pending
**Date:** 2026-07-13
**Parent specs:** `2026-07-08-follow-the-expert-design.md` (§4.2 registry, website); builds on 2c-i
(registry API core), 2c-ii (auth + trust tier), and 2c-iii (Railway deployment).

## 1. Goal

Give SherpA a public, indexable website where anyone can discover expert stacks: search and
filter the catalog, inspect stack and immutable-version metadata, understand the executable
surface and registry scan status, and copy an exact CLI command to try or clone the stack.
The website is the discovery surface; the CLI remains the action surface. There is no account,
login, or write path on the website in 2d.

The overriding architectural constraint (user decision, 2026-07-13) is that the website is a
separate Railway service with no database or shared runtime state. A website failure must not
affect the registry or CLI. A registry failure leaves the website process and static chrome up,
but dynamic discovery pages return an explicit degraded response; without a cache, the website
does not claim to preserve catalog functionality during that outage.

## 2. Review amendments (2026-07-13)

The initial design was directionally correct. Review against the merged API, CLI, and 2c-iii
deployment found and settles these gaps:

1. **Detail pages did not actually receive `repo_url`.** It exists only on `/v1/search`, while
   the draft promised exact commands on stack and version pages. 2d therefore includes a small,
   backward-compatible registry read-API extension adding the already-pinned `repo_url` to stack
   and version detail responses.
2. **Both catalog and version history were unbounded.** Optional `limit`/`offset` pagination is
   added to the existing read endpoints. Omitting it preserves current CLI/API behavior; the
   website always requests a bounded page.
3. **Version-pinned try is not implemented by the CLI.** Version pages show the immutable tag
   being inspected, but their action is explicitly the current/latest stack. They do not invent
   `@owner/name@vN` syntax or imply that a historical version will be checked out.
4. **A successful publish has a clean scan report.** Rejected findings are not persisted.
   The website presents the authoritative passed/clean state. If legacy or future data contains
   findings, it may show count, kind, file, and line, but never the publisher-adjacent `excerpt`
   field, which can contain sensitive material.
5. **Upstream resilience needed resource bounds, not only a timeout.** Responses are size-limited,
   redirects are rejected, malformed JSON becomes `502`, and transport/timeout/5xx failures become
   `503`. Error causes and upstream bodies are never rendered or logged verbatim.
6. **Railway service layout was underspecified.** The existing root `Dockerfile` and
   `railway.json` belong to the registry. The website uses `deploy/web/Dockerfile` and
   `deploy/web/railway.json`, selected explicitly in the second service's settings. The listener
   binds on `[::]` for Railway private-network compatibility, and the registry's private port is
   an explicit service variable.
7. **The UI and indexing contract needed definition.** The design now pins responsive layout,
   accessibility, progressive copy controls, canonical/meta/robots behavior, and visual QA rather
   than leaving those decisions to implementation.

## 3. Scope

**In:**

- `cmd/web`, a server-rendered Go HTTP service using stdlib `net/http` and `html/template`.
- Home/catalog, search results, stack detail, and version detail pages.
- A typed, bounded registry API client and backward-compatible pagination/detail additions to the
  existing public read API only.
- Embedded templates, CSS, minimal progressive-enhancement JavaScript, a compact SherpA bitmap
  brand mark/favicon, security headers, SEO metadata, and `/robots.txt`.
- Independent `/healthz`, graceful shutdown, safe request logging, a non-root production image,
  Railway service config, smoke test, runbook addition, and a live website staging gate.

**Cut:**

- Accounts, web login, cookies carrying identity, follows, stars, trial feedback, and notifications
  (2e).
- In-browser git tree/README browsing, diffs, and rendered file contents. Those require a new
  content-read API and belong to a later sub-project.
- A server-side response cache. It remains a measured follow-up if upstream load or availability
  requires it; v1 is a bounded direct renderer.
- Trending, most-followed, expert profiles, and trust-tier filtering. The current data/API cannot
  support them honestly; the home page is a recent catalog, ordered by latest publish time.
- Any registry write/auth/storage change. The only registry work is additive public read-contract
  support for `repo_url` and optional pagination.

## 4. Decisions

- **Fault isolation through HTTP, not Postgres.** The website consumes only the registry's public
  read API. It has no DB pool, migration dependency, content volume, registry token, GitHub
  credential, or session store.
- **Server-rendered and useful without JavaScript.** Navigation, search, filtering, pagination,
  and all content work without JS. A small embedded script adds copy-to-clipboard behavior using
  `textContent`; it never injects HTML.
- **No web framework or SPA build chain.** The existing Go module, `net/http`, `html/template`,
  `embed`, and a small amount of hand-authored CSS/JS are sufficient. This keeps the runtime and
  dependency surface aligned with the repository.
- **Additive API evolution is part of 2d.** Website needs are expressed through the same public
  contract available to other clients, not via a shared DB or website-only back channel.
- **One initial web replica is a cost setting, not an invariant.** The service is stateless and can
  later scale horizontally without code or storage changes.

## 5. Architecture and API contract

```
 browser ──HTTP──▶ cmd/web (Railway service #2) ──private HTTP──▶ registry (service #1)
                    │ handlers + html/template                     /v1/search
                    │ embedded CSS/JS/bitmap                       /v1/stacks/{o}/{n}
                    │ bounded typed API client                     /versions/{v}
                    └ /healthz (process-local; no upstream call)
```

- **`cmd/web`** loads validated configuration, constructs the client/router, starts a hardened
  `http.Server`, and performs bounded SIGINT/SIGTERM shutdown.
- **`internal/web/registryclient`** owns public registry response types and `Search`, `GetStack`,
  and `GetVersion`. Requests use the inbound context, a dedicated bounded transport/client,
  `url.Values`, and validated escaped path segments.
- **`internal/web`** owns page models, parsing, routing, middleware, safe template rendering, and
  embedded assets. Templates are parsed at startup and executed into a buffer before response
  headers are committed.
- **`internal/web/templates` and `internal/web/static`** hold embedded templates/assets. No asset
  is loaded from an external CDN or third-party origin.

### 5.1 Additive registry API extension

Existing clients remain compatible.

- `GET /v1/search?q=&harness=&tag=&limit=&offset=`: `limit` is optional, `1..50`; `offset` is
  non-negative and only valid with `limit`. With `limit`, the response may include
  `next_offset`; without it, current unbounded behavior and response shape are preserved. The
  stable order remains latest `published_at` descending, then owner/name as deterministic ties.
- `GET /v1/stacks/{owner}/{name}?versions_limit=&versions_offset=`: equivalent optional paging
  for the descending version list, with optional `next_versions_offset`.
- Stack and version detail responses gain `repo_url`, generated exclusively by the registry's
  pinned `SHERPA_PUBLIC_BASE_URL` path. No existing field changes meaning.
- Invalid paging values return JSON `400`. The website maps this to `502` because it indicates a
  client/contract bug, not a user-visible not-found condition.

The website requests 24 search rows and 25 versions per page. It never fetches the old unbounded
forms. The client also caps every decoded upstream body at 1 MiB; an oversized manifest/detail is
treated as a bad upstream response rather than allowed to exhaust web-service memory.

### 5.2 Website routes

| Route | Behavior |
|---|---|
| `GET /` | Recent catalog, first 24 results from an empty search |
| `GET /search?q=&harness=&tag=&page=` | Linkable filtered results; `page` is 1-based |
| `GET /stacks/{owner}/{name}?versions_page=` | Stack metadata and bounded version history |
| `GET /stacks/{owner}/{name}/v/{version}` | Immutable version metadata and scan state |
| `GET /healthz` | Process readiness only; never calls the registry |
| `GET /robots.txt` | Static crawler policy |
| `GET /static/app.css` | Embedded stylesheet |
| `GET /static/app.js` | Embedded progressive-enhancement script |
| `GET /static/sherpa-mark.png` | Embedded brand asset |
| `GET /static/favicon.png` | Embedded favicon |

Non-GET methods are rejected. Unknown routes render the site 404. `{owner}` and `{name}` match
the registry's publish-segment character rules before being escaped into an upstream URL;
`version` must be a positive integer. Invalid website paths are local `404`, not upstream calls.
Query text is trimmed and bounded (`q` 200 bytes; `harness` and `tag` 64 bytes); invalid page
numbers are local `400` with no upstream call.

## 6. Page and interaction contract

### 6.1 Shared shell and visual direction

- A compact first-viewport SherpA header/wordmark, search access, and content area. The home page
  opens on the usable catalog, not a marketing landing page or oversized hero.
- Quiet, work-focused visual language: near-white canvas, charcoal text, green trust/success,
  blue interactive accent, and amber warning/degraded state. It must not collapse into a one-hue
  palette, gradient decoration, or card-within-card composition.
- Results and versions are dense, full-width rows separated by rules; only the command block and
  repeated compact stack items may use a bordered surface. Radius is at most 8px.
- A generated transparent bitmap mark is used at header/favicon sizes and verified for legibility;
  it is not a decorative hero illustration.
- Responsive layouts cover 360px mobile through wide desktop. Stable grid tracks, wrapping tags,
  and constrained command blocks prevent text or controls from overlapping or resizing the page.
  Font sizes do not scale with viewport width; letter spacing is zero.

### 6.2 Home and search

The home page has an `h1`, compact product statement, search input, harness selector (`Any`,
`Claude Code`, `Codex`), tag input, and the recent stack list. Empty-query search is already
ordered by latest published version descending in Postgres, so the section is accurately called
"Recently published", not "trending".

Search uses GET parameters and retains entered filters. Each result shows `@owner/name`, summary,
harness, tags, latest version, latest-version trust tier, and fork provenance when valid. Empty
results are a normal `200` state. Previous/next links preserve all filters and use `rel` where
appropriate.

### 6.3 Stack detail

The header shows `@owner/name`, summary, harness, tags, and the latest version's trust tier (the
API version list is descending). A valid `forked_from` ref is linked only after strict parsing;
arbitrary provenance text is displayed as text, never interpreted as a URL.

The command surface uses the API-provided `repo_url` verbatim after client validation that it is
an absolute HTTP(S) URL. The exact commands are:

```sh
sherpa try <repo_url>
sherpa clone <repo_url>
```

This avoids depending on a user's `SHERPA_REGISTRY_URL` environment and never reconstructs a URL
from the website request host. A progressive copy button accompanies each command; selecting the
text remains the no-JS fallback.

Version history shows version, published time, changelog, scan summary, and trust tier, newest
first, with bounded pagination. Times use semantic `<time datetime="...">` markup and UTC input.

### 6.4 Version detail

The page identifies `@owner/name`, the numeric version, immutable `git_tag`, published time,
version trust tier, and changelog. It renders the manifest as structured definition rows for known
fields (`name`, `owner`, `version`, `harness`, `summary`, tags, declared hooks/MCP servers, and
parameters), with an escaped pretty-JSON fallback for unknown fields. No publisher value becomes
HTML, CSS, JavaScript, or a URL.

The scan panel says the registry scan passed when findings are empty. If non-empty historical or
future data appears, show count plus sanitized kind/file/line only. **Never render `excerpt`.**

Because the current CLI cannot select a historical registry tag, this page labels the command
surface "Try latest" and uses the unmodified API `repo_url`. The inspected `git_tag` is never
appended to the command or URL.

### 6.5 Accessibility and indexing

- Semantic landmarks/headings, explicit form labels, keyboard-visible focus, logical tab order,
  descriptive link text, accessible copy-button names/status, and WCAG AA color contrast.
- The bitmap mark has useful alt text only where it conveys brand; redundant instances use empty
  alt text. Status is never communicated by color alone.
- Stack/version `200` pages get pinned canonical URLs from `SHERPA_WEB_PUBLIC_BASE_URL`, unique
  title/description, Open Graph text metadata, and `index,follow`.
- Search pages are `noindex,follow`; empty, `400`, `404`, `502`, and `503` pages are `noindex`.
  Canonical/meta URLs are omitted locally when no public base is configured. Request `Host` and
  forwarded-host headers are never used to build them.

## 7. Configuration

- `SHERPA_WEB_ADDR`: explicit listen address. If absent, use `[::]:$PORT`; if `PORT` is absent,
  use `[::]:8080`.
- `SHERPA_REGISTRY_API_URL`: required fixed upstream base. Accept only absolute HTTP(S) URLs with
  scheme and host, optional normalized path prefix, and no userinfo/query/fragment. Production
  uses Railway private HTTP.
- `SHERPA_WEB_PUBLIC_BASE_URL`: optional locally, required by the deployment runbook. Accept only
  an absolute HTTP(S) origin/path prefix with no userinfo/query/fragment. It is the sole source for
  canonical and Open Graph page URLs.
- `SHERPA_WEB_UPSTREAM_TIMEOUT`: default `5s`, bounded to `100ms..30s`.

No `DATABASE_URL`, auth token, GitHub credential, cookie secret, or private key is accepted by the
web process. Railway reference variables and internal DNS names are configuration, not secrets.

## 8. Security invariants

- **Stored/reflected XSS:** every registry field and query value is untrusted and rendered only
  through `html/template` contextual escaping. There is no `template.HTML`, `template.JS`, or
  `template.URL` conversion of dynamic values. JS reads command text and uses the Clipboard API;
  it never uses `innerHTML`.
- **Pinned URLs:** clone commands use validated API `repo_url`; canonical/meta URLs use only the
  validated public-base config. `Host`, `Forwarded`, and `X-Forwarded-Host` never affect output.
- **Fixed upstream:** API paths are built from validated segments and `url.URL`/`url.Values`.
  The HTTP client rejects every redirect, caps response bodies, checks JSON content/status, and
  never forwards browser headers or cookies upstream.
- **Strict headers:** at minimum:
  `Content-Security-Policy: default-src 'none'; style-src 'self'; script-src 'self'; img-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; object-src 'none'`,
  `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`,
  `X-Frame-Options: DENY`, and
  `Permissions-Policy: camera=(), microphone=(), geolocation=(), payment=(), usb=()`.
- **No sensitive scan excerpts:** raw scan JSON is decoded into a type that omits `excerpt` from
  every page model. Pretty-printing the whole scan report is prohibited.
- **Safe errors/logging:** public responses never contain upstream URLs, response bodies, or Go
  error causes. Access logs contain method, route path without query, status, duration, and a
  sanitized Railway request ID; search text, headers, cookies, and upstream bodies are not logged.

## 9. Resilience and HTTP behavior

- The dedicated upstream client has bounded dialing, TLS handshake, response-header, whole-call,
  and idle-connection behavior. Request cancellation propagates to the registry.
- Registry `404` maps to website `404`. A malformed/oversized response or unexpected upstream
  `4xx` maps to `502 Bad Gateway`. Transport errors, timeouts, and registry `5xx` map to
  `503 Service Unavailable` with `Retry-After: 60`. Error pages retain local site chrome and do
  not retry in-process.
- `/healthz` becomes `200` only after config and templates/assets are ready, then remains
  independent of registry health. Railway uses it as a deployment readiness gate; continuous
  monitoring is configured separately because Railway does not continuously poll service
  healthchecks after cutover.
- Server defaults: `ReadHeaderTimeout 5s`, `ReadTimeout 10s`, `WriteTimeout 15s`,
  `IdleTimeout 60s`, `MaxHeaderBytes 1 MiB`, and `10s` graceful shutdown. The write timeout is
  greater than the maximum upstream call plus render time.
- Error responses and dynamic HTML use `Cache-Control: no-store` in 2d. Static assets may use a
  bounded public cache with validators. There is no stale data or in-process retry loop to mask
  the direct dependency.

## 10. Testing strategy

- **Registry API extension:** pagination parsing/bounds, stable ties, next offsets, omitted-param
  compatibility, and pinned `repo_url` on GET detail and POST publish responses. Existing publish
  fail-closed/auth/50 MiB tests remain unchanged and green.
- **Registry client:** exact query/path construction, typed decode, 404/4xx/5xx mapping,
  cancellation/timeout, redirect rejection, content/body limit, invalid JSON/time/URL, and no
  browser-header forwarding using `httptest.Server`.
- **Handlers/templates:** every page and state; pagination filter retention; invalid local input
  causes zero upstream calls; template execution failure cannot leak a partial `200`; health stays
  `200` while dynamic pages return `503`.
- **Adversarial rendering:** payloads in summary, changelog, tags, provenance, manifest, scan
  fields, query values, and `repo_url`; assert no live script/event-handler/unsafe URL appears.
  Assert scan excerpts never appear and spoofed Host/forwarded-host values cannot affect commands
  or canonical tags.
- **Headers/SEO/a11y:** exact CSP and hardening headers on HTML/errors/assets as applicable;
  canonical/noindex rules; semantic labels/headings and copy controls.
- **Config/server:** precedence and validation for address/PORT/URLs/duration; timeout constants;
  signal-driven graceful shutdown.
- **Visual QA:** desktop and 360px mobile browser screenshots for home, results, stack, version,
  empty, 404, and degraded states; inspect for overflow, overlap, focus visibility, long unbroken
  publisher values, and the brand mark at rendered size.
- **Container smoke:** build and run the production image against a fake registry, assert
  `/healthz`, a rendered page, non-root PID 1, no shell/Go source/toolchain, and degraded behavior
  after the fake registry stops.

## 11. Deployment and live gate

The repository remains a shared-root Go module. The registry keeps root `Dockerfile` and
`railway.json`; the website adds:

- `deploy/web/Dockerfile`: Go build stage, then a digest-pinned distroless static Debian non-root
  runtime containing only CA certificates and `/web`.
- `deploy/web/railway.json`: explicit Dockerfile path, `/healthz`, one initial replica,
  `ON_FAILURE`, and bounded healthcheck timeout.
- `deploy/web/smoke_build.sh`: local production-image validation.

In Railway, create a second service from the same repository and branch. Set its config-file path
to absolute `/deploy/web/railway.json`; do not change the shared root directory because the build
needs root `go.mod` and `internal/**`. Set a fixed registry service `PORT` variable (for example
`8080`) and configure the website upstream using reference variables:

```text
SHERPA_REGISTRY_API_URL=http://${{registry.RAILWAY_PRIVATE_DOMAIN}}:${{registry.PORT}}
```

Private service traffic uses HTTP. Both services listen on `[::]`. The website receives its own
public domain and pinned `SHERPA_WEB_PUBLIC_BASE_URL`. The registry retains its independent public
domain and `SHERPA_PUBLIC_BASE_URL` for git clone URLs.

### 11.1 Website staging acceptance gate

Run after the registry's 2c-iii staging gate passes:

1. Deploy the website service from `deploy/web/railway.json`; confirm the image/config source is
   the web path, not the root registry config.
2. Confirm `/healthz` is `200` and PID 1 is non-root; confirm the service environment has no DB,
   OAuth, registry-session, admin, or export secret.
3. Load home/search/stack/version through the public domain and compare displayed metadata and
   `repo_url` commands with direct registry API responses.
4. Run both copied commands on a clean CLI home and confirm clone/try reaches the registry host,
   not the website or any request-supplied host.
5. Send spoofed `Host`, `X-Forwarded-Host`, and `Forwarded` headers; canonical URLs and commands
   remain pinned. Check CSP/security headers at the real edge.
6. Exercise next/previous search and version pages; filters persist, duplicates/gaps do not appear
   for a stable dataset, and absurd paging/query inputs are bounded.
7. Stop or block the registry: dynamic pages return branded `503` with `Retry-After`, `/healthz`
   stays `200`, and the website process/restart count remains stable. Restore registry and confirm
   the next request recovers without redeploying web.
8. Redeploy only the website and confirm registry API, git clone, login, and publish continue
   uninterrupted. Redeploy only the registry and confirm web degrades/recovers as above.
9. Inspect desktop/mobile screenshots and keyboard navigation for overflow, overlap, readable
   focus, form labels, command copying, and long malicious-looking publisher text.
10. Inspect Railway logs: no query text, command content, upstream body, scan excerpt, headers,
    credentials, or full internal/public URL with query is present.
11. Configure external continuous uptime checks for both the public home page and `/healthz`;
    Railway's deploy healthcheck alone is not continuous monitoring.

## 12. Settled questions and sources

- Empty search is usable and already ordered by latest publish time (`store.PostgresStore.Search`).
- Version-pinned CLI try/clone is unsupported; 2d never claims otherwise.
- Runtime choice is digest-pinned distroless static Debian as non-root; the web binary shells out
  to nothing.
- Railway facts re-verified 2026-07-13: [private networking and explicit port/listener guidance](https://docs.railway.com/private-networking),
  [shared-monorepo config path behavior](https://docs.railway.com/deployments/monorepo),
  [custom Dockerfile paths](https://docs.railway.com/builds/dockerfiles), and
  [deploy-only healthcheck behavior](https://docs.railway.com/deployments/healthchecks).
