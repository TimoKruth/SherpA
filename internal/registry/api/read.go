package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"sherpa/internal/registry/store"
)

type searchResponse struct {
	Stacks []searchStackResponse `json:"stacks"`
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
	Name       string           `json:"name"`
	Owner      string           `json:"owner"`
	Summary    string           `json:"summary"`
	Tags       []string         `json:"tags"`
	Harness    string           `json:"harness"`
	ForkedFrom string           `json:"forked_from"`
	Versions   []versionSummary `json:"versions"`
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
}

func (s *server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	matches, err := s.store.Search(r.Context(), q.Get("q"), q.Get("harness"), q.Get("tag"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	resp := searchResponse{Stacks: make([]searchStackResponse, 0, len(matches))}
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
			RepoURL:    repoURL(r, match.Owner, match.Name),
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *server) handleStack(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("name")

	stack, versions, err := s.store.GetStack(r.Context(), owner, name)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "stack not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	resp := stackResponse{
		Name:       stack.Name,
		Owner:      stack.Owner,
		Summary:    stack.Summary,
		Tags:       tagsOrEmpty(stack.Tags),
		Harness:    stack.Harness,
		ForkedFrom: stack.ForkedFrom,
		Versions:   make([]versionSummary, 0, len(versions)),
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
		writeError(w, http.StatusInternalServerError, "internal server error")
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
	})
}

func repoURL(r *http.Request, owner, name string) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/v1/stacks/" + owner + "/" + name + ".git"
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
