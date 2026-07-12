package main

import (
	"testing"

	"sherpa/internal/registry/audit"
	"sherpa/internal/registry/store"
)

func TestAuditExitCodeBlocksMissingContentOnly(t *testing.T) {
	if got := auditExitCode(audit.Report{}); got != 0 {
		t.Fatalf("consistent exit code = %d, want 0", got)
	}
	if got := auditExitCode(audit.Report{ExtraTags: []string{"alice/reviewer:extra"}}); got != 0 {
		t.Fatalf("extra-only exit code = %d, want 0", got)
	}
	if got := auditExitCode(audit.Report{MissingContent: []store.VersionRef{{Owner: "alice", Name: "reviewer", Version: 1, GitTag: "v1"}}}); got != 1 {
		t.Fatalf("missing-content exit code = %d, want 1", got)
	}
}
