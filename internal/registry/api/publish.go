package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"sherpa/internal/gitutil"
	"sherpa/internal/harness"
	"sherpa/internal/publishscan"
	"sherpa/internal/registry/content"
	"sherpa/internal/registry/store"
	"sherpa/internal/sanitize"
	"sherpa/internal/stack"

	"gopkg.in/yaml.v3"
)

const maxPublishBundleBytes = 50 << 20

func (s *server) handlePublish(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("name")

	_, trustTier, authStatus, err := s.authorizePublish(r, owner)
	if err != nil {
		if authStatus == http.StatusInternalServerError {
			log.Printf("authorize publish: %v", err)
			writeError(w, authStatus, "internal server error")
		} else {
			writeError(w, authStatus, err.Error())
		}
		return
	}
	if !validPublishSegment(owner) || !validPublishSegment(name) {
		writeError(w, http.StatusBadRequest, "invalid owner or name")
		return
	}

	bundle, err := readMultipartBundle(w, r)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errBundleTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		writeError(w, status, err.Error())
		return
	}

	stageDir, worktreeDir, cleanup, err := s.content.StageBundle(bundle, "")
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid bundle")
		return
	}
	defer cleanup()

	manifestBytes, err := os.ReadFile(filepath.Join(worktreeDir, "stack.yaml"))
	if err != nil {
		writeValidationError(w, []string{"stack.yaml missing or unreadable"})
		return
	}
	m, err := stack.Parse(manifestBytes)
	if err != nil {
		writeValidationError(w, []string{err.Error()})
		return
	}
	if m.Name != name {
		writeValidationError(w, []string{fmt.Sprintf("stack.yaml name %q does not match path name %q", m.Name, name)})
		return
	}
	if m.Owner != "" && strings.TrimPrefix(m.Owner, "@") != owner {
		writeValidationError(w, []string{fmt.Sprintf("stack.yaml owner %q does not match path owner %q", m.Owner, owner)})
		return
	}
	gitTag := "v" + strconv.Itoa(m.Version)
	if !stagedTagExists(stageDir, gitTag) {
		writeValidationError(w, []string{fmt.Sprintf("stack.yaml version %d has no matching tag %s", m.Version, gitTag)})
		return
	}
	pointsAtHead, err := stagedTagPointsAtHead(stageDir, gitTag)
	if err != nil {
		internalServerError(w, "resolve staged tag", err)
		return
	}
	if !pointsAtHead {
		writeValidationError(w, []string{fmt.Sprintf("version tag %s does not point at the published HEAD", gitTag)})
		return
	}

	h, err := harness.For(m.Harness)
	if err != nil {
		writeValidationError(w, []string{err.Error()})
		return
	}
	if violations := m.Validate(worktreeDir, h); len(violations) > 0 {
		writeValidationError(w, violations)
		return
	}

	findings, err := publishscan.ScanRepo(worktreeDir, h, "")
	if err != nil {
		internalServerError(w, "scan published content", err)
		return
	}
	if len(findings) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"findings": publishFindingResponses(findings)})
		return
	}

	if _, err := s.store.GetVersion(r.Context(), owner, name, m.Version); err == nil {
		writeError(w, http.StatusConflict, "version already exists")
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		internalServerError(w, "lookup version", err)
		return
	}

	stagedCommit, err := gitutil.Run(stageDir, "rev-parse", gitTag+"^{commit}")
	if err != nil {
		internalServerError(w, "resolve staged tag", err)
		return
	}
	orphanCommit, orphanErr := s.content.TagCommit(owner, name, gitTag)
	adoptOrphan := false
	switch {
	case orphanErr == nil && strings.TrimSpace(orphanCommit) == strings.TrimSpace(stagedCommit):
		adoptOrphan = true
	case orphanErr == nil:
		writeError(w, http.StatusConflict, "orphan tag exists with different content; reconcile before publishing")
		return
	case !errors.Is(orphanErr, content.ErrNotFound):
		internalServerError(w, "inspect orphan tag", orphanErr)
		return
	}

	manifestJSON, err := manifestSnapshotJSON(manifestBytes)
	if err != nil {
		internalServerError(w, "snapshot manifest", err)
		return
	}
	scanReportJSON, err := json.Marshal(map[string]any{"findings": publishFindingResponses(findings)})
	if err != nil {
		internalServerError(w, "encode scan report", err)
		return
	}

	if !adoptOrphan {
		if err := s.content.Commit(owner, name, stageDir); err != nil {
			internalServerError(w, "commit published content", err)
			return
		}
	}
	if _, err := s.store.UpsertUser(r.Context(), owner); err != nil {
		internalServerError(w, "upsert owner", err)
		return
	}
	stackID, err := s.store.UpsertStack(r.Context(), store.Stack{
		Owner:      owner,
		Name:       name,
		Summary:    m.Summary,
		Harness:    m.Harness,
		ForkedFrom: m.ForkedFrom,
		Tags:       m.Tags,
	})
	if err != nil {
		internalServerError(w, "upsert stack", err)
		return
	}
	version := store.Version{
		StackID:    stackID,
		Version:    m.Version,
		GitTag:     gitTag,
		Manifest:   manifestJSON,
		ScanReport: scanReportJSON,
		TrustTier:  trustTier,
	}
	if err := s.store.InsertVersion(r.Context(), version); errors.Is(err, store.ErrVersionExists) {
		writeError(w, http.StatusConflict, "version already exists")
		return
	} else if err != nil {
		internalServerError(w, "insert version", err)
		return
	}

	writeJSON(w, http.StatusCreated, versionResponse{
		Version:    version.Version,
		GitTag:     version.GitTag,
		Manifest:   rawOrEmptyObject(version.Manifest),
		ScanReport: rawOrEmptyObject(version.ScanReport),
		Changelog:  version.Changelog,
		TrustTier:  version.TrustTier,
		RepoURL:    s.repoURL(r, owner, name),
	})
}

var errBundleTooLarge = errors.New("bundle too large")

func readMultipartBundle(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxPublishBundleBytes+1)
	reader, err := r.MultipartReader()
	if err != nil {
		return nil, errors.New("expected multipart/form-data with bundle file")
	}
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("missing bundle file")
		}
		if err != nil {
			return nil, errors.New("invalid multipart body")
		}
		if part.FormName() != "bundle" {
			continue
		}
		bundle, err := io.ReadAll(io.LimitReader(part, maxPublishBundleBytes+1))
		if err != nil {
			return nil, errors.New("read bundle file")
		}
		if len(bundle) > maxPublishBundleBytes {
			return nil, errBundleTooLarge
		}
		if len(bundle) == 0 {
			return nil, errors.New("empty bundle file")
		}
		return bundle, nil
	}
}

func writeValidationError(w http.ResponseWriter, violations []string) {
	writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
		"error":      "validation failed",
		"violations": violations,
	})
}

func manifestSnapshotJSON(manifestBytes []byte) (json.RawMessage, error) {
	var snapshot any
	if err := yaml.Unmarshal(manifestBytes, &snapshot); err != nil {
		return nil, err
	}
	b, err := json.Marshal(snapshot)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(b), nil
}

func stagedTagExists(stageDir, tag string) bool {
	out, err := gitutil.Run(stageDir, "tag", "-l", tag)
	return err == nil && strings.TrimSpace(out) == tag
}

func stagedTagPointsAtHead(stageDir, tag string) (bool, error) {
	tagCommit, err := gitutil.Run(stageDir, "rev-parse", tag+"^{commit}")
	if err != nil {
		return false, err
	}
	headCommit, err := gitutil.Run(stageDir, "rev-parse", "HEAD")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(tagCommit) == strings.TrimSpace(headCommit), nil
}

type publishFindingResponse struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Kind    string `json:"kind"`
	Excerpt string `json:"excerpt"`
}

func publishFindingResponses(findings []sanitize.Finding) []publishFindingResponse {
	resp := make([]publishFindingResponse, 0, len(findings))
	for _, finding := range findings {
		resp = append(resp, publishFindingResponse{
			File:    finding.File,
			Line:    finding.Line,
			Kind:    finding.Kind,
			Excerpt: finding.Excerpt,
		})
	}
	return resp
}

func validPublishSegment(s string) bool {
	if s == "" {
		return false
	}
	if strings.ContainsAny(s, `/\`) {
		return false
	}
	if s == ".." || strings.HasPrefix(s, ".") {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '-':
		case r == '.':
		default:
			return false
		}
	}
	return true
}
