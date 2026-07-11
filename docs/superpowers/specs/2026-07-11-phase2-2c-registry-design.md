# SherpA Phase 2 · Sub-project 2c — Registry API + Backend

**Status:** Design for approval
**Date:** 2026-07-11
**Parent spec:** `docs/superpowers/specs/2026-07-08-follow-the-expert-design.md` (§4.2 registry, §5 data model)

## 1. Goal

Stand up the **registry**: the server that stores expert stacks and their metadata, and the
API the CLI (and later the website/MCP server) talks to. It replaces the CLI's current
placeholders — `publish --remote <bare-git>` and `search <static-index-url>` — with a real
service where **trust does not depend on the client**: the server re-runs the sanitizer and
setup-state barrier on every publish.

## 2. Decomposition (2c is three buildable pieces)

| # | Piece | Delivers | External deps |
|---|---|---|---|
| **2c-i** | **Registry API core** (this spec's plan) | Go HTTP server: publish / search / stack+version detail / fetch-for-clone; local via docker-compose; CLI reroutes publish/search/clone to it | Postgres |
| 2c-ii | Auth + trust + scan pipeline | GitHub OAuth (device flow), publisher identity, trust tiers, async re-scan workers | GitHub OAuth |
| 2c-iii | Deployment | Railway: API + Postgres + persistent git volume, migrations; hosting spike | Railway |

Each gets its own spec→plan→build cycle. This document specifies **2c-i** and sketches the
seams 2c-ii/iii plug into.

## 3. Decisions (2026-07-11)

- **Backend: Go**, one module, reusing `internal/stack` (manifest parse/validate) and
  `internal/sanitize` (the scanner) directly — the security-critical code is written once and
  shared by CLI and server. `net/http` stdlib server; a thin router.
- **Storage: plain bare git repos behind a `ContentStore` interface — NOT Forgejo (yet).**
  The parent spec named Forgejo, but Forgejo is bare-git + a web UI + management. The core
  needs only: `git init --bare` per stack, receive the CLI's push, serve clones. A storage
  interface keeps Forgejo/Gitea/GitHub swappable for later (2d website git-browsing). Deferring
  Forgejo removes a whole service from 2c-i.
- **2c-i auth: a static API token** (`SHERPA_REGISTRY_TOKEN`) the CLI sends on publish; the
  server compares against a configured value. Gates publish from day one without OAuth; real
  GitHub OAuth (device flow) replaces it in 2c-ii. Read endpoints (search/detail/fetch) are
  public.
- **DB: Postgres everywhere**, behind a `Store` interface. Production and integration tests
  both run against Postgres (temp instance / testcontainers) — one SQL dialect, highest
  fidelity.

## 4. Architecture (2c-i)

```
                         ┌──────────────────────────── registry (Go) ────────────┐
  sherpa CLI  ──HTTP──▶  │  router → handlers                                     │
   publish              │    /v1/search        search      ─┐                     │
   search               │    /v1/stacks/…      detail       ├─▶ Store (Postgres) │
   clone (git)          │    POST …/versions   publish  ────┤                     │
                        │    /…​.git/…          git fetch ───┼─▶ ContentStore     │
                        │                     (re-scan gate)│    (bare git repos) │
                        └────────────────────────────────────┘                    │
                                    │                                              │
                              Postgres  +  git-repos volume                        │
```

- **`cmd/registry`** — main: config from env, open Store, mount handlers, serve.
- **`internal/registry/api`** — HTTP handlers + routing + request/response types. One file per
  resource (search, stacks, publish, git). Thin: parse → call service → encode.
- **`internal/registry/store`** — the `Store` interface + a `postgres` implementation +
  schema migrations. Owns all SQL.
- **`internal/registry/content`** — the `ContentStore` interface + a `bipgit` (bare git)
  implementation: create repo, accept a pushed bundle/pack, expose an http git endpoint for
  clone.
- Reuses `internal/stack` and `internal/sanitize` unchanged.

## 5. API (2c-i)

All JSON except the git endpoint. Versioned under `/v1`.

- `GET /v1/search?q=<text>&harness=<name>&tag=<t>` → `{stacks: [{ref, name, owner, summary,
  tags, harness, version, forked_from, repo_url}]}`. Case-insensitive substring over
  name/summary/tags; optional harness/tag filters. This is the source the CLI's `search`
  reads (replacing the static `index.json`).
- `GET /v1/stacks/{owner}/{name}` → stack detail + `versions: [{version, published_at,
  changelog, scan_summary}]`.
- `GET /v1/stacks/{owner}/{name}/versions/{v}` → one version's manifest snapshot + scan report.
- `POST /v1/stacks/{owner}/{name}/versions` (auth: `Authorization: Bearer <token>`) →
  **publish**. Body: the stack as a git bundle (or pack) + declared manifest. Server:
  1. Extracts the pushed tree into a scratch dir.
  2. **Re-runs `sanitize.Scan` + `ScanSetupState` + history scan** over the exact pushed set,
     using the harness resolved from the manifest — **fail-closed, no override** (identical
     policy to the CLI, but authoritative server-side).
  3. Validates the manifest (`stack.Parse`/`Validate`), enforces version immutability
     (reject if the version already exists), and `forked_from` provenance is recorded.
  4. On pass: `ContentStore.Push` the bundle into the stack's bare repo (tagging `v<version>`)
     and insert the immutable `stack_versions` row (manifest snapshot + scan report).
  5. Returns 201 with the version; on any scan finding, 422 with the findings (no store write).
- `GET /v1/stacks/{owner}/{name}.git/…` → smart-HTTP git fetch, so `sherpa clone/try` does a
  normal `git clone <registry>/v1/stacks/{owner}/{name}.git`.

## 6. Data model (Postgres, 2c-i subset of parent §5)

```sql
users(id, handle UNIQUE, display_name, created_at)               -- token-owner stub in 2c-i
stacks(id, owner_id→users, name, summary, tags text[], harness,
       forked_from text, created_at, UNIQUE(owner_id, name))
stack_versions(id, stack_id→stacks, version int, git_tag,
       manifest jsonb, scan_report jsonb, changelog text,
       published_at, UNIQUE(stack_id, version))                   -- immutable
```

Social tables (follows, events, trial_feedback) are **2e**, not here. Migrations live in
`internal/registry/store/migrations` and run on startup.

## 7. CLI changes (2c-i)

- `sherpa publish`: gains `--registry <url>` (or `SHERPA_REGISTRY_URL` env; keep `--remote`
  for a raw bare-git target as a fallback). Sends the bundle + token to `POST …/versions`;
  surfaces server scan findings on a 422 (fail-closed remains — now enforced twice).
- `sherpa search`: `SHERPA_REGISTRY_URL`'s `/v1/search` replaces the static index URL (the
  static `index.json` + site stay for offline/demo).
- `sherpa clone/try`: accept a `@owner/name` ref that resolves to the registry's git URL, in
  addition to a raw git URL.
- The client keeps its own pre-publish scan (fast local feedback); the server scan is the
  authority.

## 8. Error handling / invariants

- **Publish is fail-closed server-side**: any scan finding or validation error → 422, nothing
  written to Postgres or the git store (scan happens in a scratch dir before any store call).
- **Versions immutable**: re-publishing an existing version → 409.
- **Partial-write safety**: insert the DB row only after the git push succeeds; if the DB
  insert fails, the pushed tag is orphaned but harmless (a reconcile/GC job is 2c-iii) — never
  the reverse (no metadata pointing at absent content).
- **Auth**: missing/blank/wrong token on publish → 401. Read endpoints public.
- Store/ContentStore errors → 500 with a generic message (details logged, never leaked).

## 9. Testing strategy

- **Store**: contract tests against a real temp Postgres (testcontainers-go or a
  `SHERPA_TEST_DATABASE_URL`); schema-migration up/down; immutability + uniqueness constraints.
- **ContentStore**: bare-git create/push/fetch round-trip in a temp dir.
- **Publish handler (the security gate)**: a stack with a planted secret / setup-state file →
  422, and **assert nothing was written** (no version row, no git tag) — the server-side
  fail-closed proof. A clean stack → 201, then a `git clone` of the endpoint returns the tree.
- **End-to-end**: `publish → search → clone` against a running test registry, driven through
  the CLI (extend the integration suite behind a `SHERPA_REGISTRY_URL` guard).
- **Day-1 spike** (before the plan's other tasks): verify the **smart-HTTP git push/fetch
  mechanics** — can the CLI push a stack to the API's git endpoint and clone it back? — since
  that transport is the one unproven assumption. (Railway hosting is spiked in 2c-iii.)

## 10. Alternatives considered

- **Forgejo now.** Rejected for 2c-i: a whole extra service + admin surface for functionality
  (bare repos) the API can own directly. Kept swappable behind `ContentStore` for 2d.
- **Store stacks as tarballs, not git.** Rejected: loses the fork/diff/update history the whole
  product is built on; `git` is the versioning model already.
- **SQLite for tests.** Rejected (decision §3): dialect drift on the security-relevant queries.
- **A framework (chi/echo/gin).** Deferred: stdlib `net/http` + a tiny router is enough for
  2c-i; revisit only if routing grows unwieldy.

## 11. Open questions (for approval)

1. **Git transport for publish** (spike-pinned): smart-HTTP `git push` to the API's endpoint,
   vs the CLI uploading a `git bundle` blob that the server unpacks. Recommendation: **git
   bundle upload** for publish (simplest, one authenticated POST, no server-side git-receive-
   pack auth dance) + smart-HTTP for the read/clone side. The spike confirms both.
2. **`@owner/name` ref resolution** — done client-side (CLI asks `/v1/search` or a resolve
   endpoint) or via a redirecting git URL. Minor; settle in the plan.
