# Phase 2 · 2c-iii — Railway Deployment Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship the authenticated registry as a production-shaped Railway service — hardened HTTP server + `/healthz` + graceful shutdown, bounded public auth endpoints, pinned public clone/issuer URL, startup staging-cleanup, a consistency-audit and scheduled off-site export, a non-root Docker image + `railway.json` + entrypoint, and a staging runbook/gate — without weakening any 2c-i/2c-ii invariant.

**Architecture:** Mostly small, well-tested changes to `cmd/registry`/`internal/registry/{config,api,content}` plus two read-only ops commands, wrapped by deployment artifacts (Dockerfile, entrypoint, railway.json, runbook). The API stays one replica with a single Railway volume for bare git; migrations stay startup-owned and additive; the fail-closed publish gate and identity checks are unchanged.

**Tech Stack:** Go ≥1.22 stdlib (`net/http`, `os/signal`, `context`), existing `github.com/jackc/pgx/v5` (+ `pgxpool`), system `git`, Docker (multi-stage), Railway config-as-code. No web framework.

## Dependency & scope

- **Execute AFTER 2c-ii is merged.** This plan references 2c-ii symbols: the session file
  `$SHERPA_HOME/registry-session.json`, `api.New(store, content, adminToken, github)`, the
  `SHERPA_GITHUB_CLIENT_ID` config, and the device endpoints. Do not start until 2c-ii is on `main`.
- Spec: `docs/superpowers/specs/2026-07-12-phase2-2c-iii-railway-deployment-design.md` (Fable-reviewed) — re-read §3 (Railway constraints), §5, §6, §8 before starting. **Re-verify the §3 Railway platform facts against current docs before provisioning any production state** (they were checked 2026-07-12).

## Global Constraints

- All 2c-i/2c-ii invariants unchanged: publish is fail-closed server-side (scan before any write); publish authz is deny-by-default (own-login-only session / env-gated admin token); nothing-written on rejection.
- **Canonical clone/issuer host is pinned by `SHERPA_PUBLIC_BASE_URL`, never read from `X-Forwarded-Host`** (spec §5.3). `X-Forwarded-Proto` gives scheme only, and only when `SHERPA_TRUST_PROXY=true`.
- **No secret ever in logs/health responses/Docker layers/`railway.json`/command lines**: `DATABASE_URL`, admin/session/GitHub tokens.
- `/healthz` returns 200 only after startup init (config + DB connect + migrations) completes; it must not call GitHub.
- Migrations stay **startup-owned, additive, idempotent** (no Railway pre-deploy migration).
- Git bundle/checkout/scan run as a **non-root** user in the container.
- One API replica + one volume (Railway does not support replicas with a volume). Content dir = `SHERPA_CONTENT_DIR=/data/git`.
- Off-site DR export (`git bundle` + `pg_dump` to storage outside the Railway project) is in scope (spec §8).
- Export consistency is database-first: `pg_dump` completes before any repo bundle is created. Since publish commits Git before inserting immutable metadata, every version in the dump is then guaranteed to be present in the later bundles; a concurrent publish can produce only harmless extra Git content. A completion manifest is written last and the finished archive is uploaded by the volume-owning API process to an external HTTPS collector.
- Public device-flow JSON bodies are bounded; start/poll calls are rate-limited in-process (single replica), GitHub HTTP calls have bounded timeouts, and request logs never include authorization/body/token values (spec §7).
- Module `sherpa`; gofmt/vet clean; existing `go test ./...` stays green (Postgres tests via the existing docker `StartPostgres` harness; Docker up here).
- Commit after every task; `feat:`/`fix:`/`docs:`/`chore:`.

## Codex Delegation

Codex implements via `~/.claude/skills/codex-call/codex-run.sh --mode workspace-write --cwd /Users/timokruth/Projekte/feat --timeout 800 --prompt-file <task>`; the controller verifies (`go test ./...`, gofmt, vet, and for Task 6 a real `docker build`+run) and commits. Offline env `GOFLAGS=-mod=mod GOPROXY=off GOCACHE=/private/tmp/sherpa-go-build`. Reviews: Opus, **Fable for Task 2 (pinned public URL — host-header security) and Task 5 (DR export correctness)**.

---

### Task 1: Hardened HTTP server, `/healthz`, graceful shutdown, config additions

**Files:**
- Modify: `internal/registry/config.go`, `cmd/registry/main.go`, `internal/registry/api/router.go`
- Test: `internal/registry/config_test.go` (or `cmd/registry/main_test.go`), `internal/registry/api/health_test.go`

**Interfaces:**
- Produces: `Config` gains `PublicBaseURL string`, `TrustProxy bool`, `DBMaxConns int`, plus export scheduler settings added in Task 5; `run(ctx, cfg)` builds an `*http.Server` with timeouts and returns it + a shutdown func; `GET /healthz` handler returning 200 once `ready` is set.

- [ ] **Step 1: Write failing tests** — (a) `LoadConfig` reads `SHERPA_PUBLIC_BASE_URL`, `SHERPA_TRUST_PROXY` (parsed bool), `SHERPA_DB_MAX_CONNS` (non-negative int, default 0=pool default); malformed bool/int values fail config; `DATABASE_URL` still required, admin token still optional (2c-ii). (b) `/healthz` returns 503 before init and 200 after (drive via a `ready *atomic.Bool` the handler reads; a test flips it). (c) the server is an `http.Server` with non-zero `ReadHeaderTimeout`/`ReadTimeout`/`WriteTimeout`/`IdleTimeout`. (d) database startup retries transient open failures with bounded exponential backoff and stops on context cancellation; inject an opener/sleeper in the unit test rather than waiting in real time.

```go
func TestHealthzReadyGate(t *testing.T) {
	var ready atomic.Bool
	h := healthHandler(&ready)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rr.Code != http.StatusServiceUnavailable { t.Fatalf("pre-ready = %d", rr.Code) }
	ready.Store(true)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rr.Code != http.StatusOK { t.Fatalf("post-ready = %d", rr.Code) }
}
```

- [ ] **Step 2: Verify fail** — `go test ./internal/registry/... ./cmd/registry/` → FAIL.
- [ ] **Step 3: Implement** — config fields + strict parsing (`strconv.ParseBool`/`Atoi`, empty means default); pass `DBMaxConns` into `OpenPostgres` (extend it to parse `pgxpool.Config` and set `MaxConns` when >0). Add `openPostgresWithRetry`: maximum 5 attempts, exponential 250ms→4s delay, context-cancellable, logs operation/attempt only (never DSN). In `run`: create `var ready atomic.Bool`; mount `/healthz` via `healthHandler(&ready)` on the mux (add to `api.New` or wrap the handler in `run`); build `srv := &http.Server{Addr: ":"+port, Handler: h, ReadHeaderTimeout: 5*time.Second, ReadTimeout: 2*time.Minute, WriteTimeout: 5*time.Minute, IdleTimeout: 2*time.Minute}`; after migrations succeed set `ready.Store(true)`; `main` runs `srv.ListenAndServe()` in a goroutine and on `signal.NotifyContext(SIGINT,SIGTERM)` calls `srv.Shutdown(ctxWithTimeout)` then the store cleanup. Health handler: `if !ready.Load() { 503 } else { 200 "ok" }`, never touches GitHub or (for the 200 path) the DB.
- [ ] **Step 4: Verify** — `go test ./...` → PASS; gofmt/vet clean.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: registry hardened http.Server + /healthz gate + graceful shutdown + config"`

---

### Task 1B: Public auth abuse controls and secret-safe request logging

**Files:**
- Modify: `internal/registry/api/auth.go`, `internal/registry/api/router.go`, `internal/registry/auth/httpgithub.go`
- Test: `internal/registry/api/auth_test.go`, `internal/registry/auth/httpgithub_test.go`

**Interfaces:**
- Produces: device body cap (4 KiB); an injected-clock in-memory limiter keyed by client IP for starts and by SHA-256(device code) for polls; a dedicated GitHub `http.Client` with a 30-second overall timeout; request logging middleware that logs method/path/status/duration and `X-Railway-Request-Id`, never headers or bodies.

- [ ] **Step 1: Write failing tests** — oversized poll JSON → 413 and zero GitHub calls; a second start inside the configured window → 429; poll before its device interval → 429 and zero additional GitHub calls; `slow_down` increases the next interval; limiter entries expire; logger output includes request ID/status but excludes planted Bearer, GitHub token, session token, and body values. Assert the real GitHub adapter uses a non-default client with non-zero timeout and honors context cancellation.
- [ ] **Step 2: Verify fail.**
- [ ] **Step 3: Implement** — `http.MaxBytesReader` + strict one-object JSON decode; mutex-protected limiter with bounded/expired entry cleanup (single replica, no new dependency); derive IP from `X-Real-IP` only when trust-proxy is enabled, otherwise `RemoteAddr`; hash device codes before using them as map keys/log context; wrap the router in secret-safe logging middleware; use `&http.Client{Timeout: 30*time.Second}` in `NewGitHubClient`. Return 429 with `Retry-After`; do not call GitHub for locally rejected traffic.
- [ ] **Step 4: Verify** — focused auth/API tests, `go test ./...`, gofmt/vet all pass.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: bound and rate-limit public registry auth endpoints"`

---

### Task 2: Pinned public clone/issuer URL (host-header hardening)

**Files:**
- Modify: `internal/registry/api/read.go` (`repoURL`), `internal/registry/api/router.go` or a small `publicurl.go`
- Test: `internal/registry/api/read_test.go`

**Interfaces:**
- Consumes: `Config.PublicBaseURL`, `Config.TrustProxy` (thread into `api.New` as a `PublicURL` value or pass the two fields). Extend `api.New(store, content, adminToken, github, publicBaseURL string, trustProxy bool) http.Handler` (update all call sites incl. `run` and tests).
- Produces: `repoURL(r, owner, name)` = `PublicBaseURL + "/v1/stacks/owner/name.git"` when `PublicBaseURL != ""`; else scheme from `X-Forwarded-Proto` (only if `trustProxy`) or `r`, host from `r.Host` (never `X-Forwarded-Host`).

- [ ] **Step 1: Write failing tests** — (a) config accepts and canonicalizes `https://registry.example/` to `https://registry.example`; rejects missing scheme/host, userinfo, query, fragment, and non-http(s) schemes. (b) with `PublicBaseURL="https://registry.example"`, `repo_url` is `https://registry.example/v1/stacks/o/n.git` **regardless of** a spoofed `X-Forwarded-Host: evil.com` / `Host: evil.com` on the request. (c) with no base URL + `trustProxy=true` + `X-Forwarded-Proto: https`, scheme is https and host is `r.Host` (NOT the forwarded host). (d) no base URL + `trustProxy=false`: ignores `X-Forwarded-Proto`, uses direct scheme/host (local-test fallback unchanged).
- [ ] **Step 2: Verify fail.**
- [ ] **Step 3: Implement** — parse `PublicBaseURL` once at config load as a canonical origin (scheme + host, optional normalized path prefix; no credentials/query/fragment); pin from that value; only consult `X-Forwarded-Proto` (scheme) under `trustProxy`; **never read `X-Forwarded-Host`**. Wire the two config values through `api.New` and `run`. Production/runbook requires a non-empty public base URL; fallback exists only for direct/local compatibility.
- [ ] **Step 4: Verify** — `go test ./internal/registry/api/ ./...` → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: pin public clone/issuer URL via SHERPA_PUBLIC_BASE_URL (host-header hardening)"`

---

### Task 3: Startup content-dir prep + abandoned staging cleanup

**Files:**
- Modify: `internal/registry/content/baregit.go` (a `CleanAbandonedStages` method), `cmd/registry/main.go` (call it in `run` before serving)
- Test: `internal/registry/content/baregit_test.go`

**Interfaces:**
- Produces: `(*BareGit).CleanAbandonedStages() (removed int, err error)` — removes `<root>/.stage-*` wrapper dirs (the StageBundle temp dirs), never touches `<root>/profiles/**` published repos. `run` calls it after content-dir prep, before `ready`.

- [ ] **Step 1: Write failing test** — create a content root with a published repo (via `NewBareGit`+`StageBundle`+`Commit`) AND a leftover `.stage-abc` dir with a file; `CleanAbandonedStages()` removes the `.stage-*` dir (returns removed≥1) and leaves the published repo + its clone intact.
- [ ] **Step 2: Verify fail.**
- [ ] **Step 3: Implement** — glob `filepath.Join(root, ".stage-*")`, `os.RemoveAll` each (dirs only); safe because single-replica + single volume means no live stage exists at startup (the old container is stopped before the new mounts — note this in a comment). In `run`: `os.MkdirAll(cfg.ContentDir, 0o755)` then `bareGit.CleanAbandonedStages()` (log the count; a cleanup error is fatal — a broken content dir must not serve).
- [ ] **Step 4: Verify** — `go test ./internal/registry/content/ ./...` → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: startup cleanup of abandoned staging dirs"`

---

### Task 4: Consistency-audit command

**Files:**
- Create: `internal/registry/audit/audit.go`, `internal/registry/audit/audit_test.go`, `cmd/registry/audit.go` (a `registry audit` subcommand: `os.Args[1]=="audit"` dispatch in main)
- Modify: `cmd/registry/main.go` (subcommand dispatch)

**Interfaces:**
- Consumes: `store.Store` (a method to list all `(owner,name,version,git_tag)` — add `AllVersionRefs(ctx) ([]VersionRef, error)`) and content inventory methods `ListRepositories() ([]RepositoryRef,error)` + `ListTags(owner,name) ([]string,error)` in addition to `TagCommit`.
- Produces: `audit.Run(ctx, st store.Store, cs content.ContentStore) (Report, error)` with `Report{MissingContent []VersionRef; ExtraTags []string}`; `MissingContent` non-empty → nonzero exit (blocks cutover). Also add `store.AllVersionRefs`.

- [ ] **Step 1: Write failing test** (real Postgres via `StartPostgres`) — seed two versions; delete one version's tag from the bare repo (or delete the repo) → `Run` reports it in `MissingContent`; add an extra tag in a repo with no version row → `ExtraTags`; a consistent store → empty report. `MissingContent` non-empty ⇒ `Run`'s caller exits nonzero.
- [ ] **Step 2: Verify fail.**
- [ ] **Step 3: Implement** — `AllVersionRefs` (a `SELECT` join over stacks+stack_versions); `BareGit.ListRepositories` safely walks exactly `profiles/<owner>/<name>.git` and validates segments; `ListTags` uses `git tag -l`; `audit.Run` cross-checks each metadata ref via `TagCommit` (missing → MissingContent), then inventories every content repo/tag (including repos absent from metadata) and diffs against known versions. `cmd/registry audit` opens config/store/content, runs, prints a deterministic summary, exits 1 if `MissingContent` non-empty.
- [ ] **Step 4: Verify** — `go test ./internal/registry/audit/ ./...` → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: registry consistency-audit (tag vs metadata) command"`

---

### Task 5: Off-site DR export command

**Files:**
- Create: `internal/registry/export/export.go`, `internal/registry/export/export_test.go`, `cmd/registry/export.go`
- Modify: `cmd/registry/main.go` (subcommand dispatch)

**Interfaces:**
- Consumes: `BareGit.ListRepositories`, `DATABASE_URL`, system `pg_dump`, and optional external collector settings `SHERPA_EXPORT_URL`, `SHERPA_EXPORT_TOKEN`, `SHERPA_EXPORT_INTERVAL`.
- Produces: `export.Run(ctx, contentDir, dbURL, archivePath string) error` creates one atomic `.tar.gz`: `postgres.dump` first, then collision-free `repos/<owner>/<name>.bundle` files, then `manifest.json` with SHA-256/size for every artifact. `export.Upload(ctx, archivePath, collectorURL, token)` POSTs the completed archive to an HTTPS collector. `export.StartScheduler` runs in the volume-owning API process when configured; `registry export <archivePath>` remains the manual/on-demand wrapper.

- [ ] **Step 1: Write failing tests** — seed two repos + temp Postgres; the runner records that `pg_dump` completes before the first bundle command; unpacked archive has cloneable bundles under collision-free owner/name paths, non-empty dump, and a final manifest whose hashes/sizes verify. A failed command leaves no final archive. `Upload` sends the complete gzip bytes to `httptest.Server`, sets only the configured Bearer, rejects non-HTTPS collectors except loopback tests, and never places DB/export credentials in command args or errors. Scheduler cancellation stops cleanly and an upload failure is logged without its URL query/token.
- [ ] **Step 2: Verify fail.**
- [ ] **Step 3: Implement** — create a temp workspace beside the requested archive; invoke `pg_dump` **first** with the DSN in `PGDATABASE` environment (never argv/logs); enumerate validated repos and create `--all` bundles; hash artifacts and write `manifest.json` last; tar/gzip the workspace to a temp archive, fsync/close, then rename to the final path. On any error remove temps. Upload via Go `net/http` POST, not shell. Scheduler uses a context-aware ticker, writes temp archives outside the content tree, uploads, then deletes local archives only after a 2xx response. Empty export URL/interval disables scheduling; partial configuration fails startup.
- [ ] **Step 4: Verify** — `go test ./internal/registry/export/ ./...` → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: off-site DR export (git bundle + pg_dump) command"`

---

### Task 6: Docker image, .dockerignore, entrypoint, railway.json

**Files:**
- Create: `Dockerfile`, `.dockerignore`, `deploy/entrypoint.sh`, `railway.json`
- Test: `deploy/smoke_build.sh` (a build+run validation script the controller runs; not a Go unit test)

**Interfaces:** Produces a runnable production image. Validation, not TDD (infra artifact).

- [ ] **Step 1: Write the Dockerfile** — multi-stage:

```dockerfile
# build
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOFLAGS=-trimpath go build -o /out/registry ./cmd/registry
# runtime
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates postgresql-client util-linux \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --system --create-home --shell /usr/sbin/nologin sherpa
COPY --from=build /out/registry /usr/local/bin/registry
COPY deploy/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
```

`deploy/entrypoint.sh` (runs as root only to prep the volume, then drops to `sherpa`):

```bash
#!/usr/bin/env bash
set -euo pipefail
DIR="${SHERPA_CONTENT_DIR:-/data/git}"
mkdir -p "$DIR"
chown sherpa:sherpa "$(dirname "$DIR")" "$DIR"
# abandoned-stage cleanup also runs in-process at startup; this is belt-and-suspenders.
exec setpriv --reuid=sherpa --regid=sherpa --init-groups /usr/local/bin/registry "$@"
```

`.dockerignore`: `.git`, `dist/`, `docs/`, `spike/`, `*_test.go`? (keep tests out of the image build context except what `go build` needs — simplest: ignore `.git dist docs spike .superpowers registry-content`). `railway.json`:

```json
{
  "$schema": "https://railway.com/railway.schema.json",
  "build": { "builder": "DOCKERFILE", "dockerfilePath": "Dockerfile" },
  "deploy": { "healthcheckPath": "/healthz", "healthcheckTimeout": 120, "restartPolicyType": "ON_FAILURE", "restartPolicyMaxRetries": 3, "numReplicas": 1 }
}
```

- [ ] **Step 2: Validation script `deploy/smoke_build.sh`** — `docker build -t sherpa-registry:test .`; run it with a throwaway Postgres (reuse the pg-docker recipe) + a temp volume dir mounted at `/data`, `SHERPA_GITHUB_CLIENT_ID=x`, `SHERPA_PUBLIC_BASE_URL=https://registry.example`, no admin token; assert: container starts, `curl /healthz` → 200, `/proc/1/status` shows the registry PID 1 UID equals the `sherpa` UID (do not use plain `docker exec id`, which runs as the image's root config user), `/data/git` is writable by that process, `git` and `pg_dump` execute, and `docker history` / image inspect shows no Go toolchain/source/secrets in the final image.
- [ ] **Step 3: Controller runs** `bash deploy/smoke_build.sh` (Docker is up) → all checks pass. (Codex cannot run Docker in-sandbox; the controller executes this validation.)
- [ ] **Step 4: Commit** — `git add -A && git commit -m "feat: production Dockerfile + entrypoint (non-root) + railway.json + build smoke check"`

---

### Task 7: Deployment runbook + staging gate + verify registry-scoped CLI session

**Files:**
- Create: `docs/deployment/railway.md`
- Verify existing 2c-ii issuer scoping: `internal/cli/cmd_auth.go`, `internal/cli/cmd_auth_test.go`, `internal/cli/cmd_publish.go`

**Interfaces:** Produces the operator runbook. Registry-scoped sessions are already implemented on `main` (`registry_url`, normalized issuer comparison, legacy fail-closed); this task verifies and documents that prerequisite rather than reimplementing it.

- [ ] **Step 1 (session prerequisite):** run the existing tests proving a `registry_url=https://staging` session is never sent to `https://prod`, matching normalized issuers work, env admin-token precedence remains, and legacy issuer-less sessions fail closed. Add only missing message/coverage; do not rename the existing JSON field or duplicate the implementation.
- [ ] **Step 2:** `go test ./internal/cli/ ./...` green.
- [ ] **Step 3: Write `docs/deployment/railway.md`** — a complete operator runbook covering: the Railway settings NOT in `railway.json` (create the Postgres service + reference `DATABASE_URL`; attach one volume, mount `/data`, set `SHERPA_CONTENT_DIR=/data/git`; set required `SHERPA_PUBLIC_BASE_URL`, `SHERPA_GITHUB_CLIENT_ID`, optional `SHERPA_TRUST_PROXY=true`, optional admin token; region + domain; disable sleep; set CPU/mem/volume/spend limits + alerts; enable Postgres + volume backups); configure the in-process export scheduler with an **external** HTTPS collector, token, interval, retention and a quarterly restore drill; the **staging acceptance gate** as a numbered checklist copied from spec §10.2 (login → own-login publish `linked` → wrong-owner 403 → auth rate-limit/body tests → search+clone over HTTPS → redeploy → search+clone → force export/upload → restore archive into recovery target + `registry audit` passes → logs contain no secrets); the **restore runbook** from spec §8 (stop writer → restore Postgres+git into a sibling/recovery target → `registry audit` → block cutover on missing content → smoke checks → switch domain); and the note that §3 Railway facts must be re-verified before provisioning.
- [ ] **Step 4: Verify** — `go test ./...` green; the runbook covers every §10.2 gate step and every §8 restore step (self-check the doc against those sections).
- [ ] **Step 5: Commit** — `git add -A && git commit -m "docs: Railway deployment runbook + staging gate; feat: registry-scoped CLI session"`

---

## Execution order & dependencies

Prerequisite: **2c-ii merged** (satisfied on `main`). Then 1 → 1B → 2 → 3 → 4 → 5 → 6 → 7. Tasks 1–5 are testable Go (whole-suite-green gate, Postgres via docker); Task 6 is an infra artifact validated by the controller's real `docker build`+run; Task 7 verifies the already-implemented session prerequisite and adds the runbook. The Railway staging gate itself (spec §10.2, §11) is executed by the operator against a live deployment **after** merge — it is the acceptance criterion, not an automated task.

## Deliverables checklist (spec §4-11)

- [ ] Hardened `http.Server` (timeouts covering the full publish handler) + `/healthz` ready-gate + graceful shutdown (Task 1, spec §5.2).
- [ ] Bounded/rate-limited public device auth, bounded GitHub client, and secret-safe request logging (Task 1B, spec §7).
- [ ] Pinned `SHERPA_PUBLIC_BASE_URL` clone/issuer host; `X-Forwarded-Host` never trusted (Task 2, spec §5.3).
- [ ] Startup content-dir prep + abandoned-stage cleanup (Task 3, spec §4.2/§5.1).
- [ ] Consistency-audit command (missing-content blocks cutover; extra tags separate) (Task 4, spec §8/§10).
- [ ] Database-first atomic off-site DR archive (`pg_dump` + Git bundles + manifest), scheduled upload from the volume-owning API process to an external HTTPS collector (Task 5, spec §8).
- [ ] Non-root multi-stage Docker image + entrypoint + `.dockerignore` + `railway.json` + build smoke check (Task 6, spec §4.2/§9).
- [ ] Deployment runbook + staging-gate + restore checklist; existing registry-scoped CLI session reverified (Task 7, spec §6/§8/§10/§11).
- [ ] Config: `SHERPA_PUBLIC_BASE_URL`, `SHERPA_TRUST_PROXY`, `SHERPA_DB_MAX_CONNS`; no secret logged (Tasks 1,2, spec §6).
- [ ] All invariants unchanged; `go test ./...`/`go vet` green throughout.
