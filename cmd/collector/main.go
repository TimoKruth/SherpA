package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"sherpa/internal/recoveryarchive"
)

const verifyUsage = "usage: collector verify <archive-path>"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	handled, code := dispatch(ctx, os.Args[1:], os.Stdout, os.Stderr)
	if !handled {
		fmt.Fprintln(os.Stderr, verifyUsage)
		code = 2
	}
	os.Exit(code)
}

func dispatch(ctx context.Context, args []string, stdout, stderr io.Writer) (bool, int) {
	if len(args) == 0 {
		return false, 0
	}
	switch args[0] {
	case "verify":
		if len(args) != 2 {
			fmt.Fprintln(stderr, verifyUsage)
			return true, 2
		}
		return true, runVerify(ctx, args[1], stdout, stderr)
	default:
		return false, 0
	}
}

func runVerify(ctx context.Context, archivePath string, stdout, stderr io.Writer) int {
	report, err := recoveryarchive.ValidateFile(ctx, archivePath, recoveryarchive.DefaultLimits())
	if err != nil {
		classification := recoveryarchive.Classify(err)
		if classification == "" {
			classification = "archive_validation_failed"
		}
		fmt.Fprintln(stderr, "invalid")
		fmt.Fprintf(stderr, "classification=%s\n", classification)
		return 1
	}
	fmt.Fprintln(stdout, "valid")
	fmt.Fprintf(stdout, "artifacts=%d\n", report.ArtifactCount)
	fmt.Fprintf(stdout, "verified_bytes=%d\n", report.VerifiedBytes)
	fmt.Fprintf(stdout, "manifest_final=%t\n", report.ManifestFinal)
	return 0
}
