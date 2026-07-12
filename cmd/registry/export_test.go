package main

import (
	"bytes"
	"context"
	"testing"
)

func TestRunExportCommandRequiresArchivePath(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if got := runExportCommand(context.Background(), Config{}, "", &stdout, &stderr); got != 2 {
		t.Fatalf("exit code = %d, want 2", got)
	}
	if stderr.String() != "usage: registry export <archive-path>\n" {
		t.Fatalf("stderr = %q", stderr.String())
	}
}
