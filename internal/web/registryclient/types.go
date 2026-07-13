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

type Me struct {
	Login   string `json:"login"`
	Purpose string `json:"purpose"`
}
type WebSession struct {
	AccessToken string `json:"access_token"`
	Login       string `json:"login"`
	Purpose     string `json:"purpose"`
}
type Follow struct {
	Ref               string    `json:"ref"`
	Owner             string    `json:"owner"`
	Name              string    `json:"name"`
	Summary           string    `json:"summary"`
	Harness           string    `json:"harness"`
	Tags              []string  `json:"tags"`
	LatestVersion     int       `json:"latest_version"`
	LatestGitTag      string    `json:"latest_git_tag"`
	LatestTrustTier   string    `json:"latest_trust_tier"`
	LatestPublishedAt time.Time `json:"latest_published_at"`
	LastSeenVersion   int       `json:"last_seen_version"`
	FollowerCount     int       `json:"follower_count"`
	FollowedAt        time.Time `json:"followed_at"`
}
type Update struct {
	Ref         string    `json:"ref"`
	Owner       string    `json:"owner"`
	Name        string    `json:"name"`
	Version     int       `json:"version"`
	GitTag      string    `json:"git_tag"`
	Changelog   string    `json:"changelog"`
	TrustTier   string    `json:"trust_tier"`
	PublishedAt time.Time `json:"published_at"`
	SeenVersion int       `json:"seen_version"`
}
type FollowPage struct {
	Follows    []Follow `json:"follows"`
	NextCursor string   `json:"next_cursor"`
}
type UpdatePage struct {
	Updates    []Update `json:"updates"`
	NextCursor string   `json:"next_cursor"`
}
type UserProfile struct {
	Handle            string        `json:"handle"`
	TotalStackFollows int           `json:"total_stack_follows"`
	Stacks            []SearchStack `json:"stacks"`
	NextOffset        *int          `json:"next_offset,omitempty"`
}
