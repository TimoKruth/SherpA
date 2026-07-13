package web

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"sherpa/internal/web/registryclient"
)

type searchPageView struct {
	Home        bool
	Heading     string
	Description string
	Form        searchFormView
	Rows        []searchRowView
	PreviousURL string
	NextURL     string
}

type searchFormView struct {
	Q                  string
	Harness            string
	Tag                string
	AnySelected        bool
	ClaudeCodeSelected bool
	CodexSelected      bool
	OtherHarness       bool
}

type searchRowView struct {
	Ref            string
	StackURL       string
	Summary        string
	Harness        string
	Tags           []string
	Version        int
	TrustTier      string
	ForkedFromText string
	ForkedFromURL  string
}

func (s *server) handleHome(w http.ResponseWriter, r *http.Request) {
	query := registryclient.SearchQuery{Page: registryclient.Page{Limit: searchPageSize}}
	result, err := s.registry.Search(r.Context(), query)
	if err != nil {
		s.renderRegistryError(w, err)
		return
	}
	page := &searchPageView{
		Home:        true,
		Heading:     "Recently published",
		Description: "Discover complete, versioned SherpA agent setups.",
		Form:        newSearchFormView(registryclient.SearchQuery{}),
		Rows:        searchRows(result.Stacks),
	}
	s.renderSearchPage(w, "SherpA", false, page)
}

func (s *server) handleSearch(w http.ResponseWriter, r *http.Request) {
	query, ok := parseSearchQuery(r.URL.Query())
	if !ok {
		s.renderError(w, http.StatusBadRequest)
		return
	}
	result, err := s.registry.Search(r.Context(), query)
	if err != nil {
		s.renderRegistryError(w, err)
		return
	}
	pageNumber := query.Page.Offset/query.Page.Limit + 1
	page := &searchPageView{
		Heading: "Search stacks",
		Form:    newSearchFormView(query),
		Rows:    searchRows(result.Stacks),
	}
	if pageNumber > 1 {
		page.PreviousURL = searchURL(query, pageNumber-1)
	}
	if result.NextOffset != nil {
		page.NextURL = searchURL(query, pageNumber+1)
	}
	s.renderSearchPage(w, "Search", true, page)
}

func (s *server) renderSearchPage(w http.ResponseWriter, title string, noIndex bool, page *searchPageView) {
	if err := s.renderer.render(w, http.StatusOK, pageData{Title: title, NoIndex: noIndex, SearchPage: page}); err != nil {
		s.logger.Printf("render search page failed error_type=%T", err)
		writeFallbackError(w)
	}
}

func newSearchFormView(query registryclient.SearchQuery) searchFormView {
	return searchFormView{
		Q:                  query.Q,
		Harness:            query.Harness,
		Tag:                query.Tag,
		AnySelected:        query.Harness == "",
		ClaudeCodeSelected: query.Harness == "claude-code",
		CodexSelected:      query.Harness == "codex",
		OtherHarness:       query.Harness != "" && query.Harness != "claude-code" && query.Harness != "codex",
	}
}

func searchRows(stacks []registryclient.SearchStack) []searchRowView {
	rows := make([]searchRowView, 0, len(stacks))
	for _, stack := range stacks {
		row := searchRowView{
			Ref:            stack.Ref,
			Summary:        stack.Summary,
			Harness:        stack.Harness,
			Tags:           append([]string(nil), stack.Tags...),
			Version:        stack.Version,
			TrustTier:      stack.TrustTier,
			ForkedFromText: stack.ForkedFrom,
		}
		if validSegment(stack.Owner) && validSegment(stack.Name) {
			row.StackURL = stackPath(stack.Owner, stack.Name)
		}
		if owner, name, ok := parseProvenance(stack.ForkedFrom); ok {
			row.ForkedFromURL = stackPath(owner, name)
		}
		rows = append(rows, row)
	}
	return rows
}

func parseProvenance(value string) (owner, name string, ok bool) {
	if value == "" || strings.TrimSpace(value) != value || !strings.HasPrefix(value, "@") {
		return "", "", false
	}
	ref := strings.TrimPrefix(value, "@")
	if versionAt := strings.LastIndex(ref, "@v"); versionAt >= 0 {
		if _, valid := parsePositiveInteger(ref[versionAt+2:]); !valid {
			return "", "", false
		}
		ref = ref[:versionAt]
	} else if strings.Contains(ref, "@") {
		return "", "", false
	}
	parts := strings.Split(ref, "/")
	if len(parts) != 2 || !validSegment(parts[0]) || !validSegment(parts[1]) {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func stackPath(owner, name string) string {
	return (&url.URL{Path: "/stacks/" + owner + "/" + name}).String()
}

func searchURL(query registryclient.SearchQuery, page int) string {
	values := url.Values{}
	if query.Q != "" {
		values.Set("q", query.Q)
	}
	if query.Harness != "" {
		values.Set("harness", query.Harness)
	}
	if query.Tag != "" {
		values.Set("tag", query.Tag)
	}
	if page > 1 {
		values.Set("page", strconv.Itoa(page))
	}
	return (&url.URL{Path: "/search", RawQuery: values.Encode()}).String()
}
