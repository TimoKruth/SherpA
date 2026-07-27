// Package recoveryarchive validates SherpA application recovery archives.
package recoveryarchive

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path"
	"strings"
)

const tarBlockBytes = 512

type Limits struct {
	MaxCompressedBytes   int64
	MaxUncompressedBytes int64
	MaxMembers           int
	MaxManifestBytes     int64
	MaxPathBytes         int
}

type Report struct {
	ArtifactCount int
	VerifiedBytes int64
	ManifestFinal bool
}

type FailureClass string

const (
	FailureInvalidGzip       FailureClass = "invalid_gzip"
	FailureTrailingData      FailureClass = "trailing_data"
	FailureInvalidTar        FailureClass = "invalid_tar"
	FailureUnsafePath        FailureClass = "unsafe_path"
	FailureDuplicateMember   FailureClass = "duplicate_member"
	FailureUnsupportedType   FailureClass = "unsupported_member_type"
	FailureLimitExceeded     FailureClass = "limit_exceeded"
	FailureMissingPostgres   FailureClass = "missing_postgres_dump"
	FailureInvalidRepository FailureClass = "invalid_repository_path"
	FailureMissingManifest   FailureClass = "missing_manifest"
	FailureManifestNotFinal  FailureClass = "manifest_not_final"
	FailureInvalidManifest   FailureClass = "invalid_manifest"
	FailureManifestMismatch  FailureClass = "manifest_mismatch"
)

type validationError struct {
	class FailureClass
}

func (err validationError) Error() string {
	return string(err.class)
}

func DefaultLimits() Limits {
	return Limits{
		MaxCompressedBytes:   8 << 30,
		MaxUncompressedBytes: 32 << 30,
		MaxMembers:           100_000,
		MaxManifestBytes:     maxManifestBytes,
		MaxPathBytes:         1_024,
	}
}

func ValidateFile(ctx context.Context, archivePath string, limits Limits) (Report, error) {
	if err := ctx.Err(); err != nil {
		return Report{}, err
	}
	file, err := os.Open(archivePath)
	if err != nil {
		return Report{}, errors.New("open recovery archive")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return Report{}, errors.New("stat recovery archive")
	}
	if !info.Mode().IsRegular() {
		return Report{}, validationError{class: FailureInvalidGzip}
	}
	if info.Size() > limits.MaxCompressedBytes {
		return Report{}, validationError{class: FailureLimitExceeded}
	}

	compressed := &boundedReader{reader: &contextReader{ctx: ctx, reader: file}, limit: limits.MaxCompressedBytes}
	buffered := bufio.NewReader(compressed)
	gzipReader, err := gzip.NewReader(buffered)
	if err != nil {
		if compressed.exceeded {
			return Report{}, validationError{class: FailureLimitExceeded}
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Report{}, ctxErr
		}
		return Report{}, validationError{class: FailureInvalidGzip}
	}
	gzipReader.Multistream(false)
	decompressed := &boundedReader{reader: gzipReader, limit: limits.MaxUncompressedBytes}
	framing := &tarFramingReader{reader: decompressed}
	contextualDecompressed := &contextReader{ctx: ctx, reader: framing}
	tarReader := tar.NewReader(contextualDecompressed)

	artifacts := make(map[string]Artifact)
	seen := make(map[string]struct{})
	var manifest Manifest
	manifestSeen := false
	postgresSeen := false
	memberCount := 0
	logicalBudget := artifactBudget{limit: limits.MaxUncompressedBytes}
	var verifiedBytes int64

members:
	for {
		if err := ctx.Err(); err != nil {
			return Report{}, err
		}
		header, err := tarReader.Next()
		if err == io.EOF {
			if !framing.hasCanonicalTerminator() {
				return Report{}, validationError{class: FailureInvalidTar}
			}
			break
		}
		if err != nil {
			return Report{}, classifyStreamError(ctx, compressed, decompressed, err, FailureInvalidTar)
		}
		memberCount++
		if memberCount > limits.MaxMembers {
			return Report{}, validationError{class: FailureLimitExceeded}
		}
		if len(header.Name) > limits.MaxPathBytes {
			return Report{}, validationError{class: FailureLimitExceeded}
		}
		if !safeMemberPath(header.Name) {
			return Report{}, validationError{class: FailureUnsafePath}
		}
		if _, exists := seen[header.Name]; exists {
			return Report{}, validationError{class: FailureDuplicateMember}
		}
		seen[header.Name] = struct{}{}
		if sparseMember(header) || (header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA) {
			return Report{}, validationError{class: FailureUnsupportedType}
		}

		switch header.Name {
		case "postgres.dump":
			postgresSeen = true
			artifact, size, err := readArtifact(ctx, tarReader, header.Name, header.Size, &logicalBudget)
			if err != nil {
				return Report{}, classifyStreamError(ctx, compressed, decompressed, err, FailureInvalidTar)
			}
			artifacts[header.Name] = artifact
			verifiedBytes += size
		case "manifest.json":
			manifestLimit := limits.MaxManifestBytes
			if manifestLimit > maxManifestBytes {
				manifestLimit = maxManifestBytes
			}
			if header.Size > manifestLimit {
				return Report{}, validationError{class: FailureLimitExceeded}
			}
			encoded, err := io.ReadAll(tarReader)
			if err != nil {
				return Report{}, classifyStreamError(ctx, compressed, decompressed, err, FailureInvalidTar)
			}
			manifest, err = decodeManifest(encoded)
			if err != nil {
				return Report{}, err
			}
			if err := validateFinalTerminator(contextualDecompressed, header.Size); err != nil {
				return Report{}, classifyStreamError(ctx, compressed, decompressed, err, FailureInvalidTar)
			}
			manifestSeen = true
			break members
		default:
			if !validRepositoryPath(header.Name) {
				return Report{}, validationError{class: FailureInvalidRepository}
			}
			artifact, size, err := readArtifact(ctx, tarReader, header.Name, header.Size, &logicalBudget)
			if err != nil {
				return Report{}, classifyStreamError(ctx, compressed, decompressed, err, FailureInvalidTar)
			}
			artifacts[header.Name] = artifact
			verifiedBytes += size
		}
	}

	var trailing [1]byte
	for {
		n, err := contextualDecompressed.Read(trailing[:])
		if decompressed.exceeded || compressed.exceeded {
			return Report{}, validationError{class: FailureLimitExceeded}
		}
		if n != 0 {
			return Report{}, validationError{class: FailureTrailingData}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return Report{}, classifyStreamError(ctx, compressed, decompressed, err, FailureInvalidGzip)
		}
	}
	if err := gzipReader.Close(); err != nil {
		return Report{}, validationError{class: FailureInvalidGzip}
	}
	if _, err := buffered.Peek(1); err == nil {
		return Report{}, validationError{class: FailureTrailingData}
	} else if err != io.EOF {
		if compressed.exceeded {
			return Report{}, validationError{class: FailureLimitExceeded}
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Report{}, ctxErr
		}
		return Report{}, validationError{class: FailureInvalidGzip}
	}

	if !postgresSeen {
		return Report{}, validationError{class: FailureMissingPostgres}
	}
	if !manifestSeen {
		return Report{}, validationError{class: FailureMissingManifest}
	}
	if !manifestMatches(manifest, artifacts) {
		return Report{}, validationError{class: FailureManifestMismatch}
	}
	return Report{
		ArtifactCount: len(artifacts),
		VerifiedBytes: verifiedBytes,
		ManifestFinal: true,
	}, nil
}

func Classify(err error) FailureClass {
	var validation validationError
	if errors.As(err, &validation) {
		return validation.class
	}
	return ""
}

func validateFinalTerminator(reader io.Reader, size int64) error {
	padding := (tarBlockBytes - size%tarBlockBytes) % tarBlockBytes
	if _, err := io.CopyN(io.Discard, reader, padding); err != nil {
		return err
	}

	var block [tarBlockBytes]byte
	if _, err := io.ReadFull(reader, block[:]); err != nil {
		return err
	}
	if !zeroBlock(block[:]) {
		return validationError{class: FailureManifestNotFinal}
	}
	if _, err := io.ReadFull(reader, block[:]); err != nil {
		return err
	}
	if !zeroBlock(block[:]) {
		return validationError{class: FailureInvalidTar}
	}
	return nil
}

func zeroBlock(block []byte) bool {
	for _, value := range block {
		if value != 0 {
			return false
		}
	}
	return true
}

func sparseMember(header *tar.Header) bool {
	if header.Typeflag == tar.TypeGNUSparse {
		return true
	}
	for key := range header.PAXRecords {
		if strings.HasPrefix(key, "GNU.sparse.") {
			return true
		}
	}
	return false
}

func safeMemberPath(name string) bool {
	if name == "" || path.IsAbs(name) || strings.Contains(name, "\\") {
		return false
	}
	for _, segment := range strings.Split(name, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func validRepositoryPath(name string) bool {
	segments := strings.Split(name, "/")
	return len(segments) == 3 && segments[0] == "repos" && segments[1] != "" &&
		strings.HasSuffix(segments[2], ".bundle") && strings.TrimSuffix(segments[2], ".bundle") != ""
}

func readArtifact(ctx context.Context, reader io.Reader, name string, size int64, budget *artifactBudget) (Artifact, int64, error) {
	if err := budget.reserve(size); err != nil {
		return Artifact{}, 0, err
	}
	hash := sha256.New()
	counter := &byteCounter{}
	contextual := &contextReader{ctx: ctx, reader: reader}
	if _, err := io.Copy(io.MultiWriter(hash, counter), contextual); err != nil {
		return Artifact{}, 0, err
	}
	if counter.size != size {
		return Artifact{}, 0, io.ErrUnexpectedEOF
	}
	return Artifact{
		Path:   name,
		SHA256: hex.EncodeToString(hash.Sum(nil)),
		Size:   counter.size,
	}, counter.size, nil
}

type artifactBudget struct {
	limit int64
	used  int64
}

func (budget *artifactBudget) reserve(size int64) error {
	if size < 0 || budget.limit < 0 || budget.used > budget.limit || size > budget.limit-budget.used {
		return validationError{class: FailureLimitExceeded}
	}
	budget.used += size
	return nil
}

func manifestMatches(manifest Manifest, artifacts map[string]Artifact) bool {
	if len(manifest.Artifacts) != len(artifacts) {
		return false
	}
	seen := make(map[string]struct{}, len(manifest.Artifacts))
	for _, expected := range manifest.Artifacts {
		if _, duplicate := seen[expected.Path]; duplicate {
			return false
		}
		seen[expected.Path] = struct{}{}
		actual, exists := artifacts[expected.Path]
		if !exists || actual != expected {
			return false
		}
	}
	return true
}

func classifyStreamError(ctx context.Context, compressed, decompressed *boundedReader, err error, fallback FailureClass) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if compressed.exceeded || decompressed.exceeded {
		return validationError{class: FailureLimitExceeded}
	}
	var validation validationError
	if errors.As(err, &validation) {
		return validation
	}
	if errors.Is(err, gzip.ErrHeader) || errors.Is(err, gzip.ErrChecksum) {
		return validationError{class: FailureInvalidGzip}
	}
	return validationError{class: fallback}
}

type tarFramingReader struct {
	reader io.Reader
	tail   [2 * tarBlockBytes]byte
	length int
}

func (reader *tarFramingReader) Read(contents []byte) (int, error) {
	n, err := reader.reader.Read(contents)
	reader.record(contents[:n])
	return n, err
}

func (reader *tarFramingReader) record(contents []byte) {
	if len(contents) >= len(reader.tail) {
		copy(reader.tail[:], contents[len(contents)-len(reader.tail):])
		reader.length = len(reader.tail)
		return
	}
	if reader.length+len(contents) > len(reader.tail) {
		overflow := reader.length + len(contents) - len(reader.tail)
		copy(reader.tail[:], reader.tail[overflow:reader.length])
		reader.length -= overflow
	}
	copy(reader.tail[reader.length:], contents)
	reader.length += len(contents)
}

func (reader *tarFramingReader) hasCanonicalTerminator() bool {
	if reader.length != len(reader.tail) {
		return false
	}
	for _, value := range reader.tail {
		if value != 0 {
			return false
		}
	}
	return true
}

type byteCounter struct {
	size int64
}

func (counter *byteCounter) Write(contents []byte) (int, error) {
	counter.size += int64(len(contents))
	return len(contents), nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader *contextReader) Read(contents []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(contents)
}

type boundedReader struct {
	reader   io.Reader
	limit    int64
	read     int64
	exceeded bool
}

func (reader *boundedReader) Read(contents []byte) (int, error) {
	if reader.exceeded || reader.limit < 0 {
		reader.exceeded = true
		return 0, validationError{class: FailureLimitExceeded}
	}
	remaining := reader.limit - reader.read
	if remaining < int64(len(contents)) {
		contents = contents[:int(remaining)+1]
	}
	n, err := reader.reader.Read(contents)
	reader.read += int64(n)
	if reader.read > reader.limit {
		reader.exceeded = true
		return n, validationError{class: FailureLimitExceeded}
	}
	return n, err
}
