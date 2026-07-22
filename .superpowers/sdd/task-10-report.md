# Task 10 TDD and Verification Report

## Status

DONE

Task 10 only was implemented and committed. No VPS, Railway, Traefik control plane, Hetzner, Storage Box, or other deployment infrastructure was accessed or mutated. The only network access was the required public Docker image pulls and pinned BorgBackup PyPI artifact download during local image construction.

## Source and artifact identity

- Starting HEAD: `8976ff6ab2052b978d3a0ac92a13e02c32b8bbd4`
- Task 10 commit: `c1a85fa580000e50257d5033577b3999c9d23261`
- Commit subject: `build(collector): add hardened Compose deployment`
- Commit trailer: `Co-Authored-By: Claude <noreply@anthropic.com>`
- Final committed image ID: `sha256:29c1edbad690930cd7ddc4d5ab5cc14241021b70db4fe517faa0686ed0e1d7b9`
- Final OCI revision: `c1a85fa580000e50257d5033577b3999c9d23261`
- Final OCI source: `https://github.com/TimoKruth/SherpA`
- Final image size: `182807131` bytes
- Final image creation timestamp: `2026-07-22T06:43:35.539143965Z`

Safe final metadata inspection:

```text
id=sha256:29c1edbad690930cd7ddc4d5ab5cc14241021b70db4fe517faa0686ed0e1d7b9 user=10001:10001 source=https://github.com/TimoKruth/SherpA revision=c1a85fa580000e50257d5033577b3999c9d23261 entrypoint=["/usr/local/bin/collector"] cmd=["serve"]
size=182807131 created=2026-07-22T06:43:35.539143965Z
```

## TDD RED

The complete smoke assertions were written first in `deploy/collector/smoke_build.sh`, before the Dockerfile, Compose file, or runtime example existed.

Syntax RED prerequisite:

```text
$ bash -n deploy/collector/smoke_build.sh
(exit 0; no output)
```

Intended smoke RED:

```text
$ bash deploy/collector/smoke_build.sh
missing collector deployment file: deploy/collector/Dockerfile
(exit 1)
```

The failure was the intended missing-production-artifact failure, not a syntax or fixture error.

## Implementation

### Pinned multi-stage image

`deploy/collector/Dockerfile` now:

- Pins the exact reviewed image-index digests:
  - `golang:1.26.3-bookworm@sha256:386d475a660466863d9f8c766fec64d7fdad3edac2c6a05020c09534d71edb4b`
  - `python:3.13-slim-bookworm@sha256:9d7f287598e1a5a978c015ee176d8216435aaf335ed69ac3c38dd1bbb10e8d64`
- Builds the collector statically from `./cmd/collector` with trimpath and stripped symbols.
- Builds the Borg-tagged collector integration test binary separately from the production binary.
- Installs only the reviewed Borg build packages.
- Downloads only BorgBackup `1.4.5` from the exact PyPI source URL.
- Verifies SHA-256 `4f9a5fe584c504b15485841236750dea16aa7cd2ddbc4a594e9d2ce5c49c4508` before installation.
- Executes both Borg-tagged integration tests as UID/GID `10001:10001` against the installed Borg binary during the image build.
- Installs only the reviewed runtime packages.
- Removes apt caches, pip bytecode caches, the downloaded source artifact, temporary integration artifacts, and all setuid/setgid bits.
- Creates `/data`, `/data/spool`, `/data/state`, `/data/borg`, `/data/borg/cache`, `/data/borg/config`, and `/data/borg/security` as mode `0700`, UID/GID `10001:10001`.
- Sets only Borg cache/config/security paths under `/data/borg`; it does not bake a Borg repository or SSH command into the image.
- Uses exact runtime identity, entrypoint, and command:
  - `USER 10001:10001`
  - `ENTRYPOINT ["/usr/local/bin/collector"]`
  - `CMD ["serve"]`

### Hardened Compose deployment

`deploy/collector/compose.yaml` uses the required mirrored deployment layout and exact settings:

- Build context `./source` with the Task 10 Dockerfile.
- Exact `SOURCE_REVISION` build argument.
- `restart: unless-stopped`.
- UID/GID `10001:10001`.
- Read-only root filesystem.
- All capabilities dropped.
- `no-new-privileges:true`.
- PID, CPU, and memory limits.
- Internal exposure of `8080` only; no `ports` key and no host publication.
- `/tmp` tmpfs with `nosuid,nodev,noexec` and 64 MiB limit.
- Writable data mount and five exact read-only config/secret file mounts.
- Exact collector healthcheck command and timings.
- JSON log rotation at 10 MiB and five files.
- Exact existing Traefik `websecure`/`letsencrypt` labels.
- No buffering middleware, custom proxy network, FUSE device, privileged mode, `cap_add`, or device addition.

### Runtime example, ignore protections, and Makefile

- `deploy/collector/runtime.env.example` contains all approved non-secret collector variable names and safe defaults/paths only.
- It contains no repository location, SSH command, `SOURCE_REVISION`, key material, token value, database URL, or OAuth value.
- `.dockerignore` preserves existing exclusions and adds `.env*`, secrets, private-key patterns, known-host files, collector data, active runtime env, and local Compose override exclusions.
- Explicit negation keeps `deploy/collector/Dockerfile` in the build context; the Compose file, runtime example, and smoke script also remain available.
- `Makefile` adds `collector-image` and `collector-smoke` wiring.

## Final smoke GREEN on committed candidate

Command:

```text
$ bash deploy/collector/smoke_build.sh
```

The committed candidate rebuild completed with exit `0`. The safe acceptance lines were:

```text
--- PASS: TestBorgLocalRepositoryCreateAndExactPresence (0.99s)
--- PASS: TestCollectorEndToEndWithLocalBorgRepository (1.02s)
Collector Docker smoke passed: image=sha256:29c1edbad690930cd7ddc4d5ab5cc14241021b70db4fe517faa0686ed0e1d7b9 revision=c1a85fa580000e50257d5033577b3999c9d23261
```

A separate skip scan returned only the two PASS lines and no `SKIP` line:

```text
#16 1.157 --- PASS: TestBorgLocalRepositoryCreateAndExactPresence (0.99s)
#16 2.180 --- PASS: TestCollectorEndToEndWithLocalBorgRepository (1.02s)
```

The smoke run exercised the real final image and proved all of the following before printing its pass line:

- `collector verify` accepts a canonical safe recovery archive and returns exactly:
  - `valid`
  - `artifacts=1`
  - `verified_bytes=23`
  - `manifest_final=true`
- Borg reports exactly `borg 1.4.5`.
- The Borg-tagged integration tests run in the pinned Borg build rather than skipping for a missing local-host Borg binary.
- A separate local unencrypted Borg repository can be initialized in the final image.
- Borg create/list returns exactly the single expected smoke archive.
- The image config user is exactly `10001:10001`.
- The default entrypoint and command are exactly the collector and `serve`.
- Real collector `serve` is PID 1.
- PID 1 real/effective/saved/filesystem UID and GID are all `10001`.
- PID 1 `CapInh`, `CapPrm`, `CapEff`, and `CapBnd` are all zero.
- PID 1 has `NoNewPrivs: 1`.
- The root filesystem rejects writes.
- `/data` accepts writes.
- All runtime-owned data and Borg directories remain exact mode/owner `700:10001:10001`.
- The exact local command `/usr/local/bin/collector healthcheck http://127.0.0.1:8080/healthz` returns `healthy`.
- Docker runtime inspection shows `CapDrop=["ALL"]` and `SecurityOpt=["no-new-privileges:true"]`.
- Docker runtime inspection and `docker port` show no host port binding.
- The final image has no Go toolchain, C compiler command, build tool, curl, Git, FUSE command/device, SherpA source tree, `.go` file, Git metadata, SherpA fixture/testdata tree, apt cache, pip cache, setuid/setgid regular file, or exported file capability.
- Image config, history, environment, and labels do not contain the safe fixture values or secret-like runtime setting keys.
- Labels contain only the exact OCI source and revision keys.
- Compose renders the exact resources, security options, mounts, healthcheck, logging, labels, and build args.
- Rendered Compose contains no `ports:` entry.

## Compose validation GREEN

Standalone mirrored-layout validation:

```text
$ SOURCE_REVISION="$(git rev-parse HEAD)" docker compose --project-directory "$work" -f "$work/compose.yaml" config
Compose config passed: services=1 published_ports=0
(exit 0)
```

The smoke script also ran both YAML and JSON Compose rendering from a temporary `/docker/SherpA-collector`-shaped root, copied `runtime.env.example` to `runtime.env`, symlinked `source` to the repository root, and created safe temporary `config`, `secrets`, and `data` fixtures.

## Makefile GREEN

Dry-run wiring:

```text
$ make -n collector-image collector-smoke
docker build --file deploy/collector/Dockerfile --build-arg SOURCE_REVISION="$(git rev-parse HEAD)" --tag sherpa-collector:local .
bash deploy/collector/smoke_build.sh
```

Real `collector-image` target:

```text
$ make collector-image
--- PASS: TestBorgLocalRepositoryCreateAndExactPresence (1.09s)
--- PASS: TestCollectorEndToEndWithLocalBorgRepository (1.14s)
PASS
writing image sha256:ca6aa00f41b5a47d3d83139036686dc8f1b4d351b56c9c8ea4e29d073be5911a
naming to docker.io/library/sherpa-collector:local
(exit 0)
```

That Makefile build occurred before the Task 10 commit, so the final acceptance image was rebuilt afterward and carries the committed revision and final image ID listed above.

## Go verification GREEN

Focused committed collector tests:

```text
$ go test -count=1 ./cmd/collector ./internal/collector
ok  	sherpa/cmd/collector	0.581s
ok  	sherpa/internal/collector	4.323s
```

Fresh committed full repository test:

```text
$ go test -count=1 ./...
ok  	sherpa/cmd/collector	0.391s
ok  	sherpa/cmd/registry	38.510s
?   	sherpa/cmd/sherpa	[no test files]
ok  	sherpa/cmd/web	0.752s
?   	sherpa/deploy/web/testfixture	[no test files]
ok  	sherpa/internal/cli	26.334s
ok  	sherpa/internal/collector	7.589s
ok  	sherpa/internal/gitutil	1.970s
ok  	sherpa/internal/harness	1.864s
ok  	sherpa/internal/integration	29.477s
ok  	sherpa/internal/launch	8.854s
ok  	sherpa/internal/profile	2.866s
ok  	sherpa/internal/publishscan	3.631s
ok  	sherpa/internal/quarantine	2.901s
ok  	sherpa/internal/recoveryarchive	2.041s
?   	sherpa/internal/recoveryarchive/testfixture	[no test files]
ok  	sherpa/internal/registry	2.221s
ok  	sherpa/internal/registry/api	14.732s
ok  	sherpa/internal/registry/audit	15.419s
ok  	sherpa/internal/registry/auth	2.295s
ok  	sherpa/internal/registry/content	13.984s
ok  	sherpa/internal/registry/export	5.153s
ok  	sherpa/internal/registry/store	32.263s
ok  	sherpa/internal/registryurl	0.815s
ok  	sherpa/internal/review	0.973s
ok  	sherpa/internal/sanitize	0.946s
ok  	sherpa/internal/stack	0.881s
ok  	sherpa/internal/state	1.189s
ok  	sherpa/internal/update	6.237s
ok  	sherpa/internal/web	2.047s
ok  	sherpa/internal/web/registryclient	1.413s
```

Fresh committed full race test:

```text
$ go test -race -count=1 ./...
ok  	sherpa/cmd/collector	1.616s
ok  	sherpa/cmd/registry	22.367s
?   	sherpa/cmd/sherpa	[no test files]
ok  	sherpa/cmd/web	2.318s
?   	sherpa/deploy/web/testfixture	[no test files]
ok  	sherpa/internal/cli	24.628s
ok  	sherpa/internal/collector	58.149s
ok  	sherpa/internal/gitutil	3.629s
ok  	sherpa/internal/harness	3.521s
ok  	sherpa/internal/integration	25.539s
ok  	sherpa/internal/launch	9.501s
ok  	sherpa/internal/profile	4.469s
ok  	sherpa/internal/publishscan	4.025s
ok  	sherpa/internal/quarantine	3.130s
ok  	sherpa/internal/recoveryarchive	2.755s
?   	sherpa/internal/recoveryarchive/testfixture	[no test files]
ok  	sherpa/internal/registry	2.839s
ok  	sherpa/internal/registry/api	8.153s
ok  	sherpa/internal/registry/audit	7.304s
ok  	sherpa/internal/registry/auth	2.742s
ok  	sherpa/internal/registry/content	8.296s
ok  	sherpa/internal/registry/export	4.870s
ok  	sherpa/internal/registry/store	24.716s
ok  	sherpa/internal/registryurl	1.514s
ok  	sherpa/internal/review	1.396s
ok  	sherpa/internal/sanitize	1.563s
ok  	sherpa/internal/stack	1.428s
ok  	sherpa/internal/state	1.660s
ok  	sherpa/internal/update	4.521s
ok  	sherpa/internal/web	2.691s
ok  	sherpa/internal/web/registryclient	1.656s
```

## Static and diff checks GREEN

```text
$ bash -n deploy/collector/smoke_build.sh
(exit 0; no output)

$ git diff --check
(exit 0; no output)

$ git diff --cached --check
(exit 0; no output before commit)
```

Credential-material scan over the Task 10 files checked for private-key headers, database URLs, OAuth/client-secret assignments, and age private identities:

```text
(exit 0; no matches)
```

Final status:

```text
?? .DS_Store
?? docs/.DS_Store
?? docs/superpowers/.DS_Store
```

The three required `.DS_Store` files remained unchanged, unstaged, and uncommitted.

## Transient failures and corrections

All transient failures were kept as honest evidence and corrected without weakening the required security contract:

1. Compose emitted canonicalized temporary paths, so exact string assertions initially failed. Assertions now compare real paths while still proving the build context and every bind source resolve to the expected mirrored root.
2. Compose JSON omits `read_only: false` on writable volumes. The assertion now treats an omitted value as false and still requires all five sensitive mounts to be explicitly read-only.
3. The Borg integration initially failed because the runtime user home is intentionally `/nonexistent`. The build-only integration invocation now uses a temporary UID/GID-10001-owned home that is deleted in the same layer.
4. Removing Borg's upstream `borg.testsuite` broke `borg --version` because Borg 1.4.5 imports a subset for its built-in self-test. The cleanup was corrected to retain Borg's required upstream runtime module while still rejecting SherpA tests, testdata, fixtures, `.go` files, and source trees.
5. The filesystem scan initially mistook Debian's `/usr/bin/test` and `/usr/share/gcc` directory for a shipped test fixture/compiler. Assertions were narrowed to regular compiler/build-tool executables and SherpA fixture paths; runtime `command -v` checks independently prove the tools are unavailable.
6. Debian left `/var/cache/apt/archives/lock`; runtime and build stages now remove `/var/cache/apt/*` as well as apt lists.
7. Python `tarfile` emitted additional record padding that the collector correctly classified as trailing data. The smoke fixture generator now uses Go's canonical tar/gzip writers, matching the production archive contract.
8. Docker preserves `no-new-privileges:true` in `HostConfig.SecurityOpt`; the assertion was corrected to Docker's exact inspected representation.
9. The first pre-commit full race run encountered a transient local PostgreSQL test-container connection reset in `internal/registry/store`. Immediate focused retry passed:

```text
$ go test -race -count=1 ./internal/registry/store
ok  	sherpa/internal/registry/store	19.441s
```

The full race suite then passed, and the post-commit full race suite also passed as recorded above.

## Changed files

Committed files only:

- `.dockerignore`
- `Makefile`
- `deploy/collector/Dockerfile`
- `deploy/collector/compose.yaml`
- `deploy/collector/runtime.env.example`
- `deploy/collector/smoke_build.sh`

The report remains in ignored scratch storage and was not staged or committed.

## Self-review

The exact staged diff and final commit were reviewed for:

- Secret leakage: no real secret, key, token, repository location, database URL, OAuth value, request body, or manifest payload is committed or printed in acceptance evidence. Smoke values are explicit safe dummy fixtures and are scanned out of image metadata/history.
- Image metadata leakage: final image environment has Borg cache/config/security paths only; labels contain only exact public source and committed revision.
- Source/cache inclusion: no SherpA source tree, Git metadata, `.go` file, SherpA testdata/fixtures, Go/C compiler executable, build tool, apt cache, pip cache, or temporary integration artifact remains in the final image.
- Borg upstream self-test module: retained because Borg 1.4.5 imports it at runtime; this is not SherpA source/testdata and removing it makes the pinned Borg artifact nonfunctional.
- Host exposure: Compose and runtime publish no host port. `EXPOSE 8080` is image metadata/internal documentation only.
- Runtime identity: image, Compose, PID 1, and data ownership all use exact UID/GID `10001:10001`.
- Root filesystem/capabilities: Compose and real runtime use read-only root, all capabilities dropped, no-new-privileges, no setuid/setgid files, and no exported file capabilities.
- FUSE/privilege additions: no FUSE package/command/device, `cap_add`, privileged mode, or device mount exists.
- Proxy integration: exact existing Traefik labels only; no buffering and no invented network.
- Scope: only Task 10 files were committed; no external infrastructure or unrelated source was changed.

## Deviations and concerns

- No requirement deviation remains.
- BorgBackup 1.4.5 necessarily ships its upstream `borg.testsuite` module because `borg.selftest` imports it during normal startup. The final assertions therefore distinguish required upstream Borg runtime support from forbidden SherpA source/tests/testdata. Borg version, integration, create, list, serve, and healthcheck all pass with this pinned artifact.
- No deployment was attempted. The produced artifact is local only, as required.

# Task 10 Independent-Review Fix Wave

## Status and commits

DONE. The four independent-review findings were fixed in three focused follow-up commits without amending the original Task 10 commit:

- `42bb032d36bafba34b2d4b22ab1243e6aeb42154` — `fix(collector): harden image smoke provenance`
- `1d511b732f7da2f824d0efb8d3b3786326a253ae` — `fix(collector): verify Compose bind source`
- `b4db6041a01d047e79d08e0e34dbc6998fbd8e2e` — `fix(collector): scan image layers and fixtures`

Each commit has the required `Co-Authored-By: Claude <noreply@anthropic.com>` trailer. No commit was amended, rebased, pushed, or deployed.

Final artifact identity:

```text
id=sha256:47d88d7a166b8701957a756cb7716f38b475a2bb55e598543be0c6dce3919cb4
revision=b4db6041a01d047e79d08e0e34dbc6998fbd8e2e
source=https://github.com/TimoKruth/SherpA
user=10001:10001
entrypoint=["/usr/local/bin/collector"]
cmd=["serve"]
```

## Strict RED evidence

### A. Empty real bind mount

The old image was started with the real empty host bind instead of Docker volume copy-up:

```text
$ <safe temporary fixture> docker run ... --mount type=bind,src=<temp>/data,dst=/data sherpa-collector:test
empty-bind-startup=exited:1
```

This reproduced the masked startup failure. The old smoke used `data_volume=` and `type=volume,src=$data_volume,dst=/data`.

### B. Exact commit provenance

A committed archive was copied to a temporary context, a relevant untracked `cmd/collector/provenance_probe.go` was added, and the old live-context build was labeled with the clean HEAD. The dirty and clean collector binary hashes differed while the OCI revision stayed at HEAD:

```text
$ <safe temporary dirty-context build and binary comparison>
dirty-binary-with-clean-revision=accepted
```

The focused provenance test initially failed because `deploy/collector/smoke_checks.py` did not exist and the smoke still built from `"$root_dir"`.

### C. Python optimization bypass

```text
$ PYTHONOPTIMIZE=1 python3 -c 'assert False, "security gate must fail"'; printf 'optimized-assert-exit=0\n'
optimized-assert-exit=0
```

The first focused architecture test also failed on the old `python3 -` heredoc gates and their `assert` statements.

### D. Metadata/history and regular-file content leakage

The old metadata scanner accepted an OAuth secret-style history assignment, and the old exported-filesystem scanner accepted an OpenSSH private-key header plus a safe content sentinel:

```text
$ <safe in-memory mutation of the old scanner logic>
current-metadata-mutation=accepted
current-file-content-mutation=accepted
```

Additional RED regressions caught the old `lstrip("./")` dotfile normalization bug, JSON/YAML-style assignments, missing saved-layer scanning, runtime secret ownership, and Docker Desktop `/host_mnt` path translation.

## Implementation and GREEN evidence

### Real bind-mounted startup

- The mirrored root now creates exact `data/spool`, `data/state`, `data/borg`, `data/borg/cache`, `data/borg/config`, and `data/borg/security` directories through the final image with mode `0700` and UID/GID `10001:10001`.
- Config and secret fixtures are also assigned exact runtime-readable ownership and modes before startup: config directory/files `0755`/`0644`; secrets directory/files `0700`/`0600`.
- Layout and fixture ownership are checked inside the exact bind namespace before startup and rechecked after startup.
- The real rendered Compose service is started with `docker compose ... up --detach --no-build`; the exact `/data` mount is verified as a writable bind with the expected source.
- Real `serve` remains PID 1, the exact healthcheck returns `healthy`, root is read-only, `/data` is writable, all capabilities remain dropped, no-new-privileges remains active, and no host port is published.
- Docker Desktop `/host_mnt` source translations are canonicalized without weakening exact source matching.

### Exact provenance

- `smoke_build.sh` constructs its build context with `git archive <HEAD>` into a temporary clean source directory.
- Compose validation, Go fixture generation, Dockerfile selection, and the Docker build all use that exact archive.
- Dirty tracked, staged, and relevant untracked sources are excluded by construction; the focused test proves the archive retains committed content only and contains neither the dirty/untracked source nor `.git`.
- `Makefile` now routes `collector-image` through `smoke_build.sh --build-only`, so the real Make target has the same exact-source guarantee.
- The compiled collector and OCI revision both originate from final commit `b4db6041a01d047e79d08e0e34dbc6998fbd8e2e`.

### Explicit isolated Python gates

- All Compose, metadata, filesystem, layout, runtime-file, bind-source, and saved-layer gates use explicit `require(...)` checks that exit nonzero.
- Host Python invocations use `python3 -I`; the Borg JSON check uses `python -I` and explicit `SystemExit` logic.
- No production smoke gate contains Python `assert`.
- The optimization mutation passes only when rejected: the focused test runs the invalid Compose fixture under both isolated and optimized Python and observes a nonzero exit.

### Complete leakage scan

- Environment, labels, and full history are checked for secret-like token/secret/password/key/repository/SSH/database/OAuth assignments, including shell, JSON, and YAML-style separators.
- Only the exact public OCI labels, pinned public base-image variables, and exact Borg path variables are permitted.
- The merged exported filesystem is scanned binary-safely and with explicit per-file/total bounds for safe sentinels, private-key/OpenSSH/age identity headers, assignment keys, prohibited deployment artifacts, source/tool/cache leakage, privilege bits, and capabilities.
- `docker image save` layers are additionally scanned, so content copied and deleted in a later layer cannot evade detection.
- Exact `./` normalization preserves `.env` and `.git` names rather than stripping their leading dot.
- Known vendor runtime trees are exempted only from generic assignment parsing to avoid false positives from Debian, Python, OpenSSH, certificates, and Borg runtime code; all regular files still receive bounded sentinel/private-header scans and prohibited-path checks.
- Failure messages identify only the category/path/key context and never print matched values or file contents.

Focused GREEN:

```text
$ python3 -I deploy/collector/smoke_checks_test.py
Ran 15 tests in 1.576s
OK
```

The 15 focused tests include dirty/staged/untracked provenance, optimization bypass, exact Compose bind source, Docker Desktop translation, runtime fixture ownership/modes, metadata/history mutations, JSON-style assignments, dotfile artifacts, regular-file private headers/sentinels, deleted-layer content, vendor false-positive controls, and static smoke architecture assertions.

Final exact committed smoke GREEN:

```text
$ bash deploy/collector/smoke_build.sh
--- PASS: TestBorgLocalRepositoryCreateAndExactPresence (0.97s)
--- PASS: TestCollectorEndToEndWithLocalBorgRepository (0.99s)
Collector Docker smoke passed: image=sha256:47d88d7a166b8701957a756cb7716f38b475a2bb55e598543be0c6dce3919cb4 revision=b4db6041a01d047e79d08e0e34dbc6998fbd8e2e
```

No pinned Borg integration was skipped. The same run validated Compose config, no host ports, the exact bind-mounted `serve` process, exact healthcheck, ownership/modes before and after startup, metadata/history, merged filesystem, and every saved image layer.

Makefile GREEN:

```text
$ make -n collector-image collector-smoke
bash deploy/collector/smoke_build.sh --build-only sherpa-collector:local
bash deploy/collector/smoke_build.sh

$ make collector-image
--- PASS: TestBorgLocalRepositoryCreateAndExactPresence (0.97s)
--- PASS: TestCollectorEndToEndWithLocalBorgRepository (0.95s)
Collector exact build passed: image=sha256:47d88d7a166b8701957a756cb7716f38b475a2bb55e598543be0c6dce3919cb4 revision=b4db6041a01d047e79d08e0e34dbc6998fbd8e2e
```

Go gates on final HEAD:

```text
$ go test -count=1 ./cmd/collector ./internal/collector
ok  sherpa/cmd/collector
ok  sherpa/internal/collector

$ go test -count=1 ./...
(all packages passed on the exact rerun)

$ go test -race -count=1 ./...
(all packages passed on the exact final rerun)
```

Static/material gates:

```text
$ bash -n deploy/collector/smoke_build.sh
(exit 0)

$ git diff --check
(exit 0)

$ <bounded credential-material scan over Makefile and deploy/collector>
(exit 0; no material detected)

$ git status --short
?? .DS_Store
?? docs/.DS_Store
?? docs/superpowers/.DS_Store
```

## Transients and corrections

1. The first committed full smoke reached real Compose startup but its shell string comparison rejected Docker's canonical bind-source representation. A focused runtime-inspection test was added first, then the check was moved to explicit JSON validation with exact canonical path matching.
2. Independent read-only review identified native-Linux secret fixture ownership, Docker Desktop `/host_mnt` translation, dotfile normalization, structured assignment coverage, and deleted-layer recovery. Focused REDs were added and each issue was fixed before the final candidate.
3. The final non-race repository gate had one PostgreSQL connection reset in `internal/registry/store`. The focused retry passed, then the exact full gate passed.
4. The final race gate experienced repeated transient PostgreSQL container resets in `internal/registry/store` and `internal/integration`. Each focused retry passed. The exact final `go test -race -count=1 ./...` rerun then passed all packages.
5. A final optional Codex re-review timed out after its MCP stream expired; it made no file changes. The earlier completed review findings were all covered by focused tests and the final gates above.

## Changed files in the fix wave

Committed:

- `Makefile`
- `deploy/collector/smoke_build.sh`
- `deploy/collector/smoke_checks.py`
- `deploy/collector/smoke_checks_test.py`

The original Dockerfile, Compose file, runtime example, and `.dockerignore` contract remain unchanged. This report remains ignored and uncommitted. The three preserved `.DS_Store` files remain untouched, unstaged, and uncommitted.

## Fix-wave self-review and deviations

- All original Task 10 assertions remain active; the Python assertions were replaced rather than removed.
- The exact committed source archive is the only Docker build context used by both smoke and Makefile builds.
- The real Compose service, exact bind source, exact directory/file ownership/modes, PID 1, healthcheck, security options, and no-port contract are exercised together.
- Secret leakage checks now cover approved metadata, broad assignment forms, merged regular-file contents, and all recoverable final-image layers without printing values.
- Public age recipients, known-host material, certificates, pinned Python/Borg runtime, and required `borg.testsuite` support do not false-positive.
- macOS native `stat` reports the host account because Docker Desktop virtualizes UID ownership; the exact bind as presented to the Linux engine and runtime is verified as UID/GID `10001:10001`. On native Linux, the same root-container initialization applies UID/GID 10001 directly to the host bind files and directories.
- No external infrastructure was accessed. No real secret was used or printed.
- No requirement deviation remains.

# Task 10 Final Binary Scanner and Cleanup Fix Wave

## Scope and RED evidence

A final strict RED→GREEN wave addressed generalized private-material and reviewed-source bypasses plus partial Compose startup cleanup.

The focused generalized RED invoked four test methods and produced 18 failing subtests:

- NUL-adjacent generic PKCS#8, encrypted PKCS#8, RSA, DSA, and EC headers passed direct, merged-filesystem, and saved-layer scans.
- Reviewed Python/Borg source files accepted `UPLOAD_TOKEN`, `DATABASE_PASSWORD`, and `SECRET_KEY` assignments.
- A further RED proved that a new sensitive key in an otherwise reviewed path (`usr/local/lib/python3.13/http/server.py`) was accepted.

No RED failure printed a matched credential value or file content.

## Implemented controls

- Every entry in `PRIVATE_HEADERS` now receives the same NUL-adjacent structured byte check; the one-off OpenSSH branch was removed.
- The NUL check remains bounded by the existing 64 MiB per-file and 1 GiB total limits and runs before binary assignment parsing returns.
- Six pinned libgnutls test-vector collisions are allowed only by normalized exact path plus SHA-256 of the complete NUL-delimited record. A changed record, different path, extra bytes before its terminating NUL, or any unreviewed private header fails closed.
- The secondary partial `SOURCE_CREDENTIAL_KEY` vocabulary was removed.
- Reviewed Python, Borg, pip, and Perl source now uses an exact 406-path/1263-key allowlist built from both the merged image and all recoverable layers. Unknown paths and new sensitive keys fail closed. Architecture-specific Perl and Python paths are normalized without broadening keys.
- Exact GPG value/history-form checks and bearer credential classification remain active.
- Compose cleanup is armed before `docker compose up`, always runs `down --volumes --remove-orphans` after a partial start, and removes the temporary `${project}-collector` image alias on both success and failure.

## Focused GREEN

All 31 scanner/smoke architecture tests passed in each required mode:

```text
$ python3 -I deploy/collector/smoke_checks_test.py
Ran 31 tests in 5.610s
OK

$ PYTHONOPTIMIZE=1 python3 -I deploy/collector/smoke_checks_test.py
Ran 31 tests in 5.706s
OK

$ python3 -I -O deploy/collector/smoke_checks_test.py
Ran 31 tests in 5.714s
OK
```

The generalized direct, merged-filesystem, and saved-layer private-header mutations all reject without disclosure. Arbitrary source credentials and a new sensitive key in a known reviewed path also reject. Exact public runtime false-positive fixtures still pass.

## Final committed gates

Committed fix:

```text
62efa227f4df2413eedf10408446cb0ef4228d5d  fix(collector): close binary scanner bypasses
```

Exact committed smoke:

```text
$ bash deploy/collector/smoke_build.sh
--- PASS: TestBorgLocalRepositoryCreateAndExactPresence (0.99s)
--- PASS: TestCollectorEndToEndWithLocalBorgRepository (0.98s)
Collector Docker smoke passed: image=sha256:0a3462927a43843bc2a7d0d867e4f1ff52841af69551f7ee0209efc7aa4bf9e5 revision=62efa227f4df2413eedf10408446cb0ef4228d5d
```

The smoke validated exact archive provenance, metadata/history, complete merged filesystem, every saved layer, Borg 1.4.5, archive verification, real bind-mounted Compose serve, PID 1/security settings, and the exact local healthcheck. Neither pinned Borg integration test skipped. Post-run checks found no smoke containers, networks, or temporary image aliases; stale aliases from earlier pre-fix runs were also removed.

Make image gate:

```text
$ make collector-image
--- PASS: TestBorgLocalRepositoryCreateAndExactPresence (0.93s)
--- PASS: TestCollectorEndToEndWithLocalBorgRepository (0.95s)
Collector exact build passed: image=sha256:0a3462927a43843bc2a7d0d867e4f1ff52841af69551f7ee0209efc7aa4bf9e5 revision=62efa227f4df2413eedf10408446cb0ef4228d5d
```

Go and static/material gates:

```text
$ go test -count=1 ./cmd/collector ./internal/collector
ok  sherpa/cmd/collector
ok  sherpa/internal/collector

$ go test -count=1 ./...
(all packages passed on the exact rerun)

$ go test -race -count=1 ./...
(all packages passed)

$ bash -n deploy/collector/smoke_build.sh
(exit 0)

$ git diff HEAD^ HEAD --check
(exit 0)

$ <bounded credential-material scan over Makefile and deploy/collector>
bounded credential-material scan passed
```

The first full non-race Go run had one local PostgreSQL connection reset in `internal/integration`. The exact focused retry passed, followed by a clean exact full-suite rerun. The race suite passed on its first run.

## Final paths and status

The commit contains only:

- `deploy/collector/smoke_build.sh`
- `deploy/collector/smoke_checks.py`
- `deploy/collector/smoke_checks_test.py`

Final status contains only the three preserved untracked `.DS_Store` files. This report remains ignored and uncommitted. No external infrastructure was accessed, no real secret was used or printed, and nothing was pushed or deployed.

# Task 10 Independent-Review Binary Assignment and Link-Metadata Fix

## Scope

This focused follow-up addresses the two Important defects from the independent read-only review:

1. NUL-containing regular-file bodies no longer bypass sensitive assignment scanning.
2. Exported-filesystem and saved-layer scans now inspect symlink/hardlink targets, and saved layers apply prohibited-path checks before filtering by tar member type.

The existing exact runtime/source assignment allowlists and exact libgnutls collision exceptions were not changed.

## Strict RED evidence

The focused regressions were added before production changes. They cover direct `check_file_contents`, exported filesystems, and saved layers for `UPLOAD_TOKEN`, `DATABASE_PASSWORD`, `SECRET_KEY`, `OAUTH_CLIENT_SECRET`, `THIRD_PARTY_API_KEY`, and a new `NEW_VENDOR_CREDENTIAL` key. They also cover symlink and hardlink targets containing assignments, safe secret sentinels, and prohibited artifact paths; prohibited member paths across regular file, directory, symlink, hardlink, and FIFO types; non-disclosing diagnostics; and accepted benign relative runtime links.

Exact RED commands and summarized output:

```text
$ python3 -I deploy/collector/smoke_checks_test.py
Ran 39 tests in 10.890s
FAILED (failures=34)
exit=1

$ PYTHONOPTIMIZE=1 python3 -I deploy/collector/smoke_checks_test.py
Ran 39 tests in 10.914s
FAILED (failures=34)
exit=1

$ python3 -I -O deploy/collector/smoke_checks_test.py
Ran 39 tests in 10.914s
FAILED (failures=34)
exit=1
```

The failures were the expected existing bypasses: 18 NUL-assignment subtests, 12 malicious link-target subtests, and four non-regular saved-layer prohibited-path subtests. No failed diagnostic printed a target or assigned value.

## Implementation

- Removed the NUL-body early return so the existing bounded byte assignment parser and exact path/key allowlists apply to binary and text bodies alike.
- Added a 64 KiB link-target scan bound.
- Symlink and hardlink targets are checked for safe secret sentinels, private-key/age/bearer material, sensitive assignments, and prohibited deployment artifact paths without including the target in diagnostics.
- Saved layers now reject prohibited member paths before skipping non-regular entries.
- Benign relative symlink and hardlink targets remain accepted; there is no blanket link rejection.

## GREEN evidence

Focused eight-test subset in all required modes:

```text
$ python3 -I deploy/collector/smoke_checks_test.py <8 focused test methods>
Ran 8 tests in 5.077s
OK

$ PYTHONOPTIMIZE=1 python3 -I deploy/collector/smoke_checks_test.py <8 focused test methods>
Ran 8 tests in 5.123s
OK

$ python3 -I -O deploy/collector/smoke_checks_test.py <8 focused test methods>
Ran 8 tests in 5.188s
OK
```

Fresh full scanner verification in all required modes:

```text
$ python3 -I deploy/collector/smoke_checks_test.py
Ran 39 tests in 10.846s
OK

$ PYTHONOPTIMIZE=1 python3 -I deploy/collector/smoke_checks_test.py
Ran 39 tests in 10.856s
OK

$ python3 -I -O deploy/collector/smoke_checks_test.py
Ran 39 tests in 10.860s
OK
```

Static verification:

```text
$ python3 -m py_compile deploy/collector/smoke_checks.py deploy/collector/smoke_checks_test.py
(exit 0; no output)

$ git diff --check
(exit 0; no output)
```

No Docker, Go suite, race suite, VPS, Railway, Traefik, Hetzner, Storage Box, or other external infrastructure was accessed. Final independent re-review is still required before Task 10 can be considered complete.

# Task 10 Second Independent-Review Link-Metadata Isolation Fix

## Scope

This second focused independent-review fix addresses the two remaining Important link-metadata defects:

1. Symlink and hardlink metadata no longer inherits regular-file path/key assignment allowlists.
2. Private-key headers and age private identities are rejected when embedded after `../`, `./`, or `/` path prefixes in link targets.

The existing 64 KiB link target bound, bounded regular-file reads, exact libgnutls collision exceptions, regular-file assignment allowlists, prohibited-path checks, and non-disclosing diagnostics remain unchanged.

## Strict RED evidence

Focused regressions were added before production changes for both symlinks and hardlinks through exported filesystems and saved image layers. The tests used an allowlisted member path (`etc/ssl/openssl.cnf`) with a sensitive assignment target, and every private header plus an age private identity after each required path prefix.

Exact RED results in all required Python modes:

```text
$ python3 -I deploy/collector/smoke_checks_test.py <2 focused test methods>
Ran 2 tests in 12.446s
FAILED (failures=88)
exit=1

$ PYTHONOPTIMIZE=1 python3 -I deploy/collector/smoke_checks_test.py <2 focused test methods>
Ran 2 tests in 13.368s
FAILED (failures=88)
exit=1

$ python3 -I -O deploy/collector/smoke_checks_test.py <2 focused test methods>
Ran 2 tests in 13.146s
FAILED (failures=88)
exit=1
```

The 88 expected failures were four regular-file-allowlist inheritance bypasses plus 84 path-prefixed private/age material bypasses. There were no test errors or archive-serialization failures, and diagnostics did not print target values or private material.

## Implementation

- Added a distinct bounded link-content scanner used only for symlink and hardlink metadata.
- Link assignment scanning never calls `runtime_assignment_allowed`, so regular-file path/key exceptions cannot authorize link targets.
- Every private-key header and age private identity is detected anywhere in the bounded link target, including after relative and absolute path prefixes.
- Safe sentinels, Bearer credentials, NUL-delimited sensitive assignments, oversized targets, and prohibited target paths remain rejected without disclosure.
- Benign relative runtime links remain accepted; links are not blanket-rejected.
- Regular-file bodies continue using the unchanged exact path/key allowlists and exact libgnutls NUL-record exceptions.

## GREEN evidence

The 11-test focused link scanner set passed in all required modes. It covers both link types and both archive scanners, allowlist isolation, prefixed private/age material, Bearer material, NUL assignments, the 64 KiB bound, prohibited targets/member paths, benign links, non-disclosing diagnostics, and a positive regular-file exact allowlist case.

```text
$ python3 -I deploy/collector/smoke_checks_test.py <11 focused test methods>
Ran 11 tests in 17.453s
OK

$ PYTHONOPTIMIZE=1 python3 -I deploy/collector/smoke_checks_test.py <11 focused test methods>
Ran 11 tests in 17.387s
OK

$ python3 -I -O deploy/collector/smoke_checks_test.py <11 focused test methods>
Ran 11 tests in 17.555s
OK
```

Fresh full scanner verification passed in all required modes:

```text
$ python3 -I deploy/collector/smoke_checks_test.py
Ran 45 tests in 24.621s
OK

$ PYTHONOPTIMIZE=1 python3 -I deploy/collector/smoke_checks_test.py
Ran 45 tests in 24.355s
OK

$ python3 -I -O deploy/collector/smoke_checks_test.py
Ran 45 tests in 24.323s
OK
```

No Docker, Go suite, race suite, VPS, Railway, Traefik, Hetzner, Storage Box, or other external infrastructure was accessed. Another independent re-review is required before Task 10 can be considered complete.

# Task 10 Third Independent-Review Parser Length and Standalone Bearer Fix

## Scope

This third focused independent-review fix addresses the two remaining Important general parser defects:

1. Sensitive assignment recognition no longer has a finite key-length ceiling.
2. Standalone case-insensitive `Bearer <value>` material is rejected without requiring an Authorization assignment.

The existing bounded file/link reads, exact regular-file path/key allowlists, exact libgnutls NUL-record exceptions, prohibited-path checks, and non-disclosing diagnostics remain unchanged.

## Strict RED evidence

Eight focused regressions were added before production changes. They cover 129-byte, 1215-byte, and 32781-byte sensitive keys with the sensitive segment at the beginning, middle, and end; mixed case; `_`, `.`, and `-` separators; both `=` and `:` assignments; direct regular-file scanning; the direct link helper; exported regular files; exported symlinks and hardlinks; and malicious content placed independently in every saved layer. Standalone Bearer regressions cover case variants, NUL/newline/path-prefixed forms, regular files, symlinks, hardlinks, exported filesystems, and every saved layer. All failure assertions also verify that matched keys, targets, and credential values are not disclosed. A positive regression retains benign bearer prose/code.

Exact RED results in all required modes:

```text
$ python3 -I deploy/collector/smoke_checks_test.py <8 focused test methods>
Ran 8 tests in 3.602s
FAILED (failures=43)
exit=1

$ PYTHONOPTIMIZE=1 python3 -I deploy/collector/smoke_checks_test.py <8 focused test methods>
Ran 8 tests in 3.578s
FAILED (failures=43)
exit=1

$ python3 -I -O deploy/collector/smoke_checks_test.py <8 focused test methods>
Ran 8 tests in 3.563s
FAILED (failures=43)
exit=1
```

The failures were the expected prior bypasses only: long sensitive assignments and standalone Bearer material were accepted. The benign bearer fixture passed during RED, and there were no test errors in the recorded RED runs.

## Implementation

- Both text and byte assignment patterns now accept the complete identifier key within the already bounded input rather than stopping at 128 bytes. No replacement key-length threshold was introduced.
- The patterns remain a single bounded pass over the existing 64 MiB per-file or 64 KiB per-link input, with the existing total saved-content bound unchanged.
- Authorization Bearer/Token detection remains active.
- A separate standalone case-insensitive Bearer detector rejects credential material in text, binary/NUL-delimited files, and link targets.
- A narrow exact follower vocabulary preserves benign prose such as `bearer authentication`, `bearer token`, and `bearer scheme`; arbitrary following material still fails closed.
- Diagnostics continue to report category, location, and member path only, never the assignment key, link target, or credential value.

## GREEN evidence

The focused eight-test set passed in all required modes:

```text
$ python3 -I deploy/collector/smoke_checks_test.py <8 focused test methods>
Ran 8 tests in 3.572s
OK

$ PYTHONOPTIMIZE=1 python3 -I deploy/collector/smoke_checks_test.py <8 focused test methods>
Ran 8 tests in 3.600s
OK

$ python3 -I -O deploy/collector/smoke_checks_test.py <8 focused test methods>
Ran 8 tests in 3.607s
OK
```

Fresh full scanner verification passed in all required modes, including all prior NUL assignment, private/age, exact libgnutls collision, regular-file allowlist, link allowlist isolation, prohibited-path, and benign-link regressions:

```text
$ python3 -I deploy/collector/smoke_checks_test.py
Ran 53 tests in 27.753s
OK

$ PYTHONOPTIMIZE=1 python3 -I deploy/collector/smoke_checks_test.py
Ran 53 tests in 27.620s
OK

$ python3 -I -O deploy/collector/smoke_checks_test.py
Ran 53 tests in 27.670s
OK
```

Static verification:

```text
$ python3 -m py_compile deploy/collector/smoke_checks.py deploy/collector/smoke_checks_test.py
(exit 0; no output)

$ git diff --check
(exit 0; no output)
```

No Docker, Go suite, race suite, VPS, Railway, Traefik, Hetzner, Storage Box, or other external infrastructure was accessed. Another independent re-review is required before Task 10 can be considered complete.


# Task 10 Fourth Independent-Review Standalone Bearer Context Fix

## Scope

This focused follow-up closes the remaining standalone Bearer bypasses without weakening the prior scanner controls:

1. Quoted standalone credentials are rejected, including `Bearer "safe-review-value"` and `Bearer 'safe-review-value'`.
2. Credentials equal to the former benign follower vocabulary are rejected, including `Bearer token` and case/punctuation variants such as `Bearer TOKEN,`.
3. Benign prose is accepted only through exact reviewed whole-record context, including `The bearer of this certificate may present it.`

The existing assignment parsing without a key-length ceiling, bounded regular-file/link/total scans, NUL-delimited scanning, private-header and age detection, exact runtime/source allowlists, link-metadata isolation, merged-filesystem coverage, every-saved-layer coverage, and non-disclosing diagnostics remain active.

## Strict RED evidence

The regressions were added before any production edit. They exercised direct text and regular-file helpers, metadata history, direct symlink/hardlink targets, exported regular files/symlinks/hardlinks, and malicious content placed independently in every saved layer. Every rejection assertion checked that credential values or complete link targets were absent from diagnostics. The benign contextual prose regression also failed under the old first-follower heuristic.

Exact RED command:

```text
$ python3 -I deploy/collector/smoke_checks_test.py LeakageGateTests.test_quoted_and_vocabulary_bearer_credentials_are_rejected_directly LeakageGateTests.test_quoted_and_vocabulary_bearer_credentials_are_rejected_from_metadata LeakageGateTests.test_quoted_and_vocabulary_bearer_credentials_are_rejected_from_exported_filesystem LeakageGateTests.test_quoted_and_vocabulary_bearer_credentials_are_rejected_from_every_saved_layer LeakageGateTests.test_benign_bearer_prose_and_code_remain_accepted
```

Exact RED result summary from the command output:

```text
Ran 5 tests in 1.402s

FAILED (failures=23)
(exit 1)
```

The 23 failures were the intended missing behavior: quoted and vocabulary-equal Bearer material passed direct, metadata, link, exported-filesystem, and saved-layer scans, while the contextual certificate prose was rejected. There were no test errors in the recorded RED run.

## Implementation rationale

- Standalone Bearer patterns now treat any non-whitespace, non-NUL token after case-insensitive `Bearer` as credential material; quote characters are no longer excluded.
- The unsafe normalized first-follower vocabulary exemption was removed completely.
- Benign collisions are permitted only when the complete newline/NUL-delimited record, after outer whitespace trimming, exactly equals one of four reviewed public prose/code lines already represented by positive regressions.
- Exact whole-record matching means `Bearer token`, `Bearer TOKEN,`, quoted values, prefixed commands/history, appended material, and arbitrary prose do not inherit an exemption from their first word.
- No credential/key-length threshold was added. The detector continues to operate within the existing bounded file, link-target, and total scan limits.
- Failure messages remain category/location/path-only and never include the matched credential or link target.

## GREEN and final verification

Focused GREEN:

```text
$ python3 -I deploy/collector/smoke_checks_test.py LeakageGateTests.test_quoted_and_vocabulary_bearer_credentials_are_rejected_directly LeakageGateTests.test_quoted_and_vocabulary_bearer_credentials_are_rejected_from_metadata LeakageGateTests.test_quoted_and_vocabulary_bearer_credentials_are_rejected_from_exported_filesystem LeakageGateTests.test_quoted_and_vocabulary_bearer_credentials_are_rejected_from_every_saved_layer LeakageGateTests.test_benign_bearer_prose_and_code_remain_accepted
Ran 5 tests in 1.682s

OK
```

Complete scanner suite in all required modes:

```text
$ python3 -I deploy/collector/smoke_checks_test.py
Ran 57 tests in 29.657s

OK

$ PYTHONOPTIMIZE=1 python3 -I deploy/collector/smoke_checks_test.py
Ran 57 tests in 29.797s

OK

$ python3 -I -O deploy/collector/smoke_checks_test.py
Ran 57 tests in 29.453s

OK
```

Static verification:

```text
$ python3 -m py_compile deploy/collector/smoke_checks.py deploy/collector/smoke_checks_test.py
(exit 0; no output)

$ git diff --check
(exit 0; no output)
```

## Changed files and self-review

Changed files:

- `deploy/collector/smoke_checks.py`
- `deploy/collector/smoke_checks_test.py`
- `.superpowers/sdd/task-10-report.md`

The exact diff was reviewed for quoted/unmatched-token bypasses, vocabulary collisions, overbroad prose exemptions, data disclosure, unbounded behavior, and archive-path realism. The exception is deliberately exact and record-scoped rather than a value-word allowlist. All previous scanner regressions remain GREEN.

No Docker, Go suite, race suite, VPS, Railway, Traefik, Hetzner, Storage Box, or other external infrastructure was accessed. No concern or requirement deviation remains; another independent re-review is still required before Task 10 is considered complete.

# Task 10 Fifth Independent-Review Linear Bearer Record Scan Fix

## Scope and strict RED

This focused correction removes the quadratic prefix rescans from standalone Bearer benign-record classification while preserving the exact four-record allowlist and all prior scanner protections. Starting HEAD was `585b4ebda5f47f113dc1e5587a30659b86cff8d9`.

A deterministic regression was added before production changes. It uses `str` and `bytes` subclasses that count the prefix range passed to `rfind`, repeats exact reviewed newline/NUL-delimited benign records, and retains functional checks for outer whitespace trimming, newline/NUL boundaries, exact benign acceptance, and appended-material rejection.

Exact RED command and result:

```text
$ python3 -I deploy/collector/smoke_checks_test.py LeakageGateTests.test_repeated_benign_bearer_records_do_not_rescan_prefixes
test_repeated_benign_bearer_records_do_not_rescan_prefixes (__main__.LeakageGateTests) ...
======================================================================
FAIL: test_repeated_benign_bearer_records_do_not_rescan_prefixes (__main__.LeakageGateTests) (text_type='PrefixCountingStr')
AssertionError: 5069376 not less than or equal to 79476 : standalone Bearer scan repeatedly rescanned already-checked prefixes

======================================================================
FAIL: test_repeated_benign_bearer_records_do_not_rescan_prefixes (__main__.LeakageGateTests) (text_type='PrefixCountingBytes')
AssertionError: 5069376 not less than or equal to 79476 : standalone Bearer scan repeatedly rescanned already-checked prefixes

Ran 1 test in 0.004s
FAILED (failures=2)
(exit 1)
```

The failure was the intended O(n²) work-count defect in both supported input types, not a timing threshold or functional-classification failure.

## Implementation rationale

- Replaced per-match backward/forward whole-input searches with a lazy newline/NUL/CR record-bound iterator.
- The separator iterator and standalone Bearer matcher each advance only forward through the bounded input.
- Record content is sliced only for the record containing a match; no boundary list or second full-file representation is allocated.
- The record containing the start of a cross-boundary match is still used, preserving the previous fail-closed behavior.
- `str` and `bytes` use separate compiled separator patterns and retain the same exact four benign records after outer whitespace trimming.
- Quoted credentials, vocabulary-equal credentials, punctuation/case variants, prefixes, suffixes, metadata/history, links, merged filesystems, and every saved layer retain the prior rejection behavior and value-free diagnostics.

## GREEN evidence

Focused GREEN:

```text
$ python3 -I deploy/collector/smoke_checks_test.py LeakageGateTests.test_repeated_benign_bearer_records_do_not_rescan_prefixes
test_repeated_benign_bearer_records_do_not_rescan_prefixes (__main__.LeakageGateTests) ... ok

Ran 1 test in 0.001s
OK
```

Complete 58-test scanner suite in all required modes:

```text
$ python3 -I deploy/collector/smoke_checks_test.py
Ran 58 tests in 29.645s
OK

$ PYTHONOPTIMIZE=1 python3 -I deploy/collector/smoke_checks_test.py
Ran 58 tests in 29.276s
OK

$ python3 -I -O deploy/collector/smoke_checks_test.py
Ran 58 tests in 29.138s
OK
```

Static verification:

```text
$ python3 -m py_compile deploy/collector/smoke_checks.py deploy/collector/smoke_checks_test.py
(exit 0; no output)

$ git diff --check
(exit 0; no output)
```

Bounded local scaling evidence used 12 scans per sample and the median of seven samples; it is directional evidence rather than a precise benchmark:

```text
small_bytes=376000 small_seconds=0.128568
large_bytes=752000 large_seconds=0.258415
large_to_small_ratio=2.010
```

The 2x input completed in approximately 2.01x the time rather than the prior approximately 4x growth.

## Changed files and self-review

Changed files:

- `deploy/collector/smoke_checks.py`
- `deploy/collector/smoke_checks_test.py`
- `.superpowers/sdd/task-10-report.md`

The exact diff was reviewed for hidden prefix/suffix rescans, regex restart behavior, extra whole-file allocations, `str`/`bytes` divergence, CR/LF/NUL boundary handling, cross-record matches, exact-allowlist bypasses, and diagnostic disclosure. No finite token or assignment-key threshold was introduced, and all prior bounds and fail-closed controls remain active.

No Docker, Go suite, race suite, VPS, Railway, Traefik, Hetzner, Storage Box, or other external infrastructure was accessed. Task 10 is not marked complete; independent re-review remains required.

# Task 10 Reviewed msgpack Binary-Record Collision Fix

## Scope and root cause

This focused correction started from exact HEAD `c1e0080b114e8fd852aeb97a3a00ddff8a79ef48`. The exact-HEAD Docker smoke had already built local image `sha256:81aa978d8fdab9a1ece1f0374188c073f6cc336826089a8c33a53934c2a1bd05`, verified OCI revision `c1e0080b114e8fd852aeb97a3a00ddff8a79ef48`, runtime user `10001:10001`, and both pinned Borg integration tests, then rejected the compiled msgpack 1.2.1 extension as a secret-like assignment.

Independent artifact review established that the four `strict_map_key` detector matches are stable non-secret compiled-extension records shared by the official CPython 3.13 manylinux2014 x86_64 and aarch64 msgpack 1.2.1 wheels. Three complete terminating-NUL record digests were reviewed:

```text
d63020dcc481de704039ce44faf1ca726f0d3e70765b2900c0886eb5d6f21525
0cf91e3e6f10e36b6bb7698c7a1561a80ace0d9a28b672d3e8560d79487d5b03
c4ac930f2245301678e8afab2a124d54ecdc1c4ee7393ca3cd71a677781ca24b
```

The Dockerfile and plan-mandated local-artifact Borg 1.4.5 pip installation were not changed.

## Strict RED evidence

Four focused regressions were added before production changes. They cover exact aarch64/x86_64 acceptance, moved and unknown architecture paths, modified records, changed keys, missing NUL termination, one hash for two matches in one record, merged export and every saved layer, non-disclosing diagnostics, and the exact production digest set.

```text
$ python3 -I deploy/collector/smoke_checks_test.py <4 focused msgpack tests>
Ran 4 tests in 0.007s
FAILED (failures=1, errors=4)
(exit 1)
```

The errors were the intended current-code `CheckFailure` rejections of exact reviewed records for both deployment paths, direct multi-match scanning, and exported-filesystem scanning. The failure showed that the production allowlist was absent. No diagnostic disclosed a test value or complete record.

## Implementation

- Added a distinct binary-assignment exception; the broad regular `runtime_assignment_allowed` source allowlist remains unchanged.
- Normalized only the exact CPython 3.13 aarch64 and x86_64 compiled-msgpack paths to one reviewed key. Moved paths and all other architecture suffixes fail closed.
- Required the exact key `strict_map_key` plus one of the three reviewed SHA-256 digests of the complete record including its terminating NUL.
- Unterminated records, byte changes, prefixes or suffixes that change the complete record, changed keys, and unknown digests remain rejected.
- Added a monotonically advancing NUL-record iterator alongside the already ordered assignment matches. Each candidate complete record is sliced and hashed at most once, and the digest is reused for additional matches in that record. No backward scan, per-match prefix scan, record-bound list, or second whole-file representation was added.
- Preserved all previous private-header, age, Bearer, link, bounds, merged-filesystem, saved-layer, exact-source allowlist, and non-disclosure checks.

## GREEN evidence

Focused GREEN:

```text
$ python3 -I deploy/collector/smoke_checks_test.py <4 focused msgpack tests>
Ran 4 tests in 0.014s
OK
```

Complete scanner suite in all required modes:

```text
$ python3 -I deploy/collector/smoke_checks_test.py
Ran 62 tests in 29.802s
OK

$ PYTHONOPTIMIZE=1 python3 -I deploy/collector/smoke_checks_test.py
Ran 62 tests in 30.228s
OK

$ python3 -I -O deploy/collector/smoke_checks_test.py
Ran 62 tests in 30.012s
OK
```

Static verification:

```text
$ python3 -m py_compile deploy/collector/smoke_checks.py deploy/collector/smoke_checks_test.py
(exit 0; no output)

$ git diff --check
(exit 0; no output)
```

## Existing-image focused validation

The corrected working-tree scanner was exercised read-only against the already-built exact-HEAD local image, without rebuilding it. The compiled extension was exported from a temporary `--rm` container and scanned at its exact normalized path:

```text
id=sha256:81aa978d8fdab9a1ece1f0374188c073f6cc336826089a8c33a53934c2a1bd05 revision=c1e0080b114e8fd852aeb97a3a00ddff8a79ef48 user=10001:10001
reviewed msgpack scan passed: path=opt/borg/lib/python3.13/site-packages/msgpack/_cmsgpack.cpython-313-aarch64-linux-gnu.so bytes=1472904
```

This is focused validation of the prior image with the corrected working-tree scanner, not a final exact-commit Docker smoke. The final Docker build/smoke must be rerun only after this correction is committed and independently reviewed.

## Changed files and self-review

Changed files:

- `deploy/collector/smoke_checks.py`
- `deploy/collector/smoke_checks_test.py`
- `.superpowers/sdd/task-10-report.md`

The exact diff was reviewed for broad source exemptions, path or architecture overmatching, missing NUL termination, digest/key ambiguity, repeated hashing for multiple matches, hidden quadratic work, diagnostic disclosure, and regression of prior scanners. No concern or requirement deviation remains. Task 10 is not marked complete; the final exact-commit Docker smoke remains pending.

# Task 10 Binary Assignment Record Gate Isolation Fix

## Scope and root cause

This focused correction started from exact HEAD `bf490e98b43b906fce3139ed96cf9dc47f539f1d`. The reviewed binary-assignment gate classified broad source allowlist entries before locating the assignment's containing NUL record. Consequently, a complete terminating-NUL `strict_map_key=...` record at either already reviewed source path bypassed the exact compiled-path, key, and digest gate:

- `opt/borg/lib/python3.13/site-packages/msgpack/fallback.py`
- `opt/borg/lib/python3.13/site-packages/borg/helpers/msgpack.py`

The bypass affected direct scanning, the merged exported filesystem, and every recoverable saved image layer.

## Strict RED evidence

Four focused regressions were added before the production edit. They cover both reviewed source paths, a test-patched exact approved record digest, a modified unknown digest, direct scanning, merged export, each of three saved layers, non-disclosing diagnostics, exact compiled-extension acceptance, and preservation of ordinary unterminated textual source assignments.

```text
$ python3 -I deploy/collector/smoke_checks_test.py LeakageGateTests.test_reviewed_source_paths_cannot_allow_complete_nul_assignment_records_directly LeakageGateTests.test_reviewed_source_paths_cannot_allow_complete_nul_assignment_records_in_export LeakageGateTests.test_reviewed_source_paths_cannot_allow_complete_nul_assignment_records_in_any_saved_layer LeakageGateTests.test_reviewed_source_paths_still_allow_ordinary_unterminated_text_assignments
Ran 4 tests in 0.039s
FAILED (failures=20)
(exit 1)
```

The 20 failures were the intended existing bypass: moved exact-digest and unknown-digest complete NUL records were accepted through the broad reviewed-source allowlist in direct, exported-filesystem, and every-saved-layer scans. The ordinary non-NUL textual assignment positive case passed during RED. No diagnostic disclosed the assigned value or complete record.

## Implementation

- Sensitive assignments are now advanced to and classified against their containing complete terminating-NUL record before any broad textual source allowlist lookup.
- Assignments inside complete NUL records can be accepted only by the existing exact normalized compiled-msgpack path, exact `strict_map_key`, and exact complete-record digest gate.
- `runtime_assignment_allowed` is consulted only when no terminating NUL contains the assignment, preserving ordinary textual records and the unterminated tail.
- The assignment and NUL-record iterators remain monotonically forward-only. Record digests remain cached once per record and reused for multiple matches.
- No backward search, path wildcard, broad binary exemption, record-bound list, second whole-file representation, or new key/value threshold was added.
- Existing 64 MiB per-file and 1 GiB total bounds, Bearer/private-header/age/link protections, merged export, saved-layer coverage, exact source allowlists, and non-disclosing diagnostics remain unchanged.

## GREEN evidence

Focused GREEN:

```text
$ python3 -I deploy/collector/smoke_checks_test.py <4 focused binary-isolation tests>
Ran 4 tests in 0.040s
OK
```

Complete scanner suite in all required modes:

```text
$ python3 -I deploy/collector/smoke_checks_test.py
Ran 66 tests in 29.915s
OK

$ PYTHONOPTIMIZE=1 python3 -I deploy/collector/smoke_checks_test.py
Ran 66 tests in 30.500s
OK

$ python3 -I -O deploy/collector/smoke_checks_test.py
Ran 66 tests in 29.899s
OK
```

Static verification:

```text
$ python3 -m py_compile deploy/collector/smoke_checks.py deploy/collector/smoke_checks_test.py
(exit 0; no output)

$ git diff --check
(exit 0; no output)
```

## Existing-image focused validation

The corrected working-tree scanner was exercised read-only against the existing local `sherpa-collector:test` image without rebuilding it:

```text
image=sha256:81aa978d8fdab9a1ece1f0374188c073f6cc336826089a8c33a53934c2a1bd05 revision=c1e0080b114e8fd852aeb97a3a00ddff8a79ef48 user=10001:10001
reviewed compiled msgpack passed: bytes=1472904
reviewed source-path NUL probes rejected without disclosure: paths=2
```

The real aarch64 compiled extension still passes at its exact normalized path. The reproduced NUL record now rejects at both broadly reviewed `.py` paths without printing its value or complete record.

## Self-review and pending gate

Focused boundary probes passed for an exact reviewed assignment in the first, middle, and final complete NUL records and for an ordinary reviewed-source assignment in the unterminated tail:

```text
self-review probes passed: first/middle/final complete records=3 unterminated_tail=1
```

The exact diff was reviewed for complete-record classification, broad textual allowlist preservation, exact binary-gate isolation, multiple matches per record, one-hash caching, monotonic iteration, bounded memory/time behavior, and diagnostic disclosure. No concern or requirement deviation remains.

No image rebuild, final Docker smoke, Go suite, race suite, VPS, Railway, Traefik, Hetzner, Storage Box, or external infrastructure operation was performed. Task 10 is not marked complete; the final exact-commit Docker build/smoke remains pending after commit and independent review.
