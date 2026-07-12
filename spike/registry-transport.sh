#!/usr/bin/env bash
# spike/registry-transport.sh — pin the two unproven registry mechanics for 2c-i:
#   (A) publish transport: git bundle -> unbundle into a bare repo
#   (B) clone serve: dumb-HTTP git clone of a bare repo (update-server-info + static file serve)
#   (C) scan surface: get a working tree from the imported content to scan
#   (D) Postgres via docker for tests: start/ready/port-capture/stop
# Everything runs in temp dirs; nothing touches real state.
set -uo pipefail
BASE="$(mktemp -d)"; cd "$BASE"
GC="git -c user.email=s@x -c user.name=s -c commit.gpgsign=false"
ok(){ echo "  PASS: $1"; }; no(){ echo "  FAIL: $1"; }

echo "=== (A) publish transport: bundle -> bare repo ==="
mkdir stack && ( cd stack && $GC init -q -b main && echo "name: demo" > stack.yaml && echo "# demo" > AGENTS.md \
  && $GC add -A && $GC commit -q -m v1 && $GC tag v1 )
( cd stack && git bundle create ../stack.bundle --all ) >/dev/null 2>&1 && ok "bundle created" || no "bundle create"
git init -q --bare bare.git
# import all refs from the bundle into the bare repo
( cd bare.git && git fetch -q ../stack.bundle 'refs/heads/*:refs/heads/*' 'refs/tags/*:refs/tags/*' ) \
  && [ -n "$(git -C bare.git tag -l v1)" ] && ok "unbundled into bare (tag v1 present)" || no "unbundle"
git -C bare.git rev-parse main >/dev/null 2>&1 && ok "main branch present in bare" || no "main missing"

echo "=== (C) scan surface: worktree from the bare repo at the tag ==="
git -C bare.git worktree add -q "$BASE/wt" v1 2>/dev/null \
  && [ -f "$BASE/wt/stack.yaml" ] && ok "worktree checkout of v1 has stack.yaml" || no "worktree"

echo "=== (B) clone serve: dumb-HTTP ==="
git -C bare.git update-server-info && [ -f bare.git/info/refs ] && ok "update-server-info wrote info/refs" || no "update-server-info"
( cd bare.git && python3 -m http.server 8791 >/dev/null 2>&1 & echo $! > "$BASE/http.pid" )
sleep 1
if git clone -q "http://localhost:8791/" "$BASE/cloned" 2>/dev/null && [ -f "$BASE/cloned/stack.yaml" ]; then
  ok "dumb-HTTP git clone succeeded (tree present)"
  [ -n "$(git -C "$BASE/cloned" tag -l v1)" ] && ok "cloned tag v1 present" || no "cloned tag missing"
else
  no "dumb-HTTP git clone FAILED — record: may need git-http-backend (smart HTTP)"
fi
kill "$(cat "$BASE/http.pid")" 2>/dev/null

echo "=== (D) Postgres via docker ==="
if ! docker info >/dev/null 2>&1; then echo "  SKIP: docker daemon not available"; else
  CID=$(docker run --rm -d -e POSTGRES_PASSWORD=pw -e POSTGRES_DB=sherpa -p 0:5432 postgres:16 2>/dev/null)
  if [ -n "$CID" ]; then
    ok "postgres:16 container started ($CID)"
    PORT=$(docker inspect --format '{{ (index (index .NetworkSettings.Ports "5432/tcp") 0).HostPort }}' "$CID")
    echo "  mapped host port: $PORT"
    for i in $(seq 1 30); do docker exec "$CID" pg_isready -U postgres >/dev/null 2>&1 && break; sleep 1; done
    docker exec "$CID" pg_isready -U postgres >/dev/null 2>&1 && ok "pg_isready after $i s" || no "pg never ready"
    echo "  DSN would be: postgres://postgres:pw@localhost:$PORT/sherpa?sslmode=disable"
    docker stop "$CID" >/dev/null 2>&1 && ok "container stopped" || no "stop failed"
  else no "docker run postgres failed"; fi
fi

echo; echo "base (delete): rm -rf $BASE"; git -C bare.git worktree prune 2>/dev/null; rm -rf "$BASE"
