package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"sherpa/internal/web/registryclient"
)

func TestExpertProfileRendersCountsPaginationAndEscapesPublisherData(t *testing.T) {
	next := searchPageSize * 2
	registry := &fakeRegistry{userResult: registryclient.UserProfile{
		Handle: "alice", TotalStackFollows: 42, NextOffset: &next,
		Stacks: []registryclient.SearchStack{{
			Ref: "@alice/reviewer", Owner: "alice", Name: "reviewer", Version: 3,
			Summary: "<script>planted()</script>", Harness: "codex", FollowerCount: 11,
			RepoURL: "https://registry.example/v1/stacks/alice/reviewer.git",
		}},
	}}
	base, _ := url.Parse("https://web.example")
	handler := newTestHandler(t, registry, Options{PublicBaseURL: base})
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "https://web.example/users/alice?page=2", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{"@alice", "42 stack follows", "@alice/reviewer", "11", `/users/alice`, `/users/alice?page=3`, `rel="canonical" href="https://web.example/users/alice"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, "<script>planted()") || !strings.Contains(body, "&lt;script&gt;planted()&lt;/script&gt;") {
		t.Fatalf("publisher HTML not escaped: %s", body)
	}
	if len(registry.userCalls) != 1 || registry.userCalls[0].page != (registryclient.Page{Limit: searchPageSize, Offset: searchPageSize}) {
		t.Fatalf("calls=%v", registry.userCalls)
	}
}

func TestExpertProfileRejectsInvalidAndMapsMissing(t *testing.T) {
	for _, tc := range []struct {
		path   string
		err    error
		status int
		calls  int
	}{
		{path: "/users/bad!", status: http.StatusNotFound},
		{path: "/users/alice?page=0", status: http.StatusBadRequest},
		{path: "/users/missing", err: registryclient.ErrNotFound, status: http.StatusNotFound, calls: 1},
	} {
		registry := &fakeRegistry{err: tc.err}
		handler := newTestHandler(t, registry)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rr.Code != tc.status || len(registry.userCalls) != tc.calls {
			t.Fatalf("%s status=%d calls=%v", tc.path, rr.Code, registry.userCalls)
		}
	}
}
