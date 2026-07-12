package main

import (
	"context"
	"fmt"
	"io"

	"sherpa/internal/registry/audit"
	"sherpa/internal/registry/content"
	"sherpa/internal/registry/store"
)

func runAudit(ctx context.Context, cfg Config, stdout, stderr io.Writer) int {
	st, err := store.OpenPostgres(ctx, cfg.DatabaseURL)
	if err != nil {
		fmt.Fprintf(stderr, "open registry store: %v\n", err)
		return 2
	}
	defer st.Close()

	report, err := audit.Run(ctx, st, content.NewBareGit(cfg.ContentDir))
	if err != nil {
		fmt.Fprintf(stderr, "audit registry: %v\n", err)
		return 2
	}
	for _, ref := range report.MissingContent {
		fmt.Fprintf(stdout, "MISSING %s/%s version=%d tag=%s\n", ref.Owner, ref.Name, ref.Version, ref.GitTag)
	}
	for _, tag := range report.ExtraTags {
		fmt.Fprintf(stdout, "EXTRA %s\n", tag)
	}
	fmt.Fprintf(stdout, "SUMMARY missing=%d extra=%d\n", len(report.MissingContent), len(report.ExtraTags))
	return auditExitCode(report)
}

func auditExitCode(report audit.Report) int {
	if len(report.MissingContent) != 0 {
		return 1
	}
	return 0
}
