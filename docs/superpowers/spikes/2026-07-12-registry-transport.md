# Spike: registry transport + Postgres-docker mechanics (2c-i)

**Date:** 2026-07-12 · git 2.54 · postgres:16 (docker) · macOS · script `spike/registry-transport.sh`

Pins the two unproven 2c-i mechanics before the plan builds on them. **All probes passed.**

## (A) Publish transport: git bundle → bare repo — WORKS
- CLI side: `git bundle create stack.bundle --all` (from the profile's repo).
- Server side: create bare repo `git init --bare <name>.git`, then import all refs:
  ```
  git -C <bare> fetch <bundle-path> 'refs/heads/*:refs/heads/*' 'refs/tags/*:refs/tags/*'
  ```
  Confirmed the `main` branch and `v<version>` tag land in the bare repo.
- Decision: **publish = the CLI POSTs the bundle bytes; the server writes them to a temp file
  and `git fetch`es them into a staging bare repo.** (`StageBundle` in `content`.)

## (C) Scan surface: worktree from the bare repo — WORKS
- `git -C <bare> worktree add <tmp> v<version>` yields a working tree with the stack files.
- The publish handler scans that worktree (`publishscan.ScanRepo`) BEFORE making the import
  durable — staging separates unpack+scan from commit, so a rejected publish never touches the
  live repo. (Remove the worktree + prune after scanning.)

## (B) Clone serve: dumb-HTTP — WORKS
- After every import: `git -C <bare> update-server-info` (writes `info/refs`, `objects/info/packs`).
- Serve the bare repo dir as static files; `git clone http://host/v1/stacks/o/n.git/ out` succeeds,
  tree + tags present. **No `git-http-backend` CGI needed** for 2c-i (dumb HTTP is sufficient for
  clone/fetch; smart-HTTP upload-pack can be a later optimization).
- Handler: `http.FileServer(http.Dir(RepoPath(owner,name)))` under `StripPrefix` of the
  `…/{name}.git/` route. Run `update-server-info` at `Commit` time so `info/refs` is fresh.

## (D) Postgres via docker (test harness) — WORKS
Recipe for `store.StartPostgres(t)` (no testcontainers dep; shells to `docker`):
```
CID=$(docker run --rm -d -e POSTGRES_PASSWORD=pw -e POSTGRES_DB=sherpa -p 0:5432 postgres:16)
PORT=$(docker inspect --format '{{ (index (index .NetworkSettings.Ports "5432/tcp") 0).HostPort }}' "$CID")
# poll until ready (was ready in ~2s):
until docker exec "$CID" pg_isready -U postgres >/dev/null 2>&1; do sleep 1; done
DSN=postgres://postgres:pw@localhost:$PORT/sherpa?sslmode=disable
# t.Cleanup: docker stop "$CID"
```
- `-p 0:5432` → random host port, captured via `docker inspect`. Ready in ~2s.
- The harness must `t.Skip` if `docker info` fails, so `go test ./...` stays green where Docker is
  absent. (Docker IS up in this dev environment, so the registry tests run for real here.)

## Adapter facts pinned for the plan
- ContentStore: bare repos under a content root; `StageBundle`→temp bare+worktree; `Commit`→atomic
  rename into `<root>/<owner>/<name>.git` + `update-server-info`; `RepoPath` for the clone handler.
- No `git-http-backend`; dumb HTTP for reads.
- Postgres test harness via `docker run postgres:16`, skip-if-no-docker.
