package recoveryarchive_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"sherpa/internal/recoveryarchive"
	"sherpa/internal/recoveryarchive/testfixture"
)

func TestValidateFileAcceptsCompleteArchive(t *testing.T) {
	repositories := []testfixture.Repository{
		{Path: "repos/acme/api.bundle", Contents: "synthetic api bundle"},
		{Path: "repos/acme/web.bundle", Contents: "synthetic web bundle"},
	}
	path := testfixture.Write(t, repositories...)

	report, err := recoveryarchive.ValidateFile(context.Background(), path, recoveryarchive.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	wantBytes := int64(len(testfixture.PostgresContents))
	for _, repository := range repositories {
		wantBytes += int64(len(repository.Contents))
	}
	if report.ArtifactCount != 3 || report.VerifiedBytes != wantBytes || !report.ManifestFinal {
		t.Fatalf("unexpected report: %+v", report)
	}
}

func TestValidateFileAcceptsArchiveWithoutRepositories(t *testing.T) {
	path := testfixture.Write(t)

	report, err := recoveryarchive.ValidateFile(context.Background(), path, recoveryarchive.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if report.ArtifactCount != 1 || report.VerifiedBytes != int64(len(testfixture.PostgresContents)) || !report.ManifestFinal {
		t.Fatalf("unexpected report: %+v", report)
	}
}

func TestBuildManifestHashesSizesAndSortsArtifacts(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "repos", "zeta", "web.bundle"), "synthetic web bundle")
	writeFile(t, filepath.Join(root, "postgres.dump"), "synthetic postgres dump")
	writeFile(t, filepath.Join(root, "repos", "alpha", "api.bundle"), "synthetic api bundle")
	writeFile(t, filepath.Join(root, "manifest.json"), "synthetic old manifest")

	manifest, err := recoveryarchive.BuildManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []recoveryarchive.Artifact{
		artifact("postgres.dump", "synthetic postgres dump"),
		artifact("repos/alpha/api.bundle", "synthetic api bundle"),
		artifact("repos/zeta/web.bundle", "synthetic web bundle"),
	}
	if len(manifest.Artifacts) != len(want) {
		t.Fatalf("got %d artifacts, want %d", len(manifest.Artifacts), len(want))
	}
	for i := range want {
		if manifest.Artifacts[i] != want[i] {
			t.Fatalf("artifact %d = %+v, want %+v", i, manifest.Artifacts[i], want[i])
		}
	}

	encoded, err := recoveryarchive.MarshalManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) == 0 || encoded[len(encoded)-1] != '\n' {
		t.Fatalf("manifest must end with a newline: %q", encoded)
	}
}

func TestValidateFileRequiresExactlyOnePostgresDump(t *testing.T) {
	manifest := marshalManifest(t, nil)
	path := writeArchive(t, []testMember{{name: "manifest.json", body: manifest}})

	assertFailureClass(t, path, recoveryarchive.DefaultLimits(), recoveryarchive.FailureMissingPostgres)
}

func TestValidateFileRestrictsGitBundlesToRepositoryNamespace(t *testing.T) {
	for _, name := range []string{
		"repos/acme/api",
		"repos/acme/nested/api.bundle",
		"repositories/acme/api.bundle",
		"repos/acme.bundle",
	} {
		t.Run(name, func(t *testing.T) {
			members := []testMember{
				{name: "postgres.dump", body: []byte("synthetic postgres dump")},
				{name: name, body: []byte("synthetic bundle")},
			}
			members = append(members, testMember{name: "manifest.json", body: marshalManifest(t, artifactsFor(members))})
			path := writeArchive(t, members)
			assertFailureClass(t, path, recoveryarchive.DefaultLimits(), recoveryarchive.FailureInvalidRepository)
		})
	}
}

func TestValidateFileRequiresExactlyOneFinalManifest(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		path := writeArchive(t, []testMember{{name: "postgres.dump", body: []byte("synthetic postgres dump")}})
		assertFailureClass(t, path, recoveryarchive.DefaultLimits(), recoveryarchive.FailureMissingManifest)
	})

	t.Run("not final", func(t *testing.T) {
		artifacts := []testMember{
			{name: "postgres.dump", body: []byte("synthetic postgres dump")},
			{name: "repos/acme/api.bundle", body: []byte("synthetic bundle")},
		}
		members := []testMember{
			artifacts[0],
			{name: "manifest.json", body: marshalManifest(t, artifactsFor(artifacts))},
			artifacts[1],
		}
		path := writeArchive(t, members)
		assertFailureClass(t, path, recoveryarchive.DefaultLimits(), recoveryarchive.FailureManifestNotFinal)
	})
}

func TestValidateFileRejectsMissingExtraDuplicateAndMismatchedManifestArtifacts(t *testing.T) {
	members := []testMember{
		{name: "postgres.dump", body: []byte("synthetic postgres dump")},
		{name: "repos/acme/api.bundle", body: []byte("synthetic bundle")},
	}
	valid := artifactsFor(members)
	cases := map[string][]recoveryarchive.Artifact{
		"missing":    valid[:1],
		"extra":      append(append([]recoveryarchive.Artifact{}, valid...), artifact("repos/acme/ghost.bundle", "synthetic ghost")),
		"duplicate":  {valid[0], valid[0], valid[1]},
		"wrong hash": {{Path: valid[0].Path, SHA256: hex.EncodeToString(make([]byte, sha256.Size)), Size: valid[0].Size}, valid[1]},
		"wrong size": {{Path: valid[0].Path, SHA256: valid[0].SHA256, Size: valid[0].Size + 1}, valid[1]},
	}
	for name, manifestArtifacts := range cases {
		t.Run(name, func(t *testing.T) {
			archiveMembers := append(append([]testMember{}, members...), testMember{
				name: "manifest.json",
				body: marshalManifest(t, manifestArtifacts),
			})
			path := writeArchive(t, archiveMembers)
			assertFailureClass(t, path, recoveryarchive.DefaultLimits(), recoveryarchive.FailureManifestMismatch)
		})
	}
}

func TestValidateFileRejectsUnknownManifestFieldsAndTrailingJSON(t *testing.T) {
	members := []testMember{{name: "postgres.dump", body: []byte("synthetic postgres dump")}}
	validArtifact := artifactsFor(members)[0]
	unknown, err := json.Marshal(map[string]any{
		"artifacts": []recoveryarchive.Artifact{validArtifact},
		"unknown":   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	valid := marshalManifest(t, []recoveryarchive.Artifact{validArtifact})
	cases := map[string][]byte{
		"unknown field": unknown,
		"trailing JSON": append(valid, []byte("{}")...),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			archiveMembers := append(append([]testMember{}, members...), testMember{name: "manifest.json", body: body})
			path := writeArchive(t, archiveMembers)
			assertFailureClass(t, path, recoveryarchive.DefaultLimits(), recoveryarchive.FailureInvalidManifest)
		})
	}
}

func TestValidateFileRejectsSecondGzipMember(t *testing.T) {
	first, err := os.ReadFile(testfixture.Write(t))
	if err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(testfixture.Write(t))
	if err != nil {
		t.Fatal(err)
	}
	path := writeBytes(t, append(first, second...))

	assertFailureClass(t, path, recoveryarchive.DefaultLimits(), recoveryarchive.FailureTrailingData)
}

func TestValidateFileRejectsCompressedTrailingBytes(t *testing.T) {
	contents, err := os.ReadFile(testfixture.Write(t))
	if err != nil {
		t.Fatal(err)
	}
	path := writeBytes(t, append(contents, []byte("synthetic compressed trailing bytes")...))

	assertFailureClass(t, path, recoveryarchive.DefaultLimits(), recoveryarchive.FailureTrailingData)
}

func TestValidateFileRejectsDataAfterTarTerminator(t *testing.T) {
	members := completeMembers(t)
	var tarBytes bytes.Buffer
	writeTarMembers(t, &tarBytes, members)
	tarBytes.WriteString("synthetic decompressed trailing bytes")
	path := writeGzipBytes(t, tarBytes.Bytes())

	assertFailureClass(t, path, recoveryarchive.DefaultLimits(), recoveryarchive.FailureTrailingData)
}

func TestValidateFileRejectsAbsoluteTraversalBackslashDotAndEmptySegments(t *testing.T) {
	for _, name := range []string{
		"/repos/acme/api.bundle",
		"../repos/acme/api.bundle",
		"repos\\acme\\api.bundle",
		"repos/./api.bundle",
		"repos//api.bundle",
	} {
		t.Run(name, func(t *testing.T) {
			members := []testMember{
				{name: "postgres.dump", body: []byte("synthetic postgres dump")},
				{name: name, body: []byte("synthetic bundle")},
			}
			members = append(members, testMember{name: "manifest.json", body: marshalManifest(t, artifactsFor(members))})
			path := writeArchive(t, members)
			assertFailureClass(t, path, recoveryarchive.DefaultLimits(), recoveryarchive.FailureUnsafePath)
		})
	}
}

func TestValidateFileRejectsDuplicateMembers(t *testing.T) {
	postgres := testMember{name: "postgres.dump", body: []byte("synthetic postgres dump")}
	members := []testMember{postgres, postgres}
	members = append(members, testMember{name: "manifest.json", body: marshalManifest(t, artifactsFor(members[:1]))})
	path := writeArchive(t, members)

	assertFailureClass(t, path, recoveryarchive.DefaultLimits(), recoveryarchive.FailureDuplicateMember)
}

func TestValidateFileRejectsUnsupportedMemberTypes(t *testing.T) {
	for name, typeflag := range map[string]byte{
		"directory": tar.TypeDir,
		"symlink":   tar.TypeSymlink,
	} {
		t.Run(name, func(t *testing.T) {
			members := []testMember{
				{name: "postgres.dump", body: []byte("synthetic postgres dump")},
				{name: "repos/acme/api.bundle", typeflag: typeflag},
			}
			members = append(members, testMember{name: "manifest.json", body: marshalManifest(t, artifactsFor(members))})
			path := writeArchive(t, members)
			assertFailureClass(t, path, recoveryarchive.DefaultLimits(), recoveryarchive.FailureUnsupportedType)
		})
	}
}

func TestValidateFileEnforcesCompressedUncompressedMemberManifestAndPathLimits(t *testing.T) {
	path := testfixture.Write(t)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	limits := recoveryarchive.DefaultLimits()
	limits.MaxCompressedBytes = info.Size() - 1
	assertFailureClass(t, path, limits, recoveryarchive.FailureLimitExceeded)

	limits = recoveryarchive.DefaultLimits()
	limits.MaxUncompressedBytes = 1
	assertFailureClass(t, path, limits, recoveryarchive.FailureLimitExceeded)

	limits = recoveryarchive.DefaultLimits()
	limits.MaxMembers = 1
	assertFailureClass(t, path, limits, recoveryarchive.FailureLimitExceeded)

	manifestBody := marshalManifest(t, []recoveryarchive.Artifact{artifact("postgres.dump", testfixture.PostgresContents)})
	limits = recoveryarchive.DefaultLimits()
	limits.MaxManifestBytes = int64(len(manifestBody) - 1)
	assertFailureClass(t, path, limits, recoveryarchive.FailureLimitExceeded)

	limits = recoveryarchive.DefaultLimits()
	limits.MaxPathBytes = len("postgres.dump") - 1
	assertFailureClass(t, path, limits, recoveryarchive.FailureLimitExceeded)
}

type testMember struct {
	name     string
	body     []byte
	typeflag byte
}

func completeMembers(t testing.TB) []testMember {
	t.Helper()
	artifacts := []testMember{{name: "postgres.dump", body: []byte("synthetic postgres dump")}}
	return append(artifacts, testMember{name: "manifest.json", body: marshalManifest(t, artifactsFor(artifacts))})
}

func writeArchive(t testing.TB, members []testMember) string {
	t.Helper()
	var tarBytes bytes.Buffer
	writeTarMembers(t, &tarBytes, members)
	return writeGzipBytes(t, tarBytes.Bytes())
}

func writeTarMembers(t testing.TB, destination io.Writer, members []testMember) {
	t.Helper()
	tarWriter := tar.NewWriter(destination)
	for _, member := range members {
		typeflag := member.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		header := &tar.Header{
			Name:     member.name,
			Mode:     0o600,
			Size:     int64(len(member.body)),
			Typeflag: typeflag,
			ModTime:  time.Unix(0, 0),
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write(member.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeGzipBytes(t testing.TB, contents []byte) string {
	t.Helper()
	var compressed bytes.Buffer
	gzipWriter := gzip.NewWriter(&compressed)
	if _, err := gzipWriter.Write(contents); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return writeBytes(t, compressed.Bytes())
}

func writeBytes(t testing.TB, contents []byte) string {
	t.Helper()
	path := t.TempDir() + "/archive.tar.gz"
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func artifactsFor(members []testMember) []recoveryarchive.Artifact {
	artifacts := make([]recoveryarchive.Artifact, 0, len(members))
	for _, member := range members {
		if member.name != "manifest.json" {
			artifacts = append(artifacts, artifact(member.name, string(member.body)))
		}
	}
	return artifacts
}

func artifact(path, contents string) recoveryarchive.Artifact {
	digest := sha256.Sum256([]byte(contents))
	return recoveryarchive.Artifact{
		Path:   path,
		SHA256: hex.EncodeToString(digest[:]),
		Size:   int64(len(contents)),
	}
}

func marshalManifest(t testing.TB, artifacts []recoveryarchive.Artifact) []byte {
	t.Helper()
	encoded, err := json.Marshal(recoveryarchive.Manifest{Artifacts: artifacts})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func writeFile(t testing.TB, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertFailureClass(t testing.TB, path string, limits recoveryarchive.Limits, want recoveryarchive.FailureClass) {
	t.Helper()
	_, err := recoveryarchive.ValidateFile(context.Background(), path, limits)
	if err == nil {
		t.Fatalf("ValidateFile succeeded, want %s", want)
	}
	if got := recoveryarchive.Classify(err); got != want {
		t.Fatalf("Classify(error) = %q, want %q (error: %v)", got, want, err)
	}
}
