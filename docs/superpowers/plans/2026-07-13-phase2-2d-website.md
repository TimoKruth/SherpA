# Phase 2 · 2d — Discovery Website Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: use `superpowers:subagent-driven-development`
> (recommended) or `superpowers:executing-plans` task-by-task. Keep the checkbox state current.

**Status:** Implemented; automated and browser QA gates green. The live Railway website
acceptance gate remains operator-owned.

**Implementation record (2026-07-13):** Tasks 1-9 were implemented on `main`. The generated
bitmap mark was inspected at 96px and 32px; focused race tests, the uncached whole suite, vet,
command builds, JSON/shell validation, and the real Docker fault-isolation smoke passed. Browser
QA completed at 1280px desktop and 375px mobile with keyboard focus, copy controls, responsive
stacking, horizontal overflow, and strict CSP checked. The accepted Alpine styling follow-up adds
surface hierarchy and trust/harness badges without weakening CSP or publisher-data escaping.

**Goal:** Ship an independently deployed, server-rendered SherpA discovery website that consumes
the public registry read API, remains stateless and secret-free, renders publisher data safely,
and gives users exact current-stack clone/try commands.

**Architecture:** Add backward-compatible pagination and pinned detail `repo_url` fields to the
registry read contract. Build a bounded typed registry client, then a stdlib Go renderer with
embedded templates/assets, strict security headers, safe degradation, and a hardened server.
Deploy it from service-specific files under `deploy/web` so the existing registry image/config
remain untouched.

**Tech stack:** Go 1.26 stdlib (`net/http`, `html/template`, `embed`, `encoding/json`), existing
Postgres 16 test harness for the small registry query change, Docker, Railway. No web framework,
SPA toolchain, CSS framework, JS package manager, DB connection, or web-service secret.

## Global constraints

- Preserve the registry publish fail-closed gate, own-login/admin authorization, immutable
  versions, host-pinned clone issuer, and 50 MiB publish cap. Task 1 is read-contract-only.
- Website has no DB/content-store dependency and no auth/session/cookie write path.
- All dynamic data is untrusted. Never convert it to `template.HTML`, `template.JS`, or
  `template.URL`; never render scan `excerpt`.
- Browser request headers/cookies are never forwarded upstream. Host/forwarded-host never build
  commands, links, or metadata.
- Upstream calls are timeout-, redirect-, and body-bounded; errors are status-classified and
  secret-safe.
- Templates/assets are embedded, parsed before readiness, and require no external origins.
- Gofmt, `go test ./...`, `go vet ./...`, `go build ./cmd/...`, `git diff --check`, and Docker
  smoke stay green. Use offline Go settings when needed:
  `GOFLAGS=-mod=mod GOPROXY=off GOCACHE=/private/tmp/sherpa-go-build`.
- Prefer GPT-5.6 Terra for implementation workers when that model is available. Use a separate
  adversarial/Fable-class review for Tasks 1, 2, and 5 (API bounds, upstream boundary, and XSS/
  URL rendering). The controller owns final verification and commits.
- Do not modify or operate the user's live 2c-iii Railway staging environment while this plan is
  implemented. The 2d live gate is a later operator step.

---

### Task 1: Add bounded, backward-compatible registry read pagination and detail `repo_url`

**Files:**
- Modify: `internal/registry/store/store.go`
- Modify: `internal/registry/store/postgres.go`
- Modify: `internal/registry/store/postgres_test.go`
- Modify: `internal/registry/api/read.go`
- Modify: `internal/registry/api/read_test.go`
- Modify: `internal/registry/api/publish.go`
- Modify affected Store fakes in registry tests

**Interfaces:**

```go
// maxRows == 0 preserves the existing unbounded behavior.
Search(ctx context.Context, q, harness, tag string, maxRows, offset int) ([]StackWithLatest, error)
GetStack(ctx context.Context, owner, name string, maxVersions, offset int) (Stack, []Version, error)
```

API response additions:

```go
type searchResponse struct {
    Stacks     []searchStackResponse `json:"stacks"`
    NextOffset *int                  `json:"next_offset,omitempty"`
}
type stackResponse struct {
    // existing fields
    RepoURL            string `json:"repo_url"`
    NextVersionsOffset *int   `json:"next_versions_offset,omitempty"`
}
type versionResponse struct {
    // existing fields
    RepoURL string `json:"repo_url"`
}
```

- [x] **Step 1: Write failing API tests.** Cover search `limit=2&offset=1`, stack
  `versions_limit=2&versions_offset=1`, correct next offsets by requesting `limit+1`, max limit
  50, zero/negative/non-integer/offset-without-limit `400`, and omitted parameters retaining the
  exact existing response behavior. Assert spoofed Host/forwarded-host cannot alter `repo_url`
  on stack GET, version GET, or publish POST when `PublicBaseURL` is pinned.
- [x] **Step 2: Write failing Postgres tests.** Seed tied publish timestamps across owners/names;
  prove stable order is `published_at DESC, owner, name`. Prove bounded search and version slices,
  offsets, `maxRows=0` compatibility, and descending version order.
- [x] **Step 3: Implement store bounds.** Use parameterized `LIMIT NULLIF($n, 0) OFFSET $n`; never
  interpolate paging values. The API asks for `limit+1`, trims the sentinel row, and emits the
  next offset only when more data exists. Add owner to search tie-breaking.
- [x] **Step 4: Implement response additions.** Reuse `server.repoURL`; do not add any Host-based
  fallback beyond the already-tested local behavior. Ensure POST and both GET detail responses
  populate the field.
- [x] **Step 5: Regression verification.** Run focused API/store tests plus existing adversarial
  publish/auth tests and assert `maxPublishBundleBytes` remains `50 << 20`.
- [x] **Step 6: Commit.**
  `git commit -m "feat(registry): page discovery reads and expose pinned detail URLs"`

---

### Task 2: Build the bounded typed registry client

**Files:**
- Create: `internal/web/registryclient/types.go`
- Create: `internal/web/registryclient/client.go`
- Create: `internal/web/registryclient/client_test.go`

**Interfaces:**

```go
var (
    ErrNotFound   = errors.New("registry resource not found")
    ErrBadGateway = errors.New("invalid registry response")
    ErrUnavailable = errors.New("registry unavailable")
)

type Client struct { /* validated base URL, dedicated *http.Client, max response bytes */ }
func New(rawBaseURL string, timeout time.Duration) (*Client, error)
func (c *Client) Search(ctx context.Context, q SearchQuery) (SearchResult, error)
func (c *Client) GetStack(ctx context.Context, owner, name string, page Page) (Stack, error)
func (c *Client) GetVersion(ctx context.Context, owner, name string, version int) (Version, error)
```

`SearchQuery` carries `Q`, `Harness`, `Tag`, and `Page{Limit,Offset}`. Response structs mirror the
wire JSON but use typed RFC3339 times and an absolute validated HTTP(S) `RepoURL`. `ScanReport`
may decode findings including `Excerpt` inside the client type, but no page-facing type may expose
that field.

- [x] **Step 1: Write URL/request tests.** Validate absolute HTTP(S) base URL with optional path
  prefix; reject userinfo/query/fragment/missing host/other schemes. Assert `url.Values` encoding,
  path-prefix preservation, escaped validated segments, no request-header/cookie forwarding, and
  no request for invalid owner/name/version/page input.
- [x] **Step 2: Write response-boundary tests with `httptest.Server`.** Cover valid fixtures,
  typed time decode, `404 → ErrNotFound`, unexpected `4xx`/malformed JSON/wrong content type/
  invalid repo URL/body over 1 MiB `→ ErrBadGateway`, and transport/timeout/`5xx`
  `→ ErrUnavailable`. Test an in-flight context cancellation.
- [x] **Step 3: Prove redirects are rejected.** Same-origin and cross-origin 3xx must both stop;
  the redirected server receives zero calls. Public error strings must not contain the base URL,
  response body, query, or planted credential-looking values.
- [x] **Step 4: Implement.** Use a dedicated `http.Transport` with bounded dial, TLS handshake,
  response-header, idle connection, and a client-wide timeout. Use `CheckRedirect` to reject all
  redirects. Read with `io.LimitReader(max+1)`, require JSON, close bodies, and never retry.
- [x] **Step 5: Verify and commit.** Run `go test -race ./internal/web/registryclient` and whole
  suite.
  `git commit -m "feat(web): add bounded registry API client"`

---

### Task 3: Establish the web router, rendering boundary, errors, and security middleware

**Files:**
- Create: `internal/web/web.go`
- Create: `internal/web/routes.go`
- Create: `internal/web/render.go`
- Create: `internal/web/middleware.go`
- Create: `internal/web/errors.go`
- Create: `internal/web/web_test.go`
- Create: `internal/web/templates/base.html`
- Create: `internal/web/templates/error.html`

**Interfaces:**

```go
type Registry interface {
    Search(context.Context, registryclient.SearchQuery) (registryclient.SearchResult, error)
    GetStack(context.Context, string, string, registryclient.Page) (registryclient.Stack, error)
    GetVersion(context.Context, string, string, int) (registryclient.Version, error)
}

type Options struct {
    PublicBaseURL *url.URL
    Logger        *log.Logger
}
func New(reg Registry, options Options) (http.Handler, error)
```

- [x] **Step 1: Write failing route/input tests.** Exact GET routes, unknown-route 404, non-GET
  rejection, owner/name character validation, positive version, query byte limits, and page
  parsing. Invalid input must produce zero fake-registry calls.
- [x] **Step 2: Write rendering/error tests.** A template failure is buffered and becomes a
  complete error rather than a partial `200`. Map not found to 404, bad upstream to 502, and
  unavailable to 503 with `Retry-After: 60`; every non-success page is `noindex` and `no-store`.
- [x] **Step 3: Write middleware tests.** Assert the exact CSP from spec §8, nosniff,
  no-referrer, frame deny, permissions policy, and safe content types. Request logs contain method,
  path without query, status, duration, and a newline-safe bounded Railway request ID, while
  planted query/header/cookie/upstream text is absent.
- [x] **Step 4: Implement the foundation.** Parse embedded templates once in `New`; execute page
  templates into `bytes.Buffer`; commit status/headers only after success. Keep error copy static
  and never render Go causes.
- [x] **Step 5: Verify and commit.**
  `git commit -m "feat(web): add secure server-rendered HTTP foundation"`

---

### Task 4: Implement home/search discovery and pagination

**Files:**
- Create: `internal/web/search.go`
- Create: `internal/web/search_test.go`
- Create: `internal/web/templates/home.html`
- Create: `internal/web/templates/search.html`
- Create: `internal/web/templates/partials/search_form.html`
- Create: `internal/web/templates/partials/stack_rows.html`

**Behavior:** Home calls search with `Limit:24, Offset:0` and labels the result "Recently
published". `/search` translates 1-based `page` into a checked offset, retains `q/harness/tag`,
and emits previous/next URLs through `url.URL`/`url.Values`, never template string assembly.

- [x] **Step 1: Write failing model/handler tests.** Cover home request shape, filter forwarding,
  page arithmetic overflow rejection, empty results, prior/next availability, filter retention,
  harness options, result metadata, and upstream 502/503 states.
- [x] **Step 2: Write adversarial escaping tests.** Put script/event-handler/long-unbroken values
  in ref, summary, tags, harness, and provenance plus query inputs; ensure no live markup and no
  unsafe href. Only strictly parsed `@owner/name` or `@owner/name@vN` provenance becomes an
  internal stack link.
- [x] **Step 3: Implement page models and templates.** Use one shared result-row partial. Preserve
  semantic `main`, `form`, labels, `h1`, and navigation; no-JS behavior is complete.
- [x] **Step 4: Verify and commit.**
  `git commit -m "feat(web): render recent and searchable stack catalog"`

---

### Task 5: Implement stack/version pages and the safe command/scan boundary

**Files:**
- Create: `internal/web/detail.go`
- Create: `internal/web/detail_test.go`
- Create: `internal/web/manifest.go`
- Create: `internal/web/manifest_test.go`
- Create: `internal/web/templates/stack.html`
- Create: `internal/web/templates/version.html`
- Create: `internal/web/templates/partials/commands.html`
- Create: `internal/web/templates/partials/versions.html`

**Page-facing safety types:**

```go
type ScanFindingView struct {
    File string
    Line int
    Kind string
    // Deliberately no Excerpt.
}
type CommandView struct { Try, Clone string }
```

- [x] **Step 1: Write stack tests.** Render summary/harness/tags/latest trust, strictly parsed fork
  provenance, descending versions, semantic UTC times, changelog/scan summaries, and
  `versions_page` navigation. Empty versions must not panic even though current storage normally
  prevents that state.
- [x] **Step 2: Write exact command tests.** Assert `sherpa try <repo_url>` and
  `sherpa clone <repo_url>` use the API value exactly after HTTP(S) validation. Spoofed
  Host/Forwarded/X-Forwarded-Host and a pinned website public base cannot alter them. Dangerous or
  relative repo URLs become 502, never a command or href.
- [x] **Step 3: Write version/manifest tests.** Cover known manifest fields, hooks, MCP servers,
  parameters, escaped unknown-field pretty JSON, empty scan, non-empty legacy scan, and malformed
  manifest/scan. Plant secret-like text in `excerpt` and assert it is absent from HTML and logs.
  Assert the version page says/labels latest and never synthesizes `@vN`, `?ref=`, or a tag suffix.
- [x] **Step 4: Implement safe view conversion.** Decode raw JSON before rendering; map only
  allowlisted scan fields; never pass raw scan JSON to a template. Commands are plain strings in
  `<code>` text. Manifest fallback remains auto-escaped text inside `<pre>`.
- [x] **Step 5: Run adversarial review and regression tests.** Specifically review URL contexts,
  HTML attributes, copy-button target lookup, provenance parsing, scan redaction, and template helper
  return types. Run `go test -race ./internal/web ./internal/web/registryclient`.
- [x] **Step 6: Commit.**
  `git commit -m "feat(web): add safe stack and version detail pages"`

---

### Task 6: Complete visual system, progressive copy, SEO, and accessibility

**Files:**
- Create: `internal/web/static/app.css`
- Create: `internal/web/static/app.js`
- Create: `internal/web/static/sherpa-mark.png`
- Create: `internal/web/static/favicon.png`
- Create: `internal/web/templates/partials/head.html`
- Modify: all page templates and relevant handler tests

**Visual implementation:** Use restrained near-white/charcoal foundations with distinct blue
interaction, green trust/success, amber warning, and neutral borders. Results/versions are
unframed rows; command surfaces and repeated compact stack entries may be bordered with radius
`<=8px`. No gradients, decorative blobs, nested cards, viewport-scaled type, or negative letter
spacing.

- [x] **Step 1: Generate the brand asset.** Use the image-generation skill for a simple transparent
  bitmap SherpA mark, then derive/resize the favicon without changing its design. Inspect at native
  resolution and at 32/48px; reject low-contrast, overly detailed, text-bearing, or mascot-style
  output. Keep only the selected production assets.
- [x] **Step 2: Implement responsive CSS.** Stable desktop/mobile grids, wrapping tags, bounded
  command blocks, focus-visible states, AA contrast, reduced-motion respect, and 360px support.
  Long owner/name/summary/changelog/manifest values must wrap without horizontal page overflow.
- [x] **Step 3: Implement progressive copy.** Buttons begin hidden and are enabled by JS; use a
  static inline Lucide Copy/Check icon with an accessible label, read adjacent command
  `textContent`, call Clipboard API, and announce success/failure through `aria-live`. Do not use
  `innerHTML`, inline handlers, inline script, or external libraries.
- [x] **Step 4: Implement metadata/crawler rules.** Pinned canonical and OG text tags for stack/
  version success pages, `noindex,follow` search, `noindex` errors, static `/robots.txt`, unique
  titles/descriptions, and no request-host fallback.
- [x] **Step 5: Run browser visual QA.** Capture home, populated/empty search, stack, version, 404,
  and 503 at 1440×900 and 360×800. Exercise keyboard navigation and copy. Inspect screenshots for
  overflow, overlap, unstable controls, clipped longest words, and incorrect brand/image render.
  Record only actionable fixes; do not add a browser framework dependency to the repository.
- [x] **Step 6: Commit.**
  `git commit -m "feat(web): finish responsive discovery UI and metadata"`

---

### Task 7: Add validated config, hardened server lifecycle, and `cmd/web`

**Files:**
- Create: `internal/web/config.go`
- Create: `internal/web/config_test.go`
- Create: `cmd/web/main.go`
- Create: `cmd/web/main_test.go`

**Config:**

```go
type Config struct {
    Addr            string
    RegistryAPIURL  string
    PublicBaseURL   string
    UpstreamTimeout time.Duration
}
```

- [x] **Step 1: Write config tests.** `SHERPA_WEB_ADDR` wins; otherwise `PORT → [::]:PORT`, then
  `[::]:8080`. Validate `PORT`, required registry URL, optional public URL, URL path-prefix
  normalization, disallowed userinfo/query/fragment/scheme, and timeout default/range
  `5s`/`100ms..30s`. Ensure errors do not echo credential-bearing raw URLs.
- [x] **Step 2: Write lifecycle tests.** `/healthz` is unavailable before handler/templates are
  ready and `200` afterward; registry outage never changes health. Assert server values:
  `ReadHeaderTimeout=5s`, `ReadTimeout=10s`, `WriteTimeout=15s`, `IdleTimeout=60s`,
  `MaxHeaderBytes=1<<20`; signal cancellation drains within `10s` and treats
  `http.ErrServerClosed` as success.
- [x] **Step 3: Implement `run`/`serve`.** Mirror the proven `cmd/registry` lifecycle without DB,
  stage cleanup, GitHub, or export scheduler. Construct config → client → templates/router → ready
  server, then listen/shutdown. Log configuration failures without environment dumps.
- [x] **Step 4: Verify.** Run race tests for web packages, `go test ./...`, `go vet ./...`, and
  `go build ./cmd/web ./cmd/registry ./cmd/sherpa`.
- [x] **Step 5: Commit.**
  `git commit -m "feat(web): add production server configuration and lifecycle"`

---

### Task 8: Add the non-root web image, Railway service config, and real smoke test

**Files:**
- Create: `deploy/web/Dockerfile`
- Create: `deploy/web/railway.json`
- Create: `deploy/web/smoke_build.sh`
- Create if needed: `deploy/web/testfixture/main.go`
- Modify: `.dockerignore` only if required without weakening registry build inputs

**Image:** Multi-stage `golang:1.26-bookworm` build with `CGO_ENABLED=0`, trimpath, stripped
`/web`; runtime is the multi-arch digest
`gcr.io/distroless/static-debian12:nonroot@sha256:b7bb25d9f7c31d2bdd1982feb4dafcaf137703c7075dbe2febb41c24212b946f`
(resolved 2026-07-13). Set `USER nonroot:nonroot`, `ENTRYPOINT ["/web"]`; add no shell/package
manager/entrypoint script.

- [x] **Step 1: Write deployment files.** `deploy/web/railway.json` points to
  `deploy/web/Dockerfile`, uses `/healthz`, 120-second deploy timeout, one replica,
  `ON_FAILURE`, and three max retries. Root registry Docker/config files remain byte-for-byte
  unchanged.
- [x] **Step 2: Implement smoke fixture/script.** Build from repository-root context, run a bounded
  fake registry with representative search/detail JSON, run web on an isolated Docker network,
  and assert home/stack render, exact command text, security headers, `/healthz`, and PID 1 UID
  equals the image's configured non-root UID.
- [x] **Step 3: Prove fault isolation.** Stop the fake registry; dynamic page becomes 503 with
  `Retry-After`, web `/healthz` remains 200, and the web container does not restart. Restart the
  fixture and confirm recovery without restarting web.
- [x] **Step 4: Inspect the final image.** No shell, Go command/toolchain, repository source,
  registry binary, DB/Git tooling, or secret-like build metadata. Validate JSON and shell syntax.
- [x] **Step 5: Run `bash deploy/web/smoke_build.sh` for real and commit.**
  `git commit -m "feat(web): add non-root Railway production image"`

---

### Task 9: Extend the Railway runbook and close automated verification

**Files:**
- Modify: `docs/deployment/railway.md`
- Modify: this plan only to record implementation status/corrections discovered during execution

- [x] **Step 1: Document shared-repo service settings.** Second service from same repo/branch;
  config file absolute `/deploy/web/railway.json`; no service root override; web public domain;
  pinned `SHERPA_WEB_PUBLIC_BASE_URL`; required `SHERPA_REGISTRY_API_URL`; explicit registry
  service `PORT=8080`; reference value
  `http://${{registry.RAILWAY_PRIVATE_DOMAIN}}:${{registry.PORT}}`; both listeners on `[::]`.
- [x] **Step 2: Document separation.** Web gets no volume, DB reference, GitHub OAuth vars, admin/
  session token, or export token. Registry retains its public base/domain and all 2c-iii settings.
  Add deploy/rollback instructions that target only the intended Railway service.
- [x] **Step 3: Add spec §11.1's complete eleven-step live website gate.** Keep the existing live
  registry gate unchanged. Note that Railway healthchecks gate deploy cutover but are not
  continuous monitoring; add operator-owned public home and health monitors.
- [x] **Step 4: Final automated gate.** Run:

  ```sh
  gofmt -w $(rg --files cmd/web internal/web internal/registry -g '*.go')
  go test -race ./internal/web/... ./internal/registry/api ./internal/registry/store ./cmd/web
  go test ./... -count=1
  go vet ./...
  go build ./cmd/...
  bash -n deploy/web/smoke_build.sh
  jq empty deploy/web/railway.json
  bash deploy/web/smoke_build.sh
  git diff --check
  ```

- [x] **Step 5: Review invariants/diff.** Confirm no secret/config dump, no scan excerpt, no
  host-derived URL, no template trust conversion, no registry write/auth behavior change, root
  deployment artifacts unchanged, and no `.DS_Store` or generated QA screenshots are staged.
- [x] **Step 6: Commit.**
  `git commit -m "docs: add Railway website deployment and staging gate"`

---

## Execution order and handoffs

Execute `1 → 2 → 3 → 4 → 5 → 6 → 7 → 8 → 9`. Tasks 1 and 2 settle the public boundary before
templates rely on it. Tasks 3-5 establish behavior/security before visual polish. Task 7 makes the
tested handler runnable; Task 8 proves the exact production artifact; Task 9 is the operator
handoff.

Each task gets its own implementation worker and independent review where available. The
controller reads the actual diff, runs the task's focused tests plus the whole suite, and only
then commits. Do not let workers commit or alter `.git` concurrently.

The live website Railway gate is not an automated Task 9 action. It runs after merge and only
after the operator completes the separate 2c-iii registry staging gate. Production promotion is
blocked until all eleven website steps pass.

## Deliverables checklist

- [x] Optional bounded search/version pagination and pinned `repo_url` on registry details.
- [x] Typed timeout/redirect/body-bounded registry client.
- [x] Safe server-rendered home, search, stack, version, 404, 502, and 503 pages.
- [x] Exact current-stack commands; no invented historical-version behavior.
- [x] Scan excerpt structurally absent from page models/output/logs.
- [x] Strict CSP/security headers, pinned canonical URLs, SEO/crawler behavior, safe logs.
- [x] Responsive accessible UI, progressive copy, verified bitmap brand asset.
- [x] `[::]` listener, independent health, hardened timeouts, graceful shutdown.
- [x] Distroless non-root image, service-specific Railway config, real Docker fault-isolation smoke.
- [x] Railway runbook extension and eleven-step live website staging gate.
- [x] Whole suite/race/vet/build/Docker checks green with registry security invariants unchanged.
