# SherpA Railway Staging Acceptance Rollout

**Status:** Approved 2026-07-19
**Date:** 2026-07-19
**Candidate commit:** `312479ee55539560936129db21bcec8da7b9827a`
**Project:** Railway `powerful-rebirth`
**Environment:** `staging`
**Runbook:** `docs/deployment/railway.md`

## 1. Goal

Make the existing SherpA base version runnable on Railway staging and execute the complete live acceptance sequence for Phase 2c-iii, 2d, and 2e against exact deployed commits.

The rollout is complete only when all documented registry, website, and live social/session gates pass. Partial or inferred evidence does not count as a pass. Production remains untouched.

## 2. Current State

- Local `main` matches `origin/main` at `312479e`.
- The only unrelated local files are three untracked `.DS_Store` files; the rollout must not add, delete, or commit them.
- Railway CLI is authenticated as the project owner and linked to project `powerful-rebirth`, environment `staging`.
- `postgres` uses `ghcr.io/railwayapp-templates/postgres-ssl:16`, has no active deployment, and is missing its required data volume.
- `registry` is connected to `TimoKruth/SherpA`, branch `main`, config `/railway.json`, has a ready 5 GB volume mounted at `/data`, and has domain `registry-staging-78a5.up.railway.app`. Its latest deployment failed because Postgres was unavailable.
- `web` has domain `web-staging-c58d.up.railway.app`, but no repository source and no deployment.
- Production has no services and is out of scope.

## 3. Safety and Authorization Boundaries

All mutations target Railway project `powerful-rebirth` and environment `staging` only. Use explicit service and environment selectors whenever the tool supports them.

Do not print, copy, or persist secret values. Validate Railway secrets indirectly through service behavior and narrowly scoped variable-name checks. Never record database URLs, OAuth secrets, tokens, device codes, grants, CSRF values, cookies, trial notes, request bodies, or private collector URLs.

The operator has authorized normal staging provisioning and deployment. Before each deliberate outage, rollback, destructive action, or temporary sibling/recovery resource is created, present:

1. the exact action;
2. the expected staging impact;
3. the rollback or cleanup procedure;
4. the resources that will be created or changed;
5. a request for explicit approval.

Production provisioning, production secrets, and production domains are excluded.

## 4. Tooling and Authentication

Prepare and verify the following before Railway mutations:

- Upgrade Railway CLI from 5.26.1 to the current supported release.
- Run Railway's supported agent setup so Railway skills/MCP tooling are available where possible; retain the Railway CLI as the deterministic fallback.
- Verify Railway account, project, environment, and service linkage after setup.
- Verify GitHub CLI authentication for repository and source inspection. Application OAuth remains a separate live test flow.
- Verify Go, Git, Docker, `curl`, `jq`, and required shell scripts.
- Use Playwright for public browser flows, responsive checks, keyboard navigation, cookie/redirect inspection, and security-header verification where automation is safe.
- Use two distinct GitHub identities and two genuinely different source IPs for the gates that require them. Interactive identity authorization is performed by the operator when prompted.

Installing or reconfiguring tools must not change application behavior or commit unrelated configuration. If a tool setup requires restarting the Claude session before an MCP becomes visible, continue with the supported CLI for the current run and record that limitation.

## 5. Target Topology

### 5.1 Postgres

- Service: `postgres`
- Image: `ghcr.io/railwayapp-templates/postgres-ssl:16`
- Replica count: one
- Public TCP exposure: none
- Volume: new 5 GB volume mounted at `/var/lib/postgresql/data`
- Region: same Railway region as registry and its volume

### 5.2 Registry

- Service: `registry`
- Source: `TimoKruth/SherpA`, branch `main`
- Config file: `/railway.json`
- Root directory: unset
- Volume: existing 5 GB volume mounted at `/data`
- Content directory: `/data/git`
- Public domain: `https://registry-staging-78a5.up.railway.app`
- Replica count: one
- Health gate: `/healthz`

### 5.3 Website

- Service: `web`
- Source: `TimoKruth/SherpA`, branch `main`
- Config file: `/deploy/web/railway.json`
- Root directory: unset
- Volume: none
- Database reference: none
- Application secrets: none
- Public domain: `https://web-staging-c58d.up.railway.app`
- Replica count: one
- Health gate: `/healthz`

## 6. Rollout Sequence

### 6.1 Exact-commit preflight

Before provisioning:

1. confirm `main` and `origin/main` still identify the candidate commit;
2. confirm only the known unrelated `.DS_Store` files are untracked;
3. run the complete automated Go checks, formatting/vet checks, registry image smoke build, and website smoke build required by the repository;
4. stop if any preflight fails.

If a genuine implementation defect is found, add focused regression coverage, implement the smallest permanent fix, rerun all relevant checks, commit the fix, push it for Railway deployment, and update the candidate commit recorded by every subsequent gate.

### 6.2 Postgres

1. Create one 5 GB volume for `postgres` at `/var/lib/postgresql/data`.
2. Deploy the configured PG16 image.
3. Verify the deployment is active, logs show successful database startup, required mount path is present, and no public TCP endpoint was added.
4. Confirm restart persistence without exposing database credentials.

A Postgres failure blocks all later steps.

### 6.3 Registry and Phase 2c-iii gate

1. Redeploy `registry` from the configured GitHub source.
2. Verify the deployed commit and `/railway.json` manifest.
3. Confirm public `/healthz` returns 200 and the public search API succeeds.
4. Execute all eleven registry staging acceptance steps in `docs/deployment/railway.md` in order, including OAuth, owner authorization, rate limits/body limits, two-source-IP proxy identity, host-header pinning, persistence, off-site export, recovery restore, audit, and log review.

Do not connect or deploy the website until the registry gate passes.

The configured off-site collector is initially unverified. Test it through the scheduler and retrieval workflow. If it is unavailable or incomplete, stop the gate and propose/provision a robust external collector; do not substitute local container storage or another Railway volume as the disaster-recovery copy.

### 6.4 Website and Phase 2d gate

After the registry gate passes:

1. Connect `web` to `TimoKruth/SherpA`, branch `main`.
2. Set config file `/deploy/web/railway.json` and keep root directory unset.
3. Verify the service has no volume, database reference, OAuth credentials, registry admin token, cookie key, or export secret.
4. Deploy and verify `/healthz` and the public domain.
5. Execute all eleven website staging acceptance steps in the runbook, including API parity, clone commands, header pinning, paging bounds, degraded behavior, isolated redeploys, responsive/keyboard QA, secret-safe logs, and continuous-monitoring configuration.

A website gate failure blocks Phase 2e.

### 6.5 Phase 2e live gate

After the registry and website gates pass for the same candidate:

1. Execute all twelve Phase 2e live acceptance steps in the runbook.
2. Use two GitHub identities for owner and wrong-owner/session-boundary checks.
3. Use two genuine source IPs for Railway edge rate-limit identity checks.
4. Verify OAuth handoff, cookie attributes, replay/tamper defenses, web/CLI/admin authorization boundaries, follows and pending updates, offline follow sync, private trial journal behavior, outage degradation, export/restore contents, and separate website/registry rollback behavior.

Any pre-2e registry rollback requires the documented web-session and grant revocation procedure before deployment.

## 7. Disruptive Checkpoints

Explicit operator approval is required immediately before:

- stopping or blocking the registry for degraded-mode tests;
- restarting services solely to prove persistence when it creates avoidable downtime;
- creating sibling Postgres, registry, website, or volume resources for restore testing;
- restoring an archive into temporary infrastructure;
- redeploying an older image or commit for rollback drills;
- deleting temporary recovery resources;
- changing backup schedules, export intervals, or collector configuration in a way that can affect retained data.

Normal initial provisioning and deployment do not require another approval because they are the stated objective.

## 8. Failure Handling

- Never continue past a failed dependency gate.
- Preserve the failed deployment ID and secret-safe logs before retrying.
- Prefer correction of configuration or code over repeated blind redeploys.
- Do not detach, wipe, replace, or delete a volume to fix an application deployment failure.
- Do not restore over the only staging database or Git volume.
- Do not mark a collector upload successful until the object is durably retrievable and its archive can be restored and audited.
- Do not mark proxy behavior successful unless tests use genuinely distinct edge source IPs.
- If Railway behavior differs from the runbook assumptions, stop, reconcile against current Railway documentation, update the runbook/spec if required, and only then continue.

## 9. Evidence

Create a secret-free evidence record covering:

- candidate registry and website commits;
- Railway project, environment, service, deployment, and volume identifiers;
- public domains;
- start/end timestamps and operator;
- health/API status summaries;
- GitHub identity labels that do not expose tokens;
- rate-limit, header-pinning, authorization, persistence, and degraded-mode results;
- backup/export object identifiers and restore target identifiers;
- audit summaries;
- rollback source and result;
- log-review result;
- pass/fail status for every numbered runbook step.

Evidence must not contain credentials, private payloads, request bodies, query values, device codes, grants, CSRF values, cookies, OAuth codes/verifiers, trial notes, or private export URLs.

## 10. Completion Criteria

The rollout is complete when:

1. Postgres, registry, and website are running and healthy in Railway staging;
2. registry and website deployments identify exact accepted commits;
3. all 11 Phase 2c-iii steps pass;
4. all 11 Phase 2d steps pass;
5. all 12 Phase 2e steps pass;
6. off-site export and sibling restore/audit succeed;
7. website-only and registry-only rollback drills succeed under the documented safety rules;
8. evidence is recorded without secrets;
9. production remains empty and unchanged.

If any item is incomplete, report the exact blocker and retain the documented status: automated gates green, live Railway acceptance pending.
