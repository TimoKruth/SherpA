package web

import (
	"errors"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"sherpa/internal/web/registryclient"
)

func TestHomeRendersRecentCatalog(t *testing.T) {
	registry := &fakeRegistry{searchResult: registryclient.SearchResult{
		Stacks: []registryclient.SearchStack{{
			Ref:        "@alice/reviewer",
			Owner:      "alice",
			Name:       "reviewer",
			Summary:    "Strict code review",
			Harness:    "codex",
			Tags:       []string{"review", "security"},
			Version:    3,
			TrustTier:  "linked",
			ForkedFrom: "@origin/base@v2",
		}},
	}}
	handler := newTestHandler(t, registry)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if len(registry.searchCalls) != 1 || registry.searchCalls[0] != (registryclient.SearchQuery{Page: registryclient.Page{Limit: 24}}) {
		t.Fatalf("search calls = %#v", registry.searchCalls)
	}
	body := rr.Body.String()
	for _, want := range []string{
		"Recently published", "Discover complete, versioned SherpA agent setups.",
		`href="/stacks/alice/reviewer"`, "@alice/reviewer", "Strict code review",
		"codex", "review", "security", "v3", "linked",
		`href="/stacks/origin/base"`, "@origin/base@v2",
		`class="harness-chip"`, `class="trust-tier trust-tier--linked"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("home omitted %q: %s", want, body)
		}
	}
}

func TestTrustTierClassUsesFixedAllowlist(t *testing.T) {
	for _, test := range []struct {
		tier string
		want string
	}{
		{tier: "linked", want: "trust-tier--linked"},
		{tier: "unreviewed", want: "trust-tier--unreviewed"},
		{tier: "unknown"},
		{tier: `linked" onclick="alert(1)`},
	} {
		if got := trustTierClass(test.tier); got != test.want {
			t.Fatalf("trustTierClass(%q) = %q, want %q", test.tier, got, test.want)
		}
	}
}

func TestSearchForwardsFiltersAndRendersPagination(t *testing.T) {
	next := 48
	registry := &fakeRegistry{searchResult: registryclient.SearchResult{NextOffset: &next}}
	handler := newTestHandler(t, registry)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/search?q=red+team&harness=codex&tag=security&page=2", nil))

	wantQuery := registryclient.SearchQuery{
		Q: "red team", Harness: "codex", Tag: "security",
		Page: registryclient.Page{Limit: searchPageSize, Offset: searchPageSize},
	}
	if rr.Code != http.StatusOK || len(registry.searchCalls) != 1 || registry.searchCalls[0] != wantQuery {
		t.Fatalf("response = %d, search calls = %#v", rr.Code, registry.searchCalls)
	}
	body := html.UnescapeString(rr.Body.String())
	for _, want := range []string{
		`name="q" type="search" maxlength="200" value="red team"`,
		`option value="codex" selected`,
		`name="tag" maxlength="64" value="security"`,
		`href="/search?harness=codex&q=red+team&tag=security" rel="prev"`,
		`href="/search?harness=codex&page=3&q=red+team&tag=security" rel="next"`,
		`name="robots" content="noindex,follow"`,
		"No stacks found.",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("search omitted %q: %s", want, body)
		}
	}
}

func TestSearchFirstAndLastPageNavigation(t *testing.T) {
	for _, tc := range []struct {
		name         string
		path         string
		next         *int
		wantPrevious bool
		wantNext     bool
	}{
		{name: "first with next", path: "/search", next: intPointer(24), wantNext: true},
		{name: "later final", path: "/search?page=3", wantPrevious: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := newTestHandler(t, &fakeRegistry{searchResult: registryclient.SearchResult{NextOffset: tc.next}})
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, tc.path, nil))
			body := rr.Body.String()
			if got := strings.Contains(body, `rel="prev"`); got != tc.wantPrevious {
				t.Fatalf("previous present = %v, body = %s", got, body)
			}
			if got := strings.Contains(body, `rel="next"`); got != tc.wantNext {
				t.Fatalf("next present = %v, body = %s", got, body)
			}
		})
	}
}

func TestSearchRetainsUnknownHarnessSafely(t *testing.T) {
	handler := newTestHandler(t, &fakeRegistry{})
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/search?harness=custom%3Cmode%3E", nil))
	body := rr.Body.String()
	if !strings.Contains(body, `option value="custom&lt;mode&gt;" selected`) || !strings.Contains(body, `>custom&lt;mode&gt;</option>`) {
		t.Fatalf("custom harness was not safely retained: %s", body)
	}
}

func TestProvenanceParsing(t *testing.T) {
	for _, tc := range []struct {
		value string
		owner string
		name  string
		ok    bool
	}{
		{value: "@origin/base", owner: "origin", name: "base", ok: true},
		{value: "@origin/base@v12", owner: "origin", name: "base", ok: true},
		{value: "origin/base"},
		{value: "@origin/base@v0"},
		{value: "@origin/base@v-1"},
		{value: "@origin/base@v2/extra"},
		{value: "@origin/javascript:alert(1)"},
		{value: " @origin/base"},
		{value: "@origin/base "},
		{value: "@origin/base@v2@v3"},
	} {
		owner, name, ok := parseProvenance(tc.value)
		if owner != tc.owner || name != tc.name || ok != tc.ok {
			t.Fatalf("parseProvenance(%q) = %q, %q, %v", tc.value, owner, name, ok)
		}
	}
}

func TestSearchEscapesAdversarialPublisherAndQueryData(t *testing.T) {
	payload := `<script>alert("stored")</script>`
	registry := &fakeRegistry{searchResult: registryclient.SearchResult{
		Stacks: []registryclient.SearchStack{{
			Ref:        payload,
			Owner:      "../evil",
			Name:       `reviewer" onclick="alert(1)`,
			Summary:    `<img src=x onerror=alert(1)>`,
			Harness:    `<svg onload=alert(1)>`,
			Tags:       []string{`</li><script>alert(1)</script>`, strings.Repeat("x", 512)},
			Version:    1,
			TrustTier:  `linked" onmouseover="alert(1)`,
			ForkedFrom: `javascript:alert(1)`,
		}},
	}}
	handler := newTestHandler(t, registry)
	rr := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/search?q="+url.QueryEscape(`<svg/onload=alert(1)>`), nil)
	request.Host = "attacker.example"
	request.Header.Set("X-Forwarded-Host", "attacker.example")
	handler.ServeHTTP(rr, request)
	body := rr.Body.String()

	for _, unsafe := range []string{"<script>alert", "<img src=x", "<svg onload", `href="javascript:`, `onclick="`, `onmouseover="`} {
		if strings.Contains(strings.ToLower(body), strings.ToLower(unsafe)) {
			t.Fatalf("live unsafe markup %q in: %s", unsafe, body)
		}
	}
	for _, safeText := range []string{`&lt;script&gt;`, `&lt;img src=x onerror=alert(1)&gt;`, `javascript:alert(1)`, strings.Repeat("x", 512), `value="&lt;svg/onload=alert(1)&gt;"`} {
		if !strings.Contains(body, safeText) {
			t.Fatalf("escaped text omitted %q: %s", safeText, body)
		}
	}
	if strings.Contains(body, `/stacks/../evil`) || strings.Contains(body, "attacker.example") {
		t.Fatalf("unsafe or request-derived link rendered: %s", body)
	}
}

func TestSearchUpstreamErrorsRemainBranded(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{
		{err: registryclient.ErrBadGateway, status: http.StatusBadGateway},
		{err: registryclient.ErrUnavailable, status: http.StatusServiceUnavailable},
		{err: errors.New("unknown planted upstream body"), status: http.StatusBadGateway},
	} {
		handler := newTestHandler(t, &fakeRegistry{err: tc.err})
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/search?q=safe", nil))
		if rr.Code != tc.status || !strings.Contains(rr.Body.String(), "SherpA") || strings.Contains(rr.Body.String(), "planted upstream") {
			t.Fatalf("response = %d %s", rr.Code, rr.Body.String())
		}
	}
}

func intPointer(value int) *int { return &value }
