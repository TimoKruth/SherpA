package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"sherpa/internal/registry/store"
)

type searchResponse struct {
	Stacks     []searchStackResponse `json:"stacks"`
	NextOffset *int                  `json:"next_offset,omitempty"`
}

type searchStackResponse struct {
	Ref        string   `json:"ref"`
	Name       string   `json:"name"`
	Owner      string   `json:"owner"`
	Summary    string   `json:"summary"`
	Tags       []string `json:"tags"`
	Harness    string   `json:"harness"`
	Version    int      `json:"version"`
	TrustTier  string   `json:"trust_tier"`
	ForkedFrom string   `json:"forked_from"`
	RepoURL    string   `json:"repo_url"`
}

type stackResponse struct {
	Name               string           `json:"name"`
	Owner              string           `json:"owner"`
	Summary            string           `json:"summary"`
	Tags               []string         `json:"tags"`
	Harness            string           `json:"harness"`
	ForkedFrom         string           `json:"forked_from"`
	RepoURL            string           `json:"repo_url"`
	Versions           []versionSummary `json:"versions"`
	NextVersionsOffset *int             `json:"next_versions_offset,omitempty"`
}

type versionSummary struct {
	Version     int    `json:"version"`
	PublishedAt string `json:"published_at"`
	Changelog   string `json:"changelog"`
	ScanSummary string `json:"scan_summary"`
	TrustTier   string `json:"trust_tier"`
}

type versionResponse struct {
	Version     int             `json:"version"`
	GitTag      string          `json:"git_tag"`
	Manifest    json.RawMessage `json:"manifest"`
	ScanReport  json.RawMessage `json:"scan_report"`
	Changelog   string          `json:"changelog"`
	PublishedAt string          `json:"published_at"`
	TrustTier   string          `json:"trust_tier"`
	RepoURL     string          `json:"repo_url"`
}

const maxReadPageSize = 50

func (s *server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, offset, err := parsePage(r, "limit", "offset")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid pagination")
		return
	}
	maxRows := 0
	if limit > 0 {
		maxRows = limit + 1
	}
	matches, err := s.store.Search(r.Context(), q.Get("q"), q.Get("harness"), q.Get("tag"), maxRows, offset)
	if err != nil {
		internalServerError(w, "search stacks", err)
		return
	}

	var nextOffset *int
	if limit > 0 && len(matches) > limit {
		matches = matches[:limit]
		next := offset + limit
		nextOffset = &next
	}
	resp := searchResponse{Stacks: make([]searchStackResponse, 0, len(matches)), NextOffset: nextOffset}
	for _, match := range matches {
		resp.Stacks = append(resp.Stacks, searchStackResponse{
			Ref:        "@" + match.Owner + "/" + match.Name,
			Name:       match.Name,
			Owner:      match.Owner,
			Summary:    match.Summary,
			Tags:       tagsOrEmpty(match.Tags),
			Harness:    match.Harness,
			Version:    match.Version,
			TrustTier:  match.TrustTier,
			ForkedFrom: match.ForkedFrom,
			RepoURL:    s.repoURL(r, match.Owner, match.Name),
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *server) handleStack(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("name")

	limit, offset, err := parsePage(r, "versions_limit", "versions_offset")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid pagination")
		return
	}
	maxVersions := 0
	if limit > 0 {
		maxVersions = limit + 1
	}
	stack, versions, err := s.store.GetStack(r.Context(), owner, name, maxVersions, offset)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "stack not found")
		return
	}
	if err != nil {
		internalServerError(w, "get stack", err)
		return
	}

	var nextOffset *int
	if limit > 0 && len(versions) > limit {
		versions = versions[:limit]
		next := offset + limit
		nextOffset = &next
	}
	resp := stackResponse{
		Name:               stack.Name,
		Owner:              stack.Owner,
		Summary:            stack.Summary,
		Tags:               tagsOrEmpty(stack.Tags),
		Harness:            stack.Harness,
		ForkedFrom:         stack.ForkedFrom,
		RepoURL:            s.repoURL(r, stack.Owner, stack.Name),
		Versions:           make([]versionSummary, 0, len(versions)),
		NextVersionsOffset: nextOffset,
	}
	for _, version := range versions {
		resp.Versions = append(resp.Versions, versionSummary{
			Version:     version.Version,
			PublishedAt: formatTime(version.PublishedAt),
			Changelog:   version.Changelog,
			ScanSummary: scanSummary(version.ScanReport),
			TrustTier:   version.TrustTier,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *server) handleVersion(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("name")
	v, err := strconv.Atoi(r.PathValue("version"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid version")
		return
	}

	version, err := s.store.GetVersion(r.Context(), owner, name, v)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "version not found")
		return
	}
	if err != nil {
		internalServerError(w, "get version", err)
		return
	}

	writeJSON(w, http.StatusOK, versionResponse{
		Version:     version.Version,
		GitTag:      version.GitTag,
		Manifest:    rawOrEmptyObject(version.Manifest),
		ScanReport:  rawOrEmptyObject(version.ScanReport),
		Changelog:   version.Changelog,
		PublishedAt: formatTime(version.PublishedAt),
		TrustTier:   version.TrustTier,
		RepoURL:     s.repoURL(r, owner, name),
	})
}

func parsePage(r *http.Request, limitKey, offsetKey string) (int, int, error) {
	query := r.URL.Query()
	limitRaw, hasLimit := query[limitKey]
	_, hasOffset := query[offsetKey]
	if !hasLimit {
		if hasOffset {
			return 0, 0, errors.New("offset requires limit")
		}
		return 0, 0, nil
	}
	if len(limitRaw) != 1 {
		return 0, 0, errors.New("limit must occur once")
	}
	limit, err := strconv.Atoi(limitRaw[0])
	if err != nil || limit <= 0 || limit > maxReadPageSize {
		return 0, 0, errors.New("invalid limit")
	}
	offsetRaw := query[offsetKey]
	if len(offsetRaw) > 1 {
		return 0, 0, errors.New("offset must occur at most once")
	}
	offset := 0
	if len(offsetRaw) == 1 {
		offset, err = strconv.Atoi(offsetRaw[0])
		if err != nil || offset < 0 {
			return 0, 0, errors.New("invalid offset")
		}
	}
	return limit, offset, nil
}

func (s *server) repoURL(r *http.Request, owner, name string) string {
	path := "/v1/stacks/" + owner + "/" + name + ".git"
	if s.publicBaseURL != "" {
		return s.publicBaseURL + path
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if s.trustProxy {
		forwardedScheme := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")))
		if forwardedScheme == "http" || forwardedScheme == "https" {
			scheme = forwardedScheme
		}
	}
	return scheme + "://" + r.Host + path
}

func tagsOrEmpty(tags []string) []string {
	if tags == nil {
		return []string{}
	}
	return tags
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func rawOrEmptyObject(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`{}`)
	}
	return raw
}

func scanSummary(raw json.RawMessage) string {
	var report struct {
		Findings []json.RawMessage `json:"findings"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &report) != nil || len(report.Findings) == 0 {
		return "clean"
	}
	if len(report.Findings) == 1 {
		return "1 finding"
	}
	return fmt.Sprintf("%d findings", len(report.Findings))
}
