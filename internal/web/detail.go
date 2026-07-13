package web

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"sherpa/internal/web/registryclient"
)

type CommandView struct {
	Try   string
	Clone string
}

type stackPageView struct {
	LatestLabel      bool
	Ref              string
	Summary          string
	Harness          string
	Tags             []string
	LatestTrust      string
	LatestTrustClass string
	ForkedFromText   string
	ForkedFromURL    string
	Commands         CommandView
	Versions         []versionRowView
	PreviousURL      string
	NextURL          string
	FollowerCount    int
	ShowFollow       bool
	Following        bool
	FollowAction     string
	CSRFToken        string
	ShowSignIn       bool
	Owner            string
	OwnerURL         string
}

type versionRowView struct {
	Version       int
	URL           string
	PublishedISO  string
	PublishedText string
	Changelog     string
	ScanSummary   string
	TrustTier     string
	TrustClass    string
}

type ScanFindingView struct {
	File string
	Line int
	Kind string
}

type versionPageView struct {
	LatestLabel   bool
	Ref           string
	Version       int
	GitTag        string
	PublishedISO  string
	PublishedText string
	Changelog     string
	TrustTier     string
	TrustClass    string
	Commands      CommandView
	Manifest      manifestView
	Findings      []ScanFindingView
}

func (s *server) handleStack(w http.ResponseWriter, r *http.Request, owner, name string) {
	if !validSegment(owner) || !validSegment(name) {
		s.renderError(w, http.StatusNotFound)
		return
	}
	pageNumber, ok := parsePage(r.URL.Query(), "versions_page")
	if !ok {
		s.renderError(w, http.StatusBadRequest)
		return
	}
	page, ok := pageFromNumber(pageNumber, versionPageSize)
	if !ok {
		s.renderError(w, http.StatusBadRequest)
		return
	}
	stack, err := s.registry.GetStack(r.Context(), owner, name, page)
	if err != nil {
		s.renderRegistryError(w, err)
		return
	}
	commands, ok := commandView(stack.RepoURL)
	if !ok {
		s.renderRegistryError(w, registryclient.ErrBadGateway)
		return
	}
	versions, valid := versionRows(owner, name, stack.Versions)
	if !valid {
		s.renderRegistryError(w, registryclient.ErrBadGateway)
		return
	}
	view := &stackPageView{
		Ref:            "@" + owner + "/" + name,
		Summary:        stack.Summary,
		Harness:        stack.Harness,
		Tags:           append([]string(nil), stack.Tags...),
		ForkedFromText: stack.ForkedFrom,
		Commands:       commands,
		Versions:       versions,
		FollowerCount:  stack.FollowerCount,
		Owner:          "@" + owner,
		OwnerURL:       "/users/" + owner,
	}
	auth := s.authViewFor(w, r)
	view.ShowSignIn = auth.Enabled && !auth.SignedIn
	if auth.SignedIn {
		view.ShowFollow = true
		view.CSRFToken = auth.CSRFToken
		view.FollowAction = stackPath(owner, name) + "/follow"
		if token, ok := strictCookie(r, sessionCookieName, 256); ok {
			following, e := s.authRegistry.IsFollowing(r.Context(), token, owner, name)
			if e == nil && following {
				view.Following = true
				view.FollowAction = stackPath(owner, name) + "/unfollow"
			}
		}
	}
	if len(stack.Versions) > 0 {
		view.LatestTrust = stack.Versions[0].TrustTier
		view.LatestTrustClass = trustTierClass(stack.Versions[0].TrustTier)
	}
	if forkOwner, forkName, valid := parseProvenance(stack.ForkedFrom); valid {
		view.ForkedFromURL = stackPath(forkOwner, forkName)
	}
	if pageNumber > 1 {
		view.PreviousURL = stackPageURL(owner, name, pageNumber-1)
	}
	if stack.NextVersionsOffset != nil {
		view.NextURL = stackPageURL(owner, name, pageNumber+1)
	}
	s.renderDetailPage(w, "@"+owner+"/"+name, pageData{StackPage: view, Auth: auth})
}

func (s *server) handleVersion(w http.ResponseWriter, r *http.Request, owner, name, rawVersion string) {
	if !validSegment(owner) || !validSegment(name) {
		s.renderError(w, http.StatusNotFound)
		return
	}
	versionNumber, ok := parsePositiveInteger(rawVersion)
	if !ok {
		s.renderError(w, http.StatusNotFound)
		return
	}
	version, err := s.registry.GetVersion(r.Context(), owner, name, versionNumber)
	if err != nil {
		s.renderRegistryError(w, err)
		return
	}
	if version.Version <= 0 || version.Version != versionNumber {
		s.renderRegistryError(w, registryclient.ErrBadGateway)
		return
	}
	commands, ok := commandView(version.RepoURL)
	if !ok {
		s.renderRegistryError(w, registryclient.ErrBadGateway)
		return
	}
	manifest, err := buildManifestView(version.Manifest)
	if err != nil {
		s.renderRegistryError(w, registryclient.ErrBadGateway)
		return
	}
	publishedISO, publishedText := displayTime(version.PublishedAt)
	view := &versionPageView{
		LatestLabel:   true,
		Ref:           "@" + owner + "/" + name,
		Version:       version.Version,
		GitTag:        version.GitTag,
		PublishedISO:  publishedISO,
		PublishedText: publishedText,
		Changelog:     version.Changelog,
		TrustTier:     version.TrustTier,
		TrustClass:    trustTierClass(version.TrustTier),
		Commands:      commands,
		Manifest:      manifest,
		Findings:      scanFindingViews(version.ScanReport),
	}
	s.renderDetailPage(w, "@"+owner+"/"+name+" version "+strconv.Itoa(versionNumber), pageData{VersionPage: view, Auth: s.authViewFor(w, r)})
}

func (s *server) renderDetailPage(w http.ResponseWriter, title string, data pageData) {
	data.Title = title
	data.Robots = "index,follow"
	data.OpenGraphTitle = title + " · SherpA"
	switch {
	case data.StackPage != nil:
		data.Description = data.StackPage.Summary
		if data.Description == "" {
			data.Description = "Explore " + data.StackPage.Ref + " versions, trust, and setup commands."
		}
		data.CanonicalURL = s.canonicalURL(stackPathFromRef(data.StackPage.Ref))
	case data.VersionPage != nil:
		data.Description = "Inspect " + data.VersionPage.Ref + " version " + strconv.Itoa(data.VersionPage.Version) + " manifest, trust, and registry scan."
		data.CanonicalURL = s.canonicalURL(versionPathFromRef(data.VersionPage.Ref, data.VersionPage.Version))
	}
	data.OpenGraphURL = data.CanonicalURL
	if err := s.renderer.render(w, http.StatusOK, data); err != nil {
		s.logger.Printf("render detail page failed error_type=%T", err)
		writeFallbackError(w)
	}
}

func stackPathFromRef(ref string) string {
	return "/stacks/" + strings.TrimPrefix(ref, "@")
}

func versionPathFromRef(ref string, version int) string {
	return stackPathFromRef(ref) + "/v/" + strconv.Itoa(version)
}

func commandView(repoURL string) (CommandView, bool) {
	if !validRepositoryURL(repoURL) {
		return CommandView{}, false
	}
	return CommandView{
		Try:   "sherpa try " + repoURL,
		Clone: "sherpa clone " + repoURL,
	}, true
}

func validRepositoryURL(raw string) bool {
	if raw == "" || strings.ContainsAny(raw, " \t\r\n\"'`\\;&|<>()$!*?[]{}#") {
		return false
	}
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" || parsed.Hostname() == "" {
		return false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return false
	}
	return parsed.Opaque == "" && parsed.User == nil && parsed.RawQuery == "" && !parsed.ForceQuery && parsed.Fragment == ""
}

func versionRows(owner, name string, versions []registryclient.VersionSummary) ([]versionRowView, bool) {
	rows := make([]versionRowView, 0, len(versions))
	for _, version := range versions {
		if version.Version <= 0 {
			return nil, false
		}
		publishedISO, publishedText := displayTime(version.PublishedAt)
		row := versionRowView{
			Version:       version.Version,
			PublishedISO:  publishedISO,
			PublishedText: publishedText,
			Changelog:     version.Changelog,
			ScanSummary:   version.ScanSummary,
			TrustTier:     version.TrustTier,
			TrustClass:    trustTierClass(version.TrustTier),
		}
		row.URL = versionPath(owner, name, version.Version)
		rows = append(rows, row)
	}
	return rows, true
}

func scanFindingViews(report registryclient.ScanReport) []ScanFindingView {
	findings := make([]ScanFindingView, 0, len(report.Findings))
	for _, finding := range report.Findings {
		findings = append(findings, ScanFindingView{
			File: finding.File,
			Line: finding.Line,
			Kind: finding.Kind,
		})
	}
	return findings
}

func displayTime(value time.Time) (iso, text string) {
	if value.IsZero() {
		return "", ""
	}
	value = value.UTC()
	return value.Format(time.RFC3339), value.Format("2006-01-02 15:04 UTC")
}

func versionPath(owner, name string, version int) string {
	return (&url.URL{Path: "/stacks/" + owner + "/" + name + "/v/" + strconv.Itoa(version)}).String()
}

func stackPageURL(owner, name string, page int) string {
	values := url.Values{}
	if page > 1 {
		values.Set("versions_page", strconv.Itoa(page))
	}
	return (&url.URL{Path: "/stacks/" + owner + "/" + name, RawQuery: values.Encode()}).String()
}
