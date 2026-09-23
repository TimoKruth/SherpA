# V1 scope: local comparability

The product loop is **capture → vary → compare → review → return to baseline**.
The CLI is complete on its own; an embedded loopback web app provides the same
comparison workflow with side-by-side responses and private report exports.

In scope: Claude Code and Codex configuration copies; protected per-harness
baselines; local creation/import and explicit trust review; local switching;
identical prompt/project inputs; independent working/configuration directories;
outputs, patches, elapsed time, status, history, human ratings and HTML/JSON export.
No hosted service is required. Normal harness model connections remain available.

## Preserved future work

Before removing deferred features, the complete existing development tip
`b7b72a9` was preserved and pushed under these branches. Each branch deliberately
retains the full original tree so its feature and dependencies can be resumed;
the names identify the workstream, not partial or lossy file backups.

| Remote branch | Workstream to resume there |
| --- | --- |
| [`future/registry-and-sharing-2026-09-23`](https://github.com/TimoKruth/SherpA/tree/future/registry-and-sharing-2026-09-23) | Registry API, GitHub identity, login, publish, search, follows, notifications, remote stack updates, social trial sharing, catalogue. |
| [`future/hosted-website-2026-09-23`](https://github.com/TimoKruth/SherpA/tree/future/hosted-website-2026-09-23) | Public catalogue site, hosted profiles, discovery and account dashboard. |
| [`future/deployment-and-recovery-2026-09-23`](https://github.com/TimoKruth/SherpA/tree/future/deployment-and-recovery-2026-09-23) | Railway/VPS deployment configuration, registry exports, encrypted collector/recovery infrastructure, monitoring and runbooks. |

These features, their exclusive dependencies, historic plans, deployment files,
and CI service gates are removed from the V1 tree. The six-target CLI build and
release workflows remain. Existing local beta state is preserved, including
opaque registry/trial metadata, but has no network behavior in V1.

The V1 branch starts from `origin/main` and carries forward the useful multi-harness
initialization improvement from `071b546`. No existing hosted service is shut down
or redeployed by this change. The requested delivery is a pull request targeting
`main`, left open for review.

## Deliberately deferred

Public sharing/discovery, hosted accounts, fleet/cloud runners, fully offline-only
model requirements, a hardened container/VM sandbox for hostile configurations,
automatic quality judgments, statistical benchmarking, and portable plugin
installation management are outside V1. Comparability is evidence for a user's
judgment; one result is not proof that a setup is universally better.
