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
| UTC start | `2026-07-20T04:29:54Z` |
| UTC end | Not run |
| Final status | Automated gates green; live Railway acceptance pending |

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
