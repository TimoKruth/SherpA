package main

import (
	"context"
	"fmt"
	"io"

	registryexport "sherpa/internal/registry/export"
)

// runExportCommand is kept separate from main's dispatch so the manual export
// path remains independently testable.
func runExportCommand(ctx context.Context, cfg Config, archivePath string, stdout, stderr io.Writer) int {
	if archivePath == "" {
		fmt.Fprintln(stderr, "usage: registry export <archive-path>")
		return 2
	}
	if err := registryexport.Run(ctx, cfg.ContentDir, cfg.DatabaseURL, archivePath); err != nil {
		fmt.Fprintf(stderr, "export registry: %v\n", err)
		return 2
	}
	fmt.Fprintf(stdout, "exported registry to %s\n", archivePath)
	return 0
}
