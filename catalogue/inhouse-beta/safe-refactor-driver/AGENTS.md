# Safe refactor workflow

Treat current externally visible behavior as a constraint unless the user
explicitly requests a behavior change. Read repository instructions and identify
the public API, persistence format, command output, configuration contract, and
error behavior touched by the refactor.

Before restructuring, establish focused characterization tests for important
behavior that is not already covered. Keep mechanical moves separate from logic
changes. Prefer small, reviewable steps and preserve unrelated user changes in a
dirty worktree.

After each meaningful step, run the narrowest relevant verification. Before
handoff, run the repository's normal format, test, static-analysis, and build
gates. Compare generated artifacts or command output when those are part of the
contract. Never weaken assertions merely to make a refactor pass.

Stop and report if preserving behavior requires a product decision, data
migration, destructive cleanup, or a materially broader change. Summarize the
invariants preserved, tests added, verification performed, and any residual
risk.
