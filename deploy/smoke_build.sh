#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
image="${SHERPA_SMOKE_IMAGE:-sherpa-registry:test}"
suffix="$$-${RANDOM}"
network="sherpa-smoke-${suffix}"
postgres_container="sherpa-smoke-postgres-${suffix}"
registry_container="sherpa-smoke-registry-${suffix}"
volume_dir="$(mktemp -d "${TMPDIR:-/tmp}/sherpa-smoke.XXXXXX")"
database_url="postgres://postgres:smoke-password@${postgres_container}:5432/sherpa?sslmode=disable"

cleanup() {
  docker rm -f "$registry_container" "$postgres_container" >/dev/null 2>&1 || true
  docker network rm "$network" >/dev/null 2>&1 || true
  rm -rf "$volume_dir"
}
trap cleanup EXIT

docker build --tag "$image" "$root_dir"
docker network create "$network" >/dev/null
docker run --detach --name "$postgres_container" --network "$network" \
  --env POSTGRES_PASSWORD=smoke-password \
  --env POSTGRES_DB=sherpa \
  postgres:16 >/dev/null

for _ in $(seq 1 60); do
  if docker exec "$postgres_container" pg_isready --username postgres --dbname sherpa >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
if ! docker exec "$postgres_container" pg_isready --username postgres --dbname sherpa >/dev/null 2>&1; then
  echo "smoke Postgres did not become ready" >&2
  exit 1
fi

docker run --detach --name "$registry_container" --network "$network" \
  --publish 127.0.0.1::8080 \
  --mount "type=bind,src=${volume_dir},dst=/data" \
  --env "DATABASE_URL=${database_url}" \
  --env SHERPA_CONTENT_DIR=/data/git \
  --env SHERPA_GITHUB_CLIENT_ID=x \
  --env SHERPA_PUBLIC_BASE_URL=https://registry.example \
  "$image" >/dev/null

host_port="$(docker port "$registry_container" 8080/tcp | sed -n '1s/.*://p')"
if [[ -z "$host_port" ]]; then
  echo "registry did not publish its HTTP port" >&2
  exit 1
fi
for _ in $(seq 1 60); do
  if curl --fail --silent --show-error "http://127.0.0.1:${host_port}/healthz" >/dev/null 2>&1; then
    break
  fi
  if [[ "$(docker inspect --format '{{.State.Running}}' "$registry_container")" != "true" ]]; then
    docker logs "$registry_container" >&2
    exit 1
  fi
  sleep 1
done
curl --fail --silent --show-error "http://127.0.0.1:${host_port}/healthz" | grep -qx 'ok'

sherpa_uid="$(docker exec "$registry_container" getent passwd sherpa | cut -d: -f3)"
sherpa_gid="$(docker exec "$registry_container" getent group sherpa | cut -d: -f3)"
pid_one_uid="$(docker exec "$registry_container" awk '/^Uid:/{print $2}' /proc/1/status)"
pid_one_gid="$(docker exec "$registry_container" awk '/^Gid:/{print $2}' /proc/1/status)"
if [[ "$sherpa_uid" != "998" || "$sherpa_gid" != "998" || "$pid_one_uid" != "998" || "$pid_one_gid" != "998" ]]; then
  echo "sherpa or PID 1 identity is not UID/GID 998: sherpa=${sherpa_uid:-unknown}:${sherpa_gid:-unknown} pid1=${pid_one_uid:-unknown}:${pid_one_gid:-unknown}" >&2
  exit 1
fi
docker exec --user 998:998 "$registry_container" sh -c \
  'test -w /data/git && test -w /data/exports && : > /data/git/.smoke-write && : > /data/exports/.smoke-write && rm /data/git/.smoke-write /data/exports/.smoke-write'
docker exec --user 998:998 "$registry_container" git --version >/dev/null
docker exec --user 998:998 "$registry_container" sh -c \
  'registry export /data/exports/sherpa-smoke-export.tar.gz >/dev/null && test -s /data/exports/sherpa-smoke-export.tar.gz && test -z "$(find /data/git -type f -name '\''sherpa-*.tar.gz'\'' -print -quit)" && rm /data/exports/sherpa-smoke-export.tar.gz'

docker run --rm --entrypoint /bin/sh "$image" -c \
  'test ! -e /src && test ! -e /usr/local/go && ! command -v go >/dev/null 2>&1'
image_metadata="$(docker image inspect "$image")$(docker history --no-trunc --format '{{.CreatedBy}}' "$image")"
if grep -Eqi 'DATABASE_URL|POSTGRES_PASSWORD|SHERPA_REGISTRY_TOKEN|SHERPA_EXPORT_TOKEN|smoke-password' <<<"$image_metadata"; then
  echo "image metadata contains a secret variable or smoke credential" >&2
  exit 1
fi

echo "Docker smoke check passed: ${image}"
