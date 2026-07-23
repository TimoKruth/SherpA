# Off-Site Collector Operations Runbook

This runbook covers the SherpA off-site collector, its dedicated Storage Box repository,
retention, monitoring, incident response, and independent recovery. It contains no live
credentials, private Storage Box identifiers, repository locations, or key material.

## Append-Only Capability Result and Reduced-Model Decision

The strict Task 0 capability result remains **Blocked**:

- the routine identity was unable to mutate the sacrificial repository through SFTP, SCP, or
  rsync;
- a routine `borg compact` removed zero segment files, reclaimed zero bytes, and appended
  bookkeeping only;
- an independent recovery-identity download could be rolled back offline to the trusted
  append-only transaction, restoring the original archive byte-for-byte;
- nevertheless, the routine identity could logically delete an archive from the current
  manifest and create different content under the same archive name.

That final behavior still fails the original requirement that the routine identity cannot delete or
recreate an archive. The current governing decision is **Reduced model approved**: the user
explicitly selected the implementation plan's third outcome. This clears only the threat-model
decision gate. It does not turn the strict capability result into a pass and authorizes no Railway,
VPS, Storage Box, repository, deployment, restart, drill, maintenance, cleanup, outage, rollback,
or other external action by itself.

The approved reduced model accepts these compensating controls together:

- forced BorgBackup 1.4 append-only routine access restricted to one dedicated repository;
- no routine SFTP, SCP, or rsync mutation path;
- the tested routine compact reclaimed no segments or bytes;
- offline recovery-identity download plus transaction rollback recovered the original bytes;
- the maximum practical automatic Storage Box snapshots as secondary, non-WORM protection; and
- an offline recovery SSH identity and offline age private identity.

This model does **not** provide immutable archive names. The routine identity can remove an archive
from the current manifest view and reuse its name for different content. Operators must therefore
review the expected ledger, deletion markers, transaction history, snapshots, and any archive-name
reuse anomaly before every unrestricted maintenance operation. Stop and investigate any mismatch
before prune, compact, repair, or another unrestricted write.

The capability exercise did **not** prove native undelete, in-place remote recovery, or a complete
repair/prune/compact cycle followed by restoration of append-only routine access. Its recovery proof
was offline rollback of a recovery-identity repository download. Do not describe snapshots as WORM,
do not claim immutable archive names, and do not claim that maintenance lifecycle was tested.

Every existing fresh approval checkpoint remains mandatory. In particular, Task 13 still requires
separate fresh approval before authoritative repository initialization or any VPS work; Railway
work and restarts, drills, recovery resources, prune/compact, cleanup, outage, and rollback retain
their own approvals. At publication time, production remains empty and untouched: the authoritative
repository is uninitialized and the collector is not deployed.

## Candidate Binding

The last accepted collector code and image lineage remains:

```text
0350c1d68972cda8a640cb6c6f64c18a3a6d00be
```

Task 12 historically passed exact repository commit
`e121df2623de4c2a1f52a1afdcd1155b0c518c2f`. This approval documentation changes `HEAD`, so that
Task 12 result is historical evidence, not the final candidate. Rerun Task 12 on the new exact
commit and bind that passing commit before Task 13 or any live acceptance. Do not call `e121df2`
the final candidate afterward, and do not substitute the older code/image lineage for the required
new exact-commit gate. Never build from an uncommitted working tree or infer the candidate from a
branch name.

## Storage Box Sub-Account

Create the authoritative resources only after Task 12 has been rerun on the new exact commit and Task 13 receives its separate fresh deployment approval. The reduced-model decision alone authorizes neither initialization nor provisioning.

- Use a dedicated Storage Box sub-account and a dedicated repository path used only by SherpA.
- Do not share the sub-account with unrelated backups, interactive users, or automation.
- Disable every protocol and permission that is not required by the approved Borg restriction.
- Record the owner, plan, capacity, snapshot capability, and recovery custodian in a restricted
  operator record. Do not place the username, hostname, repository URL, or password in Git,
  tickets, logs, or command output.
- Keep the repository outside Railway and outside the VPS filesystem. The VPS `/data` bind is a
  retry buffer, not the authoritative recovery store.

Before any repository write, repeat the capability test against a sacrificial path owned by the
same sub-account and configured by the same provider mechanism. A passing test against a
materially different account or restriction is not evidence for the authoritative repository.

## Routine and Recovery SSH Identities

Use separate identities with separate custody:

- **Routine identity:** the only Storage Box private key persistently mounted on the collector
  VPS. Force BorgBackup 1.4 append-only service access and restrict it to the one approved
  repository. It must have no unrestricted shell, SFTP, SCP, rsync, repository-initialization, or
  unrestricted-maintenance role. This restriction still permits logical Borg archive deletion and
  archive-name reuse in the current manifest view; never describe it as immutable-name protection.
- **Recovery identity:** unrestricted only to the dedicated recovery repository as needed for
  initialization, inspection, extraction, approved retention, and recovery. Keep it off
  Railway and off the persistent VPS. Load it temporarily only in a trusted recovery
  environment.

Generate both keypairs with a documented modern algorithm and a local `umask 077`. Never print
private keys. Store public-key fingerprints, not private material, in the restricted operator
record. Revoke and replace a routine key immediately after suspected VPS compromise. Any use of
the recovery key requires an operator change record and fresh approval for destructive work.

## Offline age Identity

Generate the age X25519 identity on a trusted recovery machine. Keep the private identity
password-manager- or hardware-backed, offline from Railway and the collector VPS, with at least
one independently controlled recovery copy. Place only the public recipient in:

```text
/docker/SherpA-collector/config/age-recipient
```

The collector rejects an age private identity in that file. Rotation creates a new encryption
epoch: deploy the new public recipient for future objects, retain every old private identity
until all archives encrypted to it have expired and an approved recovery drill has passed, and
record the epoch boundaries without recording identity material.

## Host-Key Pinning

Storage Box SSH uses port 23. Before the first connection:

1. obtain the provider-published SSH host-key fingerprints through an authenticated, independent
   channel;
2. collect the offered keys from a trusted workstation;
3. compare every accepted key's SHA-256 fingerprint to the provider publication;
4. write only matched keys to a dedicated `known_hosts` file; and
5. use that file with `StrictHostKeyChecking=yes` for every routine and recovery connection.

The final VPS file is:

```text
/docker/SherpA-collector/config/known_hosts
```

A host-key change is an incident. Stop connections, verify the new fingerprint out of band, and
replace the pin only after the provider change is authenticated. Never bypass the mismatch with
`StrictHostKeyChecking=no`, a global known-hosts file, or an interactive acceptance prompt.

## Borg Initialization

The reduced-model decision clears the threat-model choice but does not authorize initialization.
Proceed only after the new exact commit passes Task 12 and Task 13 receives separate fresh approval.
Initialize with the recovery identity, never the routine identity. The repository is unencrypted at
the Borg layer because every payload is already encrypted with the offline age recipient.

Prepare a mode-0700 recovery directory containing mode-0600 files for the repository location,
recovery SSH key, and pinned known hosts. Every initialization, retention, list, check, and extract
command must isolate all Borg private state inside that workspace; default home cache, config, and
security locations are prohibited. Then load private values without printing them:

```bash
umask 077
RECOVERY_ROOT="<trusted-mode-0700-recovery-directory>"
install -d -m 0700 "$RECOVERY_ROOT" \
  "$RECOVERY_ROOT/borg-cache" \
  "$RECOVERY_ROOT/borg-config" \
  "$RECOVERY_ROOT/borg-security"
chmod 0600 "$RECOVERY_ROOT/borg-repository" \
  "$RECOVERY_ROOT/recovery-ssh-key" \
  "$RECOVERY_ROOT/known_hosts"
export BORG_CACHE_DIR="$RECOVERY_ROOT/borg-cache"
export BORG_CONFIG_DIR="$RECOVERY_ROOT/borg-config"
export BORG_SECURITY_DIR="$RECOVERY_ROOT/borg-security"
export BORG_REPO="$(<"$RECOVERY_ROOT/borg-repository")"
export BORG_RSH="ssh -i $RECOVERY_ROOT/recovery-ssh-key -o IdentitiesOnly=yes -o UserKnownHostsFile=$RECOVERY_ROOT/known_hosts -o StrictHostKeyChecking=yes -p 23"
export BORG_UNKNOWN_UNENCRYPTED_REPO_ACCESS_IS_OK=yes
borg --version
borg init --encryption=none "$BORG_REPO"
borg check --repository-only
```

`BORG_UNKNOWN_UNENCRYPTED_REPO_ACCESS_IS_OK=yes` is the expected, scoped, noninteractive
confirmation for this deliberately unencrypted Borg repository. Set it only after the pinned
repository location and host key have been checked, never use a broad interactive auto-confirmation,
and unset it with the other Borg variables when the operation ends. Require BorgBackup 1.4.5
compatibility. After initialization, install the routine public key with the exact tested
provider-supported restriction accepted by the explicit reduced-model decision. The strict
capability result remains **Blocked** because logical archive deletion and archive-name reuse remain
possible. Reproduce the accepted reduced-model baseline against a sacrificial archive before
allowing collector writes. Do not initialize first and promise to validate the restriction later.

The approved reduced model requires the exact forced BorgBackup 1.4 append-only restriction and all
listed compensating controls. Before initialization, confirm that the separate Task 13 approval
explicitly accepts archive-name mutability and the untested maintenance lifecycle. Snapshots and
offline transaction rollback must never be described as WORM, native undelete, or immutable
archive-name protection.

## `/docker/SherpA-collector` Ownership and Modes

Use this mirrored deployment layout:

```text
/docker/SherpA-collector/
├── compose.yaml
├── runtime.env
├── source/
├── config/
│   ├── age-recipient
│   └── known_hosts
├── secrets/
│   ├── upload-token
│   ├── storage-ssh-key
│   └── borg-repository
└── data/
    ├── spool/
    ├── state/
    └── borg/
        ├── cache/
        ├── config/
        └── security/
```

Apply and verify these rules before startup:

- deployment root and `config/`: mode `0750`;
- `secrets/`: mode `0700`;
- `runtime.env`: mode `0600`;
- secret files: regular, nonsymlink, mode `0400`, readable by UID/GID `10001:10001`;
- public configuration files: regular, nonsymlink, read-only to the container;
- `data/` and every runtime-owned child: mode `0700`, UID/GID `10001:10001`;
- `compose.yaml` and the source checkout are not writable by UID/GID 10001;
- no socket, device, FIFO, symlink, or unexpected file is accepted in place of a configured file
  or runtime directory.

The container must remain UID/GID `10001:10001`, read-only at its root filesystem, with all
capabilities dropped and `no-new-privileges:true`. Do not add a host port, FUSE, privileged mode,
`cap_add`, or a device mount.

## Exact-Source Build and Rollback

Task 12 supplies the final `SOURCE_REVISION`. On the VPS, check out that exact commit in detached
state and require a clean source tree:

```bash
SOURCE_REVISION="<Task-12-approved-full-commit>"
git -C /docker/SherpA-collector/source checkout --detach "$SOURCE_REVISION"
test "$(git -C /docker/SherpA-collector/source rev-parse HEAD)" = "$SOURCE_REVISION"
test -z "$(git -C /docker/SherpA-collector/source status --porcelain)"
```

Before deployment, run the exact-source smoke from the approved checkout and record only its safe
image ID, platform, OCI revision, Borg version, and pass/fail classifications. Copy the reviewed
`deploy/collector/compose.yaml` and `runtime.env.example` shape into the mirrored root, then build
and start with the same full revision:

```bash
cd /docker/SherpA-collector/source
bash deploy/collector/smoke_build.sh
cd /docker/SherpA-collector
SOURCE_REVISION="$SOURCE_REVISION" docker compose build --pull
SOURCE_REVISION="$SOURCE_REVISION" docker compose up -d
docker compose ps
```

Require the built image's OCI revision to equal `SOURCE_REVISION`. Do not deploy a branch tip,
short hash, locally modified checkout, or image whose revision cannot be proven.

For rollback, obtain fresh approval, select a previously accepted exact collector revision, and
retain `data/`, configuration, and every remote archive. The reduced-model decision does not
include rollback or VPS authorization; the rollback approval must explicitly cover that work.

```bash
cd /docker/SherpA-collector
ROLLBACK_REVISION="<previous-accepted-full-commit>"
test "${#ROLLBACK_REVISION}" -eq 40
case "$ROLLBACK_REVISION" in (*[!0-9a-f]*) exit 2;; esac

docker compose down
git -C source checkout --detach "$ROLLBACK_REVISION"
ACTUAL_REVISION="$(git -C source rev-parse HEAD)"
test "$ACTUAL_REVISION" = "$ROLLBACK_REVISION"
test -z "$(git -C source status --porcelain=v1 --untracked-files=all)"

cd source
SOURCE_REVISION="$ROLLBACK_REVISION" bash deploy/collector/smoke_build.sh
cd ..
SOURCE_REVISION="$ROLLBACK_REVISION" docker compose build --pull
ROLLBACK_IMAGE_ID="$(SOURCE_REVISION="$ROLLBACK_REVISION" docker compose images -q collector)"
test -n "$ROLLBACK_IMAGE_ID"
test "$(docker image inspect "$ROLLBACK_IMAGE_ID" --format '{{ index .Config.Labels "org.opencontainers.image.revision" }}')" = "$ROLLBACK_REVISION"
SOURCE_REVISION="$ROLLBACK_REVISION" docker compose up -d
docker compose ps
```

Stop before building if the detached checkout contains any tracked or untracked change. The smoke
build and its gates must pass from that exact clean checkout, and the final image's OCI revision
must equal the intended rollback commit before deployment. Rollback must not delete the spool,
ledger, Borg state, Storage Box repository, or snapshots. If
the previous image cannot read the current durable local state, stop and use an explicit recovery
plan instead of discarding data.

## Traefik and Certificate Checks

Traefik is the only public listener. The collector service exposes container port 8080 but has no
host-published port. Verify the rendered Compose labels still select `websecure`, TLS, the
existing `letsencrypt` resolver, and service port 8080.

After startup, run secret-free checks:

```bash
docker compose ps
collector_id="$(docker compose ps -q collector)"
test -n "$collector_id"
docker inspect "$collector_id" --format '{{json .HostConfig.PortBindings}}'
curl --proto '=https' --tlsv1.2 --fail --silent --show-error https://sherpa-collector.kruth-support.de/healthz
curl --proto '=https' --tlsv1.2 --fail --silent --show-error https://sherpa-collector.kruth-support.de/readyz
openssl s_client -connect sherpa-collector.kruth-support.de:443 -servername sherpa-collector.kruth-support.de </dev/null 2>/dev/null \
  | openssl x509 -noout -issuer -subject -dates -fingerprint -sha256
```

Require empty host port bindings, trusted hostname validation, a currently valid certificate,
`/healthz` liveness, and `/readyz` readiness. A healthy `/healthz` with failing `/readyz` is not a
successful recovery posture.

## Uptime Kuma Readiness Monitor

Configure the existing independent Uptime Kuma installation to monitor:

```text
https://sherpa-collector.kruth-support.de/readyz
```

Use HTTPS certificate validation, a 30- or 60-second interval, multiple consecutive failures
before notification, and the approved on-call route. Alert on TLS/routing failure, non-2xx
readiness, stale successful recovery points, pending encrypted spool state beyond the configured
threshold, and terminal local state. Test notification routing without interrupting unrelated
VPS services. Railway's deploy healthcheck and Docker's `/healthz` check are not substitutes for
this continuous readiness monitor.

## Railway Export and Queue Monitoring

The registry must use exactly:

```text
SHERPA_EXPORT_URL=https://sherpa-collector.kruth-support.de/v1/exports
SHERPA_EXPORT_ARCHIVE_DIR=/data/exports
SHERPA_EXPORT_TOKEN=<Railway secret; never print or persist outside approved secret stores>
```

Keep one registry replica. `/data/exports` is a persistent, entrypoint-prepared, mode-0700 queue
owned by the registry UID/GID. No sidecar, shell, job, or second registry process may access it as
the registry identity while the scheduler owns its lifetime lock.

Alert on:

- any collector 4xx or 5xx response classification in registry logs;
- scheduler retry classifications or scheduler fatal exit;
- a completed archive remaining in `/data/exports` after the approved interval plus tolerance;
- queue count greater than zero across a registry restart;
- queue bytes growing or `/data` capacity approaching the approved threshold;
- no new verified collector recovery point within 26 hours;
- collector `/readyz` failure even when registry `/healthz` is healthy.

The scheduler suppresses creation of a new archive while an older completed archive is pending.
A queue count of one can therefore mean the recovery point is no longer advancing; do not alert
only on large file counts.

## Storage Box Snapshots

After the production posture is approved, enable the maximum practical automatic snapshot
frequency and retention count supported by the selected Storage Box plan. Record the effective
schedule and periodically prove that an authorized recovery operator can restore a snapshot to a
separate location.

Snapshots are secondary protection only. They are not independent WORM storage, do not replace
the authoritative 30-daily/12-monthly Borg policy, and do not repair the Task 0 archive-name reuse
failure. Never delete a snapshot or reduce its schedule as part of routine retention without
fresh approval.

## Secret Rotation

Never print old or new values. Use mode-0600 temporary files and remove them after validation.

- **Upload bearer token:** create a new random value in an approved secret manager. Update the
  Railway secret and the collector secret in a bounded coordinated change. A temporary mismatch
  is expected to leave the archive safely in `/data/exports`; verify a successful stored/existing
  response and queue removal before deleting the old value.
- **Routine SSH key:** stop collector writes and install a new public key with the exact tested
  restriction accepted by the reduced-model decision. On a sacrificial repository, reproduce the
  accepted baseline exactly: routine create/list works; logical delete and archive-name reuse remain
  an acknowledged limitation; SFTP, SCP, and rsync mutation is denied; routine compact reclaims no
  segments or bytes; and offline recovery-identity download plus transaction rollback recovers the
  original bytes. Any deviation blocks rotation. Only after every expected outcome is reproduced,
  replace `secrets/storage-ssh-key`, restart, prove exact remote presence, and revoke the old key.
- **Recovery SSH key:** perform a separately approved recovery-credential change from a trusted
  environment, test read-only list/extract first, and remove all temporary copies. Never place it
  on the VPS.
- **age identity:** deploy only the new public recipient. Retain old private identities through
  the final retained archive encrypted to each one and complete a recovery drill for the new
  epoch before considering old-key retirement.
- **Host keys:** rotate pins only after out-of-band provider verification; treat an unexplained
  change as an incident.

After every rotation, scan bounded logs and image/container metadata for prohibited values without
printing matches.

## Incident Handling

### Append-only or archive-history anomaly

Stop collector writes and all unrestricted Borg operations. Preserve the repository, snapshots,
VPS data, ledger, and logs. Do not prune, compact, repair, or reuse an archive name. Compare the
accepted object ledger, current archive list, provider transaction evidence, and snapshots from a
trusted recovery environment. Escalate any missing expected archive, unexpected deletion marker,
name reuse, or unexplained transaction rollback requirement.

### Collector unavailable or `/readyz` failing

Leave the registry queue intact. Check `/healthz`, container state, spool capacity, ledger
readability, Borg transport classification, host-key status, and Storage Box availability. Do not
delete encrypted spool objects or `/data/exports` archives to make readiness green. Restore the
exact accepted image/configuration or execute the rollback procedure while preserving data.

### Suspected VPS or routine-key compromise

Block public upload if necessary, revoke the routine Storage Box key, preserve forensic copies,
and use the recovery identity only from a clean trusted environment. Treat archive history after
the earliest possible compromise as untrusted until independently reconciled. Rotate the upload
token and routine key before reopening.

### Suspected age private-identity compromise

Assume confidentiality of every archive encrypted to that identity may be lost. Preserve evidence,
create a new offline identity, deploy only its public recipient, and obtain a security decision on
old archive handling. Do not destroy old recovery data as an automatic response.

### Capacity or queue growth

Stop creation of new recovery points only through an approved registry change; never manually
remove the pending archive. Increase capacity or restore remote commit capability, then let the
normal exact stored/existing response path remove local state.

## Retention Maintenance

The collector never prunes, deletes, or compacts. Retention uses temporary recovery credentials
from a trusted environment and always requires fresh approval for unrestricted writes.

### 1. Private preflight and deletion-marker check

The threat-model gate was cleared only by explicit approval of the reduced model; that approval does
not authorize maintenance. Obtain the procedure's fresh unrestricted-write approval after the
mandatory anomaly review below. Create the private Borg state directories for this maintenance
session before the first Borg command:

```bash
umask 077
RECOVERY_ROOT="<trusted-mode-0700-recovery-directory>"
install -d -m 0700 "$RECOVERY_ROOT" \
  "$RECOVERY_ROOT/borg-cache" \
  "$RECOVERY_ROOT/borg-config" \
  "$RECOVERY_ROOT/borg-security"
chmod 0600 "$RECOVERY_ROOT/borg-repository" \
  "$RECOVERY_ROOT/recovery-ssh-key" \
  "$RECOVERY_ROOT/known_hosts"
export BORG_CACHE_DIR="$RECOVERY_ROOT/borg-cache"
export BORG_CONFIG_DIR="$RECOVERY_ROOT/borg-config"
export BORG_SECURITY_DIR="$RECOVERY_ROOT/borg-security"
export BORG_REPO="$(<"$RECOVERY_ROOT/borg-repository")"
export BORG_RSH="ssh -i $RECOVERY_ROOT/recovery-ssh-key -o IdentitiesOnly=yes -o UserKnownHostsFile=$RECOVERY_ROOT/known_hosts -o StrictHostKeyChecking=yes -p 23"
export BORG_UNKNOWN_UNENCRYPTED_REPO_ACCESS_IS_OK=yes
```

The scoped unknown-unencrypted-repository confirmation is expected because age, not Borg, encrypts
payloads. Do not continue if the pinned repository or host identity differs from the approved
operator record.

1. Load the accepted object ledger and prior evidence from mode-0600 files without printing private
   values.
2. Save `borg list --json` to a mode-0600 file inside `RECOVERY_ROOT` and compare the complete
   archive set with the accepted ledger.
3. Treat any expected archive missing from the current list, any canonical archive name mapped to
   unexpected content/metadata, any provider or transaction-history deletion marker, or any
   unexplained archive-name reuse as an unexpected deletion-marked archive.
4. Run a read-only repository consistency check. Stop on any discrepancy.
5. Take or confirm a current secondary Storage Box snapshot and preserve the pre-maintenance list.

Do not perform any unrestricted write if an unexpected deletion-marked archive or unexplained
history discrepancy exists. Investigate and recover first. The Task 0 exercise did not prove an
in-place repair/prune/compact workflow or restoration of append-only protection.

### 2. Dry-run before approval

Run exactly:

```bash
borg prune --dry-run --list --glob-archives 'sherpa-*' --keep-daily 30 --keep-monthly 12
```

Capture a secret-safe summary of archive counts and dates, not repository locations or archive
payload data. Reconcile the proposed removals with the approved ledger and retention policy.

### 3. Fresh approval and maintenance

Present the exact proposed retained/removed archive counts, snapshot status, repository identity
by non-secret operator label, expected impact, rollback/recovery plan, and temporary credentials in
use. Only after fresh explicit approval run:

```bash
borg prune --list --glob-archives 'sherpa-*' --keep-daily 30 --keep-monthly 12
borg compact
borg check --verify-data
```

Stop immediately on any command failure. Do not improvise repair. List the retained archives and
compare them to the approved dry-run result.

### 4. Restore restrictions and remove credentials

Reapply the exact approved forced BorgBackup 1.4 append-only routine restriction and repeat the
sacrificial capability checks before restarting collector writes. The first repair/prune/compact
cycle followed by restoration of routine access remains an untested maintenance lifecycle: do not
record it as proven merely because the original compact reclaimed no bytes or offline rollback
worked. Stop if archive history, name mapping, restriction behavior, or recovery evidence differs
from the approved reduced-model record.

After verification, unset `BORG_REPO`, `BORG_RSH`, `BORG_CACHE_DIR`, `BORG_CONFIG_DIR`,
`BORG_SECURITY_DIR`, and `BORG_UNKNOWN_UNENCRYPTED_REPO_ACCESS_IS_OK`, and terminate the recovery
SSH agent if used. Then present the exact temporary workspace and artifacts, impact, and retention
fallback and obtain a separate fresh cleanup approval. Only after that approval securely remove
temporary key, repository, cache, config, security, list, and recovery files and confirm the
recovery private key is absent from the VPS, containers, shell history, logs, and evidence. Without
cleanup approval, retain the mode-0700 workspace under an assigned custodian; do not delete it.

## Independent Retrieval, Decryption, and Verification

Recovery must work without Railway and without the running collector. Use a trusted recovery
machine with the recovery SSH identity, pinned host keys, the required offline age identity, and
the exact accepted collector binary.

The threat-model gate was cleared only by the explicit reduced-model decision, which does not
authorize a restore. The restore/recovery operation must have its own fresh approval before this
procedure. Create a private recovery workspace and select the required object through the restricted
local ledger. Keep the object identifier only in this mode-0700 workspace for command selection and
comparisons; do not copy it into acceptance evidence:

```bash
umask 077
RECOVERY_ROOT="<trusted-mode-0700-recovery-directory>"
install -d -m 0700 "$RECOVERY_ROOT" \
  "$RECOVERY_ROOT/borg-cache" \
  "$RECOVERY_ROOT/borg-config" \
  "$RECOVERY_ROOT/borg-security"
chmod 0600 "$RECOVERY_ROOT/borg-repository" \
  "$RECOVERY_ROOT/recovery-ssh-key" \
  "$RECOVERY_ROOT/known_hosts" \
  "$RECOVERY_ROOT/age-identity"
OBJECT_HEX="<64-lowercase-hex-object-digest>"
[[ "$OBJECT_HEX" =~ ^[0-9a-f]{64}$ ]]
export BORG_CACHE_DIR="$RECOVERY_ROOT/borg-cache"
export BORG_CONFIG_DIR="$RECOVERY_ROOT/borg-config"
export BORG_SECURITY_DIR="$RECOVERY_ROOT/borg-security"
export BORG_REPO="$(<"$RECOVERY_ROOT/borg-repository")"
export BORG_RSH="ssh -i $RECOVERY_ROOT/recovery-ssh-key -o IdentitiesOnly=yes -o UserKnownHostsFile=$RECOVERY_ROOT/known_hosts -o StrictHostKeyChecking=yes -p 23"
export BORG_UNKNOWN_UNENCRYPTED_REPO_ACCESS_IS_OK=yes
cd "$RECOVERY_ROOT"
borg list --json >archives.json
chmod 0600 archives.json
borg extract "::sherpa-$OBJECT_HEX" "$OBJECT_HEX.tar.gz.age"
chmod 0600 "$OBJECT_HEX.tar.gz.age"
age --decrypt --identity "$RECOVERY_ROOT/age-identity" \
  --output "$RECOVERY_ROOT/archive.tar.gz" \
  "$RECOVERY_ROOT/$OBJECT_HEX.tar.gz.age"
chmod 0600 "$RECOVERY_ROOT/archive.tar.gz"
collector verify "$RECOVERY_ROOT/archive.tar.gz"
```

The scoped `BORG_UNKNOWN_UNENCRYPTED_REPO_ACCESS_IS_OK=yes` handles the expected confirmation for
this deliberately unencrypted repository without an interactive prompt and without writing Borg
security metadata to a default home location. Abort on any unexpected repository identity prompt.

Require `collector verify` to report valid, the expected artifact count/verified byte summary, and
`manifest_final=true`. It must not print member names or payloads. Independently verify the final
manifest and every artifact size and SHA-256 before importing anything. Abort on missing or extra
members, trailing data, checksum/size mismatch, decryption failure, or Borg inconsistency.

Proceed to the sibling Postgres/Git restore in `docs/deployment/railway.md` only after these checks
pass. Keep encrypted and decrypted recovery material mode `0600` inside a mode-0700 workspace and
retain it until cleanup receives separate approval.

## Secret-Safe Evidence

Evidence may contain only:

- exact public source commit and image ID;
- platform, Borg version, Compose checksum, and safe configuration names;
- public collector hostname, certificate classification, and health/readiness results;
- append-only decision classification and the non-secret facts supporting it;
- non-sensitive stored/existing, byte-identity, extraction, decryption, and verification summaries,
  but not archive/object IDs;
- compressed/encrypted sizes, timestamps, archive counts, and pass/fail classifications;
- recovery service/deployment/volume IDs and aggregate audit results.

Evidence must never contain bearer tokens or hashes, Storage Box account/hostname/repository
values, SSH or age private material, public-key bodies, database URLs, OAuth/session/grant/CSRF
values, request bodies, manifests, member names, archive contents, private collector paths, raw
Borg output containing endpoints, or unfiltered logs. Store detailed local evidence mode `0600`,
derive a bounded summary, and delete temporary material only after separately approved cleanup.
