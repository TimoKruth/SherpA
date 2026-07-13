#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
web_image="${SHERPA_WEB_SMOKE_IMAGE:-sherpa-web:test}"
fixture_image="${SHERPA_WEB_FIXTURE_IMAGE:-sherpa-web-fixture:test}"
suffix="$$-${RANDOM}"
network="sherpa-web-smoke-${suffix}"
fixture_container="sherpa-web-fixture-${suffix}"
web_container="sherpa-web-${suffix}"
inspect_container="sherpa-web-inspect-${suffix}"
work_dir="$(mktemp -d "${TMPDIR:-/tmp}/sherpa-web-smoke.XXXXXX")"
repo_url="https://registry.example/v1/stacks/alice/reviewer.git"

cleanup() {
  docker rm -f "$web_container" "$fixture_container" "$inspect_container" >/dev/null 2>&1 || true
  docker network rm "$network" >/dev/null 2>&1 || true
  rm -rf "$work_dir"
}
trap cleanup EXIT

request() {
  local path="$1"
  curl --fail --silent --show-error --max-time 20 "http://127.0.0.1:${host_port}${path}"
}

docker build --file "$root_dir/deploy/web/Dockerfile" --tag "$web_image" "$root_dir"
docker build --file "$root_dir/deploy/web/testfixture/Dockerfile" --tag "$fixture_image" "$root_dir"
docker network create "$network" >/dev/null
docker run --detach --name "$fixture_container" --network "$network" "$fixture_image" >/dev/null
docker run --detach --name "$web_container" --network "$network" \
  --publish 127.0.0.1::8080 \
  --env PORT=8080 \
  --env "SHERPA_REGISTRY_API_URL=http://${fixture_container}:8081" \
  --env SHERPA_WEB_PUBLIC_BASE_URL=https://sherpa.example \
  "$web_image" >/dev/null

host_port="$(docker port "$web_container" 8080/tcp | sed -n '1s/.*://p')"
if [[ -z "$host_port" ]]; then
  echo "web container did not publish its HTTP port" >&2
  exit 1
fi
for _ in $(seq 1 60); do
  if request /healthz >/dev/null 2>&1 && request / >/dev/null 2>&1; then
    break
  fi
  if [[ "$(docker inspect --format '{{.State.Running}}' "$web_container")" != "true" ]]; then
    docker logs "$web_container" >&2
    exit 1
  fi
  sleep 1
done

request /healthz | grep -qx 'ok'
request / >"$work_dir/home.html"
request /stacks/alice/reviewer >"$work_dir/stack.html"
grep -Fq '@alice/reviewer' "$work_dir/home.html"
grep -Fq 'Security-focused code review' "$work_dir/stack.html"
grep -Fq "sherpa try ${repo_url}" "$work_dir/stack.html"
grep -Fq "sherpa clone ${repo_url}" "$work_dir/stack.html"

curl --silent --show-error --max-time 20 --dump-header "$work_dir/headers" --output /dev/null \
  "http://127.0.0.1:${host_port}/stacks/alice/reviewer"
grep -Eqi "^Content-Security-Policy: .*default-src 'none'" "$work_dir/headers"
grep -Eqi '^X-Content-Type-Options: nosniff' "$work_dir/headers"
grep -Eqi '^Referrer-Policy: no-referrer' "$work_dir/headers"
grep -Eqi '^X-Frame-Options: DENY' "$work_dir/headers"

configured_user="$(docker image inspect --format '{{.Config.User}}' "$web_image")"
if [[ "$configured_user" != "nonroot:nonroot" ]]; then
  echo "image user is ${configured_user:-unset}, want nonroot:nonroot" >&2
  exit 1
fi
docker cp "$web_container:/etc/passwd" "$work_dir/passwd"
expected_uid="$(awk -F: '$1 == "nonroot" { print $3 }' "$work_dir/passwd")"
running_uid="$(docker top "$web_container" -eo uid,pid,comm | awk 'NR == 2 { print $1 }')"
if [[ -z "$expected_uid" || "$running_uid" != "$expected_uid" ]]; then
  echo "web PID 1 UID ${running_uid:-unknown} does not match configured nonroot UID ${expected_uid:-unknown}" >&2
  exit 1
fi

web_id="$(docker inspect --format '{{.Id}}' "$web_container")"
web_started="$(docker inspect --format '{{.State.StartedAt}}' "$web_container")"
web_restarts="$(docker inspect --format '{{.RestartCount}}' "$web_container")"
docker stop --time 2 "$fixture_container" >/dev/null
status="$(curl --silent --show-error --max-time 20 --dump-header "$work_dir/unavailable.headers" --output "$work_dir/unavailable.html" --write-out '%{http_code}' "http://127.0.0.1:${host_port}/stacks/alice/reviewer")"
if [[ "$status" != "503" ]]; then
  echo "dynamic page status during registry outage is $status, want 503" >&2
  exit 1
fi
grep -Eqi '^Retry-After: 60' "$work_dir/unavailable.headers"
request /healthz | grep -qx 'ok'
if [[ "$(docker inspect --format '{{.Id}}' "$web_container")" != "$web_id" \
  || "$(docker inspect --format '{{.State.StartedAt}}' "$web_container")" != "$web_started" \
  || "$(docker inspect --format '{{.RestartCount}}' "$web_container")" != "$web_restarts" ]]; then
  echo "web container restarted during registry outage" >&2
  exit 1
fi

docker start "$fixture_container" >/dev/null
for _ in $(seq 1 30); do
  if request /stacks/alice/reviewer >"$work_dir/recovered.html" 2>/dev/null; then
    break
  fi
  sleep 1
done
grep -Fq '@alice/reviewer' "$work_dir/recovered.html"
if [[ "$(docker inspect --format '{{.State.StartedAt}}' "$web_container")" != "$web_started" ]]; then
  echo "web container restarted instead of recovering in place" >&2
  exit 1
fi

docker create --name "$inspect_container" "$web_image" >/dev/null
docker export "$inspect_container" >"$work_dir/image.tar"
tar -tf "$work_dir/image.tar" >"$work_dir/files"
if grep -Eq '^usr/local/go/|^src/|\.go$|(^|/)(sh|bash|dash|busybox|registry|git|pg_dump|psql)$' "$work_dir/files"; then
  echo "production image contains a shell, toolchain, source, registry binary, or DB/Git tool" >&2
  exit 1
fi
image_metadata="$(docker image inspect "$web_image")$(docker history --no-trunc --format '{{.CreatedBy}}' "$web_image")"
if grep -Eqi 'DATABASE_URL|SHERPA_REGISTRY_TOKEN|SHERPA_EXPORT_TOKEN|PASSWORD|PRIVATE_KEY' <<<"$image_metadata"; then
  echo "production image metadata contains a secret-like setting" >&2
  exit 1
fi

echo "Web Docker smoke check passed: ${web_image}"
