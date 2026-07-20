# Railway Staging Acceptance Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Bring SherpA's Postgres, registry, and website services online in Railway staging and pass every Phase 2c-iii, 2d, and 2e live acceptance gate with secret-safe evidence.

**Architecture:** Execute one ordered rollout against Railway project `powerful-rebirth`, environment `staging`: prepare tooling, prove the exact application candidate locally, provision Postgres, pass the registry gate, connect and pass the website gate, then pass the social/session gate. Treat every outage, rollback, restore, or temporary recovery resource as an explicit approval checkpoint; never continue through a failed dependency gate.

**Tech Stack:** Railway CLI and Railway MCP/skills, GitHub CLI and GitHub OAuth, Go 1.26, Docker, Git, curl, jq, Playwright, PostgreSQL 16, Markdown evidence.

## Global Constraints

- Approved design: `docs/superpowers/specs/2026-07-19-railway-staging-acceptance-design.md`.
- Authoritative runbook: `docs/deployment/railway.md`.
- Railway project: `powerful-rebirth` (`865ca83b-9c9e-4e50-b424-e267aa43988f`).
- Railway environment: `staging` (`53d89401-bec2-49a3-ac58-8dc93b29d049`).
- Production remains empty and untouched.
- Application candidate is `origin/main` at `312479ee55539560936129db21bcec8da7b9827a` unless an application/configuration fix is required and pushed; local documentation-only commits do not change the deployed candidate.
- The local branch contains approved rollout documentation commits ahead of `origin/main`; do not push them merely to deploy the unchanged application.
- Preserve the three unrelated untracked `.DS_Store` files; never add, delete, or commit them.
- Never print or persist Railway variable values, database URLs, OAuth secrets, access tokens, device codes, grants, CSRF values, cookies, trial notes, request bodies, or private collector URLs.
- `railway variable list --json` contains raw values. It may be piped directly to `jq -r 'keys[]'` so only names reach stdout; never save or display the unfiltered JSON.
- Use explicit `--project`, `--environment`, and `--service` selectors wherever the command supports them.
- A failed local preflight blocks provisioning. A failed Postgres deployment blocks registry. A failed 2c-iii gate blocks web. A failed 2d gate blocks 2e.
- Before every deliberate outage, rollback, restore, temporary sibling/recovery resource, or cleanup of recovery resources, present the exact action, expected impact, rollback, and resources, then wait for operator approval.
- If a code defect is discovered, stop this operational plan, invoke systematic-debugging and test-driven-development, create a focused regression test and permanent fix, run the complete local gate, push only the required fix commit, update the candidate commit in the evidence file, and resume at the blocked task.
- If the configured off-site collector cannot be proven durable and retrievable, fail the gate. Do not substitute local container storage or another Railway volume.

## File Structure

- Create: `docs/deployment/evidence/2026-07-20-railway-staging-acceptance.md` — secret-safe execution record with metadata, approval checkpoints, one row per acceptance step, deployment/object IDs, and final status.
- Modify during execution: `docs/deployment/evidence/2026-07-20-railway-staging-acceptance.md` only.
- Do not modify application code unless a separately diagnosed defect requires the TDD exception in Global Constraints.
- Ephemeral test homes, auth response files, stack fixtures, and downloaded `/proc` files live beneath a mode-0700 directory created by `mktemp -d`; delete it only after the evidence record no longer needs local non-secret summaries.
- Bash tool calls do not retain shell state. Task 2 stores its exact-candidate directory in `/tmp/sherpa-accept-root-current`. Task 4 creates `/tmp/sherpa-live-root-current` and a mode-0600 `context.sh`; every later shell block that consumes live-gate variables must load them with `LIVE_ROOT="$(cat /tmp/sherpa-live-root-current)"` and `. "$LIVE_ROOT/context.sh"`.

---

### Task 1: Prepare Railway, GitHub, browser, and local tooling

**Files:**
- Create: `docs/deployment/evidence/2026-07-20-railway-staging-acceptance.md`

**Interfaces:**
- Consumes: approved spec and existing authenticated Railway account.
- Produces: current Railway CLI, Railway agent integration, verified local tools, `ACCEPT_ROOT` convention, and the evidence ledger used by every later task.

- [ ] **Step 1: Verify the project root before any write**

Run:

```sh
pwd
git status --short --branch
```

Expected: project root `/Users/timokruth/Projekte/feat`; `main` is ahead of `origin/main` by documentation-only commits; the only untracked paths are `.DS_Store`, `docs/.DS_Store`, and `docs/superpowers/.DS_Store`.

- [ ] **Step 2: Upgrade Railway CLI and install supported agent integration**

Run:

```sh
railway upgrade --yes
railway setup agent --yes
railway --version
railway whoami
```

Expected:

- Railway CLI reports the current release, at least `5.27.0`.
- Account is `Timo Kruth`.
- `railway setup agent` reports installed Railway skills/MCP configuration.

If setup reports that a Claude session restart is required, record that fact and continue this session with the CLI. Do not manually edit Claude settings unless necessary; if manual settings changes are necessary, invoke the `update-config` skill first.

- [ ] **Step 3: Verify the remaining local tools and GitHub authentication**

Run:

```sh
gh auth status
docker info >/dev/null
go version
git --version
curl --version | grep '^curl '
jq --version
python3 --version
```

Expected: every command exits 0. GitHub CLI must have repository-read access to `TimoKruth/SherpA`; application OAuth identities remain separate live-test actors.

- [ ] **Step 4: Verify Railway linkage and immutable service inventory**

Run:

```sh
railway status --json | jq '{project: .name, projectId: .id, environments: [.environments.edges[].node | {name, id}]}'
railway service list --project 865ca83b-9c9e-4e50-b424-e267aa43988f --environment 53d89401-bec2-49a3-ac58-8dc93b29d049 --json
railway volume list --json
railway domain list --project 865ca83b-9c9e-4e50-b424-e267aa43988f --environment 53d89401-bec2-49a3-ac58-8dc93b29d049 --service registry --json
railway domain list --project 865ca83b-9c9e-4e50-b424-e267aa43988f --environment 53d89401-bec2-49a3-ac58-8dc93b29d049 --service web --json
```

Expected:

- project `powerful-rebirth`, environment `staging`;
- services `postgres`, `registry`, and `web`;
- only the registry volume exists before Task 3;
- registry domain `registry-staging-78a5.up.railway.app`;
- web domain `web-staging-c58d.up.railway.app`.

- [ ] **Step 5: Create the evidence ledger**

Create `docs/deployment/evidence/2026-07-20-railway-staging-acceptance.md` with this exact initial structure:

```markdown
# Railway Staging Acceptance Evidence — 2026-07-20

## Metadata

| Field | Value |
|---|---|
| Project | `powerful-rebirth` |
| Environment | `staging` |
| Application candidate | `312479ee55539560936129db21bcec8da7b9827a` |
| Registry domain | `https://registry-staging-78a5.up.railway.app` |
| Website domain | `https://web-staging-c58d.up.railway.app` |
| Operator | Timo Kruth |
| UTC start | Not run |
| UTC end | Not run |
| Final status | Automated gates green; live Railway acceptance pending |

## Tooling

| Check | Result | Non-secret evidence |
|---|---|---|
| Railway CLI and account | Not run | |
| Railway project/environment linkage | Not run | |
| GitHub CLI | Not run | |
| Go/Git/Docker/curl/jq/Python | Not run | |
| Railway agent integration | Not run | |

## Local preflight

| Check | Result | Non-secret evidence |
|---|---|---|
| Candidate and working tree | Not run | |
| gofmt | Not run | |
| Go tests | Not run | |
| Race tests | Not run | |
| go vet | Not run | |
| Go builds | Not run | |
| Registry image smoke | Not run | |
| Website image smoke | Not run | |
| Config syntax and diff check | Not run | |

## Disruptive-action approvals

| UTC time | Action | Expected impact | Rollback/cleanup | Approval |
|---|---|---|---|---|

## Postgres provisioning

| Field | Value |
|---|---|
| Service ID | `d67d4334-d3ca-4ca4-ab4c-099452c88557` |
| Volume ID | Not run |
| Deployment ID | Not run |
| PG16 active | Not run |
| Required mount | Not run |
| Public TCP absent | Not run |

## Phase 2c-iii registry gate

| Step | Result | UTC start/end | Non-secret evidence | IDs/notes |
|---|---|---|---|---|
| 2c-iii.1 empty stores deployment | Not run | | | |
| 2c-iii.2 health and search | Not run | | | |
| 2c-iii.3 device login and issuer | Not run | | | |
| 2c-iii.4 same-owner linked publish | Not run | | | |
| 2c-iii.5 wrong-owner rejection and audit | Not run | | | |
| 2c-iii.6 auth controls and edge IP | Not run | | | |
| 2c-iii.7 search/detail/clone and host pinning | Not run | | | |
| 2c-iii.8 registry redeploy persistence | Not run | | | |
| 2c-iii.9 off-site export upload | Not run | | | |
| 2c-iii.10 sibling restore and audit | Not run | | | |
| 2c-iii.11 secret-safe logs | Not run | | | |

## Phase 2d website gate

| Step | Result | UTC start/end | Non-secret evidence | IDs/notes |
|---|---|---|---|---|
| 2d.1 web source/config deployment | Not run | | | |
| 2d.2 health, non-root, secret isolation | Not run | | | |
| 2d.3 public pages and API parity | Not run | | | |
| 2d.4 copied clone/try commands | Not run | | | |
| 2d.5 host pinning and security headers | Not run | | | |
| 2d.6 paging and input bounds | Not run | | | |
| 2d.7 registry outage degradation/recovery | Not run | | | |
| 2d.8 isolated web and registry redeploys | Not run | | | |
| 2d.9 desktop/mobile/keyboard QA | Not run | | | |
| 2d.10 secret-safe logs | Not run | | | |
| 2d.11 continuous uptime checks | Not run | | | |

## Phase 2e live gate

| Step | Result | UTC start/end | Non-secret evidence | IDs/notes |
|---|---|---|---|---|
| 2e.1 prior gates exact candidate | Not run | | | |
| 2e.2 website GitHub sign-in | Not run | | | |
| 2e.3 replay/tamper and cookie security | Not run | | | |
| 2e.4 web/CLI/admin authorization boundaries | Not run | | | |
| 2e.5 follow and pending update | Not run | | | |
| 2e.6 reviewed monotonicity | Not run | | | |
| 2e.7 clone auto-follow and offline sync | Not run | | | |
| 2e.8 private trial and verdict-only share | Not run | | | |
| 2e.9 edge origin/CSRF/redirect defenses | Not run | | | |
| 2e.10 outage degradation and logout | Not run | | | |
| 2e.11 restored social data and secret-safe logs | Not run | | | |
| 2e.12 separate rollback drills | Not run | | | |

## Export, restore, and audit summary

| Field | Value |
|---|---|
| Collector object ID | Not run |
| Recovery Postgres/service IDs | Not run |
| Recovery registry/service IDs | Not run |
| Recovery volume IDs | Not run |
| Manifest verification | Not run |
| `registry audit` | Not run |
| Recovery smoke | Not run |

## Rollback summary

| Drill | Source commit/deployment | Result | Recovery deployment |
|---|---|---|---|
| Website-only | Not run | Not run | Not run |
| Registry-only 2e-aware | Not run | Not run | Not run |

## Final checklist

- [ ] Postgres, registry, and website healthy.
- [ ] Phase 2c-iii: 11/11 passed.
- [ ] Phase 2d: 11/11 passed.
- [ ] Phase 2e: 12/12 passed.
- [ ] Off-site restore and audit passed.
- [ ] Separate rollback drills passed.
- [ ] Evidence contains no secrets or private payloads.
- [ ] Production remains empty and unchanged.
```

- [ ] **Step 6: Record tooling results without raw secret-bearing output**

Update only result summaries, versions, project/environment IDs, service IDs, domain IDs, and the UTC start time. Do not paste full `gh auth status`, Railway status JSON, or environment values.

- [ ] **Step 7: Commit the evidence skeleton**

Run:

```sh
git add docs/deployment/evidence/2026-07-20-railway-staging-acceptance.md
git commit -m $'docs: start Railway staging acceptance evidence\n\nCo-Authored-By: Claude <noreply@anthropic.com>'
```

Expected: one new evidence file committed; `.DS_Store` files remain untracked.

---

### Task 2: Run exact-candidate local preflight

**Files:**
- Modify: `docs/deployment/evidence/2026-07-20-railway-staging-acceptance.md`

**Interfaces:**
- Consumes: repository at local documentation HEAD and application candidate `origin/main`.
- Produces: proof that documentation-only drift is isolated and the exact application source passes all automated gates.

- [ ] **Step 1: Prove local drift is documentation-only**

Run:

```sh
git rev-parse HEAD main origin/main
git status --short --branch
git diff --name-status origin/main..HEAD
```

Expected: `origin/main` is `312479ee55539560936129db21bcec8da7b9827a`; every committed path after it is under `docs/`; only the three known `.DS_Store` files are untracked.

- [ ] **Step 2: Create an exact-candidate source directory without changing branches**

Run:

```sh
ACCEPT_ROOT="$(mktemp -d /tmp/sherpa-railway-acceptance.XXXXXX)"
chmod 700 "$ACCEPT_ROOT"
printf '%s\n' "$ACCEPT_ROOT" >/tmp/sherpa-accept-root-current
chmod 600 /tmp/sherpa-accept-root-current
git archive 312479ee55539560936129db21bcec8da7b9827a | tar -x -C "$ACCEPT_ROOT"
printf '%s\n' "$ACCEPT_ROOT"
```

Expected: a mode-0700 temporary directory containing the exact application candidate and no `.git` directory.

- [ ] **Step 3: Run check-only formatting, unit, integration, race, vet, and build gates**

Run from the exact-candidate directory:

```sh
ACCEPT_ROOT="$(cat /tmp/sherpa-accept-root-current)"
cd "$ACCEPT_ROOT"
test -z "$(gofmt -l .)"
go test -count=1 ./...
go test -race ./internal/registry/... ./internal/web/... ./internal/cli ./internal/state ./internal/integration
go vet ./...
go build ./cmd/...
```

Expected: all commands exit 0; formatting emits no paths.

- [ ] **Step 4: Validate deployment scripts and config syntax**

Run:

```sh
ACCEPT_ROOT="$(cat /tmp/sherpa-accept-root-current)"
cd "$ACCEPT_ROOT"
bash -n deploy/smoke_build.sh
bash -n deploy/web/smoke_build.sh
jq empty railway.json deploy/web/railway.json
git -C /Users/timokruth/Projekte/feat diff --check
```

Expected: all commands exit 0.

- [ ] **Step 5: Run both production image smoke tests**

Run:

```sh
ACCEPT_ROOT="$(cat /tmp/sherpa-accept-root-current)"
cd "$ACCEPT_ROOT"
bash deploy/smoke_build.sh
bash deploy/web/smoke_build.sh
```

Expected:

- registry image builds and passes PG16, `/healthz`, non-root PID 1, `/data` writability, Git, `pg_dump`, and final-image checks;
- website image builds and passes public-page, command, header, non-root PID 1, outage/recovery, and final-image checks.

- [ ] **Step 6: Record concise preflight results**

Return to the project root and update the evidence file with pass/fail, tool versions, exact candidate hash, and smoke image identifiers if printed. Do not attach full logs.

- [ ] **Step 7: Commit preflight evidence**

Run:

```sh
cd /Users/timokruth/Projekte/feat
git add docs/deployment/evidence/2026-07-20-railway-staging-acceptance.md
git commit -m $'docs: record Railway staging preflight\n\nCo-Authored-By: Claude <noreply@anthropic.com>'
```

Expected: evidence records a fully green exact-candidate preflight.

---

### Task 3: Provision and deploy PostgreSQL 16

**Files:**
- Modify: `docs/deployment/evidence/2026-07-20-railway-staging-acceptance.md`

**Interfaces:**
- Consumes: green Task 2 and existing `postgres` service.
- Produces: active PG16 service with one ready 5 GB volume at `/var/lib/postgresql/data`.

- [ ] **Step 1: Reconfirm no Postgres volume exists**

Run:

```sh
railway volume list --json | jq '[.volumes[] | select(.serviceName == "postgres")]'
```

Expected: `[]`. If a Postgres volume already exists, stop and inspect its mount, size, state, and creation history rather than creating another.

- [ ] **Step 2: Create the Postgres volume**

Run:

```sh
railway volume \
  --project 865ca83b-9c9e-4e50-b424-e267aa43988f \
  --environment 53d89401-bec2-49a3-ac58-8dc93b29d049 \
  --service d67d4334-d3ca-4ca4-ab4c-099452c88557 \
  add --mount-path /var/lib/postgresql/data --json
```

Expected: a new volume instance for `postgres`, mount `/var/lib/postgresql/data`, default size 5000 MB, status becoming `Ready`.

- [ ] **Step 3: Verify exactly one Postgres volume and retain its ID**

Run:

```sh
railway volume list --json | jq '[.volumes[] | select(.serviceName == "postgres") | {id, name, mountPath, sizeMB, status}]'
```

Expected: exactly one object, `mountPath` `/var/lib/postgresql/data`, `sizeMB` 5000, `status` `Ready`.

- [ ] **Step 4: Deploy the configured PG16 image**

Run:

```sh
railway redeploy \
  --from-source \
  --project 865ca83b-9c9e-4e50-b424-e267aa43988f \
  --environment 53d89401-bec2-49a3-ac58-8dc93b29d049 \
  --service d67d4334-d3ca-4ca4-ab4c-099452c88557 \
  --yes --json
```

Expected: a new deployment ID.

- [ ] **Step 5: Poll until Postgres is active**

Run repeatedly, without streaming raw logs:

```sh
railway deployment list \
  --project 865ca83b-9c9e-4e50-b424-e267aa43988f \
  --environment 53d89401-bec2-49a3-ac58-8dc93b29d049 \
  --service d67d4334-d3ca-4ca4-ab4c-099452c88557 \
  --limit 1 --json | jq '.[0] | {id, status, createdAt, image: .meta.image, mounts: .meta.volumeMounts}'
```

Expected: `status` `SUCCESS`, image `ghcr.io/railwayapp-templates/postgres-ssl:16`, mount `/var/lib/postgresql/data`.

If it fails, fetch only deployment logs and inspect the failure before any retry:

```sh
railway logs \
  --project 865ca83b-9c9e-4e50-b424-e267aa43988f \
  --environment 53d89401-bec2-49a3-ac58-8dc93b29d049 \
  --service d67d4334-d3ca-4ca4-ab4c-099452c88557 \
  --latest --deployment --lines 200
```

Do not paste connection strings into evidence.

- [ ] **Step 6: Verify Postgres remains private**

Run:

```sh
railway domain list \
  --project 865ca83b-9c9e-4e50-b424-e267aa43988f \
  --environment 53d89401-bec2-49a3-ac58-8dc93b29d049 \
  --service d67d4334-d3ca-4ca4-ab4c-099452c88557 --json
```

Expected: no public service or custom domains.

- [ ] **Step 7: Request approval for a Postgres volume-persistence redeploy**

Present:

```text
Action: write one non-secret probe file inside the Postgres PGDATA volume, redeploy Postgres once, verify the exact probe survives, then remove only that probe file.
Impact: brief Postgres restart before registry acceptance begins; no application rows or schemas are changed.
Rollback: redeploy the last healthy PG16 image with the same volume attached; do not detach, replace, or restore the volume.
Resources changed: Postgres deployment and /var/lib/postgresql/data/.sherpa-volume-probe only.
```

Wait for approval.

- [ ] **Step 8: Prove the Postgres volume survives redeploy**

Run:

```sh
railway ssh \
  --project 865ca83b-9c9e-4e50-b424-e267aa43988f \
  --environment 53d89401-bec2-49a3-ac58-8dc93b29d049 \
  --service d67d4334-d3ca-4ca4-ab4c-099452c88557 \
  sh -lc 'umask 077; printf %s sherpa-postgres-volume-probe >"$PGDATA/.sherpa-volume-probe"'
railway redeploy \
  --from-source \
  --project 865ca83b-9c9e-4e50-b424-e267aa43988f \
  --environment 53d89401-bec2-49a3-ac58-8dc93b29d049 \
  --service d67d4334-d3ca-4ca4-ab4c-099452c88557 \
  --yes --json
```

After the replacement deployment is `SUCCESS`, run:

```sh
railway ssh \
  --project 865ca83b-9c9e-4e50-b424-e267aa43988f \
  --environment 53d89401-bec2-49a3-ac58-8dc93b29d049 \
  --service d67d4334-d3ca-4ca4-ab4c-099452c88557 \
  sh -lc 'test "$(cat "$PGDATA/.sherpa-volume-probe")" = sherpa-postgres-volume-probe && rm "$PGDATA/.sherpa-volume-probe"'
```

Expected: the exact marker survives the redeploy, its cleanup succeeds, and Postgres returns healthy on the same volume ID.

- [ ] **Step 9: Record and commit Postgres evidence**

Record the volume ID, both deployment IDs, PG16 image digest if available, mount, active status, persistence-probe result, and absence of public domains. Commit:

```sh
git add docs/deployment/evidence/2026-07-20-railway-staging-acceptance.md
git commit -m $'docs: record Railway Postgres provisioning\n\nCo-Authored-By: Claude <noreply@anthropic.com>'
```

---

### Task 4: Deploy registry and pass initial Phase 2c-iii checks

**Files:**
- Modify: `docs/deployment/evidence/2026-07-20-railway-staging-acceptance.md`

**Interfaces:**
- Consumes: active Postgres, existing registry `/data` volume, two operator-controlled GitHub identities, and two source IPs.
- Produces: healthy registry, two isolated CLI sessions, one published acceptance stack, verified auth controls, host pinning, and persistence across registry redeploy.

- [ ] **Step 1: Verify required registry variable names without displaying values**

Run:

```sh
railway variable list \
  --project 865ca83b-9c9e-4e50-b424-e267aa43988f \
  --environment 53d89401-bec2-49a3-ac58-8dc93b29d049 \
  --service d5d848e6-4655-4823-839f-0a2d1659bcb4 \
  --json | jq -r 'keys[]' | sort
```

Expected names include:

```text
DATABASE_URL
PORT
SHERPA_CONTENT_DIR
SHERPA_EXPORT_INTERVAL
SHERPA_EXPORT_TOKEN
SHERPA_EXPORT_URL
SHERPA_GITHUB_CLIENT_ID
SHERPA_GITHUB_CLIENT_SECRET
SHERPA_PUBLIC_BASE_URL
SHERPA_TRUST_PROXY
SHERPA_WEB_PUBLIC_BASE_URL
```

`SHERPA_REGISTRY_TOKEN` is optional. Stop if any required name is absent.

- [ ] **Step 2: Redeploy registry from GitHub source**

Run:

```sh
railway redeploy \
  --from-source \
  --project 865ca83b-9c9e-4e50-b424-e267aa43988f \
  --environment 53d89401-bec2-49a3-ac58-8dc93b29d049 \
  --service d5d848e6-4655-4823-839f-0a2d1659bcb4 \
  --yes --json
```

Poll with:

```sh
railway deployment list \
  --project 865ca83b-9c9e-4e50-b424-e267aa43988f \
  --environment 53d89401-bec2-49a3-ac58-8dc93b29d049 \
  --service d5d848e6-4655-4823-839f-0a2d1659bcb4 \
  --limit 1 --json | jq '.[0] | {id, status, createdAt, commitHash: .meta.commitHash, configFile: .meta.configFile, volumeMounts: .meta.volumeMounts}'
```

Expected: `SUCCESS`, commit `312479ee55539560936129db21bcec8da7b9827a`, config `/railway.json`, mount `/data`.

- [ ] **Step 3: Verify health and empty public search**

Run:

```sh
LIVE_ROOT="$(mktemp -d /tmp/sherpa-live-gate.XXXXXX)"
chmod 700 "$LIVE_ROOT"
printf '%s\n' "$LIVE_ROOT" >/tmp/sherpa-live-root-current
chmod 600 /tmp/sherpa-live-root-current
cat >"$LIVE_ROOT/context.sh" <<'EOF'
export REG='https://registry-staging-78a5.up.railway.app'
export WEB='https://web-staging-c58d.up.railway.app'
EOF
chmod 600 "$LIVE_ROOT/context.sh"
. "$LIVE_ROOT/context.sh"
curl --fail --silent --show-error "$REG/healthz"
curl --fail --silent --show-error --get --data-urlencode 'q=railway-acceptance' "$REG/v1/search" | jq .
```

Expected: health `ok`; search succeeds and initially contains no acceptance stack.

- [ ] **Step 4: Create two isolated mode-0700 CLI homes and a synthetic clean stack for identity A**

Run from `/Users/timokruth/Projekte/feat`:

```sh
LIVE_ROOT="$(cat /tmp/sherpa-live-root-current)"
. "$LIVE_ROOT/context.sh"
HOME_A="$LIVE_ROOT/home-a"
HOME_B="$LIVE_ROOT/home-b"
mkdir -m 700 "$HOME_A" "$HOME_B"
printf 'GitHub login for identity A: '
IFS= read -r OWNER_A
case "$OWNER_A" in (*[!A-Za-z0-9-]*|'') exit 2;; esac
export STACK_NAME='railway-acceptance-20260720'
export STACK_DIR_A="$HOME_A/profiles/$STACK_NAME"
mkdir -p "$STACK_DIR_A"
cat >"$STACK_DIR_A/stack.yaml" <<EOF
name: $STACK_NAME
owner: "@$OWNER_A"
version: 1
harness: claude-code
harness_min_version: "2.0"
summary: "Synthetic stack for Railway staging acceptance"
tags: [railway, staging, acceptance]
EOF
cat >"$STACK_DIR_A/README.md" <<'EOF'
# Railway staging acceptance stack

Synthetic, non-secret content used only for SherpA staging acceptance.
EOF
cat >"$STACK_DIR_A/CHANGELOG.md" <<'EOF'
# Changelog

## Initial fixture

- Add synthetic acceptance content.
EOF
cat >"$STACK_DIR_A/CLAUDE.md" <<'EOF'
Use concise explanations and never expose credentials.
EOF
git -C "$STACK_DIR_A" init -b local
git -C "$STACK_DIR_A" config user.name 'SherpA Staging Gate'
git -C "$STACK_DIR_A" config user.email 'staging-gate@example.invalid'
git -C "$STACK_DIR_A" add .
git -C "$STACK_DIR_A" commit -m 'test: add staging acceptance stack'
jq -n --arg name "$STACK_NAME" --arg path "$STACK_DIR_A" '{active:$name,profiles:{($name):{name:$name,path:$path,origin:"",harness:"claude-code"}},baselines:{},registries:{},trials:[]}' >"$HOME_A/state.json"
chmod 600 "$HOME_A/state.json"
printf 'export HOME_A=%q\nexport HOME_B=%q\nexport OWNER_A=%q\nexport STACK_NAME=%q\nexport STACK_DIR_A=%q\n' "$HOME_A" "$HOME_B" "$OWNER_A" "$STACK_NAME" "$STACK_DIR_A" >>"$LIVE_ROOT/context.sh"
```

Expected: clean local Git repository on branch `local`, no credentials or executable capabilities, state file mode `0600`.

- [ ] **Step 5: Complete real device login as identity A**

Run:

```sh
LIVE_ROOT="$(cat /tmp/sherpa-live-root-current)"
. "$LIVE_ROOT/context.sh"
cd /Users/timokruth/Projekte/feat
SHERPA_HOME="$HOME_A" SHERPA_REGISTRY_URL="$REG" go run ./cmd/sherpa login
```

The operator completes GitHub authorization for identity A. Do not record the device code. Verify only:

```sh
LIVE_ROOT="$(cat /tmp/sherpa-live-root-current)"
. "$LIVE_ROOT/context.sh"
stat -f '%Lp' "$HOME_A/registry-session.json"
jq -r '.registry_url' "$HOME_A/registry-session.json"
```

Expected: mode `600`; issuer `https://registry-staging-78a5.up.railway.app`. Do not print the access token field.

- [ ] **Step 6: Publish under the matching identity and capture the immutable version**

Run:

```sh
LIVE_ROOT="$(cat /tmp/sherpa-live-root-current)"
. "$LIVE_ROOT/context.sh"
cd /Users/timokruth/Projekte/feat
printf 'yes\n' | SHERPA_HOME="$HOME_A" SHERPA_REGISTRY_URL="$REG" go run ./cmd/sherpa publish --registry "$REG"
PUBLISHED_VERSION="$(awk '/^version:/ {print $2}' "$STACK_DIR_A/stack.yaml")"
printf 'export PUBLISHED_VERSION=%q\n' "$PUBLISHED_VERSION" >>"$LIVE_ROOT/context.sh"
printf 'published version: v%s\n' "$PUBLISHED_VERSION"
curl --fail --silent --show-error "$REG/v1/stacks/$OWNER_A/$STACK_NAME" | jq '{owner,name,trust_tier,repo_url,latest_version}'
```

Expected: HTTP-created publish, `trust_tier` `linked`, registry-hosted `repo_url`, and latest version matching `PUBLISHED_VERSION`.

- [ ] **Step 7: Complete real device login as identity B and make a wrong-owner attempt from an isolated copy**

Run:

```sh
LIVE_ROOT="$(cat /tmp/sherpa-live-root-current)"
. "$LIVE_ROOT/context.sh"
cd /Users/timokruth/Projekte/feat
printf 'GitHub login for identity B: '
IFS= read -r OWNER_B
case "$OWNER_B" in (*[!A-Za-z0-9-]*|'') exit 2;; esac
test "$OWNER_A" != "$OWNER_B"
mkdir -p "$HOME_B/profiles"
cp -R "$STACK_DIR_A" "$HOME_B/profiles/$STACK_NAME"
export STACK_DIR_B="$HOME_B/profiles/$STACK_NAME"
jq -n --arg name "$STACK_NAME" --arg path "$STACK_DIR_B" '{active:$name,profiles:{($name):{name:$name,path:$path,origin:"",harness:"claude-code"}},baselines:{},registries:{},trials:[]}' >"$HOME_B/state.json"
chmod 600 "$HOME_B/state.json"
printf 'export OWNER_B=%q\nexport STACK_DIR_B=%q\n' "$OWNER_B" "$STACK_DIR_B" >>"$LIVE_ROOT/context.sh"
SHERPA_HOME="$HOME_B" SHERPA_REGISTRY_URL="$REG" go run ./cmd/sherpa login
```

The operator authorizes identity B. Then run:

```sh
LIVE_ROOT="$(cat /tmp/sherpa-live-root-current)"
. "$LIVE_ROOT/context.sh"
cd /Users/timokruth/Projekte/feat
set +e
printf 'yes\n' | SHERPA_HOME="$HOME_B" SHERPA_REGISTRY_URL="$REG" go run ./cmd/sherpa publish --registry "$REG"
WRONG_OWNER_EXIT=$?
set -e
test "$WRONG_OWNER_EXIT" -ne 0
railway ssh \
  --project 865ca83b-9c9e-4e50-b424-e267aa43988f \
  --environment 53d89401-bec2-49a3-ac58-8dc93b29d049 \
  --service d5d848e6-4655-4823-839f-0a2d1659bcb4 \
  registry audit
```

Expected: publish reports HTTP 403; audit reports no missing content. Compare stack detail before/after and confirm no new registry version row.

- [ ] **Step 8: Verify body limits, start limits, registered-code poll limits, and unknown-code rejection without printing device codes**

Create a mode-0700 operational-state directory, then keep the registered device code only in one Python process's memory:

```sh
LIVE_ROOT="$(cat /tmp/sherpa-live-root-current)"
. "$LIVE_ROOT/context.sh"
AUTH_DIR="$LIVE_ROOT/auth"
mkdir -m 700 "$AUTH_DIR"
printf 'export AUTH_DIR=%q\n' "$AUTH_DIR" >>"$LIVE_ROOT/context.sh"
# Run after the current source IP's Retry-After window has expired.
python3 - "$REG" <<'PY'
import json
import sys
import urllib.error
import urllib.request

base = sys.argv[1]

def post(path, payload=None):
    data = None if payload is None else json.dumps(payload).encode()
    headers = {} if payload is None else {"Content-Type": "application/json"}
    request = urllib.request.Request(base + path, data=data, method="POST", headers=headers)
    try:
        with urllib.request.urlopen(request, timeout=30) as response:
            return response.status, response.read()
    except urllib.error.HTTPError as error:
        return error.code, error.read()

first_start, body = post("/v1/auth/device/start")
second_start, _ = post("/v1/auth/device/start")
if first_start != 200 or second_start != 429:
    raise SystemExit(f"start statuses: {first_start} then {second_start}")
device_code = json.loads(body)["device_code"]
first_poll, _ = post("/v1/auth/device/poll", {"device_code": device_code})
second_poll, _ = post("/v1/auth/device/poll", {"device_code": device_code})
unknown, _ = post("/v1/auth/device/poll", {"device_code": "random-never-started-device-code"})
oversize, _ = post("/v1/auth/device/poll", {"device_code": "x" * 5000})
print(f"start statuses: {first_start} then {second_start}")
print(f"registered poll statuses: {first_poll} then {second_poll}")
print(f"unknown code status: {unknown}")
print(f"oversize status: {oversize}")
if second_poll != 429 or unknown != 410 or oversize != 413:
    raise SystemExit(1)
PY
```

Expected: second start 429, immediate second registered poll 429, random code 410, oversized body 413. Inspect registry logs by failure class only and confirm locally rejected requests do not emit GitHub-call errors.

- [ ] **Step 9: Verify Railway edge IP identity from two real networks**

Create `$LIVE_ROOT/edge-probe.sh`:

```sh
LIVE_ROOT="$(cat /tmp/sherpa-live-root-current)"
. "$LIVE_ROOT/context.sh"
cat >"$LIVE_ROOT/edge-probe.sh" <<'EOF'
#!/bin/sh
set -eu
REG='https://registry-staging-78a5.up.railway.app'
printf 'first=%s\n' "$(curl --silent --show-error --output /dev/null --write-out '%{http_code}' -X POST "$REG/v1/auth/device/start")"
printf 'second=%s\n' "$(curl --silent --show-error --output /dev/null --write-out '%{http_code}' -X POST "$REG/v1/auth/device/start")"
printf 'forged-real-ip=%s\n' "$(curl --silent --show-error --output /dev/null --write-out '%{http_code}' -X POST -H 'X-Real-IP: 198.51.100.99' "$REG/v1/auth/device/start")"
printf 'prepended-xff=%s\n' "$(curl --silent --show-error --output /dev/null --write-out '%{http_code}' -X POST -H 'X-Forwarded-For: 198.51.100.100' "$REG/v1/auth/device/start")"
EOF
chmod 700 "$LIVE_ROOT/edge-probe.sh"
```

After each network's current rate window has expired, the operator runs the script once from network A and once from genuinely different network B. Expected on each network: first 200, second 429, forged headers 429. Record only network labels and status sequences, never IP addresses unless the operator explicitly approves recording them.

- [ ] **Step 10: Verify search, detail, clone, and host-header pinning**

Run:

```sh
LIVE_ROOT="$(cat /tmp/sherpa-live-root-current)"
. "$LIVE_ROOT/context.sh"
cd /Users/timokruth/Projekte/feat
curl --fail --silent --show-error --get --data-urlencode "q=$STACK_NAME" "$REG/v1/search" | jq '{count: (.results | length), results: [.results[] | {owner,name,trust_tier,repo_url}]}'
curl --fail --silent --show-error -H 'Host: attacker.invalid' -H 'X-Forwarded-Host: attacker.invalid' "$REG/v1/stacks/$OWNER_A/$STACK_NAME" | jq -e --arg host 'registry-staging-78a5.up.railway.app' '.repo_url | contains($host)'
HOME_CLONE="$LIVE_ROOT/home-clone"
mkdir -m 700 "$HOME_CLONE"
printf 'export HOME_CLONE=%q\n' "$HOME_CLONE" >>"$LIVE_ROOT/context.sh"
printf 'a\n' | SHERPA_HOME="$HOME_CLONE" SHERPA_REGISTRY_URL="$REG" go run ./cmd/sherpa clone "@$OWNER_A/$STACK_NAME"
```

Expected: one linked search result, pinned registry host despite spoofed headers, and successful HTTPS clone into an isolated home.

- [ ] **Step 11: Request approval for the registry persistence redeploy**

Present:

```text
Action: redeploy the primary staging registry once from the accepted source to prove Postgres and /data Git persistence.
Impact: brief registry API/login/publish/clone interruption may occur during the /data volume handoff; no data is intentionally changed.
Rollback: if the replacement fails, preserve the failed deployment/logs and reactivate or redeploy the last healthy accepted registry deployment without changing either store.
Resources changed: primary registry deployment only; Postgres and the existing /data volume remain attached and are not replaced.
```

Wait for approval.

- [ ] **Step 12: Redeploy registry and prove persistence**

Run:

```sh
railway redeploy \
  --from-source \
  --project 865ca83b-9c9e-4e50-b424-e267aa43988f \
  --environment 53d89401-bec2-49a3-ac58-8dc93b29d049 \
  --service d5d848e6-4655-4823-839f-0a2d1659bcb4 \
  --yes --json
```

After `SUCCESS`, repeat health, search, detail, and clone with a fresh `SHERPA_HOME`. Expected: the same version and clone URL remain available.

- [ ] **Step 13: Review secret-safe registry logs**

Run filtered summaries, not unfiltered evidence dumps:

```sh
LIVE_ROOT="$(cat /tmp/sherpa-live-root-current)"
. "$LIVE_ROOT/context.sh"
railway logs \
  --project 865ca83b-9c9e-4e50-b424-e267aa43988f \
  --environment 53d89401-bec2-49a3-ac58-8dc93b29d049 \
  --service d5d848e6-4655-4823-839f-0a2d1659bcb4 \
  --since 2h --lines 500 --json >"$LIVE_ROOT/registry-logs.jsonl"
chmod 600 "$LIVE_ROOT/registry-logs.jsonl"
```

Inspect locally for credential-key names and planted private strings without printing matching lines. Record only `no prohibited values detected` or a blocker. Any suspected disclosure fails the gate.

- [ ] **Step 14: Record and commit initial registry evidence**

Update 2c-iii.1 through 2c-iii.8 and preliminary 2c-iii.11. Commit:

```sh
git add docs/deployment/evidence/2026-07-20-railway-staging-acceptance.md
git commit -m $'docs: record initial registry staging gate\n\nCo-Authored-By: Claude <noreply@anthropic.com>'
```

---

### Task 5: Prove off-site export and perform the approved sibling restore

**Files:**
- Modify: `docs/deployment/evidence/2026-07-20-railway-staging-acceptance.md`

**Interfaces:**
- Consumes: passing Task 4, configured collector variables, published acceptance stack.
- Produces: durable collector object, approved temporary recovery resources, restored Postgres and Git, passing audit and smoke tests.

- [ ] **Step 1: Save the existing export interval without displaying it**

Run:

```sh
LIVE_ROOT="$(cat /tmp/sherpa-live-root-current)"
. "$LIVE_ROOT/context.sh"
railway variable list \
  --project 865ca83b-9c9e-4e50-b424-e267aa43988f \
  --environment 53d89401-bec2-49a3-ac58-8dc93b29d049 \
  --service d5d848e6-4655-4823-839f-0a2d1659bcb4 \
  --json | jq -r '.SHERPA_EXPORT_INTERVAL' >"$AUTH_DIR/original-export-interval"
chmod 600 "$AUTH_DIR/original-export-interval"
test -s "$AUTH_DIR/original-export-interval"
```

Expected: a non-empty mode-0600 file; its contents are not printed.

- [ ] **Step 2: Request approval to change the export interval temporarily**

Present:

```text
Action: set the primary registry export interval to 1m, let Railway redeploy it, wait for one bounded collector upload attempt, then restore the exact prior interval and redeploy again.
Impact: two registry configuration deployments and temporarily increased export frequency; the primary Postgres and Git stores are read for export but not replaced.
Rollback: immediately restore the saved original interval from the mode-0600 file and wait for the registry to return healthy, even if the collector attempt fails.
Resources changed: primary registry SHERPA_EXPORT_INTERVAL and its resulting deployments; no collector object is deleted or overwritten intentionally.
```

Wait for approval.

- [ ] **Step 3: Force one scheduled upload**

Run:

```sh
railway variable set SHERPA_EXPORT_INTERVAL=1m \
  --project 865ca83b-9c9e-4e50-b424-e267aa43988f \
  --environment 53d89401-bec2-49a3-ac58-8dc93b29d049 \
  --service d5d848e6-4655-4823-839f-0a2d1659bcb4 --json
```

Wait for the triggered deployment to become `SUCCESS`, then monitor secret-safe status/log summaries until a collector upload succeeds or a bounded failure is established. Record the new collector object ID and timestamp without recording the collector URL.

- [ ] **Step 4: Restore the original export interval immediately after the upload attempt**

Run:

```sh
LIVE_ROOT="$(cat /tmp/sherpa-live-root-current)"
. "$LIVE_ROOT/context.sh"
railway variable set SHERPA_EXPORT_INTERVAL --stdin \
  --project 865ca83b-9c9e-4e50-b424-e267aa43988f \
  --environment 53d89401-bec2-49a3-ac58-8dc93b29d049 \
  --service d5d848e6-4655-4823-839f-0a2d1659bcb4 \
  --json <"$AUTH_DIR/original-export-interval"
```

Expected: another healthy registry deployment with the original interval restored.

- [ ] **Step 5: Prove the collector object is durable and retrievable**

Use the collector's authenticated retrieval interface supplied by the operator. Download the object into `$LIVE_ROOT/recovery/archive.tar.gz`, mode 0600. Verify:

```sh
LIVE_ROOT="$(cat /tmp/sherpa-live-root-current)"
. "$LIVE_ROOT/context.sh"
mkdir -m 700 "$LIVE_ROOT/recovery"
chmod 600 "$LIVE_ROOT/recovery/archive.tar.gz"
tar -tzf "$LIVE_ROOT/recovery/archive.tar.gz" >"$LIVE_ROOT/recovery/archive-members.txt"
grep -Fx 'postgres.dump' "$LIVE_ROOT/recovery/archive-members.txt"
grep -Fx 'manifest.json' "$LIVE_ROOT/recovery/archive-members.txt"
grep -F "repos/$OWNER_A/$STACK_NAME.bundle" "$LIVE_ROOT/recovery/archive-members.txt"
```

Expected: database dump, acceptance-stack bundle, and final manifest are present. If the collector cannot retrieve the object, stop: 2c-iii.9 fails and no recovery resources are created.

- [ ] **Step 6: Request explicit approval for temporary recovery infrastructure**

Present this exact checkpoint:

```text
Action: create temporary staging services postgres-recovery and registry-recovery, one Postgres data volume, and one /data Git volume; restore the retrieved archive; run audit/login/search/detail/clone smoke tests.
Impact: additional Railway usage; no writes to the primary staging stores; recovery services use separate private/public endpoints.
Rollback/cleanup: leave primary services unchanged; after evidence review, request separate approval before deleting recovery services and volumes.
Resources: 2 services, 2 volumes, 1 temporary recovery domain.
```

Wait for approval before continuing.

- [ ] **Step 7: Provision recovery resources with isolated settings**

Using Railway MCP/dashboard after approval:

- create `postgres-recovery` pinned to PG16 with one 5 GB volume at `/var/lib/postgresql/data` and no public domain;
- create `registry-recovery` from `TimoKruth/SherpA` commit `312479ee55539560936129db21bcec8da7b9827a` with `/railway.json`, one 5 GB volume at `/data`, one replica, a distinct temporary domain, and a database reference to `postgres-recovery`;
- copy only the required non-secret configuration shape and use recovery-specific public base URL;
- do not enable the export scheduler against the primary collector during restore;
- do not reuse the primary Git volume or database.

Record every recovery service, deployment, volume, and domain ID.

- [ ] **Step 8: Restore and verify the archive**

Follow `docs/deployment/railway.md:235-270` exactly:

1. extract into the recovery environment;
2. verify every manifest size and SHA-256 before import;
3. restore `postgres.dump` into empty `postgres-recovery` without placing credentials in process arguments;
4. recreate each bare repository from its bundle under the recovery `/data/git` tree;
5. start `registry-recovery`;
6. run `registry audit` inside `registry-recovery`;
7. complete health, login, search, detail, and HTTPS clone smoke tests on the recovery domain.

Expected: manifest valid, audit has no `MISSING`, acceptance version searchable/viewable/cloneable.

- [ ] **Step 9: Complete and commit the registry gate evidence**

Mark 2c-iii.9 through 2c-iii.11 only after collector retrieval, restore, audit, smoke, and log review pass. Commit:

```sh
git add docs/deployment/evidence/2026-07-20-railway-staging-acceptance.md
git commit -m $'docs: record complete registry staging gate\n\nCo-Authored-By: Claude <noreply@anthropic.com>'
```

Do not delete recovery resources yet; cleanup requires a separate explicit approval and retaining them supports Phase 2e restored-data verification.

---

### Task 6: Connect and deploy web, then pass non-disruptive Phase 2d checks

**Files:**
- Modify: `docs/deployment/evidence/2026-07-20-railway-staging-acceptance.md`

**Interfaces:**
- Consumes: completed 2c-iii gate and healthy primary registry.
- Produces: correctly sourced stateless website and passing public/API/header/responsive checks.

- [ ] **Step 1: Connect the web service source**

Run:

```sh
railway service source connect \
  --repo TimoKruth/SherpA \
  --branch main \
  --project 865ca83b-9c9e-4e50-b424-e267aa43988f \
  --environment 53d89401-bec2-49a3-ac58-8dc93b29d049 \
  --service 9e9b6144-b7d4-4e19-8fbc-7ac926991443 --json
```

Expected: source `TimoKruth/SherpA`, branch `main`.

- [ ] **Step 2: Set config path and root directory through Railway MCP/dashboard**

Set:

```text
Config file: /deploy/web/railway.json
Root directory: unset
```

Confirm Railway resolves `deploy/web/Dockerfile`, not the root `Dockerfile`.

- [ ] **Step 3: Verify website variable names and forbidden-resource isolation**

Run:

```sh
railway variable list \
  --project 865ca83b-9c9e-4e50-b424-e267aa43988f \
  --environment 53d89401-bec2-49a3-ac58-8dc93b29d049 \
  --service 9e9b6144-b7d4-4e19-8fbc-7ac926991443 \
  --json | jq -r 'keys[]' | sort
railway volume list --json | jq '[.volumes[] | select(.serviceName == "web")]'
```

Expected variable names:

```text
PORT
SHERPA_REGISTRY_API_URL
SHERPA_REGISTRY_PUBLIC_URL
SHERPA_WEB_PUBLIC_BASE_URL
```

Optional `SHERPA_WEB_UPSTREAM_TIMEOUT` is allowed. Forbidden names include `DATABASE_URL`, `SHERPA_CONTENT_DIR`, GitHub/OAuth variables, `SHERPA_REGISTRY_TOKEN`, cookie keys, and export variables. Expected volume list: `[]`.

- [ ] **Step 4: Deploy web from source**

Run:

```sh
railway redeploy \
  --from-source \
  --project 865ca83b-9c9e-4e50-b424-e267aa43988f \
  --environment 53d89401-bec2-49a3-ac58-8dc93b29d049 \
  --service 9e9b6144-b7d4-4e19-8fbc-7ac926991443 \
  --yes --json
```

Poll until `SUCCESS`. Confirm commit `312479ee55539560936129db21bcec8da7b9827a`, config `/deploy/web/railway.json`, Dockerfile `deploy/web/Dockerfile`, and no volume mounts.

- [ ] **Step 5: Verify health and live PID 1 non-root**

Run:

```sh
LIVE_ROOT="$(cat /tmp/sherpa-live-root-current)"
. "$LIVE_ROOT/context.sh"
curl --fail --silent --show-error "$WEB/healthz"
railway service files \
  --project 865ca83b-9c9e-4e50-b424-e267aa43988f \
  --environment 53d89401-bec2-49a3-ac58-8dc93b29d049 \
  --service 9e9b6144-b7d4-4e19-8fbc-7ac926991443 \
  download /proc/1/status "$LIVE_ROOT/web-proc-1-status" --overwrite --json
awk '/^Uid:/ {print $2}' "$LIVE_ROOT/web-proc-1-status"
```

Expected: health `ok`; real UID is nonzero. If Railway disallows `/proc` download, try `railway ssh ... /web` only if it does not replace the running process; otherwise record the local exact-image PID proof plus Railway's resolved `USER nonroot:nonroot` manifest as a blocker requiring a supported live-inspection method.

- [ ] **Step 6: Verify public pages against direct registry API**

Run:

```sh
LIVE_ROOT="$(cat /tmp/sherpa-live-root-current)"
. "$LIVE_ROOT/context.sh"
curl --fail --silent --show-error "$WEB/"
curl --fail --silent --show-error --get --data-urlencode "q=$STACK_NAME" "$WEB/search"
curl --fail --silent --show-error "$WEB/stacks/$OWNER_A/$STACK_NAME"
curl --fail --silent --show-error "$WEB/stacks/$OWNER_A/$STACK_NAME/v/$PUBLISHED_VERSION"
curl --fail --silent --show-error "$WEB/users/$OWNER_A"
curl --fail --silent --show-error "$REG/v1/stacks/$OWNER_A/$STACK_NAME" | jq '{owner,name,repo_url,latest_version,trust_tier}'
```

Expected: website metadata and commands match direct API data; all clone/try commands use the registry domain.

- [ ] **Step 7: Execute copied clone and try commands from clean homes**

Copy the literal commands rendered by the stack page, then run them with fresh `SHERPA_HOME` directories beneath `$LIVE_ROOT`. For `try`, provide the review input and terminate the launched harness immediately after proving the profile starts; do not allow it to modify real user configuration.

Expected: both commands reach the registry host, never the website host or a request-supplied host.

- [ ] **Step 8: Verify security headers and canonical host pinning**

Run:

```sh
LIVE_ROOT="$(cat /tmp/sherpa-live-root-current)"
. "$LIVE_ROOT/context.sh"
curl --silent --show-error --dump-header "$LIVE_ROOT/web-headers.txt" --output /dev/null "$WEB/stacks/$OWNER_A/$STACK_NAME"
grep -i '^Content-Security-Policy:' "$LIVE_ROOT/web-headers.txt"
grep -i '^X-Content-Type-Options: nosniff' "$LIVE_ROOT/web-headers.txt"
grep -i '^Referrer-Policy: no-referrer' "$LIVE_ROOT/web-headers.txt"
grep -i '^X-Frame-Options: DENY' "$LIVE_ROOT/web-headers.txt"
curl --fail --silent --show-error \
  -H 'Host: attacker.invalid' \
  -H 'X-Forwarded-Host: attacker.invalid' \
  -H 'Forwarded: host=attacker.invalid;proto=http' \
  "$WEB/stacks/$OWNER_A/$STACK_NAME" >"$LIVE_ROOT/spoofed-web-page.html"
! grep -q 'attacker.invalid' "$LIVE_ROOT/spoofed-web-page.html"
grep -q 'registry-staging-78a5.up.railway.app' "$LIVE_ROOT/spoofed-web-page.html"
```

Expected: all headers present; canonical URLs and commands remain pinned.

- [ ] **Step 9: Verify paging and absurd-input bounds**

Request search pages, version pages, and user pages with page 1, next page where present, zero, negative, and very large values. Record only status and bounded-response summaries. Expected: filters persist; stable data has no duplicate/gap; malformed values are bounded and never trigger 5xx or unbounded response size.

- [ ] **Step 10: Run Playwright desktop, mobile, and keyboard QA**

Use Playwright against home, search, stack, version, profile, login, and dashboard entry pages:

- desktop viewport 1440×900;
- mobile viewport 390×844;
- keyboard-only traversal from address load through links, form controls, copy controls, and pagination;
- inspect focus visibility, labels, overflow, overlap, long publisher text, command wrapping/copying, and malicious-looking text rendering;
- capture secret-free screenshots under `$LIVE_ROOT/screenshots`.

Expected: no overflow/overlap, readable focus, accessible labels, correct responsive layout, no executable rendering of publisher content.

- [ ] **Step 11: Configure continuous uptime checks**

Using the operator-approved monitoring provider, configure external HTTPS checks for:

```text
https://web-staging-c58d.up.railway.app/
https://web-staging-c58d.up.railway.app/healthz
```

Also retain registry `/healthz` monitoring if already configured. Record monitor IDs and intervals, not provider credentials.

- [ ] **Step 12: Review website logs for prohibited content**

Download recent logs to a mode-0600 temporary file, inspect locally, and record only the summary. Confirm no query text, command content, upstream body, scan excerpt, headers, session/grant/CSRF values, credentials, or full URL with query appears.

- [ ] **Step 13: Record and commit non-disruptive website evidence**

Update 2d.1 through 2d.6, 2d.9 through 2d.11. Commit:

```sh
git add docs/deployment/evidence/2026-07-20-railway-staging-acceptance.md
git commit -m $'docs: record non-disruptive website staging gate\n\nCo-Authored-By: Claude <noreply@anthropic.com>'
```

---

### Task 7: Perform approved website degradation and isolated redeploy checks

**Files:**
- Modify: `docs/deployment/evidence/2026-07-20-railway-staging-acceptance.md`

**Interfaces:**
- Consumes: passing non-disruptive 2d checks.
- Produces: proof that web stays healthy and degrades safely during registry outage, and that web/registry redeploy independently.

- [ ] **Step 1: Request explicit approval to scale registry to zero temporarily**

Present:

```text
Action: scale the primary staging registry from 1 replica to 0, test website degraded behavior and local CLI continuity, then restore it to 1 replica.
Impact: temporary registry API/login/publish/clone outage; website /healthz remains expected to stay 200 while dynamic pages return bounded 503.
Rollback: scale registry back to 1 in region europe-west4-drams3a and wait for /healthz 200.
Resources changed: primary registry replica count only; no data or volume changes.
```

Wait for approval.

- [ ] **Step 2: Scale registry to zero and verify bounded degradation**

Run:

```sh
LIVE_ROOT="$(cat /tmp/sherpa-live-root-current)"
. "$LIVE_ROOT/context.sh"
cd /Users/timokruth/Projekte/feat
railway service scale europe-west4-drams3a=0 \
  --project 865ca83b-9c9e-4e50-b424-e267aa43988f \
  --environment 53d89401-bec2-49a3-ac58-8dc93b29d049 \
  --service d5d848e6-4655-4823-839f-0a2d1659bcb4 --json
curl --silent --show-error --output /dev/null --write-out '%{http_code}\n' "$WEB/healthz"
curl --silent --show-error --dump-header "$LIVE_ROOT/degraded-headers.txt" --output "$LIVE_ROOT/degraded-body.html" "$WEB/stacks/$OWNER_A/$STACK_NAME"
grep -i '^Retry-After:' "$LIVE_ROOT/degraded-headers.txt"
SHERPA_HOME="$HOME_A" SHERPA_REGISTRY_URL="$REG" go run ./cmd/sherpa status
```

Expected: web health 200, dynamic page 503 with `Retry-After`, branded bounded body, stable web process/restart count, local status/profiles still available.

- [ ] **Step 3: Restore registry and prove immediate recovery**

Run:

```sh
railway service scale europe-west4-drams3a=1 \
  --project 865ca83b-9c9e-4e50-b424-e267aa43988f \
  --environment 53d89401-bec2-49a3-ac58-8dc93b29d049 \
  --service d5d848e6-4655-4823-839f-0a2d1659bcb4 --json
```

Wait for registry `/healthz` 200, then request the same web page without redeploying web. Expected: next request returns 200.

- [ ] **Step 4: Redeploy web only and verify registry continuity**

Run:

```sh
railway redeploy \
  --from-source \
  --project 865ca83b-9c9e-4e50-b424-e267aa43988f \
  --environment 53d89401-bec2-49a3-ac58-8dc93b29d049 \
  --service 9e9b6144-b7d4-4e19-8fbc-7ac926991443 \
  --yes --json
```

During deployment, verify registry health, search, HTTPS clone, login start, and same-owner publish authorization remain available. Do not publish a new version solely for this check; use login-start/status and existing read/clone operations.

- [ ] **Step 5: Request approval for the registry-only redeploy drill**

Present:

```text
Action: redeploy only the primary staging registry from the accepted source while observing website degradation and recovery.
Impact: brief registry interruption may occur during the /data volume handoff; website /healthz must remain 200 and dynamic failures must be bounded.
Rollback: reactivate or redeploy the last healthy accepted registry deployment, leave Postgres and /data unchanged, and wait for registry health plus website recovery.
Resources changed: primary registry deployment only; web deployment and both persistent stores remain unchanged.
```

Wait for approval.

- [ ] **Step 6: Redeploy registry only and verify web degrade/recover cycle**

Run:

```sh
railway redeploy \
  --from-source \
  --project 865ca83b-9c9e-4e50-b424-e267aa43988f \
  --environment 53d89401-bec2-49a3-ac58-8dc93b29d049 \
  --service d5d848e6-4655-4823-839f-0a2d1659bcb4 \
  --yes --json
```

During the volume handoff, verify web `/healthz` stays 200 and any transient dynamic failure is bounded; after registry health returns, verify the next web request recovers without web redeploy.

- [ ] **Step 7: Record and commit the complete 2d gate**

Mark 2d.7 and 2d.8 passed only after recovery. Commit:

```sh
git add docs/deployment/evidence/2026-07-20-railway-staging-acceptance.md
git commit -m $'docs: record complete website staging gate\n\nCo-Authored-By: Claude <noreply@anthropic.com>'
```

---

### Task 8: Pass non-disruptive Phase 2e OAuth, authorization, follow, update, and trial checks

**Files:**
- Modify: `docs/deployment/evidence/2026-07-20-railway-staging-acceptance.md`

**Interfaces:**
- Consumes: passing 2c-iii and 2d gates, identity A CLI home, published stack, live web service.
- Produces: verified browser session security, role boundaries, follow/update lifecycle, offline follow queue setup, and private trial behavior.

- [ ] **Step 1: Confirm both prior gates identify the same application candidate and domains**

Compare deployment metadata and evidence rows. Expected: registry and web both run `312479ee55539560936129db21bcec8da7b9827a`, with the configured staging domains.

- [ ] **Step 2: Complete website GitHub sign-in as identity A with Playwright**

Navigate to `https://web-staging-c58d.up.railway.app/login`, follow redirects, and let the operator authorize identity A on GitHub. Expected redirect chain:

```text
website /login
registry /v1/auth/web/start
GitHub authorization
registry /v1/auth/web/callback
website /auth/callback
website /dashboard
```

Confirm the registry callback and final dashboard origins are pinned. Before completing the GitHub callback, inspect the browser context while it is on the GitHub authorization page and confirm `__Host-sherpa_login` is host-only with `Secure`, `HttpOnly`, `SameSite=Lax`, and `Path=/`; the callback intentionally clears this short-lived cookie. Do not record OAuth query values or cookie values.

- [ ] **Step 3: Verify cookie attributes, cleared grant query, and replay/tamper rejection**

After the final dashboard redirect, use Playwright storage inspection to confirm `__Host-sherpa_session` and `__Host-sherpa_csrf` are host-only with `Secure`, `HttpOnly`, `SameSite=Lax`, and `Path=/`, and confirm `__Host-sherpa_login` is absent because the callback cleared it.

Confirm browser history/final URL contains no grant. Retain the already-used callback URL only in browser memory, re-submit it, replay the grant without the original handoff nonce, and submit a state value modified by one character. Expected: all rejected, no new session, no secret values recorded or written to the evidence file.

- [ ] **Step 4: Verify web, CLI, and admin authorization boundaries**

With the browser web session, follow the acceptance stack and record its current version. Use `browser_run_code_unsafe` with this exact code so the token remains only in Playwright process memory and tool output contains only the HTTP status:

```javascript
async (page) => {
  const cookies = await page.context().cookies('https://web-staging-c58d.up.railway.app');
  const session = cookies.find((cookie) => cookie.name === '__Host-sherpa_session');
  if (!session) throw new Error('web session cookie missing');
  const response = await page.request.post(
    'https://registry-staging-78a5.up.railway.app/v1/stacks/probe-owner/probe-stack/versions',
    {
      data: '',
      headers: { Authorization: `Bearer ${session.value}` },
    },
  );
  return { status: response.status() };
}
```

The fixed probe path is intentionally non-production data. The publish handler authorizes the session purpose before validating owner/name or reading multipart content, so an intentionally empty body is sufficient: expected status is exactly 403. A 400 would mean the request reached bundle parsing and therefore failed the web-purpose authorization boundary.

Publish the next immutable version with identity A's CLI session:

```sh
LIVE_ROOT="$(cat /tmp/sherpa-live-root-current)"
. "$LIVE_ROOT/context.sh"
cd /Users/timokruth/Projekte/feat
CURRENT_VERSION="$PUBLISHED_VERSION"
printf '\n## Phase 2e update\n\n- Add pending-update acceptance marker.\n' >>"$STACK_DIR_A/CHANGELOG.md"
git -C "$STACK_DIR_A" add CHANGELOG.md
git -C "$STACK_DIR_A" commit -m 'test: add pending update marker'
printf 'yes\n' | SHERPA_HOME="$HOME_A" SHERPA_REGISTRY_URL="$REG" go run ./cmd/sherpa publish --registry "$REG"
UPDATE_VERSION="$(awk '/^version:/ {print $2}' "$STACK_DIR_A/stack.yaml")"
test "$UPDATE_VERSION" -gt "$CURRENT_VERSION"
printf 'export CURRENT_VERSION=%q\nexport UPDATE_VERSION=%q\n' "$CURRENT_VERSION" "$UPDATE_VERSION" >>"$LIVE_ROOT/context.sh"
```

If `SHERPA_REGISTRY_TOKEN` exists, run this optional boundary probe:

```sh
LIVE_ROOT="$(cat /tmp/sherpa-live-root-current)"
. "$LIVE_ROOT/context.sh"
railway run \
  --project 865ca83b-9c9e-4e50-b424-e267aa43988f \
  --environment 53d89401-bec2-49a3-ac58-8dc93b29d049 \
  --service d5d848e6-4655-4823-839f-0a2d1659bcb4 \
  --no-local \
  python3 - "$REG" <<'PY'
import os
import sys
import urllib.error
import urllib.request

token = os.environ.get("SHERPA_REGISTRY_TOKEN", "")
base = sys.argv[1]
if not token:
    print("admin token absent; boundary not applicable")
    raise SystemExit(0)
for path in ("/v1/me", "/v1/me/follows"):
    request = urllib.request.Request(base + path, headers={"Authorization": f"Bearer {token}"})
    try:
        with urllib.request.urlopen(request, timeout=30) as response:
            status = response.status
    except urllib.error.HTTPError as error:
        status = error.code
    print(path, status)
    if status != 401:
        raise SystemExit(1)
PY
```

Expected: 401 for both personal endpoints, or an explicit non-applicable result when no admin token is configured. Record status/result summaries only.

- [ ] **Step 5: Prove pending-update lifecycle and read non-destructiveness**

Run repeated CLI and dashboard reads:

```sh
LIVE_ROOT="$(cat /tmp/sherpa-live-root-current)"
. "$LIVE_ROOT/context.sh"
cd /Users/timokruth/Projekte/feat
SHERPA_HOME="$HOME_A" SHERPA_REGISTRY_URL="$REG" go run ./cmd/sherpa updates
SHERPA_HOME="$HOME_A" SHERPA_REGISTRY_URL="$REG" go run ./cmd/sherpa updates
```

Load dashboard repeatedly. Expected: `UPDATE_VERSION` remains pending until explicit review; GETs never clear it.

- [ ] **Step 6: Prove explicit review and monotonic seen state**

Clear the first pending update explicitly:

```sh
LIVE_ROOT="$(cat /tmp/sherpa-live-root-current)"
. "$LIVE_ROOT/context.sh"
cd /Users/timokruth/Projekte/feat
SHERPA_HOME="$HOME_A" SHERPA_REGISTRY_URL="$REG" go run ./cmd/sherpa updates --seen "@$OWNER_A/$STACK_NAME@v$UPDATE_VERSION"
printf '\n## Phase 2e web review\n\n- Add web mark-reviewed acceptance marker.\n' >>"$STACK_DIR_A/CHANGELOG.md"
git -C "$STACK_DIR_A" add CHANGELOG.md
git -C "$STACK_DIR_A" commit -m 'test: add web review marker'
printf 'yes\n' | SHERPA_HOME="$HOME_A" SHERPA_REGISTRY_URL="$REG" go run ./cmd/sherpa publish --registry "$REG"
LATEST_VERSION="$(awk '/^version:/ {print $2}' "$STACK_DIR_A/stack.yaml")"
test "$LATEST_VERSION" -gt "$UPDATE_VERSION"
printf 'export LATEST_VERSION=%q\n' "$LATEST_VERSION" >>"$LIVE_ROOT/context.sh"
```

Use web `Mark reviewed` for `LATEST_VERSION`; expected pending clears without activating a profile. Then send the older version and reread updates:

```sh
LIVE_ROOT="$(cat /tmp/sherpa-live-root-current)"
. "$LIVE_ROOT/context.sh"
cd /Users/timokruth/Projekte/feat
SHERPA_HOME="$HOME_A" SHERPA_REGISTRY_URL="$REG" go run ./cmd/sherpa updates --seen "@$OWNER_A/$STACK_NAME@v$CURRENT_VERSION"
SHERPA_HOME="$HOME_A" SHERPA_REGISTRY_URL="$REG" go run ./cmd/sherpa updates
```

Expected: seen state does not move backward.

- [ ] **Step 7: Prove online clone auto-follow**

Run:

```sh
LIVE_ROOT="$(cat /tmp/sherpa-live-root-current)"
. "$LIVE_ROOT/context.sh"
cd /Users/timokruth/Projekte/feat
HOME_AUTO="$LIVE_ROOT/home-auto-follow"
mkdir -m 700 "$HOME_AUTO"
SHERPA_HOME="$HOME_AUTO" SHERPA_REGISTRY_URL="$REG" go run ./cmd/sherpa login
printf 'a\n' | SHERPA_HOME="$HOME_AUTO" SHERPA_REGISTRY_URL="$REG" go run ./cmd/sherpa clone "@$OWNER_A/$STACK_NAME"
SHERPA_HOME="$HOME_AUTO" SHERPA_REGISTRY_URL="$REG" go run ./cmd/sherpa updates >/dev/null
jq -e --arg reg "$REG" '.registries[$reg].pending_follows | length == 0' "$HOME_AUTO/state.json"
```

The operator authorizes identity A. Expected: clone succeeds and server-side follow exists without an explicit `follow` command.

- [ ] **Step 8: Prepare the offline follow queue case**

Create another fresh home with an identity A session and no existing clone/follow:

```sh
LIVE_ROOT="$(cat /tmp/sherpa-live-root-current)"
. "$LIVE_ROOT/context.sh"
cd /Users/timokruth/Projekte/feat
HOME_OFFLINE="$LIVE_ROOT/home-offline-follow"
mkdir -m 700 "$HOME_OFFLINE"
SHERPA_HOME="$HOME_OFFLINE" SHERPA_REGISTRY_URL="$REG" go run ./cmd/sherpa login
printf 'export HOME_OFFLINE=%q\n' "$HOME_OFFLINE" >>"$LIVE_ROOT/context.sh"
```

The operator authorizes identity A. Do not clone yet; Task 9 coordinates the registry outage between clone transport and auto-follow.

- [ ] **Step 9: Prove private local trial and verdict-only sharing**

Using an installed acceptance profile:

```sh
LIVE_ROOT="$(cat /tmp/sherpa-live-root-current)"
. "$LIVE_ROOT/context.sh"
cd /Users/timokruth/Projekte/feat
SHERPA_HOME="$HOME_CLONE" SHERPA_REGISTRY_URL="$REG" go run ./cmd/sherpa login
SHERPA_HOME="$HOME_CLONE" SHERPA_REGISTRY_URL="$REG" go run ./cmd/sherpa trial record "$STACK_NAME" --verdict keep-with-notes --notes 'private acceptance note; must never leave this file'
SHERPA_HOME="$HOME_CLONE" go run ./cmd/sherpa trial list >/dev/null
stat -f '%Lp' "$HOME_CLONE/state.json"
TRIAL_ID="$(jq -r '.trials[-1].id' "$HOME_CLONE/state.json")"
test -n "$TRIAL_ID"
printf 'export TRIAL_ID=%q\n' "$TRIAL_ID" >>"$LIVE_ROOT/context.sh"
```

The operator authorizes identity A for `HOME_CLONE`. Share without printing notes:

```sh
LIVE_ROOT="$(cat /tmp/sherpa-live-root-current)"
. "$LIVE_ROOT/context.sh"
cd /Users/timokruth/Projekte/feat
SHERPA_HOME="$HOME_CLONE" SHERPA_REGISTRY_URL="$REG" go run ./cmd/sherpa trial share "$TRIAL_ID"
```

Expected: state mode 600; network payload contains only immutable registry/owner/stack/version and verdict; no notes or local paths appear in logs, restored DB, or evidence.

- [ ] **Step 10: Verify edge origin, CSRF, return URL, and grant defenses**

Use Playwright and direct requests with non-secret test values to verify:

- spoofed Host and forwarded headers do not change origins;
- foreign or duplicate Origin is rejected;
- missing, foreign, or duplicate CSRF is rejected;
- arbitrary return URLs are rejected;
- tampered OAuth state is rejected;
- repeated grants are rejected.

Record only status classes and pass/fail.

- [ ] **Step 11: Record and commit non-disruptive 2e evidence**

Update 2e.1 through 2e.9 except the offline half of 2e.7. Commit:

```sh
git add docs/deployment/evidence/2026-07-20-railway-staging-acceptance.md
git commit -m $'docs: record non-disruptive Phase 2e staging gate\n\nCo-Authored-By: Claude <noreply@anthropic.com>'
```

---

### Task 9: Perform approved Phase 2e outage, restored-data, and rollback drills

**Files:**
- Modify: `docs/deployment/evidence/2026-07-20-railway-staging-acceptance.md`

**Interfaces:**
- Consumes: passing Task 8 and retained recovery resources from Task 5.
- Produces: completed offline queue, outage/logout, restored social-data, separate web/registry rollback evidence, approved recovery cleanup, and final 34/34 gate status.

- [ ] **Step 1: Request approval for the Phase 2e outage**

Present:

```text
Action: scale registry to 0, test authenticated web degradation/logout and offline clone follow queue, then restore registry to 1 and sync once.
Impact: temporary staging registry outage; website health remains available; authenticated dynamic pages degrade.
Rollback: restore registry to 1 replica in europe-west4-drams3a and wait for health 200.
Resources changed: registry replica count only; no data deletion.
```

Wait for approval.

- [ ] **Step 2: Execute outage, logout, and offline queue checks**

Create a temporary Git wrapper that pauses after the repository transport succeeds but before `sherpa clone` performs auto-follow:

```sh
LIVE_ROOT="$(cat /tmp/sherpa-live-root-current)"
. "$LIVE_ROOT/context.sh"
WRAP_DIR="$LIVE_ROOT/git-wrap"
mkdir -m 700 "$WRAP_DIR"
REAL_GIT="$(command -v git)"
cat >"$WRAP_DIR/git" <<'EOF'
#!/bin/sh
set -eu
if [ "${1:-}" = clone ] && [ ! -e "$CLONE_READY" ]; then
  "$REAL_GIT" "$@"
  : >"$CLONE_READY"
  while [ ! -e "$CLONE_CONTINUE" ]; do
    sleep 0.2
  done
  exit 0
fi
exec "$REAL_GIT" "$@"
EOF
chmod 700 "$WRAP_DIR/git"
printf 'export WRAP_DIR=%q\nexport REAL_GIT=%q\nexport CLONE_READY=%q\nexport CLONE_CONTINUE=%q\n' \
  "$WRAP_DIR" "$REAL_GIT" "$LIVE_ROOT/clone-ready" "$LIVE_ROOT/clone-continue" >>"$LIVE_ROOT/context.sh"
```

Launch this command with the Bash tool's `run_in_background: true` and retain the returned task ID:

```sh
LIVE_ROOT="$(cat /tmp/sherpa-live-root-current)"
. "$LIVE_ROOT/context.sh"
cd /Users/timokruth/Projekte/feat
PATH="$WRAP_DIR:$PATH" REAL_GIT="$REAL_GIT" CLONE_READY="$CLONE_READY" CLONE_CONTINUE="$CLONE_CONTINUE" \
  SHERPA_HOME="$HOME_OFFLINE" SHERPA_REGISTRY_URL="$REG" \
  go run ./cmd/sherpa clone "@$OWNER_A/$STACK_NAME"
```

Use a one-shot background watcher for `CLONE_READY`. Once the marker exists, the public metadata lookup and Git clone have completed but auto-follow has not run. Scale registry to zero:

```sh
railway service scale europe-west4-drams3a=0 \
  --project 865ca83b-9c9e-4e50-b424-e267aa43988f \
  --environment 53d89401-bec2-49a3-ac58-8dc93b29d049 \
  --service d5d848e6-4655-4823-839f-0a2d1659bcb4 --json
```

Confirm registry health is unavailable. While it is down:

- confirm web `/healthz` returns 200;
- confirm authenticated pages degrade without exposing or clearing the still-valid session;
- confirm logout clears all website cookies even though revocation cannot reach registry;
- run local `status` and confirm installed profiles remain available;
- create the continuation marker:

```sh
LIVE_ROOT="$(cat /tmp/sherpa-live-root-current)"
. "$LIVE_ROOT/context.sh"
: >"$CLONE_CONTINUE"
```

Wait for the background clone task to finish. Expected: installation succeeds with an auto-follow warning. Verify exactly one queued follow:

```sh
LIVE_ROOT="$(cat /tmp/sherpa-live-root-current)"
. "$LIVE_ROOT/context.sh"
jq -e --arg reg "$REG" --arg ref "@$OWNER_A/$STACK_NAME" '.registries[$reg].pending_follows == [$ref]' "$HOME_OFFLINE/state.json"
```

Restore registry:

```sh
railway service scale europe-west4-drams3a=1 \
  --project 865ca83b-9c9e-4e50-b424-e267aa43988f \
  --environment 53d89401-bec2-49a3-ac58-8dc93b29d049 \
  --service d5d848e6-4655-4823-839f-0a2d1659bcb4 --json
```

After `/healthz` returns 200, run one online social command and verify the queue becomes empty:

```sh
LIVE_ROOT="$(cat /tmp/sherpa-live-root-current)"
. "$LIVE_ROOT/context.sh"
cd /Users/timokruth/Projekte/feat
SHERPA_HOME="$HOME_OFFLINE" SHERPA_REGISTRY_URL="$REG" go run ./cmd/sherpa updates >/dev/null
jq -e --arg reg "$REG" '(.registries[$reg].pending_follows // []) | length == 0' "$HOME_OFFLINE/state.json"
```

Expected: the queued follow syncs once and no duplicate follow is created.

- [ ] **Step 3: Request approval and verify restored social/session data**

Because Task 8 created follows, events, verdict feedback, grants, and sessions after the Task 5 archive, present this checkpoint:

```text
Action: temporarily set the primary registry export interval to 1m, retrieve the new completed off-site archive, restore it into new postgres-recovery-2 and registry-recovery-2 services with separate 5 GB Postgres and /data volumes, then run data-shape checks and registry audit.
Impact: two primary registry configuration deployments, one additional collector object, and temporary Railway usage for 2 services, 2 volumes, and 1 recovery domain; primary stores are read but never replaced.
Rollback/cleanup: restore the exact original export interval immediately after the bounded upload attempt; leave all primary services/stores unchanged; retain both recovery generations until a later explicit cleanup approval.
Resources changed: primary SHERPA_EXPORT_INTERVAL and resulting deployments; new postgres-recovery-2, registry-recovery-2, 2 volumes, and 1 temporary domain.
```

Wait for approval. Save the current interval, force one bounded upload, and restore the interval with:

```sh
LIVE_ROOT="$(cat /tmp/sherpa-live-root-current)"
. "$LIVE_ROOT/context.sh"
railway variable list \
  --project 865ca83b-9c9e-4e50-b424-e267aa43988f \
  --environment 53d89401-bec2-49a3-ac58-8dc93b29d049 \
  --service d5d848e6-4655-4823-839f-0a2d1659bcb4 \
  --json | jq -r '.SHERPA_EXPORT_INTERVAL' >"$AUTH_DIR/original-export-interval-2"
chmod 600 "$AUTH_DIR/original-export-interval-2"
test -s "$AUTH_DIR/original-export-interval-2"
railway variable set SHERPA_EXPORT_INTERVAL=1m \
  --project 865ca83b-9c9e-4e50-b424-e267aa43988f \
  --environment 53d89401-bec2-49a3-ac58-8dc93b29d049 \
  --service d5d848e6-4655-4823-839f-0a2d1659bcb4 --json
```

After one completed collector upload or a bounded failure, restore immediately:

```sh
LIVE_ROOT="$(cat /tmp/sherpa-live-root-current)"
. "$LIVE_ROOT/context.sh"
railway variable set SHERPA_EXPORT_INTERVAL --stdin \
  --project 865ca83b-9c9e-4e50-b424-e267aa43988f \
  --environment 53d89401-bec2-49a3-ac58-8dc93b29d049 \
  --service d5d848e6-4655-4823-839f-0a2d1659bcb4 \
  --json <"$AUTH_DIR/original-export-interval-2"
```

If the upload did not complete, fail 2e.11 and do not create recovery-2 resources. Otherwise retrieve the exact new object through the collector's authenticated interface into `$LIVE_ROOT/recovery/archive-2.tar.gz`, mode 0600. Verify `postgres.dump`, final `manifest.json`, and `repos/$OWNER_A/$STACK_NAME.bundle`; verify every manifest size and SHA-256. Create `postgres-recovery-2` pinned to PG16 with a fresh 5 GB `/var/lib/postgresql/data` volume and no public domain. Create `registry-recovery-2` from the accepted commit with `/railway.json`, a fresh 5 GB `/data` volume, a distinct temporary domain, a database reference only to `postgres-recovery-2`, recovery-specific public base URL, and export scheduling disabled. Restore the dump with libpq environment variables rather than credentials in argv, reconstruct each bare Git repository with `git clone --mirror`, start the recovery registry, and run its health, data-shape, search/detail/clone, and `registry audit` checks. Never overwrite the Task 5 recovery stores.

Expected restored data:

- follows and seen versions;
- update events;
- verdict-only trial feedback;
- only grant/session hashes, never raw grants/tokens;
- no private trial notes.

Run `registry audit`; any `MISSING` fails the gate.

- [ ] **Step 4: Request approval for website-only rollback drill**

Present:

```text
Action: deploy the previous 2e-aware website commit 4b4e86f to web, verify public discovery and registry operations, then redeploy current commit 312479e.
Impact: temporary website version change; registry remains online and unchanged.
Rollback: redeploy web from current GitHub source and wait for /healthz 200.
Resources changed: web deployment only.
```

Wait for approval. Use Railway MCP/dashboard's deploy-commit function so the service remains source-connected. Verify public pages, registry health/search/clone/login/publish authorization, then restore current web and verify recovery.

- [ ] **Step 5: Request approval for registry-only 2e-aware rollback drill**

Present:

```text
Action: deploy the previous 2e-aware registry commit 4b4e86f, verify health, public discovery, clone, login, follow, and web-session publish rejection, then redeploy current commit 312479e.
Impact: brief registry downtime from the /data volume handoff; no schema or data rollback.
Rollback: redeploy registry from current GitHub source and wait for /healthz 200.
Resources changed: registry deployment only; Postgres and Git volumes are preserved.
```

Wait for approval. Do not roll back below the 2e boundary. Before changing the deployment, complete a fresh website sign-in as identity A on the current registry and keep that Playwright browser context open; do not export its token. Deploy the old 2e-aware registry image, then run `browser_run_code_unsafe` with:

```javascript
async (page) => {
  const cookies = await page.context().cookies('https://web-staging-c58d.up.railway.app');
  const session = cookies.find((cookie) => cookie.name === '__Host-sherpa_session');
  if (!session) throw new Error('web session cookie missing');
  const response = await page.request.post(
    'https://registry-staging-78a5.up.railway.app/v1/stacks/probe-owner/probe-stack/versions',
    { data: '', headers: { Authorization: `Bearer ${session.value}` } },
  );
  return { status: response.status() };
}
```

Require exactly 403. Restore the current registry and rerun audit/search/detail/clone.

- [ ] **Step 6: Perform final secret-safe log review**

Inspect primary registry, web, recovery registry, and collector logs locally. Confirm no database URL, admin/export/GitHub/session token, authorization header, OAuth code/verifier, grant, CSRF value, trial note, request body, or internal error detail appears. Record only the aggregate result.

- [ ] **Step 7: Request approval before deleting temporary recovery resources**

Present the exact recovery service and volume IDs and state that their deletion removes the temporary restored copies. Wait for approval. If approved, delete recovery services/volumes with exact IDs and `--yes`; if not approved, retain them and record ownership/cost responsibility.

- [ ] **Step 8: Finalize the evidence document**

Set UTC end time and final status. Mark complete only if:

- Postgres, registry, and web are healthy;
- 2c-iii is 11/11;
- 2d is 11/11;
- 2e is 12/12;
- off-site restore and audit pass;
- website-only and registry-only rollbacks pass;
- evidence has no secrets;
- production remains empty.

If any check is incomplete, retain `Automated gates green; live Railway acceptance pending` and list each exact blocker.

- [ ] **Step 9: Run final verification**

Run:

```sh
git diff --check
git status --short --branch
railway service status --project 865ca83b-9c9e-4e50-b424-e267aa43988f --environment 53d89401-bec2-49a3-ac58-8dc93b29d049 --json
railway status --json | jq '[.environments.edges[].node | select(.name == "production") | .serviceInstances.edges] | add | length'
```

Expected: no diff errors; only the three `.DS_Store` files untracked; all staging services healthy; production service count 0.

- [ ] **Step 10: Commit final acceptance evidence**

Run:

```sh
git add docs/deployment/evidence/2026-07-20-railway-staging-acceptance.md
git commit -m $'docs: record Railway staging acceptance result\n\nCo-Authored-By: Claude <noreply@anthropic.com>'
```

Expected: final evidence commit accurately states full pass or exact remaining blockers. Do not push documentation-only commits unless the operator separately asks to publish them.
