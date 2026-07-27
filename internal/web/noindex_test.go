package web

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sherpa/internal/web/registryclient"
)

func TestNoIndexSuppressesIndexingEverywhere(t *testing.T) {
	registry := &fakeRegistry{searchResult: registryclient.SearchResult{
		Stacks: []registryclient.SearchStack{{
			Ref: "@alice/reviewer", Owner: "alice", Name: "reviewer",
			Summary: "a stack", Harness: "claude-code", Version: 1,
		}},
	}}
	handler := newTestHandler(t, registry, Options{
		Logger:  log.New(io.Discard, "", 0),
		NoIndex: true,
	})

	for _, path := range []string{"/", "/search"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)

		if recorder.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", path, recorder.Code)
		}
		if got := recorder.Header().Get("X-Robots-Tag"); got != "noindex" {
			t.Errorf("%s: X-Robots-Tag = %q, want %q", path, got, "noindex")
		}
		body := recorder.Body.String()
		if !strings.Contains(body, `content="noindex,nofollow"`) {
			t.Errorf("%s: page does not carry a noindex robots meta tag", path)
		}
		if strings.Contains(body, `content="index,follow"`) {
			t.Errorf("%s: page still advertises index,follow", path)
		}
	}

	request := httptest.NewRequest(http.MethodGet, "/robots.txt", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if got := recorder.Body.String(); !strings.Contains(got, "Disallow: /") {
		t.Errorf("robots.txt = %q, want a Disallow directive", got)
	}
}

func TestIndexingStaysOnByDefault(t *testing.T) {
	registry := &fakeRegistry{searchResult: registryclient.SearchResult{
		Stacks: []registryclient.SearchStack{{
			Ref: "@alice/reviewer", Owner: "alice", Name: "reviewer",
			Summary: "a stack", Harness: "claude-code", Version: 1,
		}},
	}}
	handler := newTestHandler(t, registry)

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if got := recorder.Header().Get("X-Robots-Tag"); got != "" {
		t.Errorf("X-Robots-Tag = %q, want empty on an indexable deployment", got)
	}
	if body := recorder.Body.String(); !strings.Contains(body, `content="index,follow"`) {
		t.Error("default deployment should remain indexable")
	}

	request = httptest.NewRequest(http.MethodGet, "/robots.txt", nil)
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if got := recorder.Body.String(); !strings.Contains(got, "Allow: /") {
		t.Errorf("robots.txt = %q, want an Allow directive by default", got)
	}
}
