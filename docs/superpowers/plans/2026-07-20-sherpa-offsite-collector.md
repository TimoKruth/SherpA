# SherpA Off-Site Collector Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build, deploy, and prove a hardened SherpA collector that accepts the registry's authenticated HTTPS exports, validates and age-encrypts them, commits them to a Hetzner Storage Box through Borg, and supports independent recovery while making Railway export retries restart-safe.

**Architecture:** A new Go collector runs as a non-root Docker Compose service behind the Hostinger VPS's existing Traefik instance at `sherpa-collector.kruth-support.de`. It validates the complete SherpA archive, derives a digest-based idempotency key, age-encrypts with an offline recovery recipient, and persists encrypted retry state before using Borg over pinned SSH; the Railway registry drains completed archives from `/data/exports` and accepts only a matching collector object ID. Deployment cannot claim append-only protection until a sacrificial Storage Box capability gate proves the actual Hetzner key and filesystem restrictions.

**Tech Stack:** Go 1.26.3, `filippo.io/age` v1.3.1, BorgBackup 1.4.5, Docker 29, Docker Compose 5, Traefik with Let's Encrypt, Hetzner Storage Box, Railway staging, PostgreSQL 16.

## Global Constraints

- Work from `/Users/timokruth/Projekte/feat`; run `pwd` before every file-writing phase.
- Preserve `.DS_Store`, `docs/.DS_Store`, and `docs/superpowers/.DS_Store` unchanged and uncommitted.
- Never modify Railway production; all Railway work targets project `powerful-rebirth`, environment `staging`.
- Use application candidate descendants of `b2e91ac52abc3b9d24907d2f43d262b22a8fb0d6`; never redeploy the superseded `312479e` candidate.
- Follow test-driven development: write each behavior test, run it and observe the expected failure, then implement the minimum production code.
- Do not create the authoritative Storage Box repository until Task 0 proves the append-only threat model or the user explicitly approves a revised model.
- Do not store the age private identity or unrestricted recovery SSH identity on Railway or persistently on the VPS.
- Do not put bearer tokens, Storage Box credentials, repository URLs, private keys, database URLs, OAuth values, request bodies, or manifest payloads in Git, logs, terminal evidence, or acceptance evidence.
- Collector upload success requires an exact `object_id` match and verified remote Borg presence; arbitrary 2xx responses are not success.
- The routine collector exposes upload, liveness, and readiness only; no HTTP list, retrieval, delete, prune, or compact API.
- Retention is 30 daily and 12 monthly archives. Every unrestricted prune/compact operation requires fresh explicit approval.
- Pause for fresh explicit approval before Railway variable changes/restarts, the one-minute export interval, sibling recovery-resource creation, every cleanup, and every retention prune/compact.
- Build the collector on the VPS from an exact Git commit under `/docker/SherpA-collector/source`.
- Keep the collector root filesystem read-only, run PID 1 as UID/GID 10001, drop all capabilities, and publish no host port.
- Use the existing Traefik `websecure` entrypoint and `letsencrypt` resolver; do not add Traefik request buffering for the 8 GiB endpoint because buffering would duplicate the complete request body.
- Evidence may contain source commits, image IDs, safe SHA-256 object IDs, sizes, timestamps, artifact counts, and pass/fail classifications only.

---

## File Map

### Shared archive format

- Create `internal/recoveryarchive/archive.go` — streaming validation, limits, reports, and secret-safe failure classes.
- Create `internal/recoveryarchive/manifest.go` — manifest types, deterministic generation, and strict JSON decoding.
- Create `internal/recoveryarchive/archive_test.go` — malformed gzip/tar/path/manifest/limit cases.
- Create `internal/recoveryarchive/testfixture/fixture.go` — deterministic valid and deliberately invalid archives for tests only.
- Modify `internal/registry/export/export.go` — use shared manifest types and validate before publication.

### Collector

- Create `cmd/collector/main.go` and `cmd/collector/main_test.go` — `serve`, `verify`, and `healthcheck` dispatch and lifecycle.
- Create `internal/collector/config.go` and `config_test.go` — typed, file-backed, fail-fast configuration.
- Create `internal/collector/age.go` and `age_test.go` — streaming age encryption.
- Create `internal/collector/spool.go` and `spool_test.go` — lock, disk admission, atomic state transitions, discovery, cleanup.
- Create `internal/collector/state.go` and `state_test.go` — one atomic mode-0600 JSON record per object.
- Create `internal/collector/borg.go`, `borg_test.go`, and `borg_integration_test.go` — no-shell Borg execution and exact presence verification.
- Create `internal/collector/archive.go` and `archive_test.go` — ingestion state machine.
- Create `internal/collector/worker.go` and `worker_test.go` — restart reconciliation, retry, backoff, and readiness.
- Create `internal/collector/handler.go`, `handler_test.go`, and `integration_test.go` — HTTP contract and end-to-end service behavior.

### Registry restart-safe retry

- Modify `internal/registry/export/export.go` and `export_test.go` — collector response verification and filesystem queue.
- Modify `internal/registry/config.go`, `config_test.go`, and `cmd/registry/main_test.go` — mandatory token and persistent staging directory validation.
- Modify `deploy/entrypoint.sh` and `deploy/smoke_build.sh` — prepare and verify `/data/exports`.

### Container and deployment

- Create `deploy/collector/Dockerfile` — pinned Go/Python bases and BorgBackup 1.4.5.
- Create `deploy/collector/compose.yaml` — hardened collector and Traefik labels.
- Create `deploy/collector/runtime.env.example` — non-secret deployment shape.
- Create `deploy/collector/smoke_build.sh` — final-image and Compose security checks.
- Modify `.dockerignore` and `Makefile` — exclude runtime materials and add collector targets.

### Operations and acceptance

- Create `docs/deployment/collector.md` — Storage Box, VPS, monitoring, retention, rotation, and recovery runbook.
- Create `docs/deployment/evidence/2026-07-20-sherpa-offsite-collector-acceptance.md` during execution.
- Modify `docs/deployment/railway.md` — collector contract, `/data/exports`, current Railway `X-Real-IP`, and Borg recovery.
- Modify `docs/superpowers/plans/2026-07-19-railway-staging-acceptance.md` — corrected candidate and new retrieval path.
- Update `docs/deployment/evidence/2026-07-20-railway-staging-acceptance.md` only as live gates pass.

---

### Task 0: Prove the Storage Box Append-Only Capability

**Files:**
- No repository changes until the capability result is known.
- Record only the non-secret conclusion later in `docs/deployment/collector.md` and the collector evidence ledger.

**Interfaces:**
- Consumes: dedicated sacrificial Storage Box sub-account/path and two temporary SSH keypairs.
- Produces: an approved, tested routine-key restriction and recovery-key procedure, or a blocking result requiring a provider/threat-model change.

**Scheduling:** This is numbered Task 0 because it is the hard infrastructure gate. Source implementation Tasks 1–12 may be completed before requesting external-resource approval; Task 13 must not start until Task 0 passes or the user explicitly approves the documented reduced threat model.

- [ ] **Step 1: Present the external-resource checkpoint**

Before creating anything, present the user with:

```text
Resources: one dedicated Storage Box sub-account/path, one sacrificial Borg repository,
one routine test key, one recovery test key.
Impact: no Railway or primary registry outage; Storage Box capacity only.
Cleanup: retain until the capability conclusion is recorded, then request separate cleanup approval.
Blocker under test: whether the routine identity can be restricted to append-only Borg without SFTP deletion.
```

Expected: fresh explicit approval before external resource creation.

- [ ] **Step 2: Create a sacrificial repository without exposing credentials**

Use the Hetzner Console for the dedicated sub-account. Generate keys on a trusted machine with explicit paths and no passphrase output:

```bash
umask 077
ssh-keygen -t ed25519 -f "$HOME/.ssh/sherpa-storage-routine-test" -N '' -C sherpa-storage-routine-test
ssh-keygen -t ed25519 -f "$HOME/.ssh/sherpa-storage-recovery-test" -N '' -C sherpa-storage-recovery-test
```

Expected: private files mode 0600 and public files mode 0644. Do not record key contents.

- [ ] **Step 3: Verify SSH host identity out of band**

Compare the Storage Box port-23 host-key fingerprint against Hetzner's published fingerprints before writing a dedicated `known_hosts` file. Use `StrictHostKeyChecking=yes` for every later connection.

Expected: exact fingerprint match. Stop on mismatch.

- [ ] **Step 4: Test the actual routine-key restriction**

Initialize a sacrificial unencrypted Borg repository with the recovery identity, verify the Storage Box's remote Borg command is protocol-compatible with BorgBackup 1.4.5, install the routine public key using the vendor-supported append-only mechanism, and test all of these independently. Load `BORG_REPO` and the SSH command from mode-0600 local files without printing either value:

```bash
BORG_RSH="ssh -i $RECOVERY_KEY -o IdentitiesOnly=yes -o UserKnownHostsFile=$KNOWN_HOSTS -o StrictHostKeyChecking=yes -p 23" \
  borg init --encryption=none "$BORG_REPO"
BORG_RSH="ssh -i $ROUTINE_KEY -o IdentitiesOnly=yes -o UserKnownHostsFile=$KNOWN_HOSTS -o StrictHostKeyChecking=yes -p 23" \
  borg create --compression none "$BORG_REPO::sherpa-capability-create" "$SACRIFICIAL_FILE"
BORG_RSH="ssh -i $ROUTINE_KEY -o IdentitiesOnly=yes -o UserKnownHostsFile=$KNOWN_HOSTS -o StrictHostKeyChecking=yes -p 23" \
  borg list --json "$BORG_REPO"
```

Then prove independently:

```text
routine key can create a new archive
routine key can list the repository for collector idempotency
routine key cannot delete or recreate an archive
routine key cannot compact or reclaim deleted segments
routine key cannot rename or delete repository files through SFTP/SCP/rsync
a deletion request made through append-only Borg remains recoverable through the recovery identity
recovery identity can perform approved repair/prune/compact and then restore append-only protection
```

Expected: all properties pass. If generic SFTP deletion remains available to the routine identity, the gate fails even if `borg serve --append-only` works.

- [ ] **Step 5: Record the decision**

Record only one of:

```text
Pass — routine identity is constrained to the tested append-only path and cannot delete repository files through another protocol.
Blocked — Storage Box identity retains a deletion path; do not initialize the authoritative repository.
Reduced model approved — user explicitly accepts snapshot-backed deletion resistance instead of per-key append-only isolation.
```

Expected: no authoritative collector deployment proceeds on `Blocked`.

---

### Task 1: Add Shared Recovery Archive Validation

**Files:**
- Create: `internal/recoveryarchive/archive.go`
- Create: `internal/recoveryarchive/manifest.go`
- Create: `internal/recoveryarchive/archive_test.go`
- Create: `internal/recoveryarchive/testfixture/fixture.go`

**Interfaces:**
- Consumes: SherpA gzip/tar archives generated by `internal/registry/export`.
- Produces:

```go
type Limits struct {
	MaxCompressedBytes   int64
	MaxUncompressedBytes int64
	MaxMembers           int
	MaxManifestBytes     int64
	MaxPathBytes         int
}

type Artifact struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type Manifest struct {
	Artifacts []Artifact `json:"artifacts"`
}

type Report struct {
	ArtifactCount int
	VerifiedBytes int64
	ManifestFinal bool
}

type FailureClass string

func DefaultLimits() Limits
func BuildManifest(root string) (Manifest, error)
func MarshalManifest(manifest Manifest) ([]byte, error)
func ValidateFile(ctx context.Context, path string, limits Limits) (Report, error)
func Classify(err error) FailureClass
```

- [ ] **Step 1: Write the valid-archive and manifest tests**

Add tests with these exact names:

```go
func TestValidateFileAcceptsCompleteArchive(t *testing.T)
func TestValidateFileAcceptsArchiveWithoutRepositories(t *testing.T)
func TestBuildManifestHashesSizesAndSortsArtifacts(t *testing.T)
func TestValidateFileRequiresExactlyOnePostgresDump(t *testing.T)
func TestValidateFileRestrictsGitBundlesToRepositoryNamespace(t *testing.T)
func TestValidateFileRequiresExactlyOneFinalManifest(t *testing.T)
func TestValidateFileRejectsMissingExtraDuplicateAndMismatchedManifestArtifacts(t *testing.T)
func TestValidateFileRejectsUnknownManifestFieldsAndTrailingJSON(t *testing.T)
```

The fixture builder must always create `postgres.dump`, accept zero or more `repos/<owner>/<name>.bundle` members, and write a final `manifest.json` whose artifacts exclude the manifest itself. The zero-repository case is valid because a fresh registry may not contain any Git repositories.

- [ ] **Step 2: Run the tests and verify RED**

Run:

```bash
go test ./internal/recoveryarchive -run 'TestValidateFile|TestBuildManifest' -count=1
```

Expected: FAIL because the package and interfaces do not exist.

- [ ] **Step 3: Implement manifest types and strict decoding**

Use fixed secret-safe failure classes:

```go
const (
	FailureInvalidGzip       FailureClass = "invalid_gzip"
	FailureTrailingData      FailureClass = "trailing_data"
	FailureInvalidTar        FailureClass = "invalid_tar"
	FailureUnsafePath        FailureClass = "unsafe_path"
	FailureDuplicateMember   FailureClass = "duplicate_member"
	FailureUnsupportedType   FailureClass = "unsupported_member_type"
	FailureLimitExceeded     FailureClass = "limit_exceeded"
	FailureMissingPostgres   FailureClass = "missing_postgres_dump"
	FailureInvalidRepository FailureClass = "invalid_repository_path"
	FailureMissingManifest   FailureClass = "missing_manifest"
	FailureManifestNotFinal  FailureClass = "manifest_not_final"
	FailureInvalidManifest   FailureClass = "invalid_manifest"
	FailureManifestMismatch  FailureClass = "manifest_mismatch"
)
```

Decode `manifest.json` with `json.Decoder.DisallowUnknownFields()`, require exactly one JSON value, and cap it at 16 MiB.

- [ ] **Step 4: Implement streaming archive validation**

Use `gzip.Reader.Multistream(false)` over a `bufio.Reader`. Stream each regular artifact through SHA-256 and byte-count writers. Enforce:

```text
compressed bytes <= configured limit
uncompressed bytes <= configured limit
members <= configured limit
path bytes <= configured limit
regular files only
no absolute path, backslash, empty segment, '.', or '..'
no duplicate member
exactly one postgres.dump
repository path exactly repos/<owner>/<name>.bundle
exactly one manifest.json and it is final
no second gzip member or compressed trailing bytes
no decompressed data after the tar terminator
```

- [ ] **Step 5: Add the malformed-stream tests**

Add:

```go
func TestValidateFileRejectsSecondGzipMember(t *testing.T)
func TestValidateFileRejectsCompressedTrailingBytes(t *testing.T)
func TestValidateFileRejectsDataAfterTarTerminator(t *testing.T)
func TestValidateFileRejectsAbsoluteTraversalBackslashDotAndEmptySegments(t *testing.T)
func TestValidateFileRejectsDuplicateMembers(t *testing.T)
func TestValidateFileRejectsUnsupportedMemberTypes(t *testing.T)
func TestValidateFileEnforcesCompressedUncompressedMemberManifestAndPathLimits(t *testing.T)
```

- [ ] **Step 6: Run focused and race tests**

Run:

```bash
go test ./internal/recoveryarchive -count=1
go test -race ./internal/recoveryarchive
```

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/recoveryarchive
git commit -m "feat(export): add shared recovery archive validation" \
  -m "Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 2: Make the Exporter Verify Its Archive and Collector Response

**Files:**
- Modify: `internal/registry/export/export.go`
- Modify: `internal/registry/export/export_test.go`

**Interfaces:**
- Consumes: `recoveryarchive.Manifest`, `BuildManifest`, and `ValidateFile` from Task 1.
- Produces:

```go
type UploadResult struct {
	ObjectID string `json:"object_id"`
	Status   string `json:"status"`
}

func Upload(ctx context.Context, archivePath, collectorURL, token string) (UploadResult, error)
```

- [ ] **Step 1: Write failing exporter-validation tests**

Add:

```go
func TestRunProducesArchiveAcceptedBySharedValidator(t *testing.T)
func TestRunDoesNotPublishWhenSharedValidationFails(t *testing.T)
```

Inject a validator function in tests so the second test proves the atomic destination is not published after validation failure.

- [ ] **Step 2: Verify RED**

```bash
go test ./internal/registry/export -run 'TestRunProduces|TestRunDoesNotPublish' -count=1
```

Expected: FAIL because `Run` does not invoke the shared validator.

- [ ] **Step 3: Move manifest generation to the shared package**

Replace local artifact/manifest types with `recoveryarchive.Artifact` and `recoveryarchive.Manifest`. After closing and syncing the temporary archive, call:

```go
if _, err := recoveryarchive.ValidateFile(ctx, tempArchive, recoveryarchive.DefaultLimits()); err != nil {
	return fmt.Errorf("validate completed recovery archive: %s", recoveryarchive.Classify(err))
}
```

Only then atomically rename the archive to the requested destination.

- [ ] **Step 4: Write failing collector-response tests**

Add:

```go
func TestUploadAcceptsCreatedStoredResponse(t *testing.T)
func TestUploadAcceptsOKExistingResponse(t *testing.T)
func TestUploadRejectsMismatchedObjectID(t *testing.T)
func TestUploadRejectsMalformedSuccessfulResponse(t *testing.T)
func TestUploadRejectsStatusCodeStatusBodyMismatch(t *testing.T)
func TestUploadBoundsCollectorResponseBody(t *testing.T)
func TestUploadErrorDoesNotExposeURLTokenOrResponseBody(t *testing.T)
```

Expected contracts:

```text
201 + {"object_id":"sha256:<local digest>","status":"stored"}
200 + {"object_id":"sha256:<local digest>","status":"existing"}
```

- [ ] **Step 5: Verify RED**

```bash
go test ./internal/registry/export -run TestUpload -count=1
```

Expected: FAIL because `Upload` discards the body and accepts arbitrary 2xx.

- [ ] **Step 6: Implement exact response verification**

Compute the local compressed SHA-256 before the request. Read at most 64 KiB of response JSON, reject unknown fields/trailing JSON, require the status/code pair above, and require exact object-ID equality. Never include the response body, token, URL query, or private URL in returned errors.

- [ ] **Step 7: Run tests**

```bash
go test ./internal/registry/export -count=1
go test -race ./internal/registry/export
```

Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/registry/export
git commit -m "feat(export): verify collector object identity" \
  -m "Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 3: Replace Registry In-Memory Retry State With a Filesystem Queue

**Files:**
- Modify: `internal/registry/export/export.go`
- Modify: `internal/registry/export/export_test.go`
- Modify: `internal/registry/config.go`
- Modify: `internal/registry/config_test.go`
- Modify: `cmd/registry/main_test.go`
- Modify: `deploy/entrypoint.sh`
- Modify: `deploy/smoke_build.sh`

**Interfaces:**
- Consumes: validated `UploadResult` from Task 2.
- Produces:

```go
type pendingArchive struct {
	Path    string
	Base    string
	ModTime time.Time
	Size    int64
}

func discoverPendingArchives(archiveDir string) ([]pendingArchive, error)
func cleanupStaleExportPartials(archiveDir string, now time.Time, maxAge time.Duration) (int, error)
func drainPendingArchives(ctx context.Context, cfg SchedulerConfig) (remaining int, err error)
func runSchedulerCycle(ctx context.Context, cfg SchedulerConfig, now time.Time) error
```

- [ ] **Step 1: Write filesystem-discovery tests**

Add:

```go
func TestDiscoverPendingArchivesReturnsCompletedArchivesOldestFirst(t *testing.T)
func TestDiscoverPendingArchivesIgnoresTemporaryWorkspacesAndPartialArchives(t *testing.T)
func TestCleanupStaleExportPartialsRemovesOnlyGeneratedNamesOlderThan24Hours(t *testing.T)
func TestCleanupStaleExportPartialsNeverFollowsSymlinksOrRemovesCompletedArchives(t *testing.T)
func TestStartSchedulerDrainsExistingArchiveBeforeFirstTick(t *testing.T)
func TestStartSchedulerDoesNotCreateWhilePendingArchiveExists(t *testing.T)
func TestStartSchedulerDrainsMultipleArchivesOldestFirst(t *testing.T)
func TestStartSchedulerRestartAfterRemoteCommitResendsThenDeletes(t *testing.T)
func TestStartSchedulerPreservesAllArchivesAfterMidQueueFailure(t *testing.T)
func TestStartSchedulerLogsQueueCountSafeBasenameAgeAndRetryClassOnly(t *testing.T)
```

Use only regular files matching `sherpa-*.tar.gz`; reject symlinks and ignore hidden temporary names.

- [ ] **Step 2: Verify RED**

```bash
go test ./internal/registry/export -run 'TestDiscover|TestCleanup|TestStartScheduler' -count=1
```

Expected: FAIL because pending state exists only in memory.

- [ ] **Step 3: Implement oldest-first restart discovery**

Remove the `pendingArchive string` variable. At startup and before each tick, remove only regular `.sherpa-export-*.tar.gz` files and real `.sherpa-export-work-*` directories older than 24 hours; never follow symlinks and never remove completed `sherpa-*.tar.gz` files. Then discover pending files. If pending files exist at startup, drain them immediately without creating a new archive. Remove a local file only after `Upload` returns `stored` or `existing` with the validated object ID.

- [ ] **Step 4: Write mandatory-token tests**

Add:

```go
func TestValidateSchedulerConfigRequiresTokenWhenEnabled(t *testing.T)
func TestLoadConfigRequiresExportTokenWhenSchedulingEnabled(t *testing.T)
func TestRunWiresPersistentExportArchiveDirectory(t *testing.T)
```

- [ ] **Step 5: Verify RED and implement validation**

Run:

```bash
go test ./internal/registry ./cmd/registry -run 'Test.*Export|TestValidateScheduler' -count=1
```

Expected: FAIL while URL+interval without token remains accepted. Make URL, positive interval, archive directory, and non-empty token an all-or-nothing scheduled-export configuration.

- [ ] **Step 6: Prepare `/data/exports` in the registry container**

Update `deploy/entrypoint.sh` to create and chown both persistent siblings before dropping privileges:

```sh
install -d -m 0700 -o sherpa -g sherpa "$SHERPA_CONTENT_DIR" "$SHERPA_EXPORT_ARCHIVE_DIR"
```

Keep the existing guard that the export directory cannot be inside the Git content directory.

- [ ] **Step 7: Extend the production-image smoke test**

Verify `/data/git` and `/data/exports` are both writable by UID/GID 998 and that no export archive is placed under `/data/git`.

- [ ] **Step 8: Run all focused checks**

```bash
go test ./internal/registry/export ./internal/registry ./cmd/registry -count=1
go test -race ./internal/registry/export ./internal/registry ./cmd/registry
bash -n deploy/entrypoint.sh
bash deploy/smoke_build.sh
```

Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/registry/export internal/registry/config.go internal/registry/config_test.go \
  cmd/registry/main_test.go deploy/entrypoint.sh deploy/smoke_build.sh
git commit -m "fix(registry): persist export retry queue" \
  -m "Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 4: Add Collector Configuration and Offline Verification

**Files:**
- Modify: `go.mod`
- Modify: `go.sum`
- Create: `internal/collector/config.go`
- Create: `internal/collector/config_test.go`
- Create: `cmd/collector/main.go`
- Create: `cmd/collector/main_test.go`

**Interfaces:**
- Consumes: `recoveryarchive` package from Task 1.
- Produces:

```go
type Config struct {
	ListenAddr      string
	TokenDigest     [32]byte
	AgeRecipient    age.Recipient
	Borg            BorgConfig
	SpoolDir        string
	StateDir        string
	Limits          recoveryarchive.Limits
	RetryInterval   time.Duration
	PartialMaxAge   time.Duration
	StartupGrace    time.Duration
	MaxRecoveryAge  time.Duration
}

type BorgConfig struct {
	Binary         string
	Repository     string
	SSHKeyFile     string
	KnownHostsFile string
	WorkDir        string
	CreateTimeout  time.Duration
	QueryTimeout   time.Duration
}

func LoadConfig(getenv func(string) string) (Config, error)
```

- [ ] **Step 1: Add age v1.3.1**

```bash
go get filippo.io/age@v1.3.1
```

Expected: `go.mod` and `go.sum` contain the exact stable dependency.

- [ ] **Step 2: Write failing configuration tests**

Add:

```go
func TestLoadConfigAcceptsCompleteFileBackedConfiguration(t *testing.T)
func TestLoadConfigAppliesDocumentedDefaults(t *testing.T)
func TestLoadConfigRejectsEveryPartialConfiguration(t *testing.T)
func TestLoadConfigRejectsMissingEmptySymlinkAndNonRegularSecretFiles(t *testing.T)
func TestLoadConfigRejectsGroupOrWorldAccessibleSecretFiles(t *testing.T)
func TestLoadConfigRejectsMalformedAgeRecipient(t *testing.T)
func TestLoadConfigRejectsAgePrivateIdentity(t *testing.T)
func TestLoadConfigRejectsInvalidSizesCountsAndDurations(t *testing.T)
func TestLoadConfigErrorsDoNotExposeRepositoryTokenOrPrivatePaths(t *testing.T)
```

- [ ] **Step 3: Verify RED**

```bash
go test ./internal/collector -run TestLoadConfig -count=1
```

Expected: FAIL because configuration does not exist.

- [ ] **Step 4: Implement typed file-backed configuration**

Parse the public recipient with:

```go
recipient, err := age.ParseX25519Recipient(strings.TrimSpace(string(recipientBytes)))
```

Hash the bearer token immediately and retain only the digest:

```go
tokenDigest := sha256.Sum256(bytes.TrimSpace(tokenBytes))
```

Reject symlinks, non-regular files, empty files, and group/world-readable secret files. Use the exact defaults from the specification.

- [ ] **Step 5: Write failing `verify` command tests**

Add:

```go
func TestDispatchVerifyUsesSharedValidator(t *testing.T)
func TestDispatchVerifyPrintsOnlySafeSummary(t *testing.T)
func TestDispatchVerifyFailurePrintsOnlySafeClassification(t *testing.T)
func TestDispatchVerifyDoesNotRequireRuntimeSecrets(t *testing.T)
func TestDispatchRejectsInvalidVerifyUsage(t *testing.T)
```

Successful output is exactly:

```text
valid
artifacts=<decimal count>
verified_bytes=<decimal count>
manifest_final=true
```

- [ ] **Step 6: Implement and run tests**

```bash
go test ./internal/collector ./cmd/collector -run 'TestLoadConfig|TestDispatchVerify' -count=1
```

Expected: PASS, with no member paths or manifest values in output.

- [ ] **Step 7: Commit**

```bash
git add go.mod go.sum internal/collector/config.go internal/collector/config_test.go \
  cmd/collector/main.go cmd/collector/main_test.go
git commit -m "feat(collector): add configuration and archive verification" \
  -m "Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 5: Add Streaming age Encryption

**Files:**
- Create: `internal/collector/age.go`
- Create: `internal/collector/age_test.go`

**Interfaces:**

```go
type Encryptor struct {
	recipient age.Recipient
}

func NewEncryptor(recipient age.Recipient) *Encryptor
func (e *Encryptor) EncryptFile(ctx context.Context, sourcePath, partialPath string) (int64, error)
```

- [ ] **Step 1: Write failing encryption tests**

```go
func TestEncryptFileRoundTripsWithGeneratedX25519Identity(t *testing.T)
func TestEncryptFileWritesMode0600AndSyncsBeforeSuccess(t *testing.T)
func TestEncryptFileCancellationLeavesOnlyPartialOutput(t *testing.T)
func TestEncryptFileFailureDoesNotExposeRecipientOrPaths(t *testing.T)
func TestEncryptFileRejectsExistingDestination(t *testing.T)
```

- [ ] **Step 2: Verify RED**

```bash
go test ./internal/collector -run TestEncryptFile -count=1
```

Expected: FAIL because `Encryptor` is absent.

- [ ] **Step 3: Implement the encryption sequence**

Open the destination with `O_CREATE|O_EXCL|O_WRONLY`, mode 0600. Call `age.Encrypt`, copy from a context-aware reader, close the age writer to finalize authentication, synchronize the file, then close it. The caller owns removal of failed `.age.partial` files and the final atomic rename.

- [ ] **Step 4: Verify GREEN and race safety**

```bash
go test ./internal/collector -run TestEncryptFile -count=1
go test -race ./internal/collector -run TestEncryptFile
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/collector/age.go internal/collector/age_test.go
git commit -m "feat(collector): add streaming age encryption" \
  -m "Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 6: Add Crash-Safe Spool, Lock, Admission, and Ledger

**Files:**
- Create: `internal/collector/spool.go`
- Create: `internal/collector/spool_test.go`
- Create: `internal/collector/state.go`
- Create: `internal/collector/state_test.go`
- Modify: `go.mod`
- Modify: `go.sum`

**Interfaces:**

```go
type PendingObject struct {
	ObjectID      string
	DigestHex     string
	ArchiveName   string
	EncryptedPath string
	EncryptedSize int64
	ReceivedAt    time.Time
}

func OpenSpool(dir string, partialMaxAge time.Duration) (*Spool, error)
func (s *Spool) AcquireLock() (func() error, error)
func (s *Spool) Admit(contentLength int64) error
func (s *Spool) Receive(ctx context.Context, source io.Reader, contentLength int64) (string, string, int64, error)
func (s *Spool) EncryptedPaths(objectID string) (string, string, error)
func (s *Spool) CommitEncrypted(partial, final string) error
func (s *Spool) RemovePlaintext(path string) error
func (s *Spool) RemoveEncrypted(path string) error
func (s *Spool) Discover() ([]PendingObject, error)
func (s *Spool) CleanupStalePartials() (int, error)
func (s *Spool) CheckWritable() error

type ObjectRecord struct {
	ObjectID         string     `json:"object_id"`
	ArchiveName      string     `json:"archive_name"`
	CompressedSize   int64      `json:"compressed_size"`
	EncryptedSize    int64      `json:"encrypted_size"`
	ReceivedAt       time.Time  `json:"received_at"`
	StoredAt         *time.Time `json:"stored_at,omitempty"`
	LastAttemptAt    *time.Time `json:"last_attempt_at,omitempty"`
	LatestRetryClass string     `json:"latest_retry_class,omitempty"`
	RetryCount       int        `json:"retry_count"`
}

func OpenLedger(dir string) (*Ledger, error)
func (l *Ledger) Get(objectID string) (ObjectRecord, bool, error)
func (l *Ledger) Put(record ObjectRecord) error
func (l *Ledger) List() ([]ObjectRecord, error)
```

- [ ] **Step 1: Add the explicit filesystem-lock dependency**

```bash
go get golang.org/x/sys/unix@latest
```

After resolution, pin the exact version written by Go tooling and do not use a floating version in committed files.

- [ ] **Step 2: Write failing spool tests**

```go
func TestSpoolAcquireLockRejectsSecondProcess(t *testing.T)
func TestSpoolReceiveHashesExactCompressedBytes(t *testing.T)
func TestSpoolReceiveRejectsShortAndOverlongBodies(t *testing.T)
func TestSpoolAdmitRequiresSpaceForPlaintextEncryptedCopyAndReserve(t *testing.T)
func TestSpoolCommitEncryptedSyncsFileRenamesAndDirectory(t *testing.T)
func TestSpoolNeverDiscoversPartialFiles(t *testing.T)
func TestSpoolDiscoversCompleteAgeFilesOldestFirst(t *testing.T)
func TestSpoolCleanupRemovesOnlyStalePartials(t *testing.T)
func TestSpoolCheckWritableUsesMode0600Probe(t *testing.T)
func TestSpoolDiskFullClassifiesAsInsufficientStorage(t *testing.T)
```

- [ ] **Step 3: Verify RED and implement spool transitions**

```bash
go test ./internal/collector -run TestSpool -count=1
```

Expected RED. Implement exact complete and partial names:

```text
<64hex>.tar.gz.age
.sherpa-upload-<random>.upload.partial
<64hex>.tar.gz.age.partial
```

Use same-filesystem `fsync` and rename. Never discover partial names as pending work.

- [ ] **Step 4: Write failing ledger tests**

```go
func TestLedgerRoundTripContainsOnlyApprovedMetadata(t *testing.T)
func TestLedgerPutUsesMode0600AtomicReplacement(t *testing.T)
func TestLedgerListSortsByReceiptTime(t *testing.T)
func TestLedgerRejectsInvalidObjectIDFilenameMismatch(t *testing.T)
func TestLedgerFailureNeverExposesPrivateValues(t *testing.T)
```

- [ ] **Step 5: Implement one-record-per-object atomic state**

Use mode-0600 JSON records under the state directory. Validate object IDs before constructing filenames. Write a same-directory temporary file, synchronize it, rename it, then synchronize the directory.

- [ ] **Step 6: Run focused and race tests**

```bash
go test ./internal/collector -run 'TestSpool|TestLedger' -count=1
go test -race ./internal/collector -run 'TestSpool|TestLedger'
```

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add go.mod go.sum internal/collector/spool.go internal/collector/spool_test.go \
  internal/collector/state.go internal/collector/state_test.go
git commit -m "feat(collector): add durable spool and object ledger" \
  -m "Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 7: Add the Constrained Borg Backend

**Files:**
- Create: `internal/collector/borg.go`
- Create: `internal/collector/borg_test.go`
- Create: `internal/collector/borg_integration_test.go`

**Interfaces:**

```go
type ArchiveInfo struct {
	Name      string
	StartedAt time.Time
}

type Backend interface {
	Create(context.Context, PendingObject) error
	Exists(context.Context, string) (bool, error)
	List(context.Context) ([]ArchiveInfo, error)
}

func NewBorgBackend(config BorgConfig) (*BorgBackend, error)
```

- [ ] **Step 1: Write failing argument and redaction tests**

```go
func TestBorgCreateBuildsFixedArgumentsWithoutShell(t *testing.T)
func TestBorgCreatePassesPrivateValuesOnlyThroughEnvironment(t *testing.T)
func TestBorgCreateUsesPinnedKnownHostsAndStrictChecking(t *testing.T)
func TestBorgExistsUsesExactJSONNameEquality(t *testing.T)
func TestBorgExistsRejectsPrefixSuffixAndGlobMatches(t *testing.T)
func TestBorgRunnerBoundsOutput(t *testing.T)
func TestBorgRunnerTimeoutAndCancellation(t *testing.T)
func TestBorgErrorsClassifyWithoutCommandOutputOrPrivateValues(t *testing.T)
```

- [ ] **Step 2: Verify RED**

```bash
go test ./internal/collector -run TestBorg -count=1
```

Expected: FAIL because no backend exists.

- [ ] **Step 3: Implement no-shell Borg execution**

Construct fixed commands internally. The request controls no command argument.

Create:

```text
borg create --compression none ::sherpa-<64hex> <64hex>.tar.gz.age
```

List:

```text
borg list --json
```

Supply private values only in the child environment:

```text
BORG_REPO
BORG_RSH=ssh -i <configured file> -o IdentitiesOnly=yes -o UserKnownHostsFile=<configured file> -o StrictHostKeyChecking=yes -p 23
BORG_CACHE_DIR
BORG_CONFIG_DIR
BORG_SECURITY_DIR
BORG_UNKNOWN_UNENCRYPTED_REPO_ACCESS_IS_OK=yes
```

The configured file paths come from trusted startup configuration, never HTTP input. Bound captured output and expose only fixed error classes.

- [ ] **Step 4: Require exact post-create presence**

Parse Borg JSON and accept only `archive.Name == "sherpa-"+digest`. Never use substring, glob, or prefix matching as proof.

- [ ] **Step 5: Add a real local-repository integration test**

```go
func TestBorgLocalRepositoryCreateAndExactPresence(t *testing.T)
```

Use a temporary unencrypted local Borg repository. Skip only when the host lacks Borg; the collector image smoke test must run the same test with BorgBackup 1.4.5 present.

- [ ] **Step 6: Run tests**

```bash
go test ./internal/collector -run TestBorg -count=1
go test -race ./internal/collector -run TestBorg
go test -tags=integration ./internal/collector -run TestBorgLocalRepositoryCreateAndExactPresence -count=1
```

Expected: unit tests pass; integration passes or explicitly skips only outside the pinned image.

- [ ] **Step 7: Commit**

```bash
git add internal/collector/borg.go internal/collector/borg_test.go internal/collector/borg_integration_test.go
git commit -m "feat(collector): add constrained Borg backend" \
  -m "Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 8: Add Ingestion, Reconciliation, Retry, and Readiness

**Files:**
- Create: `internal/collector/archive.go`
- Create: `internal/collector/archive_test.go`
- Create: `internal/collector/worker.go`
- Create: `internal/collector/worker_test.go`

**Interfaces:**

```go
type ResultStatus string

const (
	ResultStored   ResultStatus = "stored"
	ResultExisting ResultStatus = "existing"
)

type Result struct {
	ObjectID string
	Status   ResultStatus
}

type Ingestor interface {
	Ingest(context.Context, io.Reader, int64) (Result, error)
}

type ReadinessConfig struct {
	StartedAt      time.Time
	StartupGrace   time.Duration
	MaxRecoveryAge time.Duration
}

type ReadinessSnapshot struct {
	SpoolWritable       bool
	OldestPendingAt     *time.Time
	NewestSuccessfulAt  *time.Time
	TerminalLocalError  bool
}

func NewStatusTracker(config ReadinessConfig, now func() time.Time) *StatusTracker
func (s *StatusTracker) SetSpoolWritable(writable bool)
func (s *StatusTracker) RecordPending(objectID string, receivedAt time.Time)
func (s *StatusTracker) RemovePending(objectID string)
func (s *StatusTracker) RecordSuccess(storedAt time.Time)
func (s *StatusTracker) RecordTerminalLocalError()
func (s *StatusTracker) Snapshot() ReadinessSnapshot
func (s *StatusTracker) Ready() bool
func NewService(spool *Spool, ledger *Ledger, encryptor *Encryptor, backend Backend, status *StatusTracker, limits recoveryarchive.Limits) *Service
func (s *Service) Ingest(context.Context, io.Reader, int64) (Result, error)
func (s *Service) CommitPending(context.Context, PendingObject) (bool, error)
func NewWorker(spool *Spool, ledger *Ledger, service *Service, status *StatusTracker, retryInterval time.Duration) *Worker
func (w *Worker) Run(context.Context) error
func RetryDelay(base time.Duration, retryCount int) time.Duration
```

- [ ] **Step 1: Write failing ingestion tests**

```go
func TestIngestValidatesBeforeEncryption(t *testing.T)
func TestIngestDerivesObjectIDFromCompleteCompressedArchive(t *testing.T)
func TestIngestRemovesPlaintextOnlyAfterDurableEncryptedRename(t *testing.T)
func TestIngestBackendFailureLeavesOnlyEncryptedRetryObject(t *testing.T)
func TestIngestDuplicatePendingObjectReusesDurableAgeFile(t *testing.T)
func TestIngestRemotePresenceReturnsExisting(t *testing.T)
func TestIngestCreateRequiresExactRemotePresence(t *testing.T)
func TestIngestLostResponseRechecksPresenceBeforeCreate(t *testing.T)
func TestIngestNeverDeletesAgeFileBeforeVerifiedCommit(t *testing.T)
func TestIngestSerializesBorgOperationsWithWorker(t *testing.T)
```

- [ ] **Step 2: Verify RED and implement the state machine**

```bash
go test ./internal/collector -run TestIngest -count=1
```

Expected RED. Implement this exact ordering:

```text
admit disk
receive and hash plaintext
fsync plaintext
validate plaintext
age encrypt to .partial
fsync + atomic rename to .age
remove plaintext, or reuse an already durable `.age` object with the same digest
check remote exact presence
create if absent
verify remote exact presence
atomically mark ledger stored
remove local .age
return stored/existing
```

- [ ] **Step 3: Write restart and readiness tests**

```go
func TestWorkerStartupCleansStalePartialsAndDiscoversAgeObjects(t *testing.T)
func TestWorkerReconcilesRemoteCommitBeforeLedgerUpdate(t *testing.T)
func TestWorkerQueuesMissingObjectsOldestFirst(t *testing.T)
func TestWorkerBackoffStartsAtFiveMinutesAndCapsAtOneHour(t *testing.T)
func TestWorkerSuccessfulRetryUpdatesLedgerBeforeRemovingAgeFile(t *testing.T)
func TestWorkerCorruptStateSetsTerminalReadinessError(t *testing.T)
func TestReadinessPassesDuringStartupGrace(t *testing.T)
func TestReadinessFailsAfterGraceWithoutRecoveryPoint(t *testing.T)
func TestReadinessFailsForStaleRecoveryPoint(t *testing.T)
func TestReadinessFailsForOldPendingObject(t *testing.T)
func TestLivenessRemainsHealthyDuringBackendOutage(t *testing.T)
```

Backoff is exactly `5m, 10m, 20m, 40m, 1h, 1h...`.

- [ ] **Step 4: Test every crash window**

Use injected hooks to stop after:

```text
.upload.partial write
.age.partial write
.age rename
plaintext removal
remote create
remote presence query
ledger update
.age removal
```

After restart, each state must be either safely cleaned as incomplete or retained/reconciled as a complete encrypted object.

- [ ] **Step 5: Run tests**

```bash
go test ./internal/collector -run 'TestIngest|TestWorker|TestReadiness' -count=1
go test -race ./internal/collector -run 'TestIngest|TestWorker|TestReadiness'
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/collector/archive.go internal/collector/archive_test.go \
  internal/collector/worker.go internal/collector/worker_test.go
git commit -m "feat(collector): add crash-safe ingestion and retry" \
  -m "Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 9: Add the HTTP API and Process Lifecycle

**Files:**
- Create: `internal/collector/handler.go`
- Create: `internal/collector/handler_test.go`
- Create: `internal/collector/integration_test.go`
- Modify: `cmd/collector/main.go`
- Modify: `cmd/collector/main_test.go`

**Interfaces:**

```go
type HTTPConfig struct {
	TokenDigest [32]byte
	MaxBytes    int64
}

func NewHandler(config HTTPConfig, ingestor Ingestor, status *StatusTracker, now func() time.Time, startupGrace, maxRecoveryAge time.Duration, logger *log.Logger) http.Handler
```

- [ ] **Step 1: Write pre-body rejection tests**

```go
func TestUploadRejectsMissingInvalidAndDuplicateAuthorizationBeforeBodyRead(t *testing.T)
func TestUploadRejectsWrongMethodBeforeBodyRead(t *testing.T)
func TestUploadRejectsMissingZeroNegativeAndUnknownLengthBeforeBodyRead(t *testing.T)
func TestUploadRejectsOversizedLengthBeforeBodyRead(t *testing.T)
func TestUploadRejectsWrongContentTypeBeforeBodyRead(t *testing.T)
func TestUploadAllowsOnlyOneConcurrentRequest(t *testing.T)
```

Use a body spy whose `Read` method fails the test; assert zero reads for every rejected precondition.

- [ ] **Step 2: Verify RED and implement authentication/admission**

```bash
go test ./internal/collector -run 'TestUploadRejects|TestUploadAllows' -count=1
```

Expected RED. Parse exactly one `Authorization: Bearer` value, hash the candidate, and compare with `subtle.ConstantTimeCompare`.

- [ ] **Step 3: Write response mapping and secret-safety tests**

```go
func TestUploadMapsValidationFailureTo422(t *testing.T)
func TestUploadMapsSpoolAdmissionFailureTo507(t *testing.T)
func TestUploadMapsPendingBackendFailureTo503(t *testing.T)
func TestUploadReturns201Stored(t *testing.T)
func TestUploadReturns200Existing(t *testing.T)
func TestUploadResponseUsesExactJSONContract(t *testing.T)
func TestUploadResponsesAndLogsNeverExposeSecretsBackendOutputOrPaths(t *testing.T)
func TestHealthzIsBackendIndependent(t *testing.T)
func TestReadyzUsesRecoveryReadinessWithoutDetails(t *testing.T)
```

- [ ] **Step 4: Implement the API**

Routes are exactly:

```text
POST /v1/exports
GET /healthz
GET /readyz
```

Unknown routes return a generic 404. Use explicit JSON encoding for successful upload responses and fixed text for health responses.

- [ ] **Step 5: Add lifecycle and end-to-end tests**

```go
func TestRunBuildsDocumentedServerTimeouts(t *testing.T)
func TestRunAcquiresSpoolLockBeforeStartingWorker(t *testing.T)
func TestRunFailsBeforeListenOnUnsafeConfiguration(t *testing.T)
func TestServeGracefullyStopsHTTPAndWorker(t *testing.T)
func TestHealthcheckCommandCallsOnlyLocalHealthz(t *testing.T)
func TestCollectorHTTPUploadThroughRealServer(t *testing.T)
func TestCollectorRestartDrainsEncryptedPendingObject(t *testing.T)
func TestCollectorDuplicateAfterLostResponseReturnsSameObjectID(t *testing.T)
func TestCollectorEndToEndWithLocalBorgRepository(t *testing.T)
```

- [ ] **Step 6: Run tests**

```bash
go test ./internal/collector ./cmd/collector -count=1
go test -race ./internal/collector ./cmd/collector
go test -tags=integration ./internal/collector -run 'TestCollectorEndToEnd|TestBorgLocal' -count=1
```

Expected: PASS or a documented local Borg skip outside the pinned image.

- [ ] **Step 7: Commit**

```bash
git add internal/collector/handler.go internal/collector/handler_test.go \
  internal/collector/integration_test.go cmd/collector/main.go cmd/collector/main_test.go
git commit -m "feat(collector): serve authenticated durable exports" \
  -m "Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 10: Add the Pinned Collector Image and Hardened Compose File

**Files:**
- Create: `deploy/collector/Dockerfile`
- Create: `deploy/collector/compose.yaml`
- Create: `deploy/collector/runtime.env.example`
- Create: `deploy/collector/smoke_build.sh`
- Modify: `.dockerignore`
- Modify: `Makefile`

**Interfaces:**
- Consumes: `collector` binary from Task 9.
- Produces: a local image and Compose service suitable for `/docker/SherpA-collector`.

- [ ] **Step 1: Write the smoke script assertions before the Dockerfile**

The script must fail unless all of these hold:

```text
collector verify works
Borg version is exactly 1.4.5
PID 1 is UID/GID 10001
root filesystem is unwritable
/data is writable
all capabilities are dropped
no-new-privileges is active
no Go compiler, C compiler, setuid/setgid file, Git metadata, source tree, package cache, or test fixtures remain
OCI source/revision labels contain only the public repository URL and exact public commit
no secret-like image environment, history entry, or label exists
local Borg create/list integration passes
Compose config validates
no host port is published
```

- [ ] **Step 2: Verify RED**

```bash
bash -n deploy/collector/smoke_build.sh
bash deploy/collector/smoke_build.sh
```

Expected: the syntax check passes and the smoke build fails because the image files do not exist.

- [ ] **Step 3: Add the pinned multi-stage Dockerfile**

Use these reviewed image-index digests:

```dockerfile
FROM golang:1.26.3-bookworm@sha256:386d475a660466863d9f8c766fec64d7fdad3edac2c6a05020c09534d71edb4b AS go-build
FROM python:3.13-slim-bookworm@sha256:9d7f287598e1a5a978c015ee176d8216435aaf335ed69ac3c38dd1bbb10e8d64 AS borg-build
FROM python:3.13-slim-bookworm@sha256:9d7f287598e1a5a978c015ee176d8216435aaf335ed69ac3c38dd1bbb10e8d64 AS runtime
```

In `borg-build`, install exactly `build-essential`, `pkg-config`, `curl`, `libssl-dev`, `libacl1-dev`, `liblz4-dev`, `libzstd-dev`, and `libxxhash-dev`. FUSE packages are intentionally omitted because the collector never mounts an archive. Create `/opt/borg`, download the only BorgBackup 1.4.5 PyPI release file, verify it, and install from that local artifact:

```bash
python -m venv /opt/borg
curl -fsSLo /tmp/borgbackup-1.4.5.tar.gz \
  https://files.pythonhosted.org/packages/source/b/borgbackup/borgbackup-1.4.5.tar.gz
printf '%s  %s\n' \
  '4f9a5fe584c504b15485841236750dea16aa7cd2ddbc4a594e9d2ce5c49c4508' \
  '/tmp/borgbackup-1.4.5.tar.gz' | sha256sum -c -
/opt/borg/bin/pip install --no-cache-dir /tmp/borgbackup-1.4.5.tar.gz
/opt/borg/bin/borg --version | grep -Fx 'borg 1.4.5'
```

In `runtime`, install exactly `ca-certificates`, `openssh-client`, `libssl3`, `libacl1`, `liblz4-1`, `libzstd1`, and `libxxhash0`; copy `/opt/borg` and the collector binary; create UID/GID 10001; and set `PATH=/opt/borg/bin:$PATH` plus Borg cache/config/security paths under `/data/borg`. Add OCI `org.opencontainers.image.source` and `org.opencontainers.image.revision` labels containing only the public repository URL and `SOURCE_REVISION`. The smoke test must prove the final image has no setuid/setgid files in addition to having no compiler, source, Git metadata, package cache, or test fixtures.

- [ ] **Step 4: Add hardened Compose configuration**

Use this shape without Traefik buffering or a published port:

```yaml
services:
  collector:
    build:
      context: ./source
      dockerfile: deploy/collector/Dockerfile
      args:
        SOURCE_REVISION: ${SOURCE_REVISION}
    restart: unless-stopped
    user: "10001:10001"
    read_only: true
    cap_drop: [ALL]
    security_opt:
      - no-new-privileges:true
    pids_limit: 128
    cpus: "1.0"
    mem_limit: 1g
    expose:
      - "8080"
    tmpfs:
      - /tmp:rw,nosuid,nodev,noexec,size=64m
    volumes:
      - ./data:/data
      - ./config/age-recipient:/run/config/age-recipient:ro
      - ./config/known_hosts:/run/config/known_hosts:ro
      - ./secrets/upload-token:/run/secrets/upload-token:ro
      - ./secrets/storage-ssh-key:/run/secrets/storage-ssh-key:ro
      - ./secrets/borg-repository:/run/secrets/borg-repository:ro
    env_file:
      - ./runtime.env
    healthcheck:
      test: ["CMD", "/usr/local/bin/collector", "healthcheck", "http://127.0.0.1:8080/healthz"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 20s
    logging:
      driver: json-file
      options:
        max-size: "10m"
        max-file: "5"
    labels:
      - traefik.enable=true
      - traefik.http.routers.sherpa-collector.rule=Host(`sherpa-collector.kruth-support.de`)
      - traefik.http.routers.sherpa-collector.entrypoints=websecure
      - traefik.http.routers.sherpa-collector.tls=true
      - traefik.http.routers.sherpa-collector.tls.certresolver=letsencrypt
      - traefik.http.services.sherpa-collector.loadbalancer.server.port=8080
```

The existing Traefik container uses host networking and already routes labeled bridge containers, so do not invent an external proxy network.

- [ ] **Step 5: Add the non-secret runtime example and ignore rules**

`runtime.env.example` contains documented variable names and safe example values only. Extend `.dockerignore` to exclude `.env*`, `secrets/`, private keys, `known_hosts`, collector `data/`, and deployment runtime files while preserving the existing exclusions.

- [ ] **Step 6: Run image and Compose gates**

`smoke_build.sh` must create a temporary deployment root, copy `compose.yaml` and `runtime.env.example` there as `compose.yaml` and `runtime.env`, symlink its `source` entry to the repository root, create empty `config/`, `secrets/`, and `data/` fixtures with safe dummy values, and then run:

```bash
bash deploy/collector/smoke_build.sh
```

Inside that temporary root, the script runs `SOURCE_REVISION="$(git rev-parse HEAD)" docker compose config` and asserts that no `ports:` entry appears in the rendered service. This mirrors the VPS layout and avoids incorrectly resolving `./source` relative to `deploy/collector/`.

- [ ] **Step 7: Commit**

```bash
git add deploy/collector .dockerignore Makefile
git commit -m "build(collector): add hardened Compose deployment" \
  -m "Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 11: Add Deployment, Retention, Monitoring, and Recovery Runbooks

**Files:**
- Create: `docs/deployment/collector.md`
- Modify: `docs/deployment/railway.md`
- Modify: `docs/superpowers/plans/2026-07-19-railway-staging-acceptance.md`

**Interfaces:**
- Consumes: final collector behavior and the verified Task 0 storage model.
- Produces: exact secret-safe operator procedures and corrected Railway acceptance steps.

- [ ] **Step 1: Write the collector runbook**

Include exact sections for:

```text
append-only capability result
Storage Box sub-account
routine and recovery SSH identities
offline age identity
host-key pinning
Borg initialization
/docker/SherpA-collector ownership and modes
exact-source build and rollback
Traefik and certificate checks
Uptime Kuma readiness monitor
Railway collector-error and `/data/exports` queue-growth monitoring
maximum practical automatic Storage Box snapshot schedule as secondary protection
secret rotation
incident handling
retention dry-run, approval, prune, compact, verification, and removal of temporary recovery credentials
independent retrieval, decryption, and collector verify
secret-safe evidence
```

Do not include actual credentials, private hostnames, repository URLs, or key material.

- [ ] **Step 2: Document retention commands and approval**

Dry-run before approval:

```bash
borg prune --dry-run --list --glob-archives 'sherpa-*' --keep-daily 30 --keep-monthly 12
```

Only after fresh approval:

```bash
borg prune --list --glob-archives 'sherpa-*' --keep-daily 30 --keep-monthly 12
borg compact
borg check --verify-data
```

The runbook must require checking for unexpected deletion-marked archives before any unrestricted write.

- [ ] **Step 3: Correct Railway documentation**

Update the deployment guide to require:

```text
SHERPA_EXPORT_URL=https://sherpa-collector.kruth-support.de/v1/exports
SHERPA_EXPORT_ARCHIVE_DIR=/data/exports
SHERPA_EXPORT_TOKEN set as a Railway secret
```

Replace HTTP retrieval assumptions with recovery-only Borg extraction, age decryption, and `collector verify`. Correct old rightmost-XFF wording to Railway's sanitized `X-Real-IP` behavior.

- [ ] **Step 4: Correct the existing acceptance plan**

Replace recovery candidate `312479e` with the current accepted candidate lineage. Add the restart-safe queue and remote-commit/local-delete idempotency drills. Keep all existing disruptive-action approval gates.

- [ ] **Step 5: Validate documentation**

```bash
rg -n '312479e|rightmost.*X-Forwarded-For|collector.*retrieval interface' \
  docs/deployment/railway.md docs/superpowers/plans/2026-07-19-railway-staging-acceptance.md

git diff --check
```

Expected: no stale architectural assumption remains; historical evidence may still name the superseded candidate where explicitly labeled historical.

- [ ] **Step 6: Commit**

```bash
git add docs/deployment/collector.md docs/deployment/railway.md \
  docs/superpowers/plans/2026-07-19-railway-staging-acceptance.md
git commit -m "docs: add collector operations and recovery" \
  -m "Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 12: Run the Exact-Candidate Local Release Gates

**Files:**
- Modify only files needed to fix a gate failure through a separate TDD cycle.
- Do not create live evidence until all local gates pass for one exact commit.

**Interfaces:**
- Consumes: Tasks 1–11.
- Produces: one exact collector/registry candidate safe to deploy.

- [ ] **Step 1: Verify repository root and preserve unrelated files**

```bash
pwd
git status --short
```

Expected: project root is `/Users/timokruth/Projekte/feat`; the three `.DS_Store` paths remain untracked and untouched.

- [ ] **Step 2: Run formatting, unit, race, vet, and build gates**

```bash
test -z "$(gofmt -l .)"
go test -count=1 ./...
go test -race ./internal/recoveryarchive ./internal/collector ./internal/registry/export ./internal/registry ./cmd/collector ./cmd/registry
go vet ./...
go build ./cmd/...
git diff --check
```

Expected: all pass. Diagnose transient Docker PostgreSQL host-port resets separately; do not hide them with unrelated application changes.

- [ ] **Step 3: Run all production-image gates**

```bash
bash deploy/smoke_build.sh
bash deploy/web/smoke_build.sh
bash deploy/collector/smoke_build.sh
```

Expected: all pass.

- [ ] **Step 4: Record exact non-secret candidate data**

Record only:

```text
Git commit
Go version
Borg version 1.4.5
collector image ID
registry image ID
Compose file SHA-256
pass/fail summaries
```

- [ ] **Step 5: Stop on any failure**

Do not provision or deploy while a test, race, vet, image, or Compose gate is failing.

---

### Task 13: Provision and Deploy the External Collector

**Files:**
- Create during execution: `/docker/SherpA-collector` on the authorized VPS.
- Create during execution: `docs/deployment/evidence/2026-07-20-sherpa-offsite-collector-acceptance.md` locally.

**Interfaces:**
- Consumes: accepted exact source candidate and Task 0 capability result.
- Produces: trusted HTTPS collector, verified Storage Box commit path, and readiness monitoring.

- [ ] **Step 1: Request the exact collector deployment approval**

Present:

```text
Source commit
Compose checksum
collector image build inputs
Borg version
VPS path /docker/SherpA-collector
Storage Box resources and append-only conclusion
public hostname
rollback: docker compose down while retaining data and Storage Box archives
```

Expected: explicit approval before starting the public service or creating authoritative credentials.

- [ ] **Step 2: Generate recovery and routine identities without printing them**

Generate the age identity on a trusted recovery machine and keep the private file outside the repo and VPS. Copy only the public recipient to the VPS. Generate separate routine and recovery SSH keys; install only the routine private key on the VPS.

Expected: no secret appears in command output, Git status, image history, or evidence.

- [ ] **Step 3: Prepare the VPS directory**

Over SSH, create:

```text
/docker/SherpA-collector/source
/docker/SherpA-collector/config
/docker/SherpA-collector/secrets
/docker/SherpA-collector/data
```

Use directory modes 0750 for the deployment root/config, 0700 for secrets, and ownership UID/GID 10001 for `data` and secret files needed by the container. Secret files are mode 0400; `runtime.env` is mode 0600.

- [ ] **Step 4: Install the exact source and Compose file**

Clone/fetch the repository into `source`, check out the exact accepted commit in detached state, copy the reviewed Compose file to the deployment root, and verify its SHA-256 matches local evidence.

- [ ] **Step 5: Build and start**

```bash
docker compose build --pull
docker compose up -d
docker compose ps
collector_id="$(docker compose ps -q collector)"
test -n "$collector_id"
docker inspect "$collector_id" --format '{{json .HostConfig.PortBindings}}'
```

Expected: healthy service and empty host port bindings.

- [ ] **Step 6: Verify DNS and trusted TLS**

Verify authoritative and recursive DNS resolve to the VPS, then check the public certificate and HTTPS endpoints. Stop if DNS still points elsewhere or the certificate is untrusted.

- [ ] **Step 7: Verify pre-body controls**

Against the public endpoint, verify missing/invalid auth, missing length, oversized length, and wrong content type are rejected without sending an archive. Do not print the valid token.

- [ ] **Step 8: Add Uptime Kuma monitoring**

Monitor:

```text
https://sherpa-collector.kruth-support.de/readyz
```

Verify alert routing with a separately approved temporary threshold or isolated monitor. Do not deliberately interrupt unrelated VPS services.

- [ ] **Step 9: Record secret-safe collector evidence**

Record source commit, image ID, container health, certificate classification, append-only capability result, and monitor result. Do not record Storage Box or token details.

---

### Task 14: Integrate Railway Staging and Complete the Recovery Gate

**Files:**
- Update during execution: `docs/deployment/evidence/2026-07-20-railway-staging-acceptance.md`
- Update during execution: `docs/deployment/evidence/2026-07-20-sherpa-offsite-collector-acceptance.md`

**Interfaces:**
- Consumes: healthy external collector and accepted registry candidate.
- Produces: Phase 2c-iii.9–2c-iii.11 completion and an independently verified sibling restore.

- [ ] **Step 1: Request Approval D before Railway variable changes**

Present exact variable names without values:

```text
SHERPA_EXPORT_URL
SHERPA_EXPORT_TOKEN
SHERPA_EXPORT_ARCHIVE_DIR
SHERPA_EXPORT_INTERVAL
```

State that staging registry will restart, production remains untouched, and prior values will be saved to mode-0600 temporary storage for rollback.

- [ ] **Step 2: Configure normal staging export settings**

Set the new upload endpoint, mandatory token, and `/data/exports`. Wait for the replacement deployment to reach `SUCCESS`; verify health, existing stack metadata, clone, audit, and volume persistence before continuing.

- [ ] **Step 3: Prove registry retry persistence**

Using a bounded collector-unavailable condition that does not affect unrelated services:

```text
create one complete pending archive
verify it exists under /data/exports
restart/redeploy registry
verify startup rediscovers it
restore collector reachability
verify stored/existing response
verify local archive removal only after validated response
```

This is a disruptive registry drill and requires its own fresh approval immediately before execution if not already covered by Approval D.

- [ ] **Step 4: Request Approval E before the one-minute interval**

Save the exact current interval to a mode-0600 file without printing it. Set `SHERPA_EXPORT_INTERVAL=1m` only after approval.

- [ ] **Step 5: Obtain one complete exact object**

Wait for one `201 stored` or idempotent `200 existing` response. Record only the safe SHA-256 object ID, compressed/encrypted sizes, and timestamps. Restore the original interval immediately.

- [ ] **Step 6: Retrieve independently**

From a trusted recovery environment with the collector stopped or otherwise not used for retrieval:

```text
list Borg archives with recovery identity
select exact sherpa-<64hex>
extract <64hex>.tar.gz.age
decrypt with offline age identity
run collector verify
independently verify final manifest, every size, and every SHA-256
```

Expected: complete validation before any restore resource exists.

- [ ] **Step 7: Request Approval F before recovery resources**

Present exact count and cost impact:

```text
one postgres-recovery service
one registry-recovery service
one PostgreSQL volume
one /data Git volume
one temporary recovery domain
```

Use the corrected collector/registry candidate, not `312479e`. Do not create resources before approval.

- [ ] **Step 8: Restore and audit sibling resources**

Restore PostgreSQL with credentials in libpq environment variables, recreate bare Git repositories with mirror semantics, run `registry audit` as UID/GID 998, and test health, login, search, detail, and HTTPS clone.

- [ ] **Step 9: Perform final secret-safe scans**

Scan bounded collector, registry, and recovery logs for prohibited tokens, secrets, request bodies, private endpoints, and manifest payloads. Record classifications only.

- [ ] **Step 10: Update acceptance evidence**

Mark Phase 2c-iii.9–2c-iii.11 pass only if upload, independent retrieval, decryption, manifest verification, sibling restore, audit, smoke, monitoring, and secret review all pass.

- [ ] **Step 11: Preserve resources pending cleanup approval**

Do not delete recovery services, volumes, the recovery domain, encrypted downloads, or Storage Box archives. Every cleanup requires a later fresh approval.

- [ ] **Step 12: Commit evidence separately**

```bash
git add docs/deployment/evidence/2026-07-20-sherpa-offsite-collector-acceptance.md
git commit -m "docs: record off-site collector acceptance" \
  -m "Co-Authored-By: Claude <noreply@anthropic.com>"

git add docs/deployment/evidence/2026-07-20-railway-staging-acceptance.md
git commit -m "docs: record independent recovery drill" \
  -m "Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Commit Sequence

1. `feat(export): add shared recovery archive validation`
2. `feat(export): verify collector object identity`
3. `fix(registry): persist export retry queue`
4. `feat(collector): add configuration and archive verification`
5. `feat(collector): add streaming age encryption`
6. `feat(collector): add durable spool and object ledger`
7. `feat(collector): add constrained Borg backend`
8. `feat(collector): add crash-safe ingestion and retry`
9. `feat(collector): serve authenticated durable exports`
10. `build(collector): add hardened Compose deployment`
11. `docs: add collector operations and recovery`
12. Live evidence commits only after their corresponding gates pass.

## Stop Conditions

Stop and report rather than improvising when:

- Task 0 cannot prove that the routine identity lacks an alternate deletion path.
- A test does not fail for the intended missing behavior.
- Any archive validator case is ambiguous.
- Borg output contains unclassified data that might expose a private endpoint.
- The collector would need the age private identity.
- Docker Compose renders a host port or drops the hardening settings.
- DNS or certificate verification does not match `sherpa-collector.kruth-support.de`.
- A Railway operation resolves to production.
- The registry or collector loses a completed archive in any crash-window test.
- Independent retrieval or manifest verification fails.
- A requested cleanup, prune, restart, interval change, or recovery-resource creation lacks fresh explicit approval.

## Definition of Done

- All local, race, vet, build, image, and Compose gates pass on one exact commit.
- Task 0's deletion-resistance result is documented honestly.
- Collector runs hardened under `/docker/SherpA-collector` with trusted HTTPS and no host port.
- A valid export receives an exact safe object ID only after verified Borg presence.
- Invalid requests are rejected before body read where applicable.
- Collector and registry restarts preserve their respective retry queues.
- The exact encrypted object is retrievable and decryptable without Railway or collector retrieval APIs.
- Shared validation proves the final manifest and every artifact size/SHA-256.
- Sibling restore, audit, login, search, detail, and clone pass.
- Uptime Kuma detects stale/unavailable readiness.
- Evidence passes secret review.
- Railway production remains empty and unchanged.
