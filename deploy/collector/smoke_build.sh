#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
deploy_dir="$root_dir/deploy/collector"
checks_script="$deploy_dir/smoke_checks.py"
source_url="https://github.com/TimoKruth/SherpA"
image="${SHERPA_COLLECTOR_SMOKE_IMAGE:-sherpa-collector:test}"
build_only=false
if [[ "${1:-}" == "--build-only" ]]; then
  [[ $# -eq 2 ]] || { printf '%s\n' 'usage: smoke_build.sh --build-only IMAGE' >&2; exit 2; }
  build_only=true
  image="$2"
elif [[ $# -ne 0 ]]; then
  printf '%s\n' 'usage: smoke_build.sh [--build-only IMAGE]' >&2
  exit 2
fi

suffix="$$-${RANDOM}"
project="sherpa-collector-smoke-${suffix}"
image_alias="${project}-collector"
inspect_container="sherpa-collector-inspect-${suffix}"
work_dir="$(mktemp -d "${TMPDIR:-/tmp}/sherpa-collector-smoke.XXXXXX")"
source_dir="$work_dir/source"
compose_started=false

cleanup() {
  if [[ "$compose_started" == true ]]; then
    SOURCE_REVISION="${source_revision:-}" docker compose --project-directory "$work_dir" -f "$work_dir/compose.yaml" -p "$project" down --volumes --remove-orphans >/dev/null 2>&1 || true
  fi
  docker rm -f "$inspect_container" >/dev/null 2>&1 || true
  docker image rm --force "$image_alias" >/dev/null 2>&1 || true
  # The fixture check deliberately changes bind-mounted files to the runtime
  # UID. Linux hosts enforce that ownership during cleanup, unlike Docker
  # Desktop's translated mounts, so restore the invoking user's ownership
  # through the already-reviewed image before removing the temporary tree.
  if docker image inspect "$image" >/dev/null 2>&1; then
    docker run --rm --user 0:0 --entrypoint /bin/chown \
      --mount "type=bind,src=$work_dir,dst=/cleanup" \
      "$image" -R "$(id -u):$(id -g)" /cleanup >/dev/null 2>&1 || true
  fi
  rm -rf "$work_dir"
}
trap cleanup EXIT

fail() {
  printf '%s\n' "$1" >&2
  exit 1
}

progress() {
  printf '\n==> %s\n' "$1"
}

for required in Dockerfile compose.yaml runtime.env.example smoke_checks.py; do
  [[ -f "$deploy_dir/$required" ]] || fail "missing collector deployment file: deploy/collector/$required"
done
for tool in docker git go python3 tar; do
  command -v "$tool" >/dev/null 2>&1 || fail "required smoke tool is unavailable: $tool"
done
docker compose version >/dev/null

progress "Construct exact committed build context"
source_revision="$(python3 -I "$checks_script" archive "$root_dir" "$source_dir")"
[[ "$source_revision" =~ ^[0-9a-f]{40}$ ]] || fail "collector source revision is not a full commit"
[[ -f "$source_dir/deploy/collector/Dockerfile" ]] || fail "committed source archive lacks collector Dockerfile"

build_log="$work_dir/build.log"
progress "Build pinned collector image from committed archive"
docker build --progress=plain \
  --file "$source_dir/deploy/collector/Dockerfile" \
  --build-arg "SOURCE_REVISION=$source_revision" \
  --build-arg "INTEGRATION_NONCE=$suffix" \
  --tag "$image" \
  "$source_dir" 2>&1 | tee "$build_log"
for integration_test in TestBorgLocalRepositoryCreateAndExactPresence TestCollectorEndToEndWithLocalBorgRepository; do
  grep -F -- "--- PASS: $integration_test" "$build_log" >/dev/null || fail "Borg integration did not pass in the pinned image build: $integration_test"
  if grep -F -- "--- SKIP: $integration_test" "$build_log" >/dev/null; then
    fail "Borg integration skipped in the pinned image build: $integration_test"
  fi
done
if [[ "$build_only" == true ]]; then
  image_id="$(docker image inspect --format '{{.Id}}' "$image")"
  printf 'Collector exact build passed: image=%s revision=%s\n' "$image_id" "$source_revision"
  exit 0
fi

progress "Create mirrored deployment fixtures"
cp "$source_dir/deploy/collector/compose.yaml" "$work_dir/compose.yaml"
cp "$source_dir/deploy/collector/runtime.env.example" "$work_dir/runtime.env"
mkdir -p "$work_dir/config" "$work_dir/secrets" "$work_dir/data"
chmod 0700 "$work_dir/secrets" "$work_dir/data"
chmod 0600 "$work_dir/runtime.env"

cat >"$work_dir/generate_recipient.go" <<'GO'
package main

import (
	"fmt"

	"filippo.io/age"
)

func main() {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		panic("generate smoke recipient")
	}
	fmt.Println(identity.Recipient())
}
GO
recipient="$(cd "$source_dir" && go run "$work_dir/generate_recipient.go")"
[[ "$recipient" == age1* ]] || fail "failed to generate safe public age recipient"
printf '%s\n' "$recipient" >"$work_dir/config/age-recipient"
printf '%s\n' 'example.invalid ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAISafeCollectorSmokeFixture' >"$work_dir/config/known_hosts"
printf '%s\n' 'safe-smoke-upload-token' >"$work_dir/secrets/upload-token"
printf '%s\n' 'safe-smoke-storage-key' >"$work_dir/secrets/storage-ssh-key"
printf '%s\n' '/data/repository' >"$work_dir/secrets/borg-repository"
chmod 0755 "$work_dir/config"
chmod 0644 "$work_dir/config/age-recipient" "$work_dir/config/known_hosts"
chmod 0600 "$work_dir/secrets/upload-token" "$work_dir/secrets/storage-ssh-key" "$work_dir/secrets/borg-repository"
docker run --rm --user 0:0 --entrypoint /bin/sh \
  --mount "type=bind,src=$work_dir/config,dst=/fixture/config" \
  --mount "type=bind,src=$work_dir/secrets,dst=/fixture/secrets" \
  "$image" -ceu '
    chown -R 10001:10001 /fixture/config /fixture/secrets
    chmod 0755 /fixture/config
    chmod 0700 /fixture/secrets
    chmod 0644 /fixture/config/age-recipient /fixture/config/known_hosts
    chmod 0600 /fixture/secrets/upload-token /fixture/secrets/storage-ssh-key /fixture/secrets/borg-repository
  '
docker run --rm --user 0:0 --entrypoint python3 \
  --mount "type=bind,src=$work_dir/config,dst=/fixture/config" \
  --mount "type=bind,src=$work_dir/secrets,dst=/fixture/secrets" \
  --mount "type=bind,src=$source_dir/deploy/collector/smoke_checks.py,dst=/tmp/smoke_checks.py,readonly" \
  "$image" -I /tmp/smoke_checks.py runtime-files /fixture 10001 10001

progress "Validate rendered Compose contract"
SOURCE_REVISION="$source_revision" docker compose --project-directory "$work_dir" -f "$work_dir/compose.yaml" config >"$work_dir/compose.rendered.yaml"
if grep -Eq '^[[:space:]]+ports:' "$work_dir/compose.rendered.yaml"; then
  fail "collector Compose service publishes a host port"
fi
SOURCE_REVISION="$source_revision" docker compose --project-directory "$work_dir" -f "$work_dir/compose.yaml" config --format json >"$work_dir/compose.rendered.json"
python3 -I "$source_dir/deploy/collector/smoke_checks.py" compose "$work_dir/compose.rendered.json" "$work_dir" "$source_revision"

progress "Inspect image identity and metadata"
configured_user="$(docker image inspect --format '{{.Config.User}}' "$image")"
[[ "$configured_user" == "10001:10001" ]] || fail "collector image user is not 10001:10001"
[[ "$(docker image inspect --format '{{json .Config.Entrypoint}}' "$image")" == '["/usr/local/bin/collector"]' ]] || fail "collector image entrypoint is not exact"
[[ "$(docker image inspect --format '{{json .Config.Cmd}}' "$image")" == '["serve"]' ]] || fail "collector image default command is not serve"
[[ "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.source"}}' "$image")" == "$source_url" ]] || fail "collector image source label is not exact"
[[ "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$image")" == "$source_revision" ]] || fail "collector image revision label is not exact"
[[ "$(docker run --rm --entrypoint /opt/borg/bin/borg "$image" --version)" == 'borg 1.4.5' ]] || fail "collector image Borg version is not exactly 1.4.5"
docker image inspect "$image" >"$work_dir/image.inspect.json"
docker history --no-trunc --format '{{.CreatedBy}}' "$image" >"$work_dir/image.history"
python3 -I "$source_dir/deploy/collector/smoke_checks.py" metadata "$work_dir/image.inspect.json" "$work_dir/image.history" "$source_url" "$source_revision"
image_platform="$(python3 -I "$source_dir/deploy/collector/smoke_checks.py" platform "$work_dir/image.inspect.json")"

progress "Export and scan complete image filesystem"
docker create --name "$inspect_container" "$image" >/dev/null
docker export "$inspect_container" >"$work_dir/image.tar"
python3 -I "$source_dir/deploy/collector/smoke_checks.py" filesystem "$work_dir/image.tar" "$image_platform"
docker image save "$image" >"$work_dir/image.save.tar"
python3 -I "$source_dir/deploy/collector/smoke_checks.py" layers "$work_dir/image.save.tar" "$image_platform"
docker run --rm --entrypoint /bin/sh "$image" -ceu '
  for command in go gcc cc clang make cmake pkg-config curl git fusermount fusermount3; do
    ! command -v "$command" >/dev/null 2>&1
  done
  test ! -e /src
  test ! -e /usr/local/go
  test ! -e /dev/fuse
'

progress "Verify collector archive command"
cat >"$work_dir/generate_archive.go" <<'GO'
package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"time"
)

type artifact struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type manifest struct {
	Artifacts []artifact `json:"artifacts"`
}

func main() {
	contents := []byte("synthetic postgres dump")
	digest := sha256.Sum256(contents)
	encoded, err := json.MarshalIndent(manifest{Artifacts: []artifact{{
		Path: "postgres.dump", SHA256: hex.EncodeToString(digest[:]), Size: int64(len(contents)),
	}}}, "", "  ")
	if err != nil {
		panic("encode smoke manifest")
	}
	encoded = append(encoded, '\n')
	file, err := os.Create(os.Args[1])
	if err != nil {
		panic("create smoke archive")
	}
	compressed := gzip.NewWriter(file)
	archive := tar.NewWriter(compressed)
	for _, member := range []struct {
		name string
		body []byte
	}{{"postgres.dump", contents}, {"manifest.json", encoded}} {
		header := &tar.Header{Name: member.name, Mode: 0o600, Size: int64(len(member.body)), Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0)}
		if archive.WriteHeader(header) != nil {
			panic("write smoke header")
		}
		if _, err := archive.Write(member.body); err != nil {
			panic("write smoke member")
		}
	}
	if archive.Close() != nil || compressed.Close() != nil || file.Close() != nil {
		panic("close smoke archive")
	}
}
GO
(cd "$source_dir" && go run "$work_dir/generate_archive.go" "$work_dir/recovery.tar.gz")
verify_output="$(docker run --rm --read-only --user 10001:10001 \
  --mount "type=bind,src=$work_dir/recovery.tar.gz,dst=/run/recovery.tar.gz,readonly" \
  "$image" verify /run/recovery.tar.gz)"
[[ "$verify_output" == $'valid\nartifacts=1\nverified_bytes=23\nmanifest_final=true' ]] || fail "collector verify did not accept the valid smoke archive"

progress "Initialize and verify exact bind-mounted data layout"
docker run --rm --user 0:0 --entrypoint /bin/sh \
  --mount "type=bind,src=$work_dir/data,dst=/data" \
  "$image" -ceu '
    mkdir -p /data/spool /data/state /data/borg/cache /data/borg/config /data/borg/security
    chown -R 10001:10001 /data
    chmod 0700 /data /data/spool /data/state /data/borg /data/borg/cache /data/borg/config /data/borg/security
  '
docker run --rm --user 0:0 --entrypoint python3 \
  --mount "type=bind,src=$work_dir/data,dst=/data" \
  --mount "type=bind,src=$source_dir/deploy/collector/smoke_checks.py,dst=/tmp/smoke_checks.py,readonly" \
  "$image" -I /tmp/smoke_checks.py layout /data 10001 10001

progress "Run Borg create and list against exact bind mount"
docker run --rm --user 10001:10001 --entrypoint /bin/sh \
  --mount "type=bind,src=$work_dir/data,dst=/data" \
  --env BORG_UNKNOWN_UNENCRYPTED_REPO_ACCESS_IS_OK=yes \
  "$image" -ceu '
    borg init --encryption=none /data/repository
    mkdir /data/smoke-input
    printf "%s\n" "safe Borg smoke object" > /data/smoke-input/object
    borg create /data/repository::sherpa-smoke /data/smoke-input
    borg list --json /data/repository > /data/borg-list.json
    python -I -c '\''import json; data=json.load(open("/data/borg-list.json", encoding="utf-8")); names=[item["name"] for item in data["archives"]]; raise SystemExit(0 if names == ["sherpa-smoke"] else 1)'\''
    rm -rf /data/smoke-input /data/borg-list.json
  '

progress "Start real Compose service with exact bind mount"
docker tag "$image" "$image_alias"
compose_started=true
SOURCE_REVISION="$source_revision" docker compose --project-directory "$work_dir" -f "$work_dir/compose.yaml" -p "$project" up --detach --no-build collector
container="$(SOURCE_REVISION="$source_revision" docker compose --project-directory "$work_dir" -f "$work_dir/compose.yaml" -p "$project" ps -q collector)"
[[ -n "$container" ]] || fail "collector Compose container was not created"

for _ in $(seq 1 60); do
  if docker exec "$container" /usr/local/bin/collector healthcheck http://127.0.0.1:8080/healthz >"$work_dir/healthcheck.out" 2>/dev/null; then
    break
  fi
  if [[ "$(docker inspect --format '{{.State.Running}}' "$container")" != "true" ]]; then
    docker logs "$container" >&2
    fail "collector serve process exited before becoming healthy"
  fi
  sleep 1
done
[[ -f "$work_dir/healthcheck.out" ]] || fail "collector exact local healthcheck did not run"
[[ "$(<"$work_dir/healthcheck.out")" == "healthy" ]] || fail "collector exact local healthcheck did not pass"

pid_one_status="$(docker exec "$container" /bin/sh -c "awk '/^(Uid|Gid|CapInh|CapPrm|CapEff|CapBnd|NoNewPrivs):/{print}' /proc/1/status")"
for expected in \
  $'Uid:\t10001\t10001\t10001\t10001' \
  $'Gid:\t10001\t10001\t10001\t10001' \
  $'CapInh:\t0000000000000000' \
  $'CapPrm:\t0000000000000000' \
  $'CapEff:\t0000000000000000' \
  $'CapBnd:\t0000000000000000' \
  $'NoNewPrivs:\t1'; do
  grep -Fx "$expected" <<<"$pid_one_status" >/dev/null || fail "collector PID 1 security status is not exact"
done
if docker exec "$container" /bin/sh -c ': > /rootfs-write-probe' >/dev/null 2>&1; then
  fail "collector root filesystem is writable"
fi
docker exec "$container" /bin/sh -ceu ': > /data/.write-probe; rm /data/.write-probe'
for directory in /data/spool /data/state /data/borg /data/borg/cache /data/borg/config /data/borg/security; do
  [[ "$(docker exec "$container" stat -c '%a:%u:%g' "$directory")" == "700:10001:10001" ]] || fail "collector data directory is not private and runtime-owned: $directory"
done
docker inspect "$container" >"$work_dir/runtime.inspect.json"
python3 -I "$source_dir/deploy/collector/smoke_checks.py" bind "$work_dir/runtime.inspect.json" "$work_dir/data"
[[ "$(docker inspect --format '{{json .HostConfig.CapDrop}}' "$container")" == '["ALL"]' ]] || fail "collector runtime does not drop all capabilities"
[[ "$(docker inspect --format '{{json .HostConfig.SecurityOpt}}' "$container")" == '["no-new-privileges:true"]' ]] || fail "collector runtime does not enable no-new-privileges"
[[ "$(docker inspect --format '{{json .HostConfig.PortBindings}}' "$container")" == '{}' ]] || fail "collector runtime publishes a host port"
[[ "$(docker port "$container")" == "" ]] || fail "collector runtime has a published port"

docker run --rm --user 0:0 --entrypoint python3 \
  --mount "type=bind,src=$work_dir/data,dst=/data" \
  --mount "type=bind,src=$source_dir/deploy/collector/smoke_checks.py,dst=/tmp/smoke_checks.py,readonly" \
  "$image" -I /tmp/smoke_checks.py layout /data 10001 10001

image_id="$(docker image inspect --format '{{.Id}}' "$image")"
printf 'Collector Docker smoke passed: image=%s revision=%s\n' "$image_id" "$source_revision"
