// Package testfixture creates synthetic SherpA recovery archives for tests.
package testfixture

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"sort"
	"testing"
	"time"
)

const PostgresContents = "synthetic postgres dump"

type Repository struct {
	Path     string
	Contents string
}

type artifact struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type manifest struct {
	Artifacts []artifact `json:"artifacts"`
}

func Write(t testing.TB, repositories ...Repository) string {
	t.Helper()

	members := []Repository{{Path: "postgres.dump", Contents: PostgresContents}}
	members = append(members, repositories...)
	sort.Slice(members, func(i, j int) bool { return members[i].Path < members[j].Path })

	path := t.TempDir() + "/recovery.tar.gz"
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gzipWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzipWriter)

	artifacts := make([]artifact, 0, len(members))
	for _, member := range members {
		writeMember(t, tarWriter, member.Path, []byte(member.Contents))
		digest := sha256.Sum256([]byte(member.Contents))
		artifacts = append(artifacts, artifact{
			Path:   member.Path,
			SHA256: hex.EncodeToString(digest[:]),
			Size:   int64(len(member.Contents)),
		})
	}
	manifestBytes, err := json.MarshalIndent(manifest{Artifacts: artifacts}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeMember(t, tarWriter, "manifest.json", append(manifestBytes, '\n'))

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

func writeMember(t testing.TB, writer *tar.Writer, name string, contents []byte) {
	t.Helper()
	header := &tar.Header{
		Name:     name,
		Mode:     0o600,
		Size:     int64(len(contents)),
		Typeflag: tar.TypeReg,
		ModTime:  time.Unix(0, 0),
	}
	if err := writer.WriteHeader(header); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(contents); err != nil {
		t.Fatal(err)
	}
}
