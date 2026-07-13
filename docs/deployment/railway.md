# Railway Deployment Runbook

This runbook deploys the SherpA registry as one Railway API service, one Railway
Postgres service, and one persistent Git volume. Use separate Railway environments,
databases, volumes, OAuth applications, domains, and secrets for staging and production.

Production is not approved until the staging gate in this document has been recorded for
the exact commit being promoted.

## Platform Constraints

The following Railway behavior was re-verified against the official documentation on
2026-07-12. Re-check it immediately before creating durable production state because it is
platform behavior, not an application guarantee.

- A service can have one attached volume, and replicas cannot be used with a volume. An
  attached volume also introduces brief redeploy downtime because old and new deployments
  cannot mount it simultaneously. See [Volumes](https://docs.railway.com/volumes/reference).
- Railway volume backups can be restored only in the same project and environment, and
  wiping a volume deletes its backups. See [Backups](https://docs.railway.com/volumes/backups).
- Deployment healthchecks gate activation but are not continuous monitoring. See
  [Healthchecks](https://docs.railway.com/deployments/healthchecks).
- Services in the same project and environment have private `railway.internal` networking.
  See [Private networking](https://docs.railway.com/private-networking).
- Public HTTP requests have a 15-minute maximum duration and Railway supplies
  `X-Forwarded-For`, `X-Forwarded-Proto`, and `X-Railway-Request-Id`. See
  [public-networking limits](https://docs.railway.com/networking/public-networking/specs-and-limits).
  Per-IP auth rate limiting keys on the **rightmost** `X-Forwarded-For` hop (the value the
  Railway edge appends); client-prependable headers, including `X-Real-IP` and any left-hand
  `X-Forwarded-For` entries, are not trusted for identity. This assumption is verified at the
  staging gate — see step 6.

Keep the registry at one replica while it owns a local bare-Git volume. Railway snapshots
are the fast in-project recovery tier. The application export sent outside Railway is the
disaster-recovery tier.

## Decisions Before Provisioning

Record these values in the operator change ticket. Do not put secret values in the ticket.

| Input | Required decision |
| --- | --- |
| Region | One region for API, Postgres, and volume |
| Domains | Separate canonical staging and production HTTPS origins |
| Recovery objectives | Approved RPO and RTO for Postgres and Git together |
| Off-site collector | Provider, HTTPS endpoint, encryption, access owner, and restore access |
| Retention | Immutable/off-site retention and deletion policy that satisfies the RPO |
| Railway plan | Postgres limits/PITR availability, volume size, CPU, memory, and spend caps |
| OAuth | Separate GitHub OAuth Apps with device flow enabled |
| Operations | Alert destinations, on-call owner, and quarterly restore-drill owner |
| Admin bypass | Whether production needs a separately rotated admin token at all |

The unresolved production inputs are currently the region, canonical production domain,
RPO/RTO, collector/retention policy, resource limits, and alert ownership.

## Provisioning

1. Create `staging` and `production` environments. Begin with staging only.
2. Create a Railway Postgres service in the selected region, **provisioned at major version
   16** to match the `pg_dump` baked into the runtime image (Dockerfile `postgres:16-bookworm`).
   `pg_dump` refuses a server newer than itself, so a PG17+ server silently breaks every
   scheduled off-site export; if you must run a newer server, bump the image's `postgres` base
   in lockstep. Do not expose its TCP port publicly. Add a reference variable on the registry
   service:

   ```text
   DATABASE_URL=${{Postgres.DATABASE_URL}}
   ```

3. Attach one volume to the registry service at `/data`. Set
   `SHERPA_CONTENT_DIR=/data/git`. Keep `numReplicas=1` from `railway.json`.
4. Configure the public domain and set `SHERPA_PUBLIC_BASE_URL` to its exact HTTPS origin,
   without a query or fragment. This setting is required in staging and production even
   though local development has a fallback. It pins clone URLs and prevents Host-header
   redirection.
5. Select the same region for the registry, Postgres, and volume. Disable serverless sleep.
6. Set CPU, memory, volume, and spend limits and alerts before exposing the domain. Size the
   Postgres connection limit and `SHERPA_DB_MAX_CONNS` together.
7. Enable Railway backup schedules on both the Postgres service volume and the registry Git
   volume. Enable Postgres PITR when the chosen plan supports it. Take manual backups before
   migrations, recovery work, and restore drills.
8. Confirm the Dockerfile builder, `/healthz`, restart policy, and one-replica setting from
   `railway.json`. Do not add a Railway pre-deploy migration command; startup owns the
   additive, idempotent migrations.

The image entrypoint must begin as root so it can prepare the mounted directory, then it
drops to `sherpa` before executing Git or the registry. Do not override the image entrypoint
or runtime user. If Railway runtime UID controls are enabled, retain a root startup UID
(`RAILWAY_RUN_UID=0`) so the entrypoint can prepare the volume.

## Runtime Variables

Configure variables in Railway's secret/variable UI. Never place secret values in
`railway.json`, image build arguments, shell command arguments, tickets, or logs.

| Variable | Requirement |
| --- | --- |
| `DATABASE_URL` | Required private Postgres reference variable |
| `SHERPA_CONTENT_DIR` | Required; `/data/git` |
| `SHERPA_PUBLIC_BASE_URL` | Required; canonical public HTTPS origin |
| `SHERPA_GITHUB_CLIENT_ID` | Required; environment-specific OAuth App with device flow |
| `SHERPA_TRUST_PROXY` | Set `true` on Railway; trusts `X-Forwarded-Proto` for scheme and the rightmost `X-Forwarded-For` hop for per-IP rate limiting, never host |
| `SHERPA_DB_MAX_CONNS` | Optional positive pool cap matched to the Postgres plan |
| `SHERPA_REGISTRY_TOKEN` | Optional admin/CI bypass; omit for normal session-only operation |
| `PORT` | Injected by Railway; do not hard-code it |
| `SHERPA_EXPORT_URL` | Required for DR; external HTTPS collector endpoint |
| `SHERPA_EXPORT_TOKEN` | Required operationally; collector Bearer secret |
| `SHERPA_EXPORT_INTERVAL` | Required with export URL; Go duration such as `24h` |
| `SHERPA_EXPORT_ARCHIVE_DIR` | Optional; use `/tmp/sherpa-exports`, never a child of `/data/git` |

Staging and production values must be independent. Rotate the optional admin and export
tokens through the Railway variable UI and the receiving system. A GitHub client ID is
public; GitHub access tokens and SherpA session tokens remain secret.

## Off-Site Exports

The in-process scheduler runs in the API process because only that service mounts the Git
volume. At each interval it creates `postgres.dump` first, then one `git bundle --all` per
repository, writes the checksum/size manifest last, atomically finishes the archive, and
POSTs it to `SHERPA_EXPORT_URL`. The database-first order means a concurrent publish can add
only harmless extra Git content; dumped metadata cannot refer to content absent from later
bundles.

The collector must:

- accept authenticated HTTPS `POST` requests with `Content-Type: application/gzip`;
- generate a unique object name and return 2xx only after durable storage completes;
- encrypt objects, restrict read/delete access, and live outside the Railway project/account;
- retain immutable copies according to the approved RPO and retention policy;
- alert on missed intervals and support retrieval without the running Railway project.

The scheduler deletes a local archive after a successful upload. A failed archive remains in
`SHERPA_EXPORT_ARCHIVE_DIR` and is retried on each interval; the scheduler does not create a
new archive until that pending archive uploads and is deleted. This bounds local accumulation
to one archive, but a failed upload or missing collector object is still an alert because the
off-site recovery point is not advancing. Keep the archive directory outside `/data/git` and
monitor its capacity. Collector retention is the authoritative retention policy.

For a manual local archive in a container or recovery environment:

```sh
registry export /tmp/sherpa-manual.tar.gz
```

This command packages but does not upload the archive. Do not treat a file left on the
Railway container filesystem as a backup. To force and verify a scheduler upload during the
staging gate, temporarily set `SHERPA_EXPORT_INTERVAL=1m`, redeploy, wait for a newly stored
collector object, then restore the approved interval.

Perform and record an off-site restore drill at least quarterly and after any backup-format
change. A successful upload is not proof of recoverability.

## Operational Commands

Run commands inside the volume-owning registry service/recovery container with its
`DATABASE_URL` and `SHERPA_CONTENT_DIR`; running them on a workstation does not see the
Railway Git volume.

```sh
registry audit
registry export /tmp/sherpa-manual.tar.gz
```

`registry audit` exits `1` when metadata references missing Git content and prints `MISSING`
records. That result blocks cutover. `EXTRA` tags are content without metadata; preserve and
review them as possible interrupted publishes before reconciliation or garbage collection.
Operational/configuration errors exit `2`.

## Staging Acceptance Gate

Record the commit, environment, domain, backup object IDs, timestamps, and operator for every
step. Do not record tokens or device codes.

1. Deploy the pinned commit with an empty staging Postgres database and empty Git volume.
2. Confirm Railway activates only after `GET /healthz` returns `200`, and confirm the public
   HTTPS search endpoint responds successfully.
3. Complete a real GitHub device login against staging. Confirm the session file records the
   staging registry issuer.
4. Publish a stack owned by the authenticated GitHub login. Confirm the response/search data
   reports `trust_tier=linked`.
5. Make a wrong-owner publish attempt under another login. Confirm HTTP `403`, no version
   row, and no new Git tag. Run `registry audit` to support the no-content-change check.
6. Exercise the public auth controls from a staging workstation:
   - Send two device-start requests inside one minute and confirm the second is `429` with
     `Retry-After`; send JSON larger than 4 KiB to device poll and confirm `413`.
   - Start a real device flow, then poll its (registered) device code twice without waiting and
     confirm the second poll is `429`. Poll a random, never-started device code and confirm
     `410` with no GitHub-call error in the logs (unregistered codes must never reach GitHub).
   - **Verify per-IP identity depends on the Railway edge, not a client header.** From two
     genuinely different source IPs, confirm each gets its own device-start allowance (proving
     Railway populates the rightmost `X-Forwarded-For` hop and clients are not all collapsed
     into one global bucket). Then, from a single IP already rate-limited, send device-start
     with a forged `X-Real-IP` and with an extra prepended `X-Forwarded-For` entry; confirm
     **neither** header grants a fresh allowance (no rate-limit bypass). If two distinct clients
     share a bucket, Railway is not appending `X-Forwarded-For` as assumed — stop and reconcile
     before production.
   - Confirm locally rejected requests do not produce GitHub-call errors and request logs
     contain no headers or bodies.
7. Search and view the published stack, then clone its returned `repo_url`. Confirm the URL is
   the configured public HTTPS domain even when a test request supplies a spoofed
   `X-Forwarded-Host`.
8. Redeploy the API. Confirm the same version remains searchable, viewable, and cloneable.
9. Temporarily set the export interval to one minute, redeploy, and confirm a new complete
   archive reaches the external collector. Restore the normal interval afterward.
10. Retrieve that archive into a new sibling/recovery target, restore both Postgres and Git,
    and run `registry audit`. Missing content fails the gate. Complete login, search, detail,
    and HTTPS clone smoke tests against the recovery target.
11. Review application, Railway, and collector logs. Confirm no database URL, admin token,
    export token, GitHub token, session token, authorization header, request body, or internal
    error detail appears.

Production provisioning and domain exposure require all eleven steps to pass.

## Restore and Cutover

Never restore over the only production database or Git volume.

1. Declare the recovery point and stop or isolate the production writer. Remove public write
   access or stop the registry service; record the final accepted-write timestamp.
2. Create a sibling/recovery registry, empty Postgres database, and empty Git volume. Keep its
   domain separate from production. For fast in-project recovery, Railway can restore its
   snapshots only in the same project/environment. For disaster recovery, retrieve the
   selected completed archive from the external collector.
3. Extract the archive in the recovery environment and verify every artifact's size and
   SHA-256 against `manifest.json` before importing it. Reject an incomplete archive or a
   manifest that is not the final completed member.
4. Restore `postgres.dump` into the empty recovery database. Parse the secret Railway
   `DATABASE_URL` into the standard libpq environment variables `PGHOST`, `PGPORT`, `PGUSER`,
   `PGPASSWORD`, `PGDATABASE` (database name only), and `PGSSLMODE`, as the export command
   does. Do not put the full URL or password in process arguments. Then run:

   ```sh
   pg_restore --clean --if-exists --no-owner postgres.dump
   ```

5. For each `repos/<owner>/<name>.bundle`, recreate
   `$SHERPA_CONTENT_DIR/profiles/<owner>/<name>.git` with `git clone --mirror`. Validate owner
   and repository path segments before creating destinations; do not extract or clone through
   symlinked paths.
6. Start the recovery registry on its recovery domain and run `registry audit`. Any `MISSING`
   content blocks cutover. Review `EXTRA` tags as interrupted/orphan publishes; scan and
   reconcile or deliberately retain them before reopening writes.
7. Complete login, search, stack detail, version detail, and clone smoke checks. Perform a
   test publish only if the recovery point may be advanced, then audit again.
8. Switch the production domain/environment references only after the audit and smoke checks
   pass. Re-enable writes, monitor closely, and retain the previous stores untouched until the
   recovery acceptance window closes.

Document the achieved recovery point and elapsed recovery time against the approved RPO/RTO.

## Rollback

Deploy a previously pinned application image through Railway and let `/healthz` gate it. The
current migrations are additive (`CREATE TABLE IF NOT EXISTS` and `ADD COLUMN IF NOT EXISTS`),
so application image rollback is currently schema-compatible and needs no database rollback.
An attached volume can cause brief redeploy downtime.

Do not roll Postgres or Git back merely because an application image failed; that can discard
accepted publishes and split the stores. Use the restore procedure when data rollback is
actually required. Any future non-additive migration requires an explicit
expand/migrate/contract plan and rollback gate before deployment.

## Monitoring and Routine Operations

- Use external uptime monitoring for `/healthz` and the HTTPS search endpoint. Railway's
  deployment healthcheck is not continuous monitoring.
- Alert on 5xx rates, failed GitHub calls, auth throttling, publish rejection classes,
  graceful-shutdown failures, Postgres pool saturation, CPU/memory, and Git volume capacity.
- Alert when no new off-site object arrives within the approved interval plus tolerance, on
  collector 4xx/5xx responses, and on growth in the local export archive directory.
- Review Railway Postgres and Git-volume backup jobs and test restore availability monthly.
- Run and record `registry audit` before cutover, after restore, and after suspected storage
  incidents. Missing content is always blocking.
- Run a complete off-site recovery drill at least quarterly. Rotate collector/admin secrets
  on the approved schedule and after any suspected disclosure.
- Never use credential values, device codes, session tokens, repository bodies, or database
  URLs as log fields or metric labels.

## Client Session Isolation

The CLI stores `registry_url` in `$SHERPA_HOME/registry-session.json` with mode `0600`.
Normalized issuer URLs match despite a trailing slash, but a staging session is never sent to
production. A cross-registry publish fails with a message naming the session issuer and asking
the operator to log in to the target registry. Legacy issuer-less sessions fail closed and
require login again. `SHERPA_REGISTRY_TOKEN`, when deliberately set, retains precedence for
admin/CI operation.
