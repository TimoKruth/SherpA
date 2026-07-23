# Railway Deployment Runbook

This runbook deploys the SherpA registry as one Railway API service, one Railway
Postgres service, and one persistent Git volume, plus a separate stateless discovery website.
Use separate Railway environments, databases, volumes, OAuth applications, domains, and
secrets for staging and production.

Production is not approved until the staging gate in this document has been recorded for
the exact commit being promoted.

## Fail-Closed Collector Capability Gate

Task 0 is **Blocked** and no reduced threat model is approved. While that status remains, do not
contact, query, provision, configure, validate, deploy, restart, monitor, or otherwise mutate any
Railway environment or resource, any VPS, or any Storage Box account, path, repository, snapshot,
or service. This blanket ban covers both collector-dependent and ordinary Railway work, all live
acceptance steps, authoritative repository initialization, uploads, restores, and every outage,
queue, idempotency, retention, rollback, cleanup, or recovery drill. A fresh approval for an
individual action does not override this capability gate.

Only a provider/restriction change that passes the complete capability exercise, or a separately
approved threat-model change documented in revised runbooks before execution, can clear the gate.
Only repository source work, documentation, and local-only validation through Task 12 may continue.
All Railway, VPS, and Storage Box commands below are future procedures and must not be executed
while the gate is Blocked. Production remains empty and untouched.

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
- Public HTTP requests have a 15-minute maximum duration. Railway documents that its edge strips
  client-supplied forwarding headers and supplies forwarding values including `X-Real-IP`,
  `X-Forwarded-For`, `X-Forwarded-Proto`, and `X-Railway-Request-Id`. See
  [public-networking limits](https://docs.railway.com/networking/public-networking/specs-and-limits).
  Candidate unit/integration tests prove the application's exact selection logic: with proxy trust
  enabled it uses a valid `X-Real-IP` first, otherwise the final valid `X-Forwarded-For` hop, and
  otherwise the direct peer. Live staging status checks prove only edge-level rate-limit bucketing
  and resistance to a client-supplied-header bypass; they cannot observe or prove the sanitized
  header value or the application's internal fallback branch.

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
| OAuth | Separate GitHub OAuth Apps with device flow enabled and exact registry callback URLs |
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
| `SHERPA_GITHUB_CLIENT_SECRET` | Required for website sign-in; registry-only OAuth App secret |
| `SHERPA_WEB_PUBLIC_BASE_URL` | Required with the client secret; exact canonical website HTTPS origin used for fixed handoff redirects |
| `SHERPA_TRUST_PROXY` | Set `true` on Railway; trusts `X-Forwarded-Proto` for scheme. Candidate tests prove rate-limit identity selects a valid `X-Real-IP`, then the final valid `X-Forwarded-For` hop, then the direct peer. Railway documents edge sanitization; live status checks verify bucketing/bypass behavior, not internal header values. Host is never trusted. |
| `SHERPA_DB_MAX_CONNS` | Optional positive pool cap matched to the Postgres plan |
| `SHERPA_REGISTRY_TOKEN` | Optional admin/CI bypass; omit for normal session-only operation |
| `PORT` | Injected by Railway; do not hard-code it |
| `SHERPA_EXPORT_URL` | Required for DR; exactly `https://sherpa-collector.kruth-support.de/v1/exports` |
| `SHERPA_EXPORT_TOKEN` | Required operationally; set as a Railway secret and never expose its value |
| `SHERPA_EXPORT_INTERVAL` | Required with export URL; Go duration such as `24h` |
| `SHERPA_EXPORT_ARCHIVE_DIR` | Required with export URL; exactly `/data/exports`, the persistent entrypoint-prepared queue, never a child of `/data/git` |

Staging and production values must be independent. Rotate the GitHub client secret, optional
admin token, and export token through the Railway variable UI and the receiving system. Revoke
the old OAuth App secret in GitHub after the replacement deployment is verified. A GitHub client
ID is public; the client secret, GitHub access tokens, grants, and SherpA session tokens remain
secret.

### GitHub OAuth Apps

Create a separate GitHub OAuth App for each environment. Enable device flow and set its callback
URL to the exact registry route:

```text
https://<registry-domain>/v1/auth/web/callback
```

The callback belongs to the registry, not the website. Set the registry's
`SHERPA_WEB_PUBLIC_BASE_URL` to `https://<website-domain>`; the registry redirects only to that
pinned origin after OAuth. The application requests no repository or organization scope and uses
the resulting token only to read the authenticated GitHub user's public identity. Do not add
broader scopes. Store `SHERPA_GITHUB_CLIENT_SECRET` only on the registry service. The website
must never receive it.

## Off-Site Exports

The in-process scheduler runs in the API process because only that service mounts the Git
volume. At each interval it creates `postgres.dump` first, then one `git bundle --all` per
repository, writes the checksum/size manifest last, atomically finishes the archive, and
POSTs it to `SHERPA_EXPORT_URL`. The database-first order means a concurrent publish can add
only harmless extra Git content; dumped metadata cannot refer to content absent from later
bundles.

Configure the registry with exactly:

```text
SHERPA_EXPORT_URL=https://sherpa-collector.kruth-support.de/v1/exports
SHERPA_EXPORT_ARCHIVE_DIR=/data/exports
SHERPA_EXPORT_TOKEN=<set as a Railway secret; never record the value>
```

The collector must:

- accept authenticated HTTPS `POST /v1/exports` requests with `Content-Type: application/gzip`;
- validate the archive, hash its exact compressed bytes, and use that 64-lowercase-hex SHA-256
  as the object ID;
- age-encrypt the validated bytes before remote storage, with only the public age recipient on
  the VPS and the private identity held offline;
- durably record encrypted spool and ledger state before acknowledging the request;
- use exact Borg `Exists -> Create if absent -> Exists` handling, with archive name
  `sherpa-<object-id>` and stored file `<object-id>.tar.gz.age`;
- return only `201` with status `stored` or `200` with status `existing` after proving the exact
  object ID; and
- live outside the Railway project/account and satisfy the separately approved Storage Box
  capability, retention, monitoring, and recovery posture in `docs/deployment/collector.md`.

The running collector intentionally has no authenticated retrieval/download API. Disaster
recovery uses the recovery-only Borg identity to extract the exact encrypted object, the offline
age private identity to decrypt it, and `collector verify <archive-path>` before any import.

The scheduler deletes a local archive only after validating a `stored` or `existing` response
for the matching SHA-256 object ID. A failed deletion or interruption after remote commit leaves
the exact archive locally; a restart resends the identical bytes, accepts a validated `existing`
response, and deletes only after that proof. Any failed upload remains in
`SHERPA_EXPORT_ARCHIVE_DIR` and is retried on each interval; startup rediscovers completed queue
archives, and the scheduler does not create a new archive until the oldest pending archive uploads
and is deleted. This bounds ordinary local accumulation to one archive, but a failed upload or
missing collector object is still an alert because the off-site recovery point is not advancing.
Keep the archive directory outside `/data/git` and monitor its capacity. `/data` must remain a
real, root-owned volume mountpoint that is not writable by the registry UID; the root entrypoint
alone precreates the queue beneath it as a real `0700` directory owned by the registry UID/GID.
The scheduler validates that identity and holds a cooperative lock for its lifetime. Keep
`numReplicas=1`, and do not run another registry, sidecar, shell, or job as the registry UID
against the same queue. Collector retention is the authoritative off-site retention policy only
after the blocked Storage Box posture receives an approved resolution.

For a manual local archive in a container or recovery environment:

```sh
registry export /tmp/sherpa-manual.tar.gz
```

This command packages but does not upload the archive. Do not treat a file left on the Railway
container filesystem as a backup. The forced-upload procedure (`SHERPA_EXPORT_INTERVAL=1m`,
redeploy, wait for a validated collector result, restore the interval) and every off-site restore
drill are prohibited while the Task 0 gate is Blocked. After the gate is cleared, each interval
change/redeploy and each restore drill still requires its own fresh disruptive-action approval.
A successful upload is not proof of recoverability; perform and record an approved off-site restore
drill at least quarterly and after any backup-format change.

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

Record the commit, environment, domain, timestamps, operator, and non-sensitive result summaries
for every step. Do not record collector archive/object IDs, tokens, or device codes. Keep any exact
object identifier needed for an operational comparison only in a mode-0600 temporary workspace.

The Task 0 gate must be cleared before any step that configures or starts the export scheduler,
forces an upload, accesses the collector, or creates recovery resources. While Blocked, do not
execute steps 9-10 or redeploy a registry whose configured scheduler would contact the collector.

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
   - First run the exact-candidate test
     `go test -count=1 ./internal/registry/api -run '^TestDeviceStartClientIPHonorsForwardedFor$'`.
     That test, not the live edge probe, proves valid-`X-Real-IP` preference, final-valid-hop
     `X-Forwarded-For` fallback, malformed/missing fallback to the direct peer, and distinct-bucket
     behavior in the application.
   - **Verify live edge bucketing without claiming header introspection.** From two genuinely
     different source networks, confirm each gets an independent first device-start allowance.
     From one already-limited network, repeat with a forged `X-Real-IP` and a prepended
     `X-Forwarded-For`; neither request may gain a fresh allowance. Record only network labels and
     status sequences. This proves the observed Railway edge plus application does not permit those
     client headers to bypass or collapse the rate-limit buckets; status codes do not reveal the
     sanitized header values or prove which internal fallback branch ran. If distinct real clients
     collapse into one bucket or a forged value changes the bucket, stop and reconcile before
     production.
   - Confirm locally rejected requests do not produce GitHub-call errors and request logs
     contain no headers or bodies.
7. Search and view the published stack, then clone its returned `repo_url`. Confirm the URL is
   the configured public HTTPS domain even when a test request supplies a spoofed
   `X-Forwarded-Host`.
8. Redeploy the API. Confirm the same version remains searchable, viewable, and cloneable.
9. After the separately approved temporary interval change, set the export interval to one
   minute, redeploy, and confirm the collector returns `201 stored` or `200 existing` for the
   matching object ID and the local `/data/exports` archive is removed only after that proof.
   Restore the normal interval afterward.
10. From an approved trusted recovery environment, use the recovery-only Borg identity to extract
    that exact `<object-id>.tar.gz.age`, decrypt it with the offline age private identity, and run
    `collector verify <archive-path>`. Only after verification passes, restore both Postgres and
    Git into a new sibling/recovery target and run `registry audit`. Missing content fails the
    gate. Complete login, search, detail, and HTTPS clone smoke tests against the recovery target.
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
   snapshots only in the same project/environment. For disaster recovery, use the recovery-only
   Borg identity and pinned host key from a trusted recovery environment to select the exact
   `sherpa-<object-id>` archive and extract `<object-id>.tar.gz.age`. The running collector has no
   retrieval/download API.
3. Decrypt the extracted object with the required offline age private identity, then run
   `collector verify <archive-path>`. Only after that succeeds, extract it in the recovery
   environment and independently verify every artifact's size and SHA-256 against
   `manifest.json` before importing it. Reject an incomplete archive, trailing data, unexpected
   members, or a manifest that is not the final completed member.
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

Phase 2e adds a security-critical exception for registry rollback. A pre-2e image does not know
that `purpose='web'` sessions must be denied for publish and may treat them like CLI sessions.
Before rolling the registry below the 2e boundary:

1. Disable new web login by removing both `SHERPA_GITHUB_CLIENT_SECRET` and the registry-side
   `SHERPA_WEB_PUBLIC_BASE_URL`, then deploy a current image and confirm web login is unavailable.
2. Revoke all existing web sessions and outstanding grants in Postgres (`DELETE FROM sessions
   WHERE purpose='web'`; `DELETE FROM web_grants`) from an audited recovery/operations session.
3. Confirm a previously issued web token receives `401` and no new grant can be minted.
4. Only then deploy the pre-2e image and rerun publish authorization checks.

Rolling back to another 2e-aware image does not require this purge. Website-only rollback remains
stateless and does not change registry sessions.

Do not roll Postgres or Git back merely because an application image failed; that can discard
accepted publishes and split the stores. Use the restore procedure when data rollback is
actually required. Any future non-additive migration requires an explicit
expand/migrate/contract plan and rollback gate before deployment.

## Monitoring and Routine Operations

- Use external uptime monitoring for `/healthz` and the HTTPS search endpoint. Railway's
  deployment healthcheck is not continuous monitoring.
- Alert on 5xx rates, failed GitHub calls, auth throttling, publish rejection classes,
  graceful-shutdown failures, Postgres pool saturation, CPU/memory, and Git volume capacity.
- Alert when no new verified off-site object arrives within the approved interval plus tolerance,
  on any collector 4xx/5xx response or scheduler retry/fatal classification, and when a completed
  archive remains in `/data/exports` across the interval or a registry restart. Monitor queue
  count, queue bytes, and `/data` capacity; one pending archive is already stale recovery-point
  growth because the scheduler suppresses creation of the next export.
- Review Railway Postgres and Git-volume backup jobs and test restore availability monthly.
- Run and record `registry audit` before cutover, after restore, and after suspected storage
  incidents. Missing content is always blocking.
- After the Task 0 gate is cleared, run a separately approved complete off-site recovery drill at
  least quarterly. Rotate collector/admin secrets on the approved schedule and after any suspected
  disclosure; collector-side rotations remain prohibited while Blocked.
- Never use credential values, device codes, session tokens, repository bodies, or database
  URLs as log fields or metric labels.

## Client Session Isolation

The CLI stores `registry_url` in `$SHERPA_HOME/registry-session.json` with mode `0600`.
Normalized issuer URLs match despite a trailing slash, but a staging session is never sent to
production. A cross-registry publish fails with a message naming the session issuer and asking
the operator to log in to the target registry. Legacy issuer-less sessions fail closed and
require login again. `SHERPA_REGISTRY_TOKEN`, when deliberately set, retains precedence for
admin/CI operation.

## Discovery Website Service

Add the discovery website only after the registry staging gate above has passed. The website
is a second Railway service from the **same repository and branch** as the registry, but it has
an independent deployment, pinned deployment commit, and public domain.

Configure the website service as follows:

1. Create a second service from this repository and select the same branch as the registry.
2. Set the service's config-file path to absolute `/deploy/web/railway.json`. Confirm Railway
   resolves its Dockerfile to `deploy/web/Dockerfile`, not the root registry `Dockerfile`.
3. Do not set a service root-directory override. The web build requires the root `go.mod`,
   `go.sum`, `cmd/web`, and `internal/**` from the shared Go module.
4. Give the website its own public HTTPS domain. Set `SHERPA_WEB_PUBLIC_BASE_URL` to that exact
   canonical origin, without userinfo, query, or fragment.
   Set `SHERPA_REGISTRY_PUBLIC_URL` to the registry's exact canonical public HTTPS origin. These
   two public variables are an all-or-nothing pair and their origins must be distinct.
5. Set an explicit registry-service `PORT=8080`. Both registry and website listeners bind on
   `[::]`, which is required for reliable Railway private networking.
6. Configure the web service's private upstream reference as:

   ```text
   SHERPA_REGISTRY_API_URL=http://${{registry.RAILWAY_PRIVATE_DOMAIN}}:${{registry.PORT}}
   ```

   Replace `registry` only if the Railway registry service has a different exact service name.
   Private service traffic is HTTP; public browser traffic remains HTTPS at Railway's edge.
7. Keep the one-replica setting, `/healthz` deploy gate, restart policy, and pinned non-root
   distroless image from `deploy/web/railway.json`. Do not override the image entrypoint or user.
8. Attach no volume and add no database reference to the website service.

### Website Runtime Variables

| Variable | Requirement |
| --- | --- |
| `SHERPA_REGISTRY_API_URL` | Required; fixed Railway private HTTP origin shown above |
| `SHERPA_WEB_PUBLIC_BASE_URL` | Required; website's canonical public HTTPS origin |
| `SHERPA_REGISTRY_PUBLIC_URL` | Required with the website public base; registry's canonical public HTTPS origin for sign-in redirects |
| `SHERPA_WEB_UPSTREAM_TIMEOUT` | Optional; defaults to `5s`, allowed range `100ms` through `30s` |
| `PORT` | Injected by Railway; the web process derives `[::]:PORT` |
| `SHERPA_WEB_ADDR` | Optional explicit listener override; normally omit on Railway |

The website service accepts no deploy-time secret. Railway private DNS and the public origins are
configuration, not credentials. At runtime it receives an opaque per-user web session token and
CSRF value in host-only `Secure; HttpOnly; SameSite=Lax; Path=/` cookies. Those transient values
are credentials and must not appear in variables, logs, URLs, templates, JavaScript, or caches.

### Website and Registry Separation

The website must have **none** of the following settings or resources:

- a volume or `SHERPA_CONTENT_DIR`;
- `DATABASE_URL` or any Postgres reference;
- `SHERPA_GITHUB_CLIENT_ID`, `SHERPA_GITHUB_CLIENT_SECRET`, GitHub tokens, or registry-side OAuth configuration;
- `SHERPA_REGISTRY_TOKEN`, a static registry session, or a cookie signing/encryption secret;
- `SHERPA_EXPORT_URL`, `SHERPA_EXPORT_TOKEN`, or export storage access;
- private keys or other application credentials.

The registry remains on the root `Dockerfile` and root `railway.json`. It retains its Postgres
reference, Git volume, auth and export configuration, public registry domain, and pinned
`SHERPA_PUBLIC_BASE_URL`; that registry URL, not the website domain or request host, remains the
source of Git clone/try commands.

Scope variables to their individual Railway services. A website deploy must not restart,
reconfigure, or remount the registry. A website failure cannot affect registry API, login,
publish, clone, or CLI use. During a registry outage the website process and `/healthz` remain
available. Public dynamic pages return bounded `503` responses; authenticated pages degrade
safely without clearing a still-valid cookie. Sign-in, follow, dashboard, seen, and logout
revocation depend on the registry, although logout always clears local browser cookies.

### Website Deploy and Rollback

Before deploying a website image, run the automated Go checks and
`bash deploy/web/smoke_build.sh` for the exact commit. Deploy only through
`deploy/web/railway.json` and let the website's local `/healthz` gate activation. Verify that the
selected source commit matches the registry commit when API-contract changes are being promoted.

The website is stateless. To roll it back, redeploy a previously accepted, pinned website image
or commit and let `/healthz` gate the replacement. Do not restore or roll back Postgres, the Git
volume, or the registry image as part of a website rollback. If a new website is incompatible
with the current registry contract, roll back only the website first; registry reads, login,
publish, clone, and CLI operation must remain available throughout.

Deploy registry changes independently using the registry procedure above. During a registry
redeploy, confirm the website returns its bounded degraded response and then recovers without a
website redeploy. Treat changes to either public domain or pinned public-base variable as a
separate configuration rollout and repeat the host-spoofing and command checks in the website
gate.

## Website Staging Acceptance Gate

Run this gate only after the registry's 2c-iii staging gate has passed. Record the website and
registry commits, environment, domains, timestamps, and operator for each step. Do not record
request bodies, query values, commands containing private data, or credentials.

1. Deploy the website service from `deploy/web/railway.json`; confirm the image/config source is
   the web path, not the root registry config.
2. Confirm `/healthz` is `200` and PID 1 is non-root; confirm the service environment has no DB,
   GitHub/OAuth, static registry/admin, cookie-key, or export secret. Runtime browser cookies are
   expected only after sign-in and must never appear in the service environment.
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
    session/grant/CSRF value, credentials, or full internal/public URL with query is present.
11. Configure external continuous uptime checks for both the public home page and `/healthz`;
    Railway's deploy healthcheck alone is not continuous monitoring.

Production website domain exposure requires all eleven website steps, in addition to the
registry gate, to pass for the exact commit being promoted.

## Phase 2e Live Staging Acceptance Gate

Run this gate after both preceding staging gates pass for the same candidate. For every step,
record the registry commit, website commit, staging registry and website domains, operator,
start/end time, result, and non-secret evidence references. Never record tokens, grants, device
codes, CSRF values, OAuth codes/verifiers, trial notes, request bodies, or private export URLs.

1. Confirm the complete registry 2c-iii and website 2d gates above passed for the exact candidate
   commits and domains.
2. Complete real GitHub website sign-in using the staging OAuth App. Confirm GitHub returns only
   to the pinned staging registry callback and the final browser location is the pinned staging
   website `/dashboard`.
3. Confirm a replayed OAuth callback/grant, a copied grant without its original handoff nonce,
   and tampered state fail. Inspect browser storage/history: login, session, and CSRF cookies have
   exact `Secure; HttpOnly; SameSite=Lax; Path=/` attributes and the grant query is cleared.
4. Confirm a web session can follow but receives `403` on publish, a CLI session can follow and
   same-owner publish, and the admin bearer receives `401` on `/v1/me` and personal endpoints.
5. Follow the current v1, publish v2, and confirm the dashboard and `sherpa updates` show the same
   pending version. Repeated feed/dashboard GETs must not clear it.
6. Confirm successful `sherpa update` or explicit web Mark reviewed clears pending without
   activating a profile automatically. Send older seen versions afterward and confirm pending
   state does not move backward.
7. Clone a registry ref online and confirm auto-follow. Repeat during registry outage: install
   still completes and queues one follow; after recovery an online social command syncs it once
   without duplicates.
8. Record a trial with notes and confirm the journal remains local mode `0600`. Explicit share
   sends only immutable stack/version identity and the selected verdict, never notes or paths.
9. At the real Railway edge, confirm spoofed Host/forwarded headers do not change pinned origins;
   foreign or duplicate Origin/CSRF, arbitrary return URLs, tampered OAuth state, and repeated
   grants are rejected.
10. Stop or block the registry. Confirm website `/healthz` stays `200`, authenticated pages
    degrade safely without exposing/clearing a valid session, local CLI status/profiles still
    work, and logout still clears all website cookies even if registry revocation fails.
11. Inspect Railway/application/collector logs and an off-site export/restore. Confirm no OAuth,
    session, grant, CSRF, authorization, trial-note, or database secret is exposed; restored data
    includes follows, events, verdict-only feedback, and only grant/session hashes.
12. Drill website-only and registry-only rollback separately. Confirm public discovery and
    publish authorization survive. Before any pre-2e registry rollback, perform the web-session
    revocation procedure in this runbook and prove an old web token cannot publish.

Production 2e exposure requires all twelve steps to pass for the exact deployed commits. A failed
or missing step leaves 2e at **automated gates green, live Railway acceptance pending**.
