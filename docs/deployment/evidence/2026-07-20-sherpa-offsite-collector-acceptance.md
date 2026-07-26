# SherpA Off-Site Collector Acceptance Evidence

## Metadata

| Field | Value |
|---|---|
| Task | Task 13 — provision and deploy the external collector |
| Plan | `docs/superpowers/plans/2026-07-20-sherpa-offsite-collector.md` |
| Branch | `feat/offsite-collector` |
| Exact source candidate | `3c809236fec0b9ced8e6fd2003271adbb4dbc644` |
| Local release gate | Task 12 rerun passed on this exact commit |
| Deployment root | `/docker/SherpA-collector` on the authorized VPS |
| Public hostname | `https://sherpa-collector.kruth-support.de` |
| Operator | Timo Kruth |
| Deployment UTC | Container created and started `2026-07-23T11:50:14Z` |
| Acceptance verification UTC | `2026-07-23T12:00Z` – `2026-07-23T12:20Z` |
| Final status | **Accepted with recorded deviations.** Candidate binding, container posture, filesystem posture, secret provenance, DNS/TLS, health, readiness, pre-body controls, routine restriction verification, and readiness-failure *detection* all pass. Three items did not pass and are recorded as deviations rather than counted as passes: alert *delivery* is not configured, the Storage Box snapshot schedule is unverified, and the runbook's pre-deployment VPS smoke could not be run on the VPS |

The plan document is dated 2026-07-20; deployment and acceptance were executed on
2026-07-23. This file keeps the plan-assigned filename.

## Pre-deployment approval and gate record

The deployment itself was performed by an earlier session that was stopped before it
wrote evidence. This file records what can be independently established now, and states
plainly what cannot.

| Item | Status | Detail |
|---|---|---|
| Fresh Task 13 deployment approval | Attested, not itemized here | Task 13 operated under explicit fresh approval for its defined deployment, Storage Box, TLS, pre-body, and non-disruptive monitoring operations. The interrupted session did not leave a record of the brief's Step 1 presentation — commit, Compose checksum, build inputs, Borg version, VPS path, Storage Box resources and append-only conclusion, hostname, and rollback plan — so that itemized approval exchange **cannot be reconstructed** and is not claimed here |
| Local exact-source release gate | Pass | The Task 12 rerun ran the full collector smoke on this exact commit for the default invocation and for explicit `linux/arm64` and `linux/amd64`, including both real Borg 1.4.5 integration tests, rendered Compose validation, exact OCI metadata, layer scans, recovery-archive verification, and hardened runtime checks |
| Pre-deployment smoke **on the VPS** | **Not satisfied** | The runbook directs `bash deploy/collector/smoke_build.sh` to be run from the approved checkout on the VPS before building. It was not established by the interrupted session and could not be run during acceptance either: the script requires a `go` toolchain on the host, and the VPS has none. Installing a toolchain on the VPS is outside this task's authorization. Compensating evidence is the gate above, run on the same exact commit, combined with the content-level image adjudication in the next section |
| Rollback posture | Available, not exercised | The runbook rollback procedure retains `data/`, configuration, and every remote archive. Executing it requires its own fresh approval and was not performed |

## Candidate binding

| Check | Result | Non-secret evidence |
|---|---|---|
| Deployed source revision | Pass | `git -C source rev-parse HEAD` = `3c809236fec0b9ced8e6fd2003271adbb4dbc644`, detached |
| Deployed source cleanliness | Pass | `git -C source status --porcelain=v1` empty at start and end of acceptance |
| Compose checksum | Pass | Installed `compose.yaml` SHA-256 `e3d78168d960d01c27202535f6a6a2e7e98b58c325ad040f481c8702cd6b41a6`, identical to the reviewed `deploy/collector/compose.yaml` recorded by the Task 12 rerun |
| Local repository state | Pass | Observed immediately before this file was written: local `HEAD` unchanged at the exact candidate, tracked tree and index clean, and only the three preserved `.DS_Store` paths untracked. This file is itself a new untracked path until it is committed through the normal review workflow |

## Deployed image and image-identity adjudication

The running image was rebuilt on the VPS from the exact checked-out commit rather than
transferred from the gate host. Its content-addressed image ID therefore differs from the
locally gated `linux/amd64` image ID, because this build is not bit-for-bit reproducible.
The difference was adjudicated by content comparison rather than accepted on the revision
label alone.

| Check | Result | Non-secret evidence |
|---|---|---|
| Live image ID | Recorded | `sha256:572947986ed158403bf6a0b53382c9c79a444efb1fe045fceb2912d6d5fdf460` |
| Locally gated `linux/amd64` image ID | Recorded | `sha256:43ac418750d26fceab576592f9f5c311bc7a91d3c4f0bf6a2dd114a71122d4be` |
| OCI revision label | Pass | Live image and container label `org.opencontainers.image.revision` = `3c809236fec0b9ced8e6fd2003271adbb4dbc644` |
| Platform | Pass | Live image `linux/amd64` |
| Collector binary identity | Pass | `/usr/local/bin/collector` SHA-256 `63d0152d9c0869cb5d7aeebf0c7a0017a20809db978daecf48844baf466d3463` — **byte-identical** in the live image and the locally gated `linux/amd64` image |
| Base layers | Pass | The first four `RootFS` layers are identical across both images |
| OS package manifest | Pass | `dpkg-query` manifests identical, 107 of 107 packages, same versions and architectures |
| Borg runtime tree | Pass with bounded difference | 615 files present in both, identical file sets, 605 byte-identical. The 10 differing files are 9 gcc-compiled Cython extension `.so` objects and the `dist-info/RECORD` that records their hashes |
| Native extension code identity | Pass | Every one of the 9 differing `.so` files was compared section by section. In all 9, `.text`, `.rodata`, `.data`, `.bss`, `.symtab`, `.dynsym`, `.dynstr`, `.rela.dyn`, `.rela.plt`, `.plt`, `.got`, `.init`, `.fini`, `.init_array`, `.fini_array`, `.gnu.hash`, `.eh_frame`, and `.dynamic` are **byte-identical**. Differences are confined to `.note.gnu.build-id` in all 9 files, `.debug_line_str` in all 9, and additionally `.debug_info` and `.debug_line` in one file |
| Borg version | Pass | `borg 1.4.5` reported from the live image, matching the pinned, SHA-256-verified 1.4.5 source tarball required by the Dockerfile |
| Image environment | Pass | Image `Config.Env` carries only `PATH`, Python base metadata, and the three `BORG_*` directory variables; no secret material |

**Adjudication:** the two images are not byte-identical, and this record does not claim
they are. What was established is narrower and sufficient: every executable and data
section of every native object is byte-identical, the Go collector binary is
byte-identical, the OS package set is identical, and the base layers are identical. The
only differing bytes are the linker-generated build-ID note and DWARF debug metadata,
neither of which is executed or consulted at runtime. The code that runs on the VPS is
therefore the same code that passed the Task 12 gate, built twice from the same pinned
source and build recipe.

A stronger guarantee — deploying the exact gated image rather than rebuilding — would
require a registry or image transfer path that this deployment does not use. That is a
design property of the current deployment, recorded here so a future operator does not
mistake matching revision labels alone for image identity.

## Container runtime posture

| Check | Result | Non-secret evidence |
|---|---|---|
| Service state | Pass | `running=true`, Docker healthcheck `healthy`, `RestartCount=0` |
| Published host ports | Pass | `HostConfig.PortBindings` = `{}` — no host port published |
| Traefik routing | Pass | Labels select `websecure`, `tls=true`, the existing `letsencrypt` resolver, and service port `8080` |
| Container user | Pass | `10001:10001` |
| Root filesystem | Pass | `ReadonlyRootfs=true`; an in-container write to `/` was refused with `Read-only file system` |
| Capabilities | Pass | `CapDrop=[ALL]`, `CapAdd=[]` |
| Privilege escalation | Pass | `Privileged=false`, `SecurityOpt=[no-new-privileges:true]` |
| Mount posture | Pass | All five configuration and secret binds are `rw=false`; only `/data` is writable |
| Runtime logs | Pass | Bounded log read produced no error output |

## Filesystem ownership and modes

| Path | Required | Observed | Result |
|---|---|---|---|
| Deployment root | `0750` | `0750 root:root` | Pass |
| `config/` | `0750` | `0750 root:root` | Pass |
| `secrets/` | `0700` | `0700 root:root` | Pass |
| `runtime.env` | `0600` | `0600 root:root` | Pass |
| `compose.yaml` | not writable by `10001` | `0640 root:root` | Pass |
| `source/` | not writable by `10001` | `0750 root:root` | Pass |
| Secret files | `0400`, UID/GID `10001` | `0400 10001:10001` for all three | Pass |
| Public config files | read-only, group-readable by GID `10001` | `0440 root:10001` for both | Pass |
| `data/` and children | `0700`, UID/GID `10001` | `0700 10001:10001` for `data`, `spool`, `state`, `borg`, `borg/{cache,config,security}` | Pass |
| No socket/device/FIFO/symlink substitution | required | no non-regular, non-directory entries and no symlinks found under `config/`, `secrets/`, `data/` | Pass |
| In-container readability | required | all three secret files and both configuration files readable as UID/GID `10001` at their mounted paths | Pass |

## Secret and configuration provenance

Classifications only. No token, hash, key body, recipient value, account, hostname, or
repository identifier is recorded.

| Check | Result | Non-secret evidence |
|---|---|---|
| Upload token provenance | Pass | Deployed secret matches the authoritative offline custody copy |
| Routine Storage Box key provenance | Pass | Deployed secret matches the authoritative offline custody routine identity |
| Borg repository descriptor provenance | Pass | Deployed secret matches the authoritative offline custody descriptor |
| age recipient provenance | Pass | Deployed configuration matches the authoritative custody recipient |
| age recipient is public-only | Pass | The deployed file is a public age recipient; it contains no private identity marker |
| Pinned known-hosts provenance | Pass | Deployed pin matches both the operator and recovery custody copies, which are identical to each other |
| Offline age private identity absent from the VPS | Pass | No file in the deployment tree matches the offline identity, and no private-identity marker appears in the deployment tree or the running container filesystem |
| Offline recovery SSH identity absent from the VPS | Pass | No file in the deployment tree matches the offline recovery identity |
| Custody modes | Pass | Custody directories `0700`; private identity and secret files `0600` |

## Public DNS, TLS, health, and readiness

| Check | Result | Non-secret evidence |
|---|---|---|
| Authoritative DNS | Pass | The zone's authoritative nameserver returns the authorized VPS address |
| Recursive DNS | Pass | Independent public resolver and system resolver both return the same authorized VPS address |
| Certificate issuer | Pass | Let's Encrypt, intermediate `CN=YR2` |
| Certificate subject | Pass | `CN=sherpa-collector.kruth-support.de` |
| Certificate validity | Pass | `notBefore=2026-07-23T10:51:58Z`, `notAfter=2026-10-21T10:51:57Z` — currently valid |
| Certificate fingerprint | Captured, not recorded here | The SHA-256 fingerprint was captured for substitution detection and is held in the restricted operator record. The runbook's evidence allowlist permits certificate *classification*, so the raw fingerprint is deliberately kept out of this file |
| Trusted chain validation | Pass | `curl` with default verification returned `ssl_verify_result=0` for both endpoints |
| `GET /healthz` | Pass | HTTP `200` |
| `GET /readyz` | Pass | HTTP `200` |

### Readiness semantics at acceptance time

`/readyz` reports ready only when the spool is writable, no terminal local error is
latched, no pending object is older than `SHERPA_COLLECTOR_MAX_RECOVERY_AGE`, and either
a recent successful recovery point exists or the process is still inside
`SHERPA_COLLECTOR_STARTUP_GRACE`.

At acceptance no export producer had been configured yet, so no recovery point exists
and the green result comes from the startup grace. With the configured `26h` grace and a
process start of `2026-07-23T11:50:14Z`, readiness turns red at approximately
`2026-07-24T13:50Z` unless a successful recovery point is committed first. This is the
designed behavior, not a defect, and it is the reason the readiness monitor is
meaningful.

## Pre-body controls

Every check was executed against the public endpoint over verified TLS. Each request
sent **request headers only and zero body bytes**; requests that declare a body used
`Expect: 100-continue` so the collector's rejection arrives before any body transfer.
The configured limit is `SHERPA_COLLECTOR_MAX_BYTES=8589934592`.

| Case | Expected | Observed | Body bytes sent | Result |
|---|---|---|---|---|
| Missing `Authorization` | `401` | `401 Unauthorized` | 0 | Pass |
| Invalid `Authorization` | `401` | `401 Unauthorized` | 0 | Pass |
| No `Content-Length`, no transfer encoding | `411` | `411 Length Required` | 0 | Pass |
| `Content-Length: 8589934593` (limit + 1) | `413` | `413 Request Entity Too Large` | 0 | Pass |
| `Content-Type: application/octet-stream` | `415` | `415 Unsupported Media Type` | 0 | Pass |

All five cases passed on a single clean run, including the oversized case that an
earlier interrupted attempt had left unconfirmed. The valid bearer token was supplied
from a mode-0600 custody file for the three post-authorization cases and was never
printed.

## Append-only capability result and routine restriction

The governing threat-model position is unchanged and is restated here rather than
re-decided.

| Field | Value |
|---|---|
| Strict Task 0 capability result | **Blocked** — the routine identity can logically delete an archive and reuse its name |
| Governing decision | **Reduced model approved** by explicit user selection |
| Immutable archive names | Not provided |
| WORM / native undelete / in-place remote recovery | Not provided and not claimed |
| Repair/prune/compact followed by restored append-only access | Untested lifecycle |

Live re-verification performed during acceptance:

| Check | Result | Non-secret evidence |
|---|---|---|
| Installed routine restriction | Pass | The provider-side authorized-keys entry was read back with the recovery identity and byte-matches the authoritative custody copy. The routine entry forces `borg-1.4 serve --append-only --restrict-to-repository <single approved repository>` together with `restrict` |
| Installed key inventory | Pass | Exactly two keys installed: the restricted routine identity and the unrestricted recovery identity |
| Routine interactive shell | Denied | The forced command runs the Borg service; no shell is obtained |
| Routine Borg read access | Pass | `borg list` and `borg info` both succeeded through the routine identity |
| Repository identity stability | Pass | The repository ID is unchanged from the initialization-time record |
| Archive inventory | Pass | Exactly one archive, the earlier routine verification archive created `2026-07-23T11:47:23`; the archive-name set is unchanged |
| Borg-layer encryption | Pass | `none`, as designed — payloads are encrypted to the offline age recipient before upload |
| Routine SFTP mutation | Denied | Connection closed by the forced command, exit `255` |
| Routine SCP mutation | Denied | Connection closed by the forced command, exit `255` |
| Routine rsync mutation | Denied | Rejected by the forced Borg service, exit `2` |
| Nothing created by the denial attempts | Pass | An independent recovery-identity listing shows zero objects from any denial attempt and exactly one entry in the account root |

Status of the reduced model's compensating controls at acceptance:

| Control | State |
|---|---|
| Forced BorgBackup 1.4 append-only routine access restricted to one repository | Verified live this session |
| No routine SFTP/SCP/rsync mutation path | Verified live this session |
| Routine compact reclaims no segments or bytes | Established by Task 0; not re-exercised here |
| Offline recovery-identity download plus transaction rollback | Established by Task 0; not re-exercised here |
| Offline recovery SSH identity and offline age private identity | Verified present in offline custody and absent from the VPS |
| Maximum-practical automatic Storage Box snapshots | **Intended but unconfirmed** — see Open items |

The snapshot control is part of the approved model's design but is not currently
evidenced. Until it is verified it must be treated as an intended control, not an
operative one, and the model's effective protection is correspondingly weaker than the
approved description.

**No new archive was created during acceptance.** Live verification was deliberately
limited to read-only Borg operations and non-mutating denial attempts so that no
additional resource requiring separately approved cleanup was introduced.

## Readiness monitoring

| Field | Value |
|---|---|
| Installation | The existing independent Uptime Kuma installation on the authorized VPS |
| Monitor | `SherpA Off-Site Collector /readyz` |
| Target | `https://sherpa-collector.kruth-support.de/readyz` |
| Type | HTTP(s), `GET` |
| Interval | `60` seconds |
| Retries before alerting | `3` consecutive failures, `60` second retry interval |
| Accepted status codes | `200-299` |
| Certificate validation | Enabled — `ignore_tls=0` |
| Certificate expiry notification | Enabled |
| Active | Yes |

| Check | Result | Non-secret evidence |
|---|---|---|
| Monitor created | Pass | Collector `/readyz` monitor count went from `0` to `1` |
| Monitor reporting | Pass | Consecutive `UP` heartbeats with `200 - OK` at 18–28 ms |
| Non-2xx **detection** on the same path | Pass | A temporary isolated monitor against a non-existent path on the same hostname correctly reported `DOWN` with `Request failed with status code 404`, exercising DNS, TLS, Traefik routing, and failure classification end to end |
| Isolation of the routing test | Pass | The temporary monitor targeted only a non-existent collector path. No unrelated service, and no collector configuration or state, was touched |
| Pre-existing monitors preserved | Pass | All 10 pre-existing monitors remain active and `UP` after the change; none reported `DOWN` |
| Alert **delivery** to an on-call route | **Not satisfied** | The installation has zero notification providers, so no route exists to attach or test. The brief's Step 8 requirement to verify alert routing is therefore a recorded deviation, not a pass. Detection works; nobody is told |

### Disruptive-action approvals

| UTC time | Action | Expected impact | Rollback | Approval |
|---|---|---|---|---|
| `2026-07-23T12:15:47Z` | Uptime Kuma database insert of the collector monitor and the temporary isolated routing monitor, applied with the container stopped | Uptime Kuma unavailable ~4.7 s; monitoring of 10 unrelated targets paused, their monitored services untouched | Pre-change database copy retained on the VPS | Explicit interactive approval after the alternative non-restart paths and this option's cost were presented |
| `2026-07-23T12:17:27Z` | Uptime Kuma database update deactivating the temporary routing monitor, applied with the container stopped | Uptime Kuma unavailable ~4.4 s; same bounded monitoring pause | Same pre-change database copy | Same approval |

Uptime Kuma returned to `healthy` 1 s and 6 s after the respective restarts. The
temporary routing monitor was **deactivated, not deleted** — removing it is cleanup and
requires its own fresh approval.

## Open items and deviations

| Item | Status | Impact |
|---|---|---|
| Maximum-practical automatic Storage Box snapshot schedule | **Deviation — not verified** | Snapshots are one of the reduced model's compensating controls. Verifying or configuring the schedule requires provider API or console access that was not available during acceptance. Until verified, the control is intended but not operative, which weakens the approved model. Deferral chosen explicitly by the operator. |
| On-call notification route (brief Step 8) | **Deviation — not configured** | Uptime Kuma has zero notification providers. The readiness monitor detects and records failures but cannot notify anyone, so a real outage would be visible only to someone already looking at the dashboard. Deferral chosen explicitly by the operator. |
| Pre-deployment smoke on the VPS | **Deviation — not runnable** | The runbook's VPS-side exact-source smoke requires a `go` toolchain the VPS does not have. Compensated by the same smoke passing on the gate host for this exact commit plus the content-level image adjudication above. |
| Itemized Step 1 approval exchange | Not reconstructible | The interrupted session left no record of the presentation required before deployment. Authorization is attested; its itemized content is not evidenced. |
| Readiness will fail without an export producer | Scheduled consequence | With no recovery point, `/readyz` turns red at approximately `2026-07-24T13:50Z` when the 26 h startup grace expires. Task 14 has not begun. Combined with the missing notification route, this will show as a red dashboard entry that alerts no one. |
| Temporary isolated routing monitor | Retained, inactive | Left in place because deletion is cleanup and requires separate fresh approval. |
| Routine verification archive | Retained | One verification archive remains in the authoritative repository. Removal requires separate fresh approval. |

## Scope and safety

- Railway production remains empty and untouched. Railway staging was not changed for
  the collector. No collector URL, upload token, or archive directory variable was
  installed on Railway. Task 14 has not begun and requires its own fresh approval.
- No cleanup, prune, compact, repair, restore, rollback, outage drill, unrestricted Borg
  maintenance, or disruptive alert test was performed.
- No Storage Box archive, repository, sub-account, or key was created **during the
  resumed acceptance session**. The authoritative repository, sub-account, identities, and
  one routine verification archive were provisioned earlier under the same Task 13
  approval.
- No commit, push, or merge was performed by the acceptance work itself.
- The three preserved untracked `.DS_Store` paths remain untracked and untouched.
- Every private value used during verification was read from mode-0600 files inside
  mode-0700 custody directories and never printed. No bearer token or token hash, Storage
  Box account, hostname, username, password, repository path or identifier, SSH or age
  private material, public-key body, archive name, object identifier, manifest, member
  name, or unfiltered log appears in this document.
