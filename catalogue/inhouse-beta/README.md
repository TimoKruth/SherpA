# Internal beta catalogue seeds

These directories are the reviewed source for SherpA's first useful catalogue
entries. They are intentionally instruction-only: no hooks, MCP servers,
credentials, permission overrides, or executable files.

Each directory is copied into its own temporary Git repository before
publication. Publishing is still performed by the SherpA CLI, so the same
history scan, sanitizer, manifest validation, immutable tag, and registry
checks apply as they do for every user stack.
