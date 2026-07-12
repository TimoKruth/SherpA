# Phase 2 · 2c-i — Registry API Core Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the registry API core: a Go HTTP server that stores stacks (bare git) + metadata (Postgres), re-runs the security scan server-side on publish (fail-closed), and serves search/detail/clone — with the CLI rerouted to it.

**Architecture:** One Go module (extends the existing `sherpa` module). A new `cmd/registry` server and `internal/registry/{store,content,api}` packages reuse `internal/stack` (manifest) and a new shared `internal/publishscan` (the fail-closed repo scan, extracted from the CLI so the security logic exists once). Content = bare git repos behind a `ContentStore` interface; metadata = Postgres behind a `Store` interface. Publish accepts a git bundle, scans it in a scratch dir, and only then writes.

**Tech Stack:** Go ≥1.22 (stdlib `net/http` with method+path ServeMux, no web framework), `github.com/jackc/pgx/v5` (Postgres driver — the one new dependency), `gopkg.in/yaml.v3` (existing), system `git`. Postgres for tests via a `docker run` helper (Docker daemon required; tests `t.Skip` if absent).

## Global Constraints

- Spec: `docs/superpowers/specs/2026-07-11-phase2-2c-registry-design.md` — re-read §4-9 before starting.
- **Publish is fail-closed server-side, no override** (spec §8): any scan finding or validation error → HTTP 422, and **nothing is written** to Postgres or the git store. Scan happens in a scratch dir before any store call. Re-publishing an existing version → 409.
- **Security logic written once:** the CLI and the server both call `internal/publishscan.ScanRepo`. Do not duplicate the scan logic.
- **Storage abstraction:** the API depends on `store.Store` and `content.ContentStore` interfaces, never on Postgres/git directly outside those packages. (Forgejo is a future swap-in behind `ContentStore`.)
- New dep is exactly `github.com/jackc/pgx/v5`. No web framework, no ORM, no testcontainers.
- Module `sherpa`; gofmt-clean, `go vet` clean. Existing 12 packages stay green.
- Read endpoints public; publish requires `Authorization: Bearer <SHERPA_REGISTRY_TOKEN>`.
- Commit after every task; `feat:`/`refactor:`/`test:`.

## Codex Delegation

Codex implements via the `codex-call` wrapper: `~/.claude/skills/codex-call/codex-run.sh --mode workspace-write --cwd /Users/timokruth/Projekte/feat --timeout 700 --prompt-file <task>`. Controller verifies (`go test ./...`, gofmt, vet) and commits (Codex can't write `.git`). Offline env `GOFLAGS=-mod=mod GOPROXY=off GOCACHE=/private/tmp/sherpa-go-build` — **except** the pgx dependency must be fetched first by the controller (`go get github.com/jackc/pgx/v5`) so offline builds work. Reviews: Opus, **Fable for Task 6** (the publish security gate).

---

### Task 1: Day-1 spike — git bundle transport + dumb-HTTP clone + Postgres-docker test harness

Exploratory (no TDD). **Not delegable.** Pins the two unproven mechanics before the plan builds on them.

**Files:**
- Create: `spike/registry-transport.sh`, `docs/superpowers/spikes/2026-07-12-registry-transport.md`

**Interfaces:** Produces the confirmed publish-transport and clone-serve mechanics + the pg-docker test-harness recipe, consumed by Tasks 3-7.

- [ ] **Step 1: Write the probe script** verifying, in temp dirs:
  1. **Publish transport:** make a stack repo, `git bundle create stack.bundle --all`; in a fresh bare repo `git init --bare`, unpack via `git -C bare fetch ../stack.bundle 'refs/*:refs/*'` (or `git bundle unbundle`), confirm the tag/commits land.
  2. **Clone serve (dumb HTTP):** in the bare repo `git update-server-info`, serve its dir with `python3 -m http.server`, then `git clone http://localhost:PORT/ out` from a clean dir — confirm the tree + tag arrive.
  3. **Scan surface:** confirm you can `git -C bare worktree add <tmp> <tag>` (or clone the bundle) to get a working tree to scan.
  4. **Postgres via docker:** `docker run --rm -d -e POSTGRES_PASSWORD=pw -p 0:5432 postgres:16`, poll `docker exec ... pg_isready` until ready, capture the mapped host port, `docker stop` — confirm the round-trip works and time it.
- [ ] **Step 2: Run it**, capture evidence.
- [ ] **Step 3: Write findings** — the exact git commands for bundle-push and dumb-HTTP-clone, the pg-docker start/ready/stop recipe with the port-capture command, and any gotcha (auth, `update-server-info` timing). If bundle-unbundle or dumb clone don't work as expected, record the working alternative (e.g. `git http-backend`) and STOP to revise the plan.
- [ ] **Step 4: Commit** — `git add spike/ docs/superpowers/spikes/ && git commit -m "docs: registry transport + pg-docker spike findings"`

---

### Task 2: Extract `internal/publishscan.ScanRepo` (shared fail-closed scan)

**Files:**
- Create: `internal/publishscan/scan.go`, `internal/publishscan/scan_test.go`
- Modify: `internal/cli/cmd_publish.go` (call the shared function)

**Interfaces:**
- Consumes: `internal/sanitize` (Scan/ScanPatch/ScanSetupState/ScanPatchSetupState), `internal/harness`, `internal/gitutil`.
- Produces (consumed by Task 6):

```go
// ScanRepo runs the full fail-closed publish scan over a git repo checkout: the exact
// pushed working-tree set (git ls-files) for secrets + setup-state, and the history
// patch (git log -m -p over `historyRange`, or all of it if empty) for both. h supplies
// the harness's SetupStateFilenames/LoginSignatures. Returns all findings (empty = clean).
func ScanRepo(repoDir string, h harness.Harness, historyRange string) ([]sanitize.Finding, error)
```

- [ ] **Step 1: Write the failing test** — a temp git repo with a tracked `settings.json` containing a `ghp_` token → `ScanRepo` returns a secret finding; a clean repo → empty; a tracked `auth.json`-content oauth signature under the codex harness → a setup-state finding.
- [ ] **Step 2: Verify fail** — `go test ./internal/publishscan/` → FAIL.
- [ ] **Step 3: Implement** by lifting the exact logic currently in `cmd_publish.go` (`publishScanFiles` via `git ls-files -z --cached --others --exclude-standard`, and `scanPublishHistoryPatch` via `git log -m -p <range>`), parameterized by `h.SetupStateFilenames()`/`h.LoginSignatures()` and `h`-derived allowed paths. Then rewrite `cmd_publish.go` to call `publishscan.ScanRepo(profile.Path, h, historyRange)` and drop its now-duplicated private helpers — **zero behavior change; all existing publish/security tests stay green.**
- [ ] **Step 4: Verify** — `go test ./...` → PASS (CLI publish tests unchanged).
- [ ] **Step 5: Commit** — `git add -A && git commit -m "refactor: extract shared fail-closed publishscan.ScanRepo (CLI + registry reuse)"`

---

### Task 3: `store.Store` interface + Postgres impl + migrations + pg-docker test harness

**Files:**
- Create: `internal/registry/store/store.go` (interface + types), `internal/registry/store/postgres.go`, `internal/registry/store/migrations.go`, `internal/registry/store/postgres_test.go`, `internal/registry/store/testharness.go`
- Modify: `go.mod`/`go.sum` (pgx)

**Interfaces:**
- Produces (consumed by Tasks 5, 6, 7):

```go
type Stack struct { ID int64; Owner, Name, Summary, Harness, ForkedFrom string; Tags []string; CreatedAt time.Time }
type Version struct { ID, StackID int64; Version int; GitTag string; Manifest, ScanReport json.RawMessage; Changelog string; PublishedAt time.Time }
type Store interface {
    UpsertUser(ctx, handle string) (userID int64, err error)          // token-owner stub
    UpsertStack(ctx, s Stack) (stackID int64, err error)              // by (owner,name); updates summary/tags/harness/forked_from
    InsertVersion(ctx, v Version) error                                // errors ErrVersionExists on dup (stack_id,version)
    Search(ctx, q, harness, tag string) ([]StackWithLatest, error)
    GetStack(ctx, owner, name string) (Stack, []Version, error)       // ErrNotFound if absent
    GetVersion(ctx, owner, name string, v int) (Version, error)
    Close() error
}
var ErrVersionExists = errors.New("version already exists")
var ErrNotFound = errors.New("not found")
// testharness.go: StartPostgres(t) (dsn string) — docker run postgres, wait ready, t.Cleanup stops it; t.Skip if docker/pg unavailable.
```

- [ ] **Step 1: Write failing contract tests** (`postgres_test.go`) using `StartPostgres(t)`: migrate; UpsertUser+UpsertStack+InsertVersion round-trip; InsertVersion twice → `ErrVersionExists`; Search matches substring + harness filter; GetStack returns versions newest-first; GetVersion; GetStack(missing) → `ErrNotFound`.
- [ ] **Step 2: Verify fail** — `go get github.com/jackc/pgx/v5 && go test ./internal/registry/store/` → FAIL (or SKIP if no docker — then the controller ensures docker is up).
- [ ] **Step 3: Implement** the schema (spec §6) in `migrations.go` (embedded SQL, run in order on `Migrate(ctx, pool)`), the pgx-backed `postgres.go`, and `testharness.go` per the spike's pg-docker recipe (start `postgres:16`, poll `pg_isready`, return DSN, `t.Cleanup` `docker stop`).
- [ ] **Step 4: Verify** — `go test ./internal/registry/store/` → PASS (real Postgres).
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: registry Store (Postgres) + migrations + docker test harness"`

---

### Task 4: `content.ContentStore` bare-git + bundle push/serve

**Files:**
- Create: `internal/registry/content/content.go` (interface), `internal/registry/content/baregit.go`, `internal/registry/content/baregit_test.go`

**Interfaces:**
- Produces (consumed by Tasks 5, 6):

```go
type ContentStore interface {
    // EnsureRepo creates profiles/<owner>/<name>.git bare repo if absent.
    EnsureRepo(owner, name string) error
    // ImportBundle unbundles the bundle bytes into the repo (fetching all refs), runs
    // update-server-info, and returns a scratch worktree dir checked out at gitTag for
    // scanning (caller removes it). Fail-closed callers scan BEFORE keeping the import.
    // Returns the number of new refs. Import into a temp repo first; only Publish (below)
    // makes it durable.
    StageBundle(bundle []byte, gitTag string) (stageDir, worktreeDir string, err error)
    // Commit makes a staged import durable under owner/name (atomic rename) + update-server-info.
    Commit(owner, name, stageDir string) error
    // RepoPath returns the on-disk bare repo dir (for the dumb-HTTP clone handler).
    RepoPath(owner, name string) string
}
```

(Design: staging separates "unpack + scan" from "make durable", so a rejected publish never touches the live repo — mirrors the CLI's staged-clone discipline.)

- [ ] **Step 1: Write failing test** — build a stack git repo in a temp dir, `git bundle create`; `StageBundle` → worktree contains the stack files at the tag; `Commit` → `RepoPath` bare repo exists; a `git clone` of `RepoPath` (file://) yields the tree + tag. Also: staging a second bundle and NOT committing leaves the live repo unchanged.
- [ ] **Step 2: Verify fail.**
- [ ] **Step 3: Implement** per the spike's bundle-unbundle + update-server-info commands, content root from a constructor arg (`NewBareGit(root string)`), atomic `os.Rename` for Commit.
- [ ] **Step 4: Verify** — `go test ./internal/registry/content/` → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: registry ContentStore (bare git, staged bundle import)"`

---

### Task 5: API read side — router + search + detail + git clone serving

**Files:**
- Create: `internal/registry/api/router.go`, `internal/registry/api/read.go`, `internal/registry/api/gitserve.go`, `internal/registry/api/read_test.go`

**Interfaces:**
- Consumes: `store.Store`, `content.ContentStore`.
- Produces: `api.New(st store.Store, cs content.ContentStore, token string) http.Handler` mounting all `/v1` routes (publish added in Task 6). Read handlers use Go 1.22 `ServeMux` patterns (`GET /v1/search`, `GET /v1/stacks/{owner}/{name}`, `GET /v1/stacks/{owner}/{name}/versions/{v}`, `GET /v1/stacks/{owner}/{name}.git/`).

- [ ] **Step 1: Write failing tests** (`read_test.go`, `httptest`) with a fake in-memory `Store` (define a test double implementing the interface): `/v1/search?q=` returns matching stacks JSON; `/v1/stacks/o/n` returns detail+versions; missing → 404; `/v1/stacks/o/n/versions/2` returns the version; the git route serves the bare repo files (use a real content dir with one committed repo, assert the `info/refs` file is served).
- [ ] **Step 2: Verify fail.**
- [ ] **Step 3: Implement** — `router.go` builds the mux + wires handlers; `read.go` encodes JSON from Store results (map to the spec §5 response shapes); `gitserve.go` serves `RepoPath(owner,name)` as a dumb-HTTP git dir via `http.FileServer`/`http.StripPrefix` (per spike). JSON errors are `{error: "..."}` with the right status.
- [ ] **Step 4: Verify** — `go test ./internal/registry/api/` → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: registry read API (search/detail) + dumb-HTTP git clone serving"`

---

### Task 6: Publish handler — the fail-closed security gate

**Files:**
- Create: `internal/registry/api/publish.go`, `internal/registry/api/publish_test.go`
- Modify: `internal/registry/api/router.go` (mount `POST /v1/stacks/{owner}/{name}/versions`)

**Interfaces:**
- Consumes: `publishscan.ScanRepo` (2), `store.Store` (3), `content.ContentStore` (4), `stack.Parse/Validate`, `harness.For`.

- [ ] **Step 1: Write failing tests** — the security proofs:
  - **Auth:** no/blank/wrong Bearer token → 401, nothing written.
  - **Secret blocked:** publish a bundle whose tree has a `ghp_` token in a tracked file → 422 with findings, **and assert the Store got no InsertVersion and ContentStore.Commit was never called** (fail-closed proof; use spies on the doubles).
  - **Setup-state blocked:** a tracked `auth.json`-content oauth signature (codex harness) → 422, nothing written.
  - **Immutability:** publishing an existing version → 409.
  - **Happy path:** a clean bundle → 201 with the version; ContentStore.Commit called; Store.InsertVersion called with the manifest snapshot + scan report; then `GET /v1/stacks/o/n` shows it.
- [ ] **Step 2: Verify fail.**
- [ ] **Step 3: Implement** `handlePublish`:
  1. Check Bearer token (constant-time compare) → 401 on mismatch.
  2. Read the bundle body + declared manifest (multipart or JSON envelope with base64 bundle — pick per the spike; document it).
  3. `ContentStore.StageBundle` → stageDir + worktreeDir (scratch).
  4. `stack.Parse` the worktree's `stack.yaml`; `harness.For(m.Harness)`; `m.Validate(worktreeDir, h)` → 422 on violations.
  5. `publishscan.ScanRepo(worktreeDir, h, "")` → **any finding ⇒ 422 with findings, remove scratch, return (no Commit, no InsertVersion).**
  6. Version immutability: `Store.GetVersion` exists → 409.
  7. `ContentStore.Commit(owner,name,stageDir)`; then `Store.UpsertStack` + `Store.InsertVersion` (manifest + scan report JSON). If InsertVersion returns `ErrVersionExists` → 409. Order per spec §8: content first, then metadata.
  8. 201 with the version JSON. Always clean up scratch/worktree.
- [ ] **Step 4: Verify** — `go test ./internal/registry/api/` → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: registry publish handler — server-side fail-closed scan gate"`

---

### Task 7: `cmd/registry` main + config + wiring + boot test

**Files:**
- Create: `cmd/registry/main.go`, `internal/registry/config.go`, `cmd/registry/main_test.go`
- Modify: `Makefile` (build the registry binary)

**Interfaces:**
- Consumes: everything above.
- Produces: a runnable server. Config from env: `PORT` (default 8080), `DATABASE_URL`, `SHERPA_REGISTRY_TOKEN`, `SHERPA_CONTENT_DIR` (default `./registry-content`).

- [ ] **Step 1: Write a boot test** (`main_test.go`) using `StartPostgres(t)` + a temp content dir: construct the server via a testable `func run(cfg Config) (http.Handler, func(), error)` (open Store, Migrate, build ContentStore, `api.New`), then `httptest` one `GET /v1/search` → 200 empty list. (Keep `main()` a thin wrapper around `run` for testability.)
- [ ] **Step 2: Verify fail.**
- [ ] **Step 3: Implement** `config.go` (env parse + validation: missing `DATABASE_URL` or empty token → clear error) and `main.go` (`run` opens the pgx pool, `store.Migrate`, `content.NewBareGit`, `api.New`, `http.ListenAndServe`; `main` calls `run` and logs). Makefile `make registry` target.
- [ ] **Step 4: Verify** — `go test ./cmd/registry/ ./...` → PASS; `make registry` builds.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: cmd/registry server main + config + wiring"`

---

### Task 8: CLI reroute — publish/search/clone against the registry + e2e

**Files:**
- Modify: `internal/cli/cmd_publish.go`, `internal/cli/cmd_search.go`, `internal/cli/cmd_clone.go` (ref resolution)
- Create: `internal/cli/registryclient.go`, `internal/integration/registry_e2e_test.go`

**Interfaces:**
- Consumes: the running registry (via HTTP); reuses `gitutil` for the clone.
- Produces:
  - `sherpa publish --registry <url>` (or `SHERPA_REGISTRY_URL`; `--remote` stays for raw bare-git): builds a `git bundle` of the profile's `local` branch, POSTs it + the token (`SHERPA_REGISTRY_TOKEN`) to `…/versions`; on 422 prints the server findings and exits nonzero (fail-closed, now enforced twice). Keeps the client-side pre-scan for fast feedback.
  - `sherpa search`: if `SHERPA_REGISTRY_URL` set, query `…/v1/search`; else fall back to the existing `SHERPA_INDEX_URL` static index.
  - `sherpa clone/try <@owner/name>`: resolve `@owner/name` to `<registry>/v1/stacks/owner/name.git` and `git clone` it; a raw git URL still works unchanged.

- [ ] **Step 1: Write the e2e test** (`registry_e2e_test.go`, behind `StartPostgres(t)`): boot the registry (`run` from Task 7) on an `httptest.Server` + temp content dir; via `cli.Run`: init → clone a fixture stack (raw) → publish `--registry <httptest-url>` with the token → assert 201; `search` against the registry returns it; `clone @owner/name` from the registry yields the tree; and a publish of a secret-bearing stack is rejected (nonzero, server findings) with nothing searchable.
- [ ] **Step 2: Verify fail/iterate.**
- [ ] **Step 3: Implement** `registryclient.go` (bundle build via `gitutil`, POST with token, decode 201/422/409), and the three cmd reroutes. Match the transport (multipart/JSON envelope) chosen in Task 6.
- [ ] **Step 4: Verify** — `go test ./... -count=1` → all packages PASS (registry + e2e run against Docker Postgres); gofmt/vet clean.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: CLI publish/search/clone rerouted to the registry + e2e"`

---

## Execution order & dependencies

1 (spike, gates transport+pg) → 2 (shared scan) → 3 (Store) → 4 (ContentStore) → 5 (read API) → 6 (publish gate) → 7 (server main) → 8 (CLI + e2e). Task 6 is the security-critical gate — review on Fable; the rest Opus. Whole-suite-green at every task (registry tests run against Docker Postgres; they `t.Skip` only if Docker is truly unavailable — Docker is up in this environment, so they must pass).

## Deliverables checklist (spec §4-9)

- [ ] Go server reusing internal/stack + internal/sanitize via shared publishscan (Tasks 2,6,7).
- [ ] Store (Postgres) + ContentStore (bare git) behind interfaces (Tasks 3,4).
- [ ] Read API: search + detail + git clone serving (Task 5).
- [ ] Publish: server-side fail-closed re-scan, 422/409/401, nothing-written proof (Task 6).
- [ ] Static token auth on publish; public reads (Tasks 5,6).
- [ ] CLI rerouted (publish --registry / search / clone @owner/name) + e2e (Task 8).
- [ ] Postgres-everywhere via docker test harness (Task 3); git transport spike-pinned (Task 1).
