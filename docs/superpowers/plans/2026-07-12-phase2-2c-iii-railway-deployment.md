# Phase 2 · 2c-iii — Railway Deployment Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship the authenticated registry as a production-shaped Railway service — hardened HTTP server + `/healthz` + graceful shutdown, pinned public clone/issuer URL, startup staging-cleanup, a consistency-audit and off-site-export command, a non-root Docker image + `railway.json` + entrypoint, and a staging runbook/gate — without weakening any 2c-i/2c-ii invariant.

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
- Produces: `Config` gains `PublicBaseURL, TrustProxy bool, DBMaxConns int`; `run(ctx, cfg)` builds an `*http.Server` with timeouts and returns it + a shutdown func; `GET /healthz` handler returning 200 once `ready` is set.

- [ ] **Step 1: Write failing tests** — (a) `LoadConfig` reads `SHERPA_PUBLIC_BASE_URL`, `SHERPA_TRUST_PROXY` (parsed bool), `SHERPA_DB_MAX_CONNS` (int, default 0=pool default); `DATABASE_URL` still required, admin token still optional (2c-ii). (b) `/healthz` returns 503 before init and 200 after (drive via a `ready *atomic.Bool` the handler reads; a test flips it). (c) the server is an `http.Server` with non-zero `ReadHeaderTimeout`/`ReadTimeout`/`WriteTimeout`/`IdleTimeout`.

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
- [ ] **Step 3: Implement** — config fields + parsing (`strconv.ParseBool`/`Atoi`, tolerant of empty); pass `DBMaxConns` into `OpenPostgres` (extend it to set `pgxpool.Config.MaxConns` when >0). In `run`: create `var ready atomic.Bool`; mount `/healthz` via `healthHandler(&ready)` on the mux (add to `api.New` or wrap the handler in `run`); build `srv := &http.Server{Addr: ":"+port, Handler: h, ReadHeaderTimeout: 5*time.Second, ReadTimeout: 2*time.Minute, WriteTimeout: 5*time.Minute, IdleTimeout: 2*time.Minute}`; after migrations succeed set `ready.Store(true)`; `main` runs `srv.ListenAndServe()` in a goroutine and on `signal.NotifyContext(SIGINT,SIGTERM)` calls `srv.Shutdown(ctxWithTimeout)` then the store cleanup. Health handler: `if !ready.Load() { 503 } else { 200 "ok" }`, never touches GitHub or (for the 200 path) the DB.
- [ ] **Step 4: Verify** — `go test ./...` → PASS; gofmt/vet clean.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: registry hardened http.Server + /healthz gate + graceful shutdown + config"`

---

### Task 2: Pinned public clone/issuer URL (host-header hardening)

**Files:**
- Modify: `internal/registry/api/read.go` (`repoURL`), `internal/registry/api/router.go` or a small `publicurl.go`
- Test: `internal/registry/api/read_test.go`

**Interfaces:**
- Consumes: `Config.PublicBaseURL`, `Config.TrustProxy` (thread into `api.New` as a `PublicURL` value or pass the two fields). Extend `api.New(store, content, adminToken, github, publicBaseURL string, trustProxy bool) http.Handler` (update all call sites incl. `run` and tests).
- Produces: `repoURL(r, owner, name)` = `PublicBaseURL + "/v1/stacks/owner/name.git"` when `PublicBaseURL != ""`; else scheme from `X-Forwarded-Proto` (only if `trustProxy`) or `r`, host from `r.Host` (never `X-Forwarded-Host`).

- [ ] **Step 1: Write failing tests** — (a) with `PublicBaseURL="https://registry.example"`, `repo_url` is `https://registry.example/v1/stacks/o/n.git` **regardless of** a spoofed `X-Forwarded-Host: evil.com` / `Host: evil.com` on the request. (b) with no base URL + `trustProxy=true` + `X-Forwarded-Proto: https`, scheme is https and host is `r.Host` (NOT the forwarded host). (c) no base URL + `trustProxy=false`: ignores `X-Forwarded-Proto`, uses direct scheme/host (local-test fallback unchanged).
- [ ] **Step 2: Verify fail.**
- [ ] **Step 3: Implement** — pin from `PublicBaseURL`; only consult `X-Forwarded-Proto` (scheme) under `trustProxy`; **never read `X-Forwarded-Host`**. Trim a trailing slash on `PublicBaseURL`. Wire the two config values through `api.New` and `run`.
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
- Consumes: `store.Store` (a method to list all `(owner,name,version,git_tag)` — add `AllVersionRefs(ctx) ([]VersionRef, error)` to Store) and `content.ContentStore.TagCommit` (from 2c-ii Task 5).
- Produces: `audit.Run(ctx, st store.Store, cs content.ContentStore) (Report, error)` with `Report{MissingContent []VersionRef; ExtraTags []string}`; `MissingContent` non-empty → nonzero exit (blocks cutover). Also add `store.AllVersionRefs`.

- [ ] **Step 1: Write failing test** (real Postgres via `StartPostgres`) — seed two versions; delete one version's tag from the bare repo (or delete the repo) → `Run` reports it in `MissingContent`; add an extra tag in a repo with no version row → `ExtraTags`; a consistent store → empty report. `MissingContent` non-empty ⇒ `Run`'s caller exits nonzero.
- [ ] **Step 2: Verify fail.**
- [ ] **Step 3: Implement** — `AllVersionRefs` (a `SELECT` join over stacks+stack_versions); `audit.Run` cross-checks each ref's `git_tag` via `ContentStore.TagCommit` (missing → MissingContent); to find ExtraTags, list tags per repo (`git tag -l`) and diff against known versions. `cmd/registry audit` opens config/store/content, runs, prints a summary, exits 1 if `MissingContent` non-empty.
- [ ] **Step 4: Verify** — `go test ./internal/registry/audit/ ./...` → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: registry consistency-audit (tag vs metadata) command"`

---

### Task 5: Off-site DR export command

**Files:**
- Create: `internal/registry/export/export.go`, `internal/registry/export/export_test.go`, `cmd/registry/export.go`
- Modify: `cmd/registry/main.go` (subcommand dispatch)

**Interfaces:**
- Consumes: `content.ContentStore` (repo paths / a way to enumerate repos), `DATABASE_URL` (shell out to `pg_dump`).
- Produces: `export.Run(ctx, contentDir, dbURL, outDir string) error` — for each `<contentDir>/profiles/<owner>/<name>.git`, write `<outDir>/<owner>__<name>.bundle` via `git -C <repo> bundle create <out> --all`; write `<outDir>/postgres.dump` via `pg_dump`. A read-only operation (never mutates content). `registry export <outDir>` wraps it. (Shipping `outDir` to storage outside Railway is ops config — documented in the runbook, Task 7.)

- [ ] **Step 1: Write failing test** — seed a content dir with two published repos + a temp Postgres; `Run(ctx, contentDir, dsn, outDir)` produces a `.bundle` per repo that is **cloneable** (`git clone <bundle>` yields the tree) and a non-empty `postgres.dump`. (If `pg_dump` isn't on PATH in the test env, `t.Skip` that assertion but still assert the bundles; the controller confirms `pg_dump` availability — it ships in the postgres docker image, and for the host test use `docker exec` or skip-with-note.)
- [ ] **Step 2: Verify fail.**
- [ ] **Step 3: Implement** — enumerate repos under `profiles/`, `git bundle create --all` each; `pg_dump` the DSN to `postgres.dump`. Fail closed (any bundle/dump error → error; a partial export must be obvious). Never write into the content dir.
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
RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates \
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
chown -R sherpa:sherpa "$(dirname "$DIR")" || true
# abandoned-stage cleanup also runs in-process at startup; this is belt-and-suspenders.
exec runuser -u sherpa -- /usr/local/bin/registry "$@"
```

`.dockerignore`: `.git`, `dist/`, `docs/`, `spike/`, `*_test.go`? (keep tests out of the image build context except what `go build` needs — simplest: ignore `.git dist docs spike .superpowers registry-content`). `railway.json`:

```json
{
  "$schema": "https://railway.com/railway.schema.json",
  "build": { "builder": "DOCKERFILE", "dockerfilePath": "Dockerfile" },
  "deploy": { "healthcheckPath": "/healthz", "healthcheckTimeout": 120, "restartPolicyType": "ON_FAILURE", "restartPolicyMaxRetries": 3, "numReplicas": 1 }
}
```

- [ ] **Step 2: Validation script `deploy/smoke_build.sh`** — `docker build -t sherpa-registry:test .`; run it with a throwaway Postgres (reuse the pg-docker recipe) + a temp volume dir mounted at `/data`, `SHERPA_GITHUB_CLIENT_ID=x`, no admin token; assert: container starts, `curl /healthz` → 200, `docker exec ... id -un` inside the registry process is `sherpa` (non-root), `/data/git` is writable, and `docker history` / image inspect shows no Go toolchain in the final layer.
- [ ] **Step 3: Controller runs** `bash deploy/smoke_build.sh` (Docker is up) → all checks pass. (Codex cannot run Docker in-sandbox; the controller executes this validation.)
- [ ] **Step 4: Commit** — `git add -A && git commit -m "feat: production Dockerfile + entrypoint (non-root) + railway.json + build smoke check"`

---

### Task 7: Deployment runbook + staging gate + registry-scoped CLI session

**Files:**
- Create: `docs/deployment/railway.md`
- Modify (2c-ii session touch-up): `internal/cli/registryclient.go` / the session-file code + its test
- Test: `internal/cli/*_test.go` for the session-scope change

**Interfaces:** Produces the operator runbook + a registry-scoped session so a staging login can't publish to prod.

- [ ] **Step 1 (session scope, TDD):** the 2c-ii session file gains a `registry` field (the base URL it was minted for). `publish` uses a stored session **only if** its `registry` matches the active `SHERPA_REGISTRY_URL`/`SHERPA_PUBLIC_BASE_URL`; otherwise it does not send the session token (and prints "run `sherpa login` for <registry>"). Test: a session file with `registry=https://staging` is NOT used when publishing to `https://prod` (no Bearer sent / clear error), and IS used for the matching registry.
- [ ] **Step 2:** Verify the session test fails, implement, verify pass, `go test ./...` green.
- [ ] **Step 3: Write `docs/deployment/railway.md`** — a complete operator runbook covering: the Railway settings NOT in `railway.json` (create the Postgres service + reference `DATABASE_URL`; attach one volume, mount `/data`, set `SHERPA_CONTENT_DIR=/data/git`; set `SHERPA_PUBLIC_BASE_URL`, `SHERPA_GITHUB_CLIENT_ID`, optional `SHERPA_TRUST_PROXY=true`, optional admin token; region + domain; disable sleep; set CPU/mem/volume/spend limits + alerts; enable Postgres + volume backups; schedule the off-site `registry export`); the **staging acceptance gate** as a numbered checklist copied from spec §10.2 (login → own-login publish `linked` → wrong-owner 403 → search+clone over HTTPS → redeploy → search+clone → restore-into-recovery-target + `registry audit` passes → logs contain no secrets); the **restore runbook** from spec §8 (stop writer → restore Postgres+git into a sibling/recovery target → `registry audit` → block cutover on missing content → smoke checks → switch domain); and the note that §3 Railway facts must be re-verified before provisioning.
- [ ] **Step 4: Verify** — `go test ./...` green; the runbook covers every §10.2 gate step and every §8 restore step (self-check the doc against those sections).
- [ ] **Step 5: Commit** — `git add -A && git commit -m "docs: Railway deployment runbook + staging gate; feat: registry-scoped CLI session"`

---

## Execution order & dependencies

Prerequisite: **2c-ii merged.** Then 1 → 2 → 3 → 4 → 5 → 6 → 7. Tasks 1–5 + 7's session change are testable Go (whole-suite-green gate, Postgres via docker); Task 6 is an infra artifact validated by the controller's `docker build`+run; Task 7's runbook is a doc validated against spec §8/§10.2. The Railway staging gate itself (spec §10.2, §11) is executed by the operator against a live deployment **after** merge — it is the acceptance criterion, not an automated task.

## Deliverables checklist (spec §4-11)

- [ ] Hardened `http.Server` (timeouts covering the full publish handler) + `/healthz` ready-gate + graceful shutdown (Task 1, spec §5.2).
- [ ] Pinned `SHERPA_PUBLIC_BASE_URL` clone/issuer host; `X-Forwarded-Host` never trusted (Task 2, spec §5.3).
- [ ] Startup content-dir prep + abandoned-stage cleanup (Task 3, spec §4.2/§5.1).
- [ ] Consistency-audit command (missing-content blocks cutover; extra tags separate) (Task 4, spec §8/§10).
- [ ] Off-site DR export (`git bundle` + `pg_dump`) (Task 5, spec §8).
- [ ] Non-root multi-stage Docker image + entrypoint + `.dockerignore` + `railway.json` + build smoke check (Task 6, spec §4.2/§9).
- [ ] Deployment runbook + staging-gate + restore checklist; registry-scoped CLI session (Task 7, spec §6/§8/§10/§11).
- [ ] Config: `SHERPA_PUBLIC_BASE_URL`, `SHERPA_TRUST_PROXY`, `SHERPA_DB_MAX_CONNS`; no secret logged (Tasks 1,2, spec §6).
- [ ] All invariants unchanged; `go test ./...`/`go vet` green throughout.
