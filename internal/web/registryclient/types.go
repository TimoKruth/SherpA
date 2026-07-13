package registryclient

import (
	"encoding/json"
	"time"
)

type Page struct {
	Limit  int
	Offset int
}

type SearchQuery struct {
	Q       string
	Harness string
	Tag     string
	Page    Page
}

type SearchResult struct {
	Stacks     []SearchStack `json:"stacks"`
	NextOffset *int          `json:"next_offset,omitempty"`
}

type SearchStack struct {
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

type Stack struct {
	Name               string           `json:"name"`
	Owner              string           `json:"owner"`
	Summary            string           `json:"summary"`
	Tags               []string         `json:"tags"`
	Harness            string           `json:"harness"`
	ForkedFrom         string           `json:"forked_from"`
	RepoURL            string           `json:"repo_url"`
	Versions           []VersionSummary `json:"versions"`
	NextVersionsOffset *int             `json:"next_versions_offset,omitempty"`
}

type VersionSummary struct {
	Version     int       `json:"version"`
	PublishedAt time.Time `json:"published_at"`
	Changelog   string    `json:"changelog"`
	ScanSummary string    `json:"scan_summary"`
	TrustTier   string    `json:"trust_tier"`
}

type Version struct {
	Version     int             `json:"version"`
	GitTag      string          `json:"git_tag"`
	Manifest    json.RawMessage `json:"manifest"`
	ScanReport  ScanReport      `json:"scan_report"`
	Changelog   string          `json:"changelog"`
	PublishedAt time.Time       `json:"published_at"`
	TrustTier   string          `json:"trust_tier"`
	RepoURL     string          `json:"repo_url"`
}

type ScanReport struct {
	Findings []ScanFinding `json:"findings"`
}

type ScanFinding struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Kind    string `json:"kind"`
	Excerpt string `json:"excerpt"`
}
