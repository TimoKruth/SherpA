package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sherpa/internal/recoveryarchive"
	"sherpa/internal/recoveryarchive/testfixture"
)

func TestDispatchVerifyUsesSharedValidator(t *testing.T) {
	archivePath := testfixture.Write(t,
		testfixture.Repository{Path: "repos/acme/api.bundle", Contents: "api bundle"},
		testfixture.Repository{Path: "repos/acme/web.bundle", Contents: "web bundle"},
	)
	wantReport, err := recoveryarchive.ValidateFile(context.Background(), archivePath, recoveryarchive.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder

	handled, code := dispatch(context.Background(), []string{"verify", archivePath}, &stdout, &stderr)

	want := fmt.Sprintf("valid\nartifacts=%d\nverified_bytes=%d\nmanifest_final=%t\n", wantReport.ArtifactCount, wantReport.VerifiedBytes, wantReport.ManifestFinal)
	if !handled || code != 0 || stdout.String() != want || stderr.Len() != 0 {
		t.Fatalf("dispatch handled=%v code=%d stdout=%q stderr=%q, want stdout %q", handled, code, stdout.String(), stderr.String(), want)
	}
}

func TestDispatchVerifyPrintsOnlySafeSummary(t *testing.T) {
	memberCanary := "repos/private-owner-canary/private-repository-canary.bundle"
	contentsCanary := "private-bundle-contents-canary"
	archivePath := testfixture.Write(t, testfixture.Repository{Path: memberCanary, Contents: contentsCanary})
	var stdout, stderr strings.Builder

	handled, code := dispatch(context.Background(), []string{"verify", archivePath}, &stdout, &stderr)

	wantBytes := int64(len(testfixture.PostgresContents) + len(contentsCanary))
	want := fmt.Sprintf("valid\nartifacts=2\nverified_bytes=%d\nmanifest_final=true\n", wantBytes)
	if !handled || code != 0 || stdout.String() != want || stderr.Len() != 0 {
		t.Fatalf("dispatch handled=%v code=%d stdout=%q stderr=%q", handled, code, stdout.String(), stderr.String())
	}
	for _, canary := range []string{archivePath, filepath.Dir(archivePath), memberCanary, contentsCanary, "private-owner-canary", "private-repository-canary"} {
		if strings.Contains(stdout.String(), canary) || strings.Contains(stderr.String(), canary) {
			t.Fatalf("verification output exposed canary %q: stdout=%q stderr=%q", canary, stdout.String(), stderr.String())
		}
	}
}

func TestDispatchVerifyFailurePrintsOnlySafeClassification(t *testing.T) {
	memberCanary := "../private-member-path-canary"
	manifestCanary := "private-manifest-value-canary"
	archivePath := writeArchive(t, []archiveMember{
		{name: "postgres.dump", contents: "postgres-canary"},
		{name: memberCanary, contents: "bundle-canary"},
		{name: "manifest.json", contents: manifestCanary},
	})
	var stdout, stderr strings.Builder

	handled, code := dispatch(context.Background(), []string{"verify", archivePath}, &stdout, &stderr)

	if !handled || code != 1 || stdout.Len() != 0 || stderr.String() != "invalid\nclassification=unsafe_path\n" {
		t.Fatalf("dispatch handled=%v code=%d stdout=%q stderr=%q", handled, code, stdout.String(), stderr.String())
	}
	for _, canary := range []string{archivePath, filepath.Dir(archivePath), memberCanary, manifestCanary, "bundle-canary", "postgres-canary"} {
		if strings.Contains(stdout.String(), canary) || strings.Contains(stderr.String(), canary) {
			t.Fatalf("failure output exposed canary %q: stdout=%q stderr=%q", canary, stdout.String(), stderr.String())
		}
	}

	invalidManifestPath := writeArchive(t, []archiveMember{
		{name: "postgres.dump", contents: "postgres-canary"},
		{name: "manifest.json", contents: manifestCanary},
	})
	stdout.Reset()
	stderr.Reset()
	handled, code = dispatch(context.Background(), []string{"verify", invalidManifestPath}, &stdout, &stderr)
	if !handled || code != 1 || stdout.Len() != 0 || stderr.String() != "invalid\nclassification=invalid_manifest\n" {
		t.Fatalf("invalid manifest dispatch handled=%v code=%d stdout=%q stderr=%q", handled, code, stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), invalidManifestPath) || strings.Contains(stderr.String(), manifestCanary) {
		t.Fatalf("invalid-manifest output exposed private data: %q", stderr.String())
	}

	missingPath := filepath.Join(t.TempDir(), "private-archive-path-canary.tar.gz")
	stdout.Reset()
	stderr.Reset()
	handled, code = dispatch(context.Background(), []string{"verify", missingPath}, &stdout, &stderr)
	if !handled || code != 1 || stdout.Len() != 0 || stderr.String() != "invalid\nclassification=archive_validation_failed\n" {
		t.Fatalf("missing dispatch handled=%v code=%d stdout=%q stderr=%q", handled, code, stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), missingPath) || strings.Contains(stderr.String(), filepath.Dir(missingPath)) {
		t.Fatalf("missing-archive output exposed path: %q", stderr.String())
	}
}

func TestDispatchVerifyDoesNotRequireRuntimeSecrets(t *testing.T) {
	for _, key := range []string{
		"SHERPA_COLLECTOR_TOKEN_FILE",
		"SHERPA_COLLECTOR_AGE_RECIPIENT_FILE",
		"SHERPA_COLLECTOR_BORG_REPOSITORY_FILE",
		"SHERPA_COLLECTOR_BORG_SSH_KEY_FILE",
		"SHERPA_COLLECTOR_KNOWN_HOSTS_FILE",
	} {
		t.Setenv(key, filepath.Join(t.TempDir(), "must-not-be-read-canary"))
	}
	archivePath := testfixture.Write(t)
	var stdout, stderr strings.Builder

	handled, code := dispatch(context.Background(), []string{"verify", archivePath}, &stdout, &stderr)

	if !handled || code != 0 || stdout.String() != fmt.Sprintf("valid\nartifacts=1\nverified_bytes=%d\nmanifest_final=true\n", len(testfixture.PostgresContents)) || stderr.Len() != 0 {
		t.Fatalf("dispatch handled=%v code=%d stdout=%q stderr=%q", handled, code, stdout.String(), stderr.String())
	}
}

func TestDispatchRejectsInvalidVerifyUsage(t *testing.T) {
	for _, args := range [][]string{{"verify"}, {"verify", "one", "two"}} {
		var stdout, stderr strings.Builder
		handled, code := dispatch(context.Background(), args, &stdout, &stderr)
		if !handled || code != 2 || stdout.Len() != 0 || stderr.String() != "usage: collector verify <archive-path>\n" {
			t.Fatalf("args=%q handled=%v code=%d stdout=%q stderr=%q", args, handled, code, stdout.String(), stderr.String())
		}
	}

	handled, code := dispatch(context.Background(), []string{"unknown"}, bytes.NewBuffer(nil), bytes.NewBuffer(nil))
	if handled || code != 0 {
		t.Fatalf("unknown command handled=%v code=%d", handled, code)
	}
}

type archiveMember struct {
	name     string
	contents string
}

func writeArchive(t testing.TB, members []archiveMember) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "private-archive-name-canary.tar.gz")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	gzipWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzipWriter)
	for _, member := range members {
		if err := tarWriter.WriteHeader(&tar.Header{
			Name: member.name, Mode: 0o600, Size: int64(len(member.contents)), Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write([]byte(member.contents)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}
