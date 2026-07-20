package recoveryarchive

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

const maxManifestBytes int64 = 16 << 20

type Artifact struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type Manifest struct {
	Artifacts []Artifact `json:"artifacts"`
}

func BuildManifest(root string) (Manifest, error) {
	var artifacts []Artifact
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if relative == "manifest.json" {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("artifact is not a regular file")
		}
		digest, size, err := hashFile(path)
		if err != nil {
			return err
		}
		artifacts = append(artifacts, Artifact{Path: relative, SHA256: digest, Size: size})
		return nil
	})
	if err != nil {
		return Manifest{}, fmt.Errorf("build recovery manifest: %w", err)
	}
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].Path < artifacts[j].Path })
	return Manifest{Artifacts: artifacts}, nil
}

func MarshalManifest(manifest Manifest) ([]byte, error) {
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, errors.New("encode recovery manifest")
	}
	return append(encoded, '\n'), nil
}

func decodeManifest(encoded []byte) (Manifest, error) {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, validationError{class: FailureInvalidManifest}
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return Manifest{}, validationError{class: FailureInvalidManifest}
	}
	return manifest, nil
}

func hashFile(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}
