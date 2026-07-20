# SherpA Off-Site Collector Design

## Purpose

SherpA needs an external disaster-recovery target that is independent of the Railway project and can accept the registry's existing authenticated HTTPS export uploads. The collector will run with Docker Compose on an authorized Hostinger VPS, store encrypted recovery objects in a dedicated Hetzner Storage Box sub-account, and support restoration without access to Railway or the running collector.

This design also corrects the registry's retry buffer so a failed export upload survives Railway replacement deployments.

## Goals

- Accept SherpA's existing authenticated HTTPS `POST` export contract.
- Return success only after the object is durably committed to the Storage Box.
- Encrypt every stored archive with an offline recovery key.
- Prevent the routine upload identity from normally deleting or pruning backups.
- Make duplicate uploads idempotent.
- Preserve pending uploads across collector and registry restarts.
- Validate the complete SherpA archive and final manifest before storage.
- Support independent retrieval, decryption, verification, and sibling-resource restore drills.
- Run as a hardened Docker Compose workload behind the VPS's existing Traefik deployment.
- Keep all credentials and private recovery material out of Git, container images, logs, and acceptance evidence.
- Retain 30 daily and 12 monthly recovery points.

## Non-Goals

- The collector will not provide a browser interface.
- The collector will not expose list, retrieve, delete, prune, or compact operations over HTTP.
- The collector will not run inside Railway.
- The collector will not hold the age private key or unrestricted Storage Box recovery credentials.
- The collector will not replace the Storage Box with local VPS storage.
- The collector will not restore over the primary Railway database or Git volume.
- The first release will not support multiple storage providers or multiple tenants.

## Deployment Context

- Public hostname: `sherpa-collector.kruth-support.de`.
- Runtime host: authorized Hostinger VPS with Docker and Docker Compose.
- Deployment root: `/docker/SherpA-collector`.
- HTTPS ingress: the VPS's existing Traefik Docker provider, `websecure` entrypoint, and `letsencrypt` certificate resolver.
- Authoritative storage: a dedicated Hetzner Storage Box sub-account and Borg repository.
- Source delivery: an exact SherpA Git commit checked out and built on the VPS.
- Railway production remains untouched; only the `staging` environment participates in acceptance.

## Architecture

```text
Railway staging registry
  ├─ creates complete postgres.dump + Git bundles + final manifest
  ├─ keeps restart-safe retry archives under /data/exports
  └─ HTTPS POST + bearer token
                    │
                    ▼
Traefik on Hostinger VPS
  └─ trusted TLS for sherpa-collector.kruth-support.de
                    │
                    ▼
SherpA collector container
  ├─ authenticates and bounds request before body read
  ├─ validates archive and final manifest
  ├─ computes stable compressed-archive SHA-256 object ID
  ├─ age-encrypts using a public recipient
  ├─ retains encrypted retry state under /data
  └─ commits through append-only Borg over pinned SSH
                    │
                    ▼
Dedicated Hetzner Storage Box sub-account
  ├─ append-only Borg repository
  ├─ secondary automatic Storage Box snapshots
  └─ independent recovery access held offline
```

## Repository Components

The implementation follows the existing `cmd/<service>` and `internal/<service>` layout.

- `cmd/collector/main.go`: configuration loading, startup, shutdown, and command dispatch.
- `cmd/collector/main_test.go`: startup, configuration, and offline-command behavior.
- `internal/collector/config.go`: typed configuration and fail-fast validation.
- `internal/collector/handler.go`: HTTP routing, authentication, request limits, and safe responses.
- `internal/collector/archive.go`: ingestion orchestration and integration with the shared archive validator.
- `internal/collector/age.go`: age recipient parsing and streaming encryption.
- `internal/collector/borg.go`: constrained Borg process execution and archive verification.
- `internal/collector/spool.go`: atomic spool files, restart discovery, and safe cleanup.
- `internal/collector/state.go`: atomic non-secret object ledger.
- `internal/collector/worker.go`: bounded background retry processing.
- `internal/collector/*_test.go`: unit and integration coverage.
- `deploy/collector/Dockerfile`: multi-stage production image.
- `deploy/collector/compose.yaml`: deployable Compose template.
- `deploy/collector/smoke_build.sh`: final-image and runtime security checks.
- `docs/deployment/collector.md`: provisioning, deployment, monitoring, retention, and recovery runbook.

Archive validation will be factored into a shared package usable by the registry exporter, collector ingestion, and offline verification command. It will not duplicate manifest rules in three implementations.

## VPS Filesystem Layout

```text
/docker/SherpA-collector/
├── source/                       # exact SherpA checkout
├── compose.yaml                  # reviewed copy from the exact checkout
├── runtime.env                   # non-public runtime configuration, mode 0600
├── config/
│   ├── age-recipient             # public age recipient
│   └── known_hosts               # pinned Storage Box SSH host keys
├── secrets/                      # directory mode 0700
│   ├── upload-token              # mode 0400, collector runtime UID
│   ├── storage-ssh-key           # mode 0400, collector runtime UID
│   └── borg-repository           # mode 0400, private repository location
└── data/                         # collector runtime UID
    ├── spool/
    ├── state/
    └── borg/
```

The collector uses a fixed non-root UID/GID. Secret files are readable only by that UID and are mounted read-only under `/run/secrets`. The enclosing `secrets` directory remains root-controlled. The `/data` tree is the only persistent writable container mount.

The source checkout, Compose template, and container build context never contain runtime secret files.

## HTTP API

### Upload

```http
POST /v1/exports
Authorization: Bearer <secret>
Content-Type: application/gzip
Content-Length: <positive integer>
```

The collector accepts exactly one in-flight upload. It validates authentication and headers before reading the request body.

Successful response:

```json
{
  "object_id": "sha256:<64 lowercase hexadecimal characters>",
  "status": "stored"
}
```

An idempotent replay returns the same object ID with status `existing`.

Response statuses:

- `200`: the exact object already exists and Borg confirms it is present.
- `201`: a new object was committed and verified.
- `401`: bearer token missing or invalid.
- `405`: method unsupported.
- `411`: `Content-Length` absent or not positive.
- `413`: compressed body exceeds the configured limit.
- `415`: content type is not `application/gzip`.
- `422`: malformed, unsafe, incomplete, or internally inconsistent SherpA archive.
- `429`: another upload is currently active.
- `503`: the encrypted object is safe locally but the Storage Box commit is unavailable.
- `507`: the persistent spool cannot safely accept the object.

The response never includes backend command output, request bodies, tokens, SSH details, repository locations, or private filesystem paths.

### Health

`GET /healthz` is process-local liveness. It returns success when the HTTP process and worker are running. Docker uses this endpoint and does not restart the service merely because the Storage Box is temporarily unavailable.

`GET /readyz` reports operational recovery readiness. It fails when:

- the persistent spool is not writable;
- an encrypted pending object is older than the configured threshold;
- no successful recovery point exists after the startup grace period;
- the newest successful recovery point exceeds the configured freshness limit;
- the retry worker has entered a terminal local-state error.

Neither endpoint returns object IDs, storage locations, credentials, or detailed backend errors.

## Request Authentication

- The bearer token is loaded from `SHERPA_COLLECTOR_TOKEN_FILE`.
- Startup fails if the token file is missing, empty, not a regular file, or group/world accessible.
- Token comparison uses constant-time comparison over fixed-size hashes.
- The token is required; there is no unauthenticated mode.
- Logs record only an authentication failure classification, never the header or token hash.
- Rotation replaces the secret file atomically and restarts the collector. Railway and collector changes are coordinated so no old token is retained after successful rotation.

## Request Bounds

Initial limits:

- Maximum compressed body: 8 GiB (`8589934592` bytes).
- Maximum uncompressed archive content: 32 GiB.
- Maximum tar members: 100,000.
- Maximum manifest size: 16 MiB.
- Maximum individual member path length: 1,024 bytes.
- One concurrent upload.
- Ten-minute registry upload timeout remains the initial end-to-end budget; the collector must stream and avoid buffering the body in memory.

A request with an unknown or chunked length is rejected because the registry always supplies an explicit length and bounded disk admission is required.

## Archive Validation

The collector stores only a complete SherpA recovery archive.

Validation requires:

- a valid gzip stream with no trailing second archive;
- a valid tar stream;
- normalized relative paths only;
- no absolute paths, backslashes, empty path segments, `.` segments, or `..` traversal;
- no duplicate members;
- only the member types defined by the SherpA archive format;
- exactly one `postgres.dump`;
- Git bundles only under the documented repository namespace;
- exactly one `manifest.json`;
- `manifest.json` as the final completed tar member;
- every non-manifest artifact represented exactly once in the manifest;
- no manifest entry for an absent artifact;
- exact byte-size and SHA-256 agreement for every artifact;
- no extra data after the final tar terminator.

Validation is performed from the completed mode-0600 plaintext spool file. The plaintext file is removed immediately after a durable encrypted spool file has been created.

## Object Identity and Idempotency

The stable object ID is the SHA-256 of the complete compressed archive received from Railway:

```text
sha256:<full lowercase hex digest>
```

The Borg archive name is derived solely from this digest:

```text
sherpa-<full lowercase hex digest>
```

This makes a resend after a lost HTTP response idempotent. Before returning `200 existing`, the collector verifies that Borg reports the exact archive as present. A ledger entry alone is insufficient.

A different compressed archive always receives a different object ID, even if its logical contents resemble an earlier recovery point.

## Encryption

- The collector uses age public-key encryption.
- The collector holds only the public recipient.
- The age private identity is generated and stored in offline recovery custody.
- Encryption streams the validated plaintext archive into a temporary encrypted file under `/data/spool`.
- The encrypted file is mode 0600, synchronized, and atomically renamed before the plaintext file is removed.
- Borg stores the age-encrypted object. Borg repository encryption is not relied upon for confidentiality.
- The Storage Box, routine collector credentials, and a compromised Borg repository cannot decrypt archive contents without the offline age identity.

The private age identity is never placed in Railway, the collector image, the persistent VPS deployment, Git, logs, or evidence.

## Persistent Spool and Crash Recovery

Spool states use distinct suffixes so incomplete files cannot be mistaken for durable retry objects:

- `.upload.partial`: incomplete HTTP body; never validated or uploaded.
- `.age.partial`: incomplete encryption output; never uploaded.
- `.age`: complete encrypted object eligible for Borg retry.

All transitions use file synchronization and atomic rename on the same filesystem.

On startup, the collector:

1. acquires a single-process spool lock;
2. validates the spool and state directories;
3. removes stale incomplete partial files older than 24 hours;
4. discovers complete `.age` objects;
5. reconciles them with the object ledger and Borg repository;
6. queues missing remote objects oldest-first;
7. starts bounded retries with a five-minute interval and capped backoff.

The collector never deletes a complete encrypted spool object until Borg commit and remote presence verification both succeed.

## Object Ledger

The local ledger contains non-secret operational metadata:

- object ID;
- Borg archive name;
- compressed input size;
- encrypted object size;
- receipt time;
- successful storage time;
- latest retry classification;
- retry count.

It does not store source IP addresses, bearer token material, Storage Box locations, database URLs, archive member names, repository names, or request bodies.

Ledger updates use mode-0600 temporary files, synchronization, and atomic replacement. The ledger assists reconciliation but is never accepted as proof that the remote object exists.

## Borg Storage Backend

- The collector invokes a pinned Borg version without a shell.
- Arguments are constructed by the program, not from request values.
- Repository location and SSH identity are loaded from files.
- SSH host-key verification is mandatory and uses a pinned `known_hosts` file.
- `StrictHostKeyChecking=yes` is mandatory after initial out-of-band fingerprint verification.
- Borg writable cache/config/security paths live under `/data/borg`.
- Standard output and error are captured, bounded, classified, and redacted before logging.
- A successful create is followed by an exact archive-presence query.
- Backend timeouts and process cancellation are bounded.

The Storage Box sub-account is dedicated to SherpA. The routine SSH key is configured for Borg append-only operation. It can create archives but cannot perform routine prune or compact operations.

## Retention and Deletion Resistance

The approved logical retention policy is:

- 30 daily archives;
- 12 monthly archives.

The always-running collector never prunes, deletes, or compacts the repository.

Retention maintenance is an explicit operator operation using recovery credentials that are not persistently stored on the VPS. Before any unrestricted Borg transaction, the maintenance process:

1. retrieves the expected object ledger and prior acceptance evidence;
2. lists the complete repository;
3. detects unexpected missing or deletion-marked archives;
4. stops if unexplained deletion requests exist;
5. prints the proposed retention result without secrets;
6. requires fresh explicit approval;
7. prunes to 30 daily and 12 monthly archives;
8. compacts only after prune succeeds;
9. verifies the retained set;
10. removes temporary recovery credentials from the execution environment.

Automatic Storage Box snapshots are enabled at the maximum practical schedule supported by the user's Storage Box plan. They are a secondary recovery layer and are not treated as independent WORM storage or as the authoritative 30-daily/12-monthly policy.

## Docker Image

The collector uses a multi-stage build:

- a pinned Go build image compiles a static collector binary;
- a pinned minimal Debian-compatible runtime supplies CA certificates, OpenSSH client, and the pinned Borg package;
- no compiler, source tree, Git metadata, package cache, or test fixture remains in the final image.

Runtime requirements:

- fixed non-root UID/GID;
- no setuid helper requirement;
- explicit entrypoint and collector command;
- OCI source revision labels containing only the public commit identifier;
- no build arguments or labels containing secrets.

The smoke build verifies binary startup, Borg availability, CA trust, non-root PID 1, unwritable root filesystem, writable `/data`, absent source/build tools, and absent secret-like image metadata.

## Docker Compose Hardening

The Compose service uses:

- `restart: unless-stopped`;
- no host-published collector port;
- `expose: 8080` for Docker discovery;
- `read_only: true`;
- `cap_drop: [ALL]`;
- `security_opt: [no-new-privileges:true]`;
- fixed non-root `user`;
- read-only configuration and secret mounts;
- bind-mounted persistent `/data`;
- bounded tmpfs for `/tmp`;
- CPU and memory limits appropriate for the VPS;
- Docker JSON log rotation;
- a `/healthz` health check;
- Traefik router, TLS, resolver, and service-port labels.

Traefik is the only public listener. The collector container is not reachable through a directly published host port.

## Collector Configuration

The collector accepts these settings:

- `SHERPA_COLLECTOR_LISTEN_ADDR=:8080`
- `SHERPA_COLLECTOR_TOKEN_FILE=/run/secrets/upload-token`
- `SHERPA_COLLECTOR_AGE_RECIPIENT_FILE=/run/config/age-recipient`
- `SHERPA_COLLECTOR_BORG_REPOSITORY_FILE=/run/secrets/borg-repository`
- `SHERPA_COLLECTOR_BORG_SSH_KEY_FILE=/run/secrets/storage-ssh-key`
- `SHERPA_COLLECTOR_KNOWN_HOSTS_FILE=/run/config/known_hosts`
- `SHERPA_COLLECTOR_SPOOL_DIR=/data/spool`
- `SHERPA_COLLECTOR_STATE_DIR=/data/state`
- `SHERPA_COLLECTOR_BORG_DIR=/data/borg`
- `SHERPA_COLLECTOR_MAX_BYTES=8589934592`
- `SHERPA_COLLECTOR_MAX_UNCOMPRESSED_BYTES=34359738368`
- `SHERPA_COLLECTOR_MAX_MEMBERS=100000`
- `SHERPA_COLLECTOR_RETRY_INTERVAL=5m`
- `SHERPA_COLLECTOR_PARTIAL_MAX_AGE=24h`
- `SHERPA_COLLECTOR_STARTUP_GRACE=26h`
- `SHERPA_COLLECTOR_MAX_RECOVERY_AGE=26h`

Configuration is typed and fail-fast. Unsafe partial configuration prevents startup. Error messages identify the invalid setting without printing its value when the value may contain private infrastructure information.

## Registry Retry-Buffer Correction

The Railway registry already supports `SHERPA_EXPORT_ARCHIVE_DIR`, but its pending path is process memory and is not rediscovered after restart. The corrected behavior is filesystem-driven.

- Staging uses `SHERPA_EXPORT_ARCHIVE_DIR=/data/exports`.
- The registry entrypoint creates and owns `/data/exports` alongside `/data/git`.
- Completed `.tar.gz` files are discovered on startup and before each scheduler tick.
- Completed files are uploaded oldest-first.
- No new archive is created while any completed pending archive exists.
- Temporary export workspaces and partial archives are ignored and cleaned only under documented safe rules.
- A completed archive is removed only after a collector 2xx response.
- A restart after remote commit but before local deletion causes an idempotent resend.
- Multiple completed files are preserved and drained rather than silently deleting newer recovery points.
- Logs report queue count, safe archive basename, age, and retry class without paths containing private configuration.

The collector token becomes operationally mandatory whenever scheduled export is enabled, reconciling code validation with the deployment runbook.

## Monitoring

The existing VPS Uptime Kuma deployment will monitor the collector's public `/readyz` endpoint over trusted HTTPS.

Monitoring must alert on:

- TLS or routing failure;
- collector process unavailability;
- no successful recovery point within 26 hours after the startup grace period;
- encrypted spool objects pending beyond the configured threshold;
- persistent local spool errors.

Collector logs additionally classify:

- authentication failures;
- request-limit failures;
- archive-validation failures;
- encryption failures;
- Borg transport failures;
- remote archive-verification failures;
- background retry success and failure.

Logs never include authorization headers, token values or hashes, request bodies, database URLs, SSH private material, private repository locations, age private identities, or Borg command output that may contain private endpoints.

Railway monitoring must alert on collector 4xx/5xx responses and `/data/exports` queue growth. Because the scheduler intentionally suppresses new recovery points while a pending queue exists, missed-recovery-point alerting is mandatory.

## Offline Verification Command

The collector binary provides an offline verification command for a decrypted archive:

```text
collector verify <archive-path>
```

It applies the exact ingestion archive validator and prints only:

- overall validity;
- total artifact count;
- total verified bytes;
- manifest-final status;
- safe failure classifications.

It does not print repository names, database contents, manifest payloads, credentials, or archive data.

## Independent Recovery

Recovery must work while Railway and the collector are unavailable.

1. From a trusted recovery environment, use the recovery-only Storage Box/Borg identity.
2. List Borg archives and select the exact approved object ID.
3. Extract the `.tar.gz.age` payload.
4. Decrypt it with the offline age private identity.
5. Run `collector verify` on the decrypted archive.
6. Independently verify the final manifest and all size/SHA-256 values before import.
7. Create sibling PostgreSQL and registry services and new volumes.
8. Restore `postgres.dump` without putting credentials in process arguments.
9. Recreate each bare Git repository from its bundle with mirror semantics.
10. Run `registry audit` as the actual runtime UID/GID.
11. Test health, login, search, detail, and HTTPS clone.
12. Perform a bounded secret-safe log review.
13. Leave recovery resources intact until separately approved cleanup.

A failed validation, missing member, extra member, non-final manifest, checksum mismatch, decryption failure, or Borg inconsistency aborts recovery before any import.

## Testing Strategy

Implementation follows test-first development.

### Unit and component tests

- configuration success and every unsafe partial state;
- secret-file type and permission validation;
- bearer authentication and constant-time hash comparison path;
- method, content type, length, size, and concurrency limits;
- secret-safe responses and logs;
- tar path traversal, duplicate, unsupported type, and trailing-data rejection;
- required PostgreSQL and Git artifact rules;
- manifest-final enforcement;
- size and checksum verification;
- deterministic object ID generation;
- idempotent replay behavior;
- real age encrypt/decrypt round trip using temporary test identities;
- atomic spool transitions and stale-partial cleanup;
- startup encrypted-spool discovery;
- ledger atomicity and reconciliation;
- Borg timeout, cancellation, redaction, create, and exact-presence behavior.

### Integration tests

- local Borg repository create and exact-list verification;
- collector HTTP upload through a real server with a valid SherpA fixture;
- backend failure leaves only encrypted retry state;
- process restart drains an encrypted pending object;
- duplicate request after simulated lost response returns the same object ID;
- registry restart discovers `/data/exports` files and drains oldest-first;
- registry restart after remote success safely resends and removes the local archive.

### Container and deployment tests

- production image build;
- non-root PID 1;
- read-only root filesystem;
- no Linux capabilities;
- writable persistent `/data` only;
- secret files absent from image history and metadata;
- `docker compose config` validation;
- Traefik trusted certificate and routing;
- unauthenticated upload rejection before body read;
- real append-only Storage Box upload;
- independent Borg retrieval and age decryption;
- final manifest verification.

## Rollout and Approval Gates

1. Implement collector and registry retry behavior with test-first development.
2. Run formatting, unit, integration, race, vet, build, image smoke, and Compose validation gates.
3. Commit the exact source candidate.
4. Create the dedicated Storage Box sub-account and directory.
5. Generate separate routine append-only and recovery identities.
6. Generate the offline age identity and place only its public recipient on the VPS.
7. Verify Storage Box SSH host keys out of band and pin them.
8. Initialize and test the Borg repository.
9. Build the exact source commit on the VPS.
10. Start Compose and verify trusted HTTPS, `/healthz`, `/readyz`, authentication, and request bounds.
11. Add the Uptime Kuma readiness monitor.
12. Obtain fresh explicit approval before changing Railway registry variables or causing a registry restart.
13. Configure `/data/exports`, collector URL, mandatory token, and the normal export interval.
14. Obtain fresh explicit approval before temporarily setting the export interval to one minute.
15. Upload one complete archive and establish its exact safe object ID.
16. Retrieve the exact object independently, decrypt it, and verify every member and the final manifest.
17. Obtain separate explicit approval before creating sibling Railway recovery services, volumes, or domain.
18. Restore, audit, and run recovery smoke tests.
19. Restore the normal export interval if it was changed.
20. Do not delete recovery resources until a later, separately approved cleanup.

No step modifies Railway production.

## Evidence Rules

Acceptance evidence may record:

- source commit;
- collector image ID;
- Compose project and container status;
- public certificate issuer and validity classification;
- safe object ID;
- compressed and encrypted sizes;
- upload and retrieval timestamps;
- manifest artifact count and verification result;
- Borg archive presence result;
- recovery service, deployment, and volume IDs;
- pass/fail classifications.

Evidence must not record:

- bearer tokens or hashes;
- Storage Box username, hostname, repository URL, password, or private key;
- age private identity;
- database URLs;
- OAuth secrets, access tokens, device codes, grants, CSRF values, or cookies;
- request bodies or manifest contents;
- private archive payloads;
- private collector configuration paths or prior collector URLs.

## Acceptance Criteria

The collector subsystem is accepted only when all of the following are true:

- The exact source candidate passes all local and container verification gates.
- Docker Compose runs the collector under `/docker/SherpA-collector` as a hardened non-root service.
- `sherpa-collector.kruth-support.de` presents trusted HTTPS through Traefik.
- Invalid authentication and invalid request shapes are rejected before body processing.
- A complete registry export is validated, age-encrypted, and committed to append-only Borg.
- The HTTP success response is emitted only after exact remote presence verification.
- A duplicate upload returns the same object ID without a duplicate recovery point.
- Collector restart preserves and retries encrypted pending state.
- Railway registry restart preserves and retries `/data/exports` state.
- The exact object is retrievable without Railway or the running collector.
- The offline age identity decrypts the object.
- The final manifest and every artifact size and SHA-256 verify.
- The sibling-resource restore, audit, login, search, detail, and clone checks pass.
- Uptime Kuma detects stale or unavailable collector readiness.
- Logs and evidence pass the secret-safety review.
- Railway production remains empty and unchanged.
