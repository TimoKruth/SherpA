#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
deploy_dir="$root_dir/deploy/collector"
source_revision="$(git -C "$root_dir" rev-parse HEAD)"
source_url="https://github.com/TimoKruth/SherpA"
image="${SHERPA_COLLECTOR_SMOKE_IMAGE:-sherpa-collector:test}"
suffix="$$-${RANDOM}"
container="sherpa-collector-smoke-${suffix}"
inspect_container="sherpa-collector-inspect-${suffix}"
data_volume="sherpa-collector-data-${suffix}"
config_volume="sherpa-collector-config-${suffix}"
work_dir="$(mktemp -d "${TMPDIR:-/tmp}/sherpa-collector-smoke.XXXXXX")"

cleanup() {
  docker rm -f "$container" "$inspect_container" >/dev/null 2>&1 || true
  docker volume rm "$data_volume" "$config_volume" >/dev/null 2>&1 || true
  rm -rf "$work_dir"
}
trap cleanup EXIT

fail() {
  printf '%s\n' "$1" >&2
  exit 1
}

for required in Dockerfile compose.yaml runtime.env.example; do
  [[ -f "$deploy_dir/$required" ]] || fail "missing collector deployment file: deploy/collector/$required"
done

for tool in docker git go python3; do
  command -v "$tool" >/dev/null 2>&1 || fail "required smoke tool is unavailable: $tool"
done
docker compose version >/dev/null
[[ "$source_revision" =~ ^[0-9a-f]{40}$ ]] || fail "collector source revision is not a full commit"

cp "$deploy_dir/compose.yaml" "$work_dir/compose.yaml"
cp "$deploy_dir/runtime.env.example" "$work_dir/runtime.env"
ln -s "$root_dir" "$work_dir/source"
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
recipient="$(cd "$root_dir" && go run "$work_dir/generate_recipient.go")"
[[ "$recipient" == age1* ]] || fail "failed to generate safe public age recipient"
printf '%s\n' "$recipient" >"$work_dir/config/age-recipient"
printf '%s\n' 'example.invalid ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAISafeCollectorSmokeFixture' >"$work_dir/config/known_hosts"
printf '%s\n' 'safe-smoke-upload-token' >"$work_dir/secrets/upload-token"
printf '%s\n' 'safe-smoke-storage-key' >"$work_dir/secrets/storage-ssh-key"
printf '%s\n' '/data/repository' >"$work_dir/secrets/borg-repository"
chmod 0644 "$work_dir/config/age-recipient" "$work_dir/config/known_hosts"
chmod 0600 "$work_dir/secrets/upload-token" "$work_dir/secrets/storage-ssh-key" "$work_dir/secrets/borg-repository"

SOURCE_REVISION="$source_revision" docker compose --project-directory "$work_dir" -f "$work_dir/compose.yaml" config >"$work_dir/compose.rendered.yaml"
if grep -Eq '^[[:space:]]+ports:' "$work_dir/compose.rendered.yaml"; then
  fail "collector Compose service publishes a host port"
fi
SOURCE_REVISION="$source_revision" docker compose --project-directory "$work_dir" -f "$work_dir/compose.yaml" config --format json >"$work_dir/compose.rendered.json"
python3 - "$work_dir/compose.rendered.json" "$work_dir" "$source_revision" <<'PY'
import json
import os
import sys

path, root, revision = sys.argv[1:]
with open(path, encoding="utf-8") as handle:
    config = json.load(handle)
service = config["services"]["collector"]
assert os.path.realpath(service["build"]["context"]) == os.path.realpath(os.path.join(root, "source"))
assert service["build"]["dockerfile"] == "deploy/collector/Dockerfile"
assert service["build"]["args"] == {"SOURCE_REVISION": revision}
assert service["restart"] == "unless-stopped"
assert service["user"] == "10001:10001"
assert service["read_only"] is True
assert service["cap_drop"] == ["ALL"]
assert service["security_opt"] == ["no-new-privileges:true"]
assert service["pids_limit"] == 128
assert float(service["cpus"]) == 1.0
assert int(service["mem_limit"]) == 1024 * 1024 * 1024
assert service["expose"] in [["8080"], ["8080/tcp"]]
assert "ports" not in service
assert "cap_add" not in service
assert "privileged" not in service
assert "devices" not in service
assert service.get("networks") in (None, {"default": None}, ["default"])
assert service["tmpfs"] == ["/tmp:rw,nosuid,nodev,noexec,size=64m"]
assert service["healthcheck"]["test"] == ["CMD", "/usr/local/bin/collector", "healthcheck", "http://127.0.0.1:8080/healthz"]
assert service["healthcheck"]["interval"] == "30s"
assert service["healthcheck"]["timeout"] == "5s"
assert service["healthcheck"]["retries"] == 3
assert service["healthcheck"]["start_period"] == "20s"
assert service["logging"] == {"driver": "json-file", "options": {"max-file": "5", "max-size": "10m"}}
labels = service["labels"]
expected_labels = {
    "traefik.enable": "true",
    "traefik.http.routers.sherpa-collector.rule": "Host(`sherpa-collector.kruth-support.de`)",
    "traefik.http.routers.sherpa-collector.entrypoints": "websecure",
    "traefik.http.routers.sherpa-collector.tls": "true",
    "traefik.http.routers.sherpa-collector.tls.certresolver": "letsencrypt",
    "traefik.http.services.sherpa-collector.loadbalancer.server.port": "8080",
}
assert labels == expected_labels
mounts = {(os.path.realpath(item["source"]), item["target"], item.get("read_only", False)) for item in service["volumes"]}
expected_mounts = {
    (os.path.realpath(os.path.join(root, "data")), "/data", False),
    (os.path.realpath(os.path.join(root, "config/age-recipient")), "/run/config/age-recipient", True),
    (os.path.realpath(os.path.join(root, "config/known_hosts")), "/run/config/known_hosts", True),
    (os.path.realpath(os.path.join(root, "secrets/upload-token")), "/run/secrets/upload-token", True),
    (os.path.realpath(os.path.join(root, "secrets/storage-ssh-key")), "/run/secrets/storage-ssh-key", True),
    (os.path.realpath(os.path.join(root, "secrets/borg-repository")), "/run/secrets/borg-repository", True),
}
assert mounts == expected_mounts
PY

build_log="$work_dir/build.log"
docker build --progress=plain \
  --file "$deploy_dir/Dockerfile" \
  --build-arg "SOURCE_REVISION=$source_revision" \
  --build-arg "INTEGRATION_NONCE=$suffix" \
  --tag "$image" \
  "$root_dir" 2>&1 | tee "$build_log"
for integration_test in TestBorgLocalRepositoryCreateAndExactPresence TestCollectorEndToEndWithLocalBorgRepository; do
  grep -F -- "--- PASS: $integration_test" "$build_log" >/dev/null || fail "Borg integration did not pass in the pinned image build: $integration_test"
  if grep -F -- "--- SKIP: $integration_test" "$build_log" >/dev/null; then
    fail "Borg integration skipped in the pinned image build: $integration_test"
  fi
done

configured_user="$(docker image inspect --format '{{.Config.User}}' "$image")"
[[ "$configured_user" == "10001:10001" ]] || fail "collector image user is not 10001:10001"
[[ "$(docker image inspect --format '{{json .Config.Entrypoint}}' "$image")" == '["/usr/local/bin/collector"]' ]] || fail "collector image entrypoint is not exact"
[[ "$(docker image inspect --format '{{json .Config.Cmd}}' "$image")" == '["serve"]' ]] || fail "collector image default command is not serve"
[[ "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.source"}}' "$image")" == "$source_url" ]] || fail "collector image source label is not exact"
[[ "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$image")" == "$source_revision" ]] || fail "collector image revision label is not exact"
[[ "$(docker run --rm --entrypoint /opt/borg/bin/borg "$image" --version)" == 'borg 1.4.5' ]] || fail "collector image Borg version is not exactly 1.4.5"

docker image inspect "$image" >"$work_dir/image.inspect.json"
docker history --no-trunc --format '{{.CreatedBy}}' "$image" >"$work_dir/image.history"
python3 - "$work_dir/image.inspect.json" "$work_dir/image.history" <<'PY'
import json
import re
import sys

inspect_path, history_path = sys.argv[1:]
with open(inspect_path, encoding="utf-8") as handle:
    image = json.load(handle)[0]
with open(history_path, encoding="utf-8") as handle:
    history = handle.read()
config = image["Config"]
environment = config.get("Env") or []
labels = config.get("Labels") or {}
for item in environment:
    key = item.split("=", 1)[0]
    assert not re.search(r"(TOKEN|PASSWORD|DATABASE_URL|PRIVATE_KEY|SSH_KEY|AGE_RECIPIENT|KNOWN_HOSTS|BORG_REPO)", key, re.I)
assert set(labels) == {"org.opencontainers.image.source", "org.opencontainers.image.revision"}
for text in [history, *environment, *labels.keys(), *labels.values()]:
    assert "safe-smoke-upload-token" not in text
    assert "safe-smoke-storage-key" not in text
    assert "example.invalid ssh-ed25519" not in text
    assert "/data/repository" not in text
PY

docker create --name "$inspect_container" "$image" >/dev/null
docker export "$inspect_container" >"$work_dir/image.tar"
python3 - "$work_dir/image.tar" <<'PY'
import re
import sys
import tarfile

with tarfile.open(sys.argv[1]) as archive:
    members = archive.getmembers()
paths = [member.name.lstrip("./") for member in members]
for member, path in zip(members, paths):
    lowered = path.lower()
    assert not (member.isfile() and member.mode & 0o6000), path
    assert "security.capability" not in " ".join(member.pax_headers).lower(), path
    assert not path.startswith("usr/local/go/"), path
    assert not path.startswith("src/"), path
    assert not path.endswith(".go"), path
    assert "/.git/" not in f"/{path}/" and not path.endswith("/.git"), path
    assert not re.search(r"(^|/)(testdata|fixtures)(/|$)", lowered), path
    assert "recoveryarchive/testfixture" not in lowered, path
    if member.isfile():
        assert not re.search(r"(^|/)(([^/]+-)?(gcc|g\+\+|cc|c\+\+)(-[0-9.]+)?|clang(-[0-9.]+)?|go|make|cmake|pkg-config|curl|fusermount3?)$", path), path
        assert not path.startswith("var/lib/apt/lists/"), path
        assert not path.startswith("var/cache/apt/"), path
        assert not re.search(r"(^|/)\.cache/pip/", lowered), path
PY

docker run --rm --entrypoint /bin/sh "$image" -ceu '
  for command in go gcc cc clang make cmake pkg-config curl git fusermount fusermount3; do
    ! command -v "$command" >/dev/null 2>&1
  done
  test ! -e /src
  test ! -e /usr/local/go
  test ! -e /dev/fuse
'

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
(cd "$root_dir" && go run "$work_dir/generate_archive.go" "$work_dir/recovery.tar.gz")
verify_output="$(docker run --rm --read-only --user 10001:10001 \
  --mount "type=bind,src=$work_dir/recovery.tar.gz,dst=/run/recovery.tar.gz,readonly" \
  "$image" verify /run/recovery.tar.gz)"
[[ "$verify_output" == $'valid\nartifacts=1\nverified_bytes=23\nmanifest_final=true' ]] || fail "collector verify did not accept the valid smoke archive"

docker volume create "$data_volume" >/dev/null
docker volume create "$config_volume" >/dev/null
docker run --rm --user 0:0 --entrypoint /bin/sh \
  --mount "type=volume,src=$config_volume,dst=/fixture" \
  --env "SAFE_AGE_RECIPIENT=$recipient" \
  "$image" -ceu '
    umask 077
    mkdir -p /fixture/config /fixture/secrets
    printf "%s\n" "$SAFE_AGE_RECIPIENT" > /fixture/config/age-recipient
    printf "%s\n" "example.invalid ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAISafeCollectorSmokeFixture" > /fixture/config/known_hosts
    printf "%s\n" "safe-smoke-upload-token" > /fixture/secrets/upload-token
    printf "%s\n" "safe-smoke-storage-key" > /fixture/secrets/storage-ssh-key
    printf "%s\n" "/data/repository" > /fixture/secrets/borg-repository
    chmod 0644 /fixture/config/age-recipient /fixture/config/known_hosts
    chmod 0600 /fixture/secrets/upload-token /fixture/secrets/storage-ssh-key /fixture/secrets/borg-repository
    chown -R 10001:10001 /fixture
  '

docker run --rm --user 10001:10001 --entrypoint /bin/sh \
  --mount "type=volume,src=$data_volume,dst=/data" \
  --env BORG_UNKNOWN_UNENCRYPTED_REPO_ACCESS_IS_OK=yes \
  "$image" -ceu '
    borg init --encryption=none /data/repository
    mkdir /data/smoke-input
    printf "%s\n" "safe Borg smoke object" > /data/smoke-input/object
    borg create /data/repository::sherpa-smoke /data/smoke-input
    borg list --json /data/repository > /data/borg-list.json
    python -c '\''import json; data=json.load(open("/data/borg-list.json", encoding="utf-8")); assert [item["name"] for item in data["archives"]] == ["sherpa-smoke"]'\''
    rm -rf /data/smoke-input /data/borg-list.json
  '

docker run --detach --name "$container" \
  --user 10001:10001 \
  --read-only \
  --cap-drop ALL \
  --security-opt no-new-privileges:true \
  --pids-limit 128 \
  --tmpfs /tmp:rw,nosuid,nodev,noexec,size=64m \
  --mount "type=volume,src=$data_volume,dst=/data" \
  --mount "type=volume,src=$config_volume,dst=/run,readonly" \
  "$image" >/dev/null

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
[[ "$(docker exec "$container" readlink /proc/1/exe)" == "/usr/local/bin/collector" ]] || fail "collector serve is not PID 1"
[[ "$(docker exec "$container" /bin/sh -c "tr '\\0' ' ' </proc/1/cmdline")" == "/usr/local/bin/collector serve " ]] || fail "collector PID 1 command is not exact"
if docker exec "$container" /bin/sh -c ': > /rootfs-write-probe' >/dev/null 2>&1; then
  fail "collector root filesystem is writable"
fi
docker exec "$container" /bin/sh -ceu ': > /data/.write-probe; rm /data/.write-probe'
for directory in /data/spool /data/state /data/borg /data/borg/cache /data/borg/config /data/borg/security; do
  [[ "$(docker exec "$container" stat -c '%a:%u:%g' "$directory")" == "700:10001:10001" ]] || fail "collector data directory is not private and runtime-owned: $directory"
done
[[ "$(docker inspect --format '{{json .HostConfig.CapDrop}}' "$container")" == '["ALL"]' ]] || fail "collector runtime does not drop all capabilities"
[[ "$(docker inspect --format '{{json .HostConfig.SecurityOpt}}' "$container")" == '["no-new-privileges:true"]' ]] || fail "collector runtime does not enable no-new-privileges"
[[ "$(docker inspect --format '{{json .HostConfig.PortBindings}}' "$container")" == '{}' ]] || fail "collector runtime publishes a host port"
[[ "$(docker port "$container")" == "" ]] || fail "collector runtime has a published port"

image_id="$(docker image inspect --format '{{.Id}}' "$image")"
printf 'Collector Docker smoke passed: image=%s revision=%s\n' "$image_id" "$source_revision"
