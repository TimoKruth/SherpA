# SherpA Phase 2 - Sub-project 2c-iii - Railway Deployment

**Status:** Design for approval
**Date:** 2026-07-12
**Parent specs:** `2026-07-11-phase2-2c-registry-design.md` (2c decomposition),
`2026-07-12-phase2-2c-ii-auth-identity-design.md` (authenticated registry)

## 1. Goal

Deploy the authenticated SherpA registry as a production-shaped Railway service. The
deployment must preserve the registry's two durable stores (Postgres metadata and bare Git
content), expose correct HTTPS clone URLs, run migrations safely, survive restart/redeploy,
and provide a documented backup and recovery path without weakening the 2c-i/2c-ii publish
and identity invariants.

2c-iii is complete when a staging deployment proves this loop against the real public URL:

```text
sherpa login -> own-login publish -> search -> clone -> redeploy -> search -> clone
```

The version before and after redeploy must be the same `linked` version, and a wrong-owner
publish must still fail before bundle staging.

## 2. Scope

**In:**

1. A production Docker image containing `cmd/registry`, Git, and CA certificates.
2. Railway config-as-code for build, start, health gate, and restart behavior.
3. A Railway Postgres service reached through private `DATABASE_URL`.
4. One Railway volume mounted into the registry service for bare Git repositories.
5. GitHub OAuth App client-ID configuration for the device flow.
6. Readiness endpoint, bounded HTTP server behavior, graceful shutdown, and startup cleanup.
7. Proxy-aware public repository URLs (`https`, original host).
8. Staging and production setup/runbook, backup schedules, restore drill, and smoke checks.
9. Deployment-boundary abuse controls for the public device-flow endpoints.

**Out:**

- Horizontal or multi-region registry replicas while content uses a Railway volume.
- Forgejo, object storage, or another content-store migration.
- Async re-scan workers (still deferred from 2c-ii).
- Website, follows/notifications, MCP server, and trial journal (2d-2f).
- Automated `verified` trust-tier blessing or GitHub-organization publishing.
- A general observability platform, CDN, or WAF. The spec identifies their trigger points.

## 3. Railway Constraints Verified 2026-07-12

The design depends on these current platform behaviors:

- A service can attach only one Railway volume, replicas cannot be used with volumes, and a
  volume-backed redeploy has brief downtime because two deployments cannot mount the same
  volume simultaneously. See [Railway volume reference](https://docs.railway.com/volumes/reference).
- Volumes mount only at runtime, not during image build or a pre-deploy command. Filesystem
  initialization and stale-stage cleanup therefore belong in the start path. See
  [Using volumes](https://docs.railway.com/volumes).
- Pre-deploy commands can use variables and private networking but have no volume. They are
  unsuitable for any migration spanning Postgres and bare Git. See
  [Pre-deploy commands](https://docs.railway.com/deployments/pre-deploy-command).
- A Railway healthcheck gates deployment activation but is not continuous monitoring. See
  [Healthchecks](https://docs.railway.com/deployments/healthchecks).
- Services in one project/environment communicate over private `railway.internal` networking;
  the database must not need a public TCP endpoint. See
  [Private networking](https://docs.railway.com/private-networking).
- Public requests include `X-Forwarded-Proto`, `X-Forwarded-Host`, and
  `X-Railway-Request-Id`; request duration is capped at 15 minutes. Railway provides network
  protection but not an application WAF. See
  [Public-networking limits](https://docs.railway.com/networking/public-networking/specs-and-limits).
- Volume backups can be scheduled and restored only inside the same project/environment.
  Wiping a volume also deletes its backups. See
  [Volume backups](https://docs.railway.com/volumes/backups).

These are deployment inputs, not assumptions hidden in code. The implementation plan must
re-check them before provisioning production state.

## 4. Architecture

```text
                         Railway public edge (TLS)
                                   |
                                   v
                     registry service - one replica
                     Go API + system Git, non-root
                         |                 |
             /data/git volume             +--> GitHub device-flow endpoints
                         |
                         +---- private DATABASE_URL ----> Railway Postgres
```

### 4.1 Service topology

- **Registry API:** one public Railway service built from this repository's Dockerfile.
- **Postgres:** one Railway Postgres service in the same project and environment. The API
  receives its private `DATABASE_URL` through a Railway reference variable.
- **Git content:** one volume attached to the API at `/data`; the application uses
  `SHERPA_CONTENT_DIR=/data/git`.
- **Region:** API, Postgres, and volume are colocated. Production region is an approval-time
  decision; do not create durable production state until it is settled.

One API replica is a deliberate 2c-iii limit. Scaling requires moving `ContentStore` to
shared/network storage or a Git service, not mounting the same local filesystem broadly.

### 4.2 Image and runtime user

Use a multi-stage Docker build:

1. Build `cmd/registry` with the repository's pinned Go toolchain/dependencies.
2. Copy the binary into a small Linux runtime image that also installs system Git and CA
   certificates.
3. Create a dedicated `sherpa` runtime user.
4. A minimal root entrypoint creates/chowns the mounted directory, removes abandoned
   `.stage-*` directories, then drops privileges before executing the registry.

Git bundle parsing, checkout, and scanning must not run as root. No secret or source tree is
baked into the final image.

## 5. Runtime Lifecycle

### 5.1 Startup and migrations

`OpenPostgres` remains the migration owner. Startup order is:

1. Validate configuration without printing secret values.
2. Prepare the mounted content directory and clean abandoned staging directories.
3. Connect to Postgres with bounded retry/backoff for initial service ordering.
4. Run the existing idempotent transactional migrations.
5. Construct content/auth/API dependencies.
6. Start listening on Railway's injected `PORT`.

The process must not report ready or accept traffic before database connection and migrations
succeed. A failed migration exits nonzero; Railway keeps the deployment unhealthy.

Migrations do not run as a Railway pre-deploy command in 2c-iii. Startup already owns them,
and the pre-deploy container cannot inspect the Git volume for cross-store checks.

### 5.2 Health and shutdown

Add `GET /healthz`. It returns `200` only after startup initialization has completed. It does
not call GitHub. A shallow database readiness query is optional; if used, it must be tightly
bounded and must not turn a transient database delay into unbounded request buildup.

Replace bare `http.ListenAndServe` with `http.Server` configured with at least:

- `ReadHeaderTimeout`
- `ReadTimeout` suitable for a 50 MiB multipart publish
- `WriteTimeout` below Railway's 15-minute request ceiling
- `IdleTimeout`
- bounded graceful shutdown on `SIGTERM`/`SIGINT`

Shutdown stops new requests, lets in-flight publishes finish within the grace period, then
closes the Postgres pool. A killed publish may leave staged files or a scanned orphan tag;
startup cleanup and the 2c-ii orphan reconciliation path handle those states.

### 5.3 Public URL generation

Railway terminates TLS before the Go process. `repoURL` must use trusted proxy headers in the
Railway deployment:

1. Prefer `X-Forwarded-Proto=https` over `r.TLS`.
2. Prefer validated `X-Forwarded-Host` over the internal request host.
3. Fall back to the current direct-server behavior for local tests.

Only honor forwarded headers when deployment configuration explicitly enables trusted-proxy
mode; arbitrary direct clients must not be able to forge canonical clone URLs.

## 6. Configuration and Secrets

Required runtime variables:

```text
DATABASE_URL=${{Postgres.DATABASE_URL}}
SHERPA_CONTENT_DIR=/data/git
SHERPA_GITHUB_CLIENT_ID=<GitHub OAuth App client id>
```

Optional variables:

```text
SHERPA_REGISTRY_TOKEN=<admin/CI escape hatch; absent in normal production use>
SHERPA_TRUST_PROXY=true
PORT=<injected by Railway>
```

Rules:

- `DATABASE_URL`, admin tokens, session tokens, and GitHub tokens never appear in
  `railway.json`, Docker layers, command lines, health responses, or logs.
- `SHERPA_REGISTRY_TOKEN` remains optional per the 2c-ii Fable amendment. Empty means no
  admin bypass.
- Production requires a registered GitHub OAuth App with device flow enabled. Only its
  client ID is required by the public-client device flow.
- Staging and production use different Railway environments, databases, volumes, admin
  tokens, GitHub OAuth Apps, and public domains.

## 7. Public Endpoint Hardening

Deployment makes the previously local auth surface internet-facing. Before production:

1. Bound JSON bodies on device start/poll requests.
2. Enforce per-IP start limits and per-device poll intervals, including GitHub `slow_down`.
3. Bound outbound GitHub HTTP timeouts and propagate request cancellation.
4. Keep the existing 50 MiB multipart limit and fail-closed scan ordering.
5. Add structured request logging keyed by `X-Railway-Request-Id`, excluding authorization
   headers, request bodies, GitHub tokens, and SherpA session tokens.
6. Set resource/usage limits and alerts in Railway before public beta.

Rate limiting may be in-process while the service is single-replica. A future multi-replica
deployment requires a shared limiter. Cloudflare/WAF is not required for the first staging
deployment; it becomes required if public abuse exceeds application controls or before a
broad unauthenticated launch.

## 8. Persistence, Backup, and Recovery

Postgres and bare Git are separate durable stores with no atomic cross-store snapshot. Normal
publish ordering remains content first, metadata second, so metadata must never reference
missing content during ordinary operation.

Configure automated backups for both the Postgres volume and the API Git volume, plus a
manual backup before migrations or recovery drills. For production, enable Postgres PITR if
the selected Railway plan supports it; volume snapshots remain the Git recovery mechanism.

Restore runbook:

1. Stop or isolate the registry writer.
2. Restore Postgres and Git into staging/sibling services, never directly over the only
   production copy.
3. Run a consistency audit for every `stack_versions.git_tag` against the restored bare repo.
4. A metadata row with missing content blocks cutover.
5. Extra Git tags are harmless orphans; scan and reconcile/GC them before reopening writes.
6. Run login, search, detail, and clone smoke checks.
7. Switch environment references/domain only after audit success.

The implementation plan must add either a small audit command or an equivalent tested
runbook script. Manual visual inspection is insufficient.

## 9. Deployment Artifacts

Expected implementation files:

- Create: `Dockerfile`
- Create: `.dockerignore`
- Create: `railway.json`
- Create: `deploy/entrypoint.sh`
- Create: `docs/deployment/railway.md`
- Modify: `cmd/registry/main.go`, `cmd/registry/main_test.go`
- Modify: `internal/registry/config.go`
- Modify: `internal/registry/api/router.go`, `internal/registry/api/read.go` and tests
- Modify: `internal/registry/content/baregit.go` and tests for startup cleanup/audit helpers
- Optionally create: `cmd/registry-audit` if a separate read-only audit binary is cleaner
  than a registry subcommand.

`railway.json` selects the Dockerfile builder, `/healthz`, an appropriate healthcheck timeout,
and an on-failure/always restart policy supported by the selected plan. Volume provisioning,
mount path, domain, region, reference variables, backup schedules, and usage alerts remain
documented Railway project settings because they are not all represented by one service's
config-as-code file.

## 10. Testing and Acceptance

### 10.1 Automated

- Image builds from a clean checkout; final image contains the registry binary, Git, and CA
  roots, and excludes Go build tools/source.
- Container runs Git/scanner work as the unprivileged `sherpa` user and can write `/data/git`.
- `/healthz` returns 200 only for an initialized server.
- `SIGTERM` stops the server cleanly within the configured grace period.
- Trusted proxy mode produces an `https://` clone URL with the original public host; disabled
  mode ignores spoofed forwarded headers.
- Startup cleanup removes only abandoned staging directories, never published repositories.
- Database migrations remain idempotent on an empty and already-migrated database.
- Device endpoint size/rate limits reject abuse without calling GitHub.
- Consistency audit detects missing repos/tags and reports harmless extra tags separately.
- Existing complete `go test ./...` and `go vet ./...` remain green.

### 10.2 Railway staging gate

1. Deploy empty staging Postgres + empty Git volume.
2. Confirm health gate and HTTPS search endpoint.
3. Complete real GitHub device login.
4. Publish under the authenticated login; confirm `trust_tier=linked`.
5. Confirm wrong-owner publish is 403 and creates no tag/version.
6. Search and clone the published stack over the public HTTPS URL.
7. Redeploy the API; confirm the same version remains searchable and cloneable.
8. Restore both backups into a staging recovery target and pass the consistency audit.
9. Confirm logs contain no admin, GitHub, or session token and errors expose no internals.

Production provisioning occurs only after this gate is recorded in the deployment runbook.

## 11. Rollout and Operations

- Deploy to staging first from a pinned commit.
- Keep production at one replica and serverless/sleep disabled.
- Set CPU, memory, volume, and spend limits with alerts before exposing the domain.
- Monitor availability externally because Railway's deployment healthcheck is not continuous.
- Alert on 5xx rate, failed GitHub calls, publish rejection classes, database saturation, and
  volume capacity. Never use token values as log fields or metric labels.
- Rollback application images normally when migrations are backward compatible. A migration
  that is not backward compatible requires an explicit expand/migrate/contract sequence in
  its future plan.

## 12. Alternatives Considered

- **Railway pre-deploy migrations.** Rejected for 2c-iii: startup migrations already exist,
  and pre-deploy has no Git volume for consistency checks.
- **Two or more API replicas now.** Rejected: Railway does not support replicas with an
  attached volume, and local bare-Git writes are not a distributed store.
- **Forgejo now.** Still deferred. It adds a service/admin surface without being necessary
  for the first hosted registry.
- **Object storage now.** Rejected until scale or availability justifies changing the proven
  Git content model.
- **Public Postgres URL.** Rejected: private networking is available and avoids unnecessary
  public exposure/egress.
- **Health endpoint that calls GitHub.** Rejected: an upstream GitHub incident must not make
  healthy read/clone service fail deployment readiness.

## 13. Open Questions for Approval

1. **Region:** which Railway region satisfies initial users and data-residency expectations?
2. **Canonical domain:** use a Railway-generated domain for beta or establish the final
   custom registry domain before clients store issuer-bound sessions?
3. **Recovery objective:** what RPO/RTO should determine backup frequency and Postgres PITR
   retention?
4. **Audit packaging:** a dedicated `registry-audit` binary or a `registry audit` subcommand?
5. **WAF trigger:** ship application rate limits alone for closed beta, or place Cloudflare in
   front before the first external user?

No production state should be provisioned until questions 1-3 are settled. They determine
state placement, client session issuer, and the backup configuration.
