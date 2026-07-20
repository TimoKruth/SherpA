package recoveryarchive_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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

func TestValidateFileRejectsGNUSparseAndPAXSparseArtifacts(t *testing.T) {
	limits := recoveryarchive.DefaultLimits()
	for name, archive := range map[string][]byte{
		"GNU sparse type":     gnuSparseTar(t, 8<<10),
		"PAX sparse metadata": paxSparseTar(t, 8<<10),
	} {
		t.Run(name, func(t *testing.T) {
			path := writeGzipBytes(t, archive)
			assertFailureClass(t, path, limits, recoveryarchive.FailureUnsupportedType)
		})
	}
}

func TestValidateFileHonorsCancellationWhileReadingBufferedPAXMetadata(t *testing.T) {
	path := bufferedOrphanPAXArchive(t)
	assertFitsDecompressionBuffer(t, path)
	ctx := newCancelOnErrCallContext(9)

	_, err := recoveryarchive.ValidateFile(ctx, path, recoveryarchive.DefaultLimits())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ValidateFile error = %v, want context cancellation (Err calls: %d)", err, ctx.calls)
	}
}

func TestValidateFileHonorsCancellationWhileReadingBufferedManifest(t *testing.T) {
	path := bufferedInvalidManifestArchive(t)
	assertFitsDecompressionBuffer(t, path)
	ctx := newCancelOnErrCallContext(9)

	_, err := recoveryarchive.ValidateFile(ctx, path, recoveryarchive.DefaultLimits())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ValidateFile error = %v, want context cancellation (Err calls: %d)", err, ctx.calls)
	}
}

func TestValidateFileRequiresCanonicalTarTerminator(t *testing.T) {
	var canonical bytes.Buffer
	writeTarMembers(t, &canonical, completeMembers(t))
	contents := canonical.Bytes()
	prefix := contents[:len(contents)-2*tarBlockSize]
	terminator := make([]byte, 2*tarBlockSize)
	paxSparseBody := []byte(paxRecord("GNU.sparse.numblocks", "1") + paxRecord("GNU.sparse.map", "0,1") + paxRecord("GNU.sparse.size", "8192"))
	cases := []struct {
		name    string
		archive []byte
		want    recoveryarchive.FailureClass
	}{
		{name: "no terminator", archive: append([]byte(nil), prefix...), want: recoveryarchive.FailureInvalidTar},
		{name: "one-block terminator", archive: append([]byte(nil), contents[:len(contents)-tarBlockSize]...), want: recoveryarchive.FailureInvalidTar},
		{name: "orphan PAX header", archive: joinBytes(prefix, orphanExtension(t, tar.TypeXHeader, nil, true)), want: recoveryarchive.FailureManifestNotFinal},
		{name: "orphan GNU long name", archive: joinBytes(prefix, orphanExtension(t, tar.TypeGNULongName, []byte("orphan\x00"), false)), want: recoveryarchive.FailureManifestNotFinal},
		{name: "hidden PAX before terminator", archive: joinBytes(prefix, orphanExtension(t, tar.TypeXHeader, []byte(paxRecord("comment", "orphan")), false), terminator), want: recoveryarchive.FailureManifestNotFinal},
		{name: "hidden PAX sparse metadata before terminator", archive: joinBytes(prefix, orphanExtension(t, tar.TypeXHeader, paxSparseBody, false), terminator), want: recoveryarchive.FailureManifestNotFinal},
		{name: "hidden GNU long name before terminator", archive: joinBytes(prefix, orphanExtension(t, tar.TypeGNULongName, []byte("orphan\x00"), false), terminator), want: recoveryarchive.FailureManifestNotFinal},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			path := writeGzipBytes(t, test.archive)
			assertFailureClass(t, path, recoveryarchive.DefaultLimits(), test.want)
		})
	}
}

const tarBlockSize = 512

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

func gnuSparseTar(t testing.TB, logicalSize int64) []byte {
	t.Helper()
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	if err := writer.WriteHeader(&tar.Header{
		Name:     "postgres.dump",
		Mode:     0o600,
		Typeflag: tar.TypeReg,
		Format:   tar.FormatGNU,
	}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	contents := archive.Bytes()
	header := contents[:tarBlockSize]
	header[156] = tar.TypeGNUSparse
	writeTarOctal(header[483:495], logicalSize)
	writeTarChecksum(header)
	return append([]byte(nil), contents...)
}

func paxSparseTar(t testing.TB, logicalSize int64) []byte {
	t.Helper()
	records := []byte(paxRecord("GNU.sparse.numblocks", "1") + paxRecord("GNU.sparse.map", "0,1") + paxRecord("GNU.sparse.size", fmt.Sprint(logicalSize)))
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	if err := writer.WriteHeader(&tar.Header{
		Name:     "PaxHeaders/postgres.dump",
		Mode:     0o600,
		Size:     int64(len(records)),
		Typeflag: tar.TypeReg,
		Format:   tar.FormatUSTAR,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(records); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteHeader(&tar.Header{
		Name:     "postgres.dump",
		Mode:     0o600,
		Size:     1,
		Typeflag: tar.TypeReg,
		Format:   tar.FormatUSTAR,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	contents := archive.Bytes()
	contents[156] = tar.TypeXHeader
	writeTarChecksum(contents[:tarBlockSize])
	return contents
}

func paxRecord(key, value string) string {
	body := key + "=" + value + "\n"
	length := len(body) + 2
	for {
		record := fmt.Sprintf("%d %s", length, body)
		if len(record) == length {
			return record
		}
		length = len(record)
	}
}

func writeTarOctal(field []byte, value int64) {
	for i := range field {
		field[i] = 0
	}
	copy(field, fmt.Sprintf("%0*o", len(field)-1, value))
}

func writeTarChecksum(header []byte) {
	for i := 148; i < 156; i++ {
		header[i] = ' '
	}
	var sum int
	for _, value := range header {
		sum += int(value)
	}
	copy(header[148:156], fmt.Sprintf("%06o\x00 ", sum))
}

type cancelOnErrCallContext struct {
	context.Context
	cancel   context.CancelFunc
	cancelAt int
	calls    int
}

func newCancelOnErrCallContext(cancelAt int) *cancelOnErrCallContext {
	ctx, cancel := context.WithCancel(context.Background())
	return &cancelOnErrCallContext{Context: ctx, cancel: cancel, cancelAt: cancelAt}
}

func (ctx *cancelOnErrCallContext) Err() error {
	ctx.calls++
	if ctx.calls == ctx.cancelAt {
		ctx.cancel()
	}
	return ctx.Context.Err()
}

func bufferedOrphanPAXArchive(t testing.TB) string {
	t.Helper()
	var archive bytes.Buffer
	writeTarMembers(t, &archive, []testMember{{name: "postgres.dump", body: []byte("synthetic postgres dump")}})
	contents := archive.Bytes()
	prefix := contents[:len(contents)-2*tarBlockSize]
	paxBody := []byte(paxRecord("comment", string(bytes.Repeat([]byte("a"), 256<<10))))
	contents = append(append([]byte(nil), prefix...), orphanExtension(t, tar.TypeXHeader, paxBody, false)...)
	return writeGzipBytes(t, contents)
}

func bufferedInvalidManifestArchive(t testing.TB) string {
	t.Helper()
	manifest := bytes.Repeat([]byte("x"), 256<<10)
	return writeArchive(t, []testMember{
		{name: "postgres.dump", body: []byte("synthetic postgres dump")},
		{name: "manifest.json", body: manifest},
	})
}

func assertFitsDecompressionBuffer(t testing.TB, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() >= 4096 {
		t.Fatalf("compressed fixture is %d bytes, want less than the 4096-byte buffer", info.Size())
	}
}

func joinBytes(parts ...[]byte) []byte {
	var joined []byte
	for _, part := range parts {
		joined = append(joined, part...)
	}
	return joined
}

func orphanExtension(t testing.TB, typeflag byte, body []byte, appendZeroBlock bool) []byte {
	t.Helper()
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	if err := writer.WriteHeader(&tar.Header{
		Name:     "orphan-extension",
		Mode:     0o600,
		Size:     int64(len(body)),
		Typeflag: tar.TypeReg,
		Format:   tar.FormatUSTAR,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	blocks := archive.Bytes()[:tarBlockSize+roundedTarBodyBytes(len(body))]
	blocks[156] = typeflag
	writeTarChecksum(blocks[:tarBlockSize])
	result := append([]byte(nil), blocks...)
	if appendZeroBlock {
		result = append(result, make([]byte, tarBlockSize)...)
	}
	return result
}

func roundedTarBodyBytes(size int) int {
	return (size + tarBlockSize - 1) / tarBlockSize * tarBlockSize
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
