# Railway Staging Acceptance Evidence — 2026-07-20

## Metadata

| Field | Value |
|---|---|
| Project | `powerful-rebirth` |
| Environment | `staging` |
| Application candidate | `b2e91ac52abc3b9d24907d2f43d262b22a8fb0d6` |
| Initial candidate | `312479ee55539560936129db21bcec8da7b9827a`; superseded after the live Railway edge gate reproduced a client-IP rate-limit bypass |
| Registry domain | `https://registry-staging-78a5.up.railway.app` |
| Website domain | `https://web-staging-c58d.up.railway.app` |
| Operator | Timo Kruth |
| UTC start | `2026-07-20T04:29:54Z` |
| UTC end | Not run |
| Final status | Blocked at Phase 2c-iii.9: configured off-site collector returned HTTP 404; original export interval restored |

## Tooling

| Check | Result | Non-secret evidence |
|---|---|---|
| Railway CLI and account | Pass | Railway CLI `5.27.0`; account Timo Kruth |
| Railway project/environment linkage | Pass | Project `865ca83b-9c9e-4e50-b424-e267aa43988f`; staging `53d89401-bec2-49a3-ac58-8dc93b29d049` |
| GitHub CLI | Pass | Authenticated as `TimoKruth` with repository access |
| Go/Git/Docker/curl/jq/Python | Pass | Go `1.26.3`; Git `2.54.0`; Docker reachable; curl `8.7.1`; jq `1.7.1`; Python `3.9.6` |
| Railway agent integration | Pass with session limitation | Skills revision `2405547` current; MCP configured; current session uses deterministic CLI fallback |

## Local preflight

| Check | Result | Non-secret evidence |
|---|---|---|
| Candidate and working tree | Pass | Exact candidate `312479ee55539560936129db21bcec8da7b9827a` extracted with `git archive` into a mode-0700 directory without `.git`; committed drift from `origin/main` is documentation-only; only the three preserved `.DS_Store` files are untracked |
| gofmt | Pass | Check-only `gofmt -l .` emitted no paths |
| Go tests | Pass | `go test -count=1 ./...` passed on the exact candidate. The first run saw one transient Docker host-port connection reset while opening ephemeral PostgreSQL; the isolated test and full-suite rerun passed, and no application defect reproduced |
| Race tests | Pass | `go test -race ./internal/registry/... ./internal/web/... ./internal/cli ./internal/state ./internal/integration` passed |
| go vet | Pass | `go vet ./...` exited 0 |
| Go builds | Pass | `go build ./cmd/...` exited 0 |
| Registry image smoke | Pass | `sherpa-registry:test`, image `sha256:cb83070a3e3949448fe68a4b9b1141f60634197d0e43e5a19cd1a10a5a895694`; PG16, health, non-root, writable data, Git, `pg_dump`, and final-image checks passed |
| Website image smoke | Pass | `sherpa-web:test`, image `sha256:0d7f756148d425815d36184d9604faf018aec822ab32c3ef9c474e27d5e5ec7e`; public-page, command, header, non-root, outage/recovery, and final-image checks passed |
| Config syntax and diff check | Pass | Shell syntax, both Railway JSON files, and `git diff --check` passed |
| Live-gate candidate correction | Pass | Two immediate device starts on the initial Railway deployment both returned 200. A focused regression test failed first, candidate `b2e91ac52abc3b9d24907d2f43d262b22a8fb0d6` changed trusted-proxy extraction to prefer Railway's sanitized `X-Real-IP`, and targeted tests, full Go tests, race tests, vet, builds, diff check, and registry image smoke all passed. One ephemeral PostgreSQL host-port reset recurred during the first full run; isolated and full reruns passed |

## Disruptive-action approvals

| UTC time | Action | Expected impact | Rollback/cleanup | Approval |
|---|---|---|---|---|
| `2026-07-20T04:48Z` | PostgreSQL volume-persistence redeploy | One brief PostgreSQL restart; no application schemas or rows changed | Same PG16 image and volume retained; only `/var/lib/postgresql/data/.sherpa-volume-probe` created and removed | Explicit interactive approval received immediately before the drill |
| `2026-07-20T05:55Z` | Push corrected client-IP candidate to `origin/main` | GitHub publication and automatic replacement registry deployment | Revert only the focused application commit if verification failed; preserve deployment evidence | Explicit interactive approval received before push |
| `2026-07-20T06:07Z` | Primary registry persistence redeploy | Brief registry API/login/publish/clone interruption | Corrected candidate and both existing stores retained; prior healthy deployment preserved in history | Explicit interactive approval received immediately before the drill |
| `2026-07-20T06:20Z` | Temporarily set registry export interval to 1 minute | Two configuration deployments and bounded increased export frequency | Exact prior interval restored from a mode-0600 file after the bounded attempt; primary stores unchanged | Explicit interactive approval received immediately before the drill |

## Postgres provisioning

| Field | Value |
|---|---|
| Service ID | `d67d4334-d3ca-4ca4-ab4c-099452c88557` |
| Volume ID | `1012e6ae-def9-4c6e-9ecc-aa3ccae71a3c` (`postgres-volume`, 5000 MB, `Ready`) |
| Deployment ID | Initial `8ab1f3d0-c6d5-4148-a28e-f2b9b516235f`; persistence redeploy `116d4229-50d1-4bda-8c58-0ba1ee9c41a3`; both `SUCCESS` |
| PG16 active | Pass — `ghcr.io/railwayapp-templates/postgres-ssl:16` |
| Required mount | Pass — `/var/lib/postgresql/data`; exact non-secret probe survived redeploy on the same volume and was removed |
| Public TCP absent | Pass — public service/custom domain list empty |

## Phase 2c-iii registry gate

| Step | Result | UTC start/end | Non-secret evidence | IDs/notes |
|---|---|---|---|---|
| 2c-iii.1 empty stores deployment | Pass | `2026-07-20T04:52Z`–`04:55Z` | Initial public acceptance search returned an empty `stacks` array before publication | Deployment `acf40c8c-10d6-431f-8b84-ad01550ec5e7`; candidate `312479e`; config `/railway.json`; mount `/data` |
| 2c-iii.2 health and search | Pass | `2026-07-20T04:55Z`–`06:08Z` | `/healthz` returned `ok`; empty and populated search responses matched expected states | Corrected deployment `53307516-4f94-4297-a493-e9038125d192` |
| 2c-iii.3 device login and issuer | Pass | `2026-07-20T04:56Z`–`05:18Z` | Two isolated mode-0600 CLI sessions completed real GitHub device authorization; issuer exactly matched the registry public base URL | Identity labels A and B retained; no device codes or tokens recorded |
| 2c-iii.4 same-owner linked publish | Pass | `2026-07-20T05:07Z` | Identity A published immutable version `v2`; stack detail reported trust tier `linked` and a registry-hosted HTTPS Git URL | `@TimoKruth/railway-acceptance-20260720` |
| 2c-iii.5 wrong-owner rejection and audit | Pass | `2026-07-20T05:18Z`–`05:20Z` | Identity B received HTTP 403; version count remained one; runtime-user audit reported `missing=0 extra=0` | Railway SSH sessions run as root, so audit was correctly executed as registry UID/GID 998 rather than weakening Git safe-directory checks |
| 2c-iii.6 auth controls and edge IP | Pass | `2026-07-20T05:20Z`–`06:04Z` | Oversized body 413, unknown code 410, registered-code second poll 429. Initial live test reproduced two starts as 200/200; corrected candidate then produced 200/429. Two genuinely different network labels each produced `200,429,429,429` for normal, repeat, forged `X-Real-IP`, and prepended XFF requests | Focused fix `b2e91ac`; no source IP addresses retained |
| 2c-iii.7 search/detail/clone and host pinning | Pass | `2026-07-20T05:58Z`–`06:00Z` | One linked search result; detail URL stayed pinned under forged forwarded-host input; forged routing Host was rejected 404 by Railway edge; fresh isolated HTTPS clone succeeded | Search response key is `stacks`; clone's unauthenticated auto-follow queued as expected |
| 2c-iii.8 registry redeploy persistence | Pass | `2026-07-20T06:07Z`–`06:09Z` | Health, exact `v2` metadata, linked trust, Git clone, and audit survived an approved redeploy on the same Postgres and `/data` stores | Persistence deployment `71bf77bd-016e-4244-ac85-4df0f354ceee`; candidate `b2e91ac` |
| 2c-iii.9 off-site export upload | Blocked | `2026-07-20T06:20Z`–`06:27Z` | One-minute scheduler produced a complete pending archive, but three bounded upload attempts returned HTTP 404. The collector is configured with HTTPS and a token; no URL or credential was recorded. The exact prior interval was restored and the restoring deployment reached `SUCCESS` | Export deployment `dfb04d81-403c-4fe5-b110-576bd3d582a7`; restore deployment `159180e4-d49d-4838-8f6f-5ea9cf32b89c`; no recovery resources created |
| 2c-iii.10 sibling restore and audit | Not run | | | |
| 2c-iii.11 secret-safe logs | Preliminary pass | `2026-07-20T06:09Z` | Bounded local scan of mode-0600 registry log export detected no prohibited device codes, token fields, authorization values, secret-variable names, or token patterns | Final review repeats after export/restore |

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
| Collector object ID | Blocked — collector returned HTTP 404 and no retrievable object was established |
| Recovery Postgres/service IDs | Not created; gate stopped before approved recovery infrastructure |
| Recovery registry/service IDs | Not created; gate stopped before approved recovery infrastructure |
| Recovery volume IDs | Not created |
| Manifest verification | Not run; no collector object available for retrieval |
| `registry audit` | Primary registry pass (`missing=0 extra=0`); recovery audit not run |
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
