#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
image="${SHERPA_SMOKE_IMAGE:-sherpa-registry:test}"
suffix="$$-${RANDOM}"
network="sherpa-smoke-${suffix}"
postgres_container="sherpa-smoke-postgres-${suffix}"
registry_container="sherpa-smoke-registry-${suffix}"
contender_container="sherpa-smoke-registry-contender-${suffix}"
volume="sherpa-smoke-volume-${suffix}"
database_url="postgres://postgres:smoke-password@${postgres_container}:5432/sherpa?sslmode=disable"

cleanup() {
  docker rm -f "$contender_container" "$registry_container" "$postgres_container" >/dev/null 2>&1 || true
  docker network rm "$network" >/dev/null 2>&1 || true
  docker volume rm "$volume" >/dev/null 2>&1 || true
}
trap cleanup EXIT

if ! grep -Eq '"numReplicas"[[:space:]]*:[[:space:]]*1([[:space:]]*,)?[[:space:]]*$' "$root_dir/railway.json"; then
  echo "Railway registry deployment must keep numReplicas=1" >&2
  exit 1
fi

docker build --tag "$image" "$root_dir"
docker network create "$network" >/dev/null
docker volume create "$volume" >/dev/null
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

docker run --rm --network "$network" \
  --mount "type=volume,src=${volume},dst=/data" \
  --env "DATABASE_URL=${database_url}" \
  --env SHERPA_CONTENT_DIR=/data/git \
  "$image" export /data/exports/sherpa-smoke-export.tar.gz >/dev/null
docker run --rm --entrypoint /bin/sh \
  --mount "type=volume,src=${volume},dst=/data" \
  "$image" -c \
  'test -s /data/exports/sherpa-smoke-export.tar.gz && test -z "$(find /data/git -type f -name '\''sherpa-*.tar.gz'\'' -print -quit)" && rm /data/exports/sherpa-smoke-export.tar.gz'

docker run --detach --name "$registry_container" --network "$network" \
  --publish 127.0.0.1::8080 \
  --mount "type=volume,src=${volume},dst=/data" \
  --env "DATABASE_URL=${database_url}" \
  --env SHERPA_CONTENT_DIR=/data/git \
  --env SHERPA_GITHUB_CLIENT_ID=x \
  --env SHERPA_PUBLIC_BASE_URL=https://registry.example \
  --env SHERPA_EXPORT_URL=https://collector.invalid/upload \
  --env SHERPA_EXPORT_TOKEN=smoke-export-token \
  --env SHERPA_EXPORT_INTERVAL=24h \
  --env SHERPA_EXPORT_ARCHIVE_DIR=/data/exports \
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

if ! docker exec "$registry_container" sh -c \
  'test -d /data && test ! -L /data && mountpoint -q /data'; then
  echo "export volume root is not a trusted real mountpoint" >&2
  exit 1
fi
data_identity="$(docker exec "$registry_container" stat -c '%F:%u:%g' /data)"
if [[ "$data_identity" != "directory:0:0" ]] || \
  docker exec --user 998:998 "$registry_container" test -w /data; then
  echo "export volume root is not root-owned and protected: ${data_identity:-unknown}" >&2
  exit 1
fi
sherpa_uid="$(docker exec "$registry_container" getent passwd sherpa | cut -d: -f3)"
sherpa_gid="$(docker exec "$registry_container" getent group sherpa | cut -d: -f3)"
pid_one_uid="$(docker exec "$registry_container" awk '/^Uid:/{print $2}' /proc/1/status)"
pid_one_gid="$(docker exec "$registry_container" awk '/^Gid:/{print $2}' /proc/1/status)"
if [[ "$sherpa_uid" != "998" || "$sherpa_gid" != "998" || "$pid_one_uid" != "998" || "$pid_one_gid" != "998" ]]; then
  echo "sherpa or PID 1 identity is not UID/GID 998: sherpa=${sherpa_uid:-unknown}:${sherpa_gid:-unknown} pid1=${pid_one_uid:-unknown}:${pid_one_gid:-unknown}" >&2
  exit 1
fi
if ! docker exec "$registry_container" sh -c 'test -d /data/exports && test ! -L /data/exports'; then
  echo "export queue is not a real directory" >&2
  exit 1
fi
queue_identity="$(docker exec "$registry_container" stat -c '%a:%u:%g' /data/exports)"
if [[ "$queue_identity" != "700:${sherpa_uid}:${sherpa_gid}" ]]; then
  echo "export queue is not private or registry-owned: ${queue_identity:-unknown}" >&2
  exit 1
fi
docker exec --user 998:998 "$registry_container" sh -c \
  'test -w /data/git && : > /data/git/.smoke-write && rm /data/git/.smoke-write'
docker exec --user 998:998 "$registry_container" git --version >/dev/null

docker run --detach --name "$contender_container" --network "$network" \
  --mount "type=volume,src=${volume},dst=/data" \
  --env "DATABASE_URL=${database_url}" \
  --env SHERPA_CONTENT_DIR=/data/git \
  --env SHERPA_GITHUB_CLIENT_ID=x \
  --env SHERPA_PUBLIC_BASE_URL=https://registry.example \
  --env SHERPA_EXPORT_URL=https://collector.invalid/upload \
  --env SHERPA_EXPORT_TOKEN=smoke-export-token \
  --env SHERPA_EXPORT_INTERVAL=24h \
  --env SHERPA_EXPORT_ARCHIVE_DIR=/data/exports \
  "$image" >/dev/null
for _ in $(seq 1 50); do
  if [[ "$(docker inspect --format '{{.State.Running}}' "$contender_container")" != "true" ]]; then
    break
  fi
  sleep 0.1
done
if [[ "$(docker inspect --format '{{.State.Running}}' "$contender_container")" == "true" ]]; then
  docker logs "$contender_container" >&2
  echo "second scheduler did not fail promptly on queue lock contention" >&2
  exit 1
fi
contender_exit="$(docker inspect --format '{{.State.ExitCode}}' "$contender_container")"
contender_logs="$(docker logs "$contender_container" 2>&1)"
if [[ "$contender_exit" == "0" ]] || \
  ! grep -q 'prepare off-site export scheduler: export archive directory is already in use' <<<"$contender_logs"; then
  echo "second scheduler did not fail with the fixed lock classification" >&2
  exit 1
fi
if grep -Fq 'smoke-export-token' <<<"$contender_logs" || \
  grep -Fq 'collector.invalid' <<<"$contender_logs" || \
  grep -Fq 'smoke-password' <<<"$contender_logs"; then
  echo "second scheduler lock error exposed a smoke secret or collector endpoint" >&2
  exit 1
fi

docker run --rm --entrypoint /bin/sh "$image" -c \
  'test ! -e /src && test ! -e /usr/local/go && ! command -v go >/dev/null 2>&1'
image_metadata="$(docker image inspect "$image")$(docker history --no-trunc --format '{{.CreatedBy}}' "$image")"
if grep -Eqi 'DATABASE_URL|POSTGRES_PASSWORD|SHERPA_REGISTRY_TOKEN|SHERPA_EXPORT_TOKEN|smoke-password' <<<"$image_metadata"; then
  echo "image metadata contains a secret variable or smoke credential" >&2
  exit 1
fi

echo "Docker smoke check passed: ${image}"
