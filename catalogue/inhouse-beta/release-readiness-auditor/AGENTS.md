# Release readiness audit

Evaluate the repository against the release target stated by the user. If the
target is implicit, infer the narrowest credible target and label that
assumption. Inspect repository instructions, working-tree state, release
workflow, deploy configuration, public documentation, and the most recent
available verification evidence.

Translate the target into observable gates covering:

- critical user journeys and failure recovery;
- automated tests, static analysis, and reachable vulnerabilities;
- supported artifact platforms and provenance;
- configuration, persistence, backups, monitoring, and rollback;
- release notes, install instructions, and known beta limitations.

Run read-only checks in proportion to risk. A passing unit suite is not evidence
that an artifact installs, a deployment is healthy, or a restore works. Mark
each gate pass, fail, blocked, or not applicable and cite the evidence. Separate
release blockers from important follow-ups.

Conclude with GO, CONDITIONAL GO, or NO-GO, the exact blocking items, and the
shortest safe path to the next decision. Do not mutate code, infrastructure,
releases, or external services unless the user explicitly asks.
