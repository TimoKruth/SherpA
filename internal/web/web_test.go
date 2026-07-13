package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"sherpa/internal/web/registryclient"
)

type fakeRegistry struct {
	searchResult  registryclient.SearchResult
	stackResult   registryclient.Stack
	versionResult registryclient.Version
	err           error
	searchCalls   []registryclient.SearchQuery
	stackCalls    []struct {
		owner string
		name  string
		page  registryclient.Page
	}
	versionCalls []struct {
		owner   string
		name    string
		version int
	}
}

func (f *fakeRegistry) Search(_ context.Context, query registryclient.SearchQuery) (registryclient.SearchResult, error) {
	f.searchCalls = append(f.searchCalls, query)
	return f.searchResult, f.err
}

func (f *fakeRegistry) GetStack(_ context.Context, owner, name string, page registryclient.Page) (registryclient.Stack, error) {
	f.stackCalls = append(f.stackCalls, struct {
		owner string
		name  string
		page  registryclient.Page
	}{owner: owner, name: name, page: page})
	return f.stackResult, f.err
}

func (f *fakeRegistry) GetVersion(_ context.Context, owner, name string, version int) (registryclient.Version, error) {
	f.versionCalls = append(f.versionCalls, struct {
		owner   string
		name    string
		version int
	}{owner: owner, name: name, version: version})
	return f.versionResult, f.err
}

func (f *fakeRegistry) calls() int {
	return len(f.searchCalls) + len(f.stackCalls) + len(f.versionCalls)
}

func newTestHandler(t *testing.T, registry Registry, options ...Options) http.Handler {
	t.Helper()
	option := Options{Logger: log.New(io.Discard, "", 0)}
	if len(options) > 0 {
		option = options[0]
	}
	handler, err := New(registry, option)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return handler
}

func TestExactRoutesAndInputs(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		call string
	}{
		{name: "home", path: "/", call: "search"},
		{name: "search", path: "/search", call: "search"},
		{name: "stack", path: "/stacks/alice/reviewer", call: "stack"},
		{name: "version", path: "/stacks/alice/reviewer/v/2", call: "version"},
		{name: "health", path: "/healthz"},
		{name: "robots", path: "/robots.txt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry := &fakeRegistry{
				stackResult: registryclient.Stack{RepoURL: "https://registry.example/v1/stacks/alice/reviewer.git"},
				versionResult: registryclient.Version{
					Version: 2, Manifest: json.RawMessage(`{}`),
					RepoURL: "https://registry.example/v1/stacks/alice/reviewer.git",
				},
			}
			handler := newTestHandler(t, registry)
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
			}
			switch tc.call {
			case "search":
				if len(registry.searchCalls) != 1 {
					t.Fatalf("search calls = %d", len(registry.searchCalls))
				}
			case "stack":
				if len(registry.stackCalls) != 1 {
					t.Fatalf("stack calls = %d", len(registry.stackCalls))
				}
			case "version":
				if len(registry.versionCalls) != 1 {
					t.Fatalf("version calls = %d", len(registry.versionCalls))
				}
			default:
				if registry.calls() != 0 {
					t.Fatalf("registry calls = %d", registry.calls())
				}
			}
		})
	}
}

func TestSearchAndStackPageParsing(t *testing.T) {
	registry := &fakeRegistry{stackResult: registryclient.Stack{RepoURL: "https://registry.example/v1/stacks/alice/reviewer.git"}}
	handler := newTestHandler(t, registry)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/search?q=++review++&harness=codex&tag=safe&page=2", nil))
	if rr.Code != http.StatusOK || len(registry.searchCalls) != 1 {
		t.Fatalf("search = %d, calls = %#v", rr.Code, registry.searchCalls)
	}
	wantSearch := registryclient.SearchQuery{
		Q: "review", Harness: "codex", Tag: "safe",
		Page: registryclient.Page{Limit: searchPageSize, Offset: searchPageSize},
	}
	if registry.searchCalls[0] != wantSearch {
		t.Fatalf("search query = %#v, want %#v", registry.searchCalls[0], wantSearch)
	}

	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/stacks/alice/reviewer?versions_page=3", nil))
	if rr.Code != http.StatusOK || len(registry.stackCalls) != 1 {
		t.Fatalf("stack = %d, calls = %#v", rr.Code, registry.stackCalls)
	}
	if got := registry.stackCalls[0]; got.owner != "alice" || got.name != "reviewer" || got.page != (registryclient.Page{Limit: versionPageSize, Offset: 2 * versionPageSize}) {
		t.Fatalf("stack call = %#v", got)
	}
}

func TestInvalidInputMakesNoRegistryCall(t *testing.T) {
	invalid := []struct {
		path   string
		status int
	}{
		{path: "/missing", status: http.StatusNotFound},
		{path: "/search/", status: http.StatusNotFound},
		{path: "/stacks/.alice/reviewer", status: http.StatusNotFound},
		{path: "/stacks/alice/review%2Fer", status: http.StatusNotFound},
		{path: "/stacks/alice/réviewer", status: http.StatusNotFound},
		{path: "/stacks/alice/reviewer/v/0", status: http.StatusNotFound},
		{path: "/stacks/alice/reviewer/v/-1", status: http.StatusNotFound},
		{path: "/stacks/alice/reviewer/v/+1", status: http.StatusNotFound},
		{path: "/stacks/alice/reviewer/v/nope", status: http.StatusNotFound},
		{path: "/search?q=" + url.QueryEscape(strings.Repeat("q", maxQueryBytes+1)), status: http.StatusBadRequest},
		{path: "/search?q=" + url.QueryEscape(strings.Repeat(" ", maxQueryBytes+1)), status: http.StatusBadRequest},
		{path: "/search?harness=" + strings.Repeat("h", maxFilterBytes+1), status: http.StatusBadRequest},
		{path: "/search?tag=" + strings.Repeat("t", maxFilterBytes+1), status: http.StatusBadRequest},
		{path: "/search?page=", status: http.StatusBadRequest},
		{path: "/search?page=0", status: http.StatusBadRequest},
		{path: "/search?page=-1", status: http.StatusBadRequest},
		{path: "/search?page=%2B1", status: http.StatusBadRequest},
		{path: "/search?page=two", status: http.StatusBadRequest},
		{path: "/search?page=999999999999999999999999", status: http.StatusBadRequest},
		{path: "/search?page=1&page=2", status: http.StatusBadRequest},
		{path: "/stacks/alice/reviewer?versions_page=0", status: http.StatusBadRequest},
	}
	for _, tc := range invalid {
		t.Run(tc.path, func(t *testing.T) {
			registry := &fakeRegistry{}
			handler := newTestHandler(t, registry)
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if rr.Code != tc.status {
				t.Fatalf("status = %d, want %d; body = %s", rr.Code, tc.status, rr.Body.String())
			}
			if registry.calls() != 0 {
				t.Fatalf("registry calls = %d", registry.calls())
			}
		})
	}
}

func TestNonGETRejectedWithoutRegistryCall(t *testing.T) {
	for _, path := range []string{"/", "/search", "/stacks/alice/reviewer", "/stacks/alice/reviewer/v/1", "/static/app.css", "/healthz", "/unknown"} {
		registry := &fakeRegistry{}
		handler := newTestHandler(t, registry)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, path, strings.NewReader("planted-body")))
		if rr.Code != http.StatusMethodNotAllowed || rr.Header().Get("Allow") != http.MethodGet {
			t.Fatalf("POST %s = %d, Allow=%q", path, rr.Code, rr.Header().Get("Allow"))
		}
		if registry.calls() != 0 {
			t.Fatalf("POST %s made %d calls", path, registry.calls())
		}
	}
}

func TestRegistryErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		status     int
		retryAfter string
	}{
		{name: "not found", err: registryclient.ErrNotFound, status: http.StatusNotFound},
		{name: "bad gateway", err: registryclient.ErrBadGateway, status: http.StatusBadGateway},
		{name: "unavailable", err: registryclient.ErrUnavailable, status: http.StatusServiceUnavailable, retryAfter: "60"},
		{name: "unknown upstream error", err: errors.New("planted-upstream-body-and-credential"), status: http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := newTestHandler(t, &fakeRegistry{err: tc.err})
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
			if rr.Code != tc.status || rr.Header().Get("Retry-After") != tc.retryAfter {
				t.Fatalf("response = %d, Retry-After=%q", rr.Code, rr.Header().Get("Retry-After"))
			}
			if rr.Header().Get("Cache-Control") != "no-store" || rr.Header().Get("X-Robots-Tag") != "noindex" || !strings.Contains(rr.Body.String(), `name="robots" content="noindex"`) {
				t.Fatalf("error indexing/cache headers or body missing: headers=%v body=%s", rr.Header(), rr.Body.String())
			}
			if strings.Contains(rr.Body.String(), "planted-upstream") || strings.Contains(rr.Body.String(), tc.err.Error()) {
				t.Fatalf("public error contains cause: %s", rr.Body.String())
			}
		})
	}
}

func TestTemplateFailureDoesNotLeakPartialSuccess(t *testing.T) {
	broken := template.Must(template.New("pages").Funcs(template.FuncMap{
		"fail": func() (string, error) { return "", errors.New("planted-template-cause") },
	}).Parse(`{{define "base"}}partial-success{{fail}}{{end}}`))
	s := &server{
		renderer: &renderer{templates: broken},
		logger:   log.New(io.Discard, "", 0),
	}
	rr := httptest.NewRecorder()
	s.renderSuccess(rr, "test")
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "partial-success") || strings.Contains(rr.Body.String(), "planted-template-cause") {
		t.Fatalf("partial output or cause leaked: %s", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "Page unavailable") {
		t.Fatalf("fallback missing: %s", rr.Body.String())
	}
}

func TestSecurityHeadersAndContentTypes(t *testing.T) {
	handler := newTestHandler(t, &fakeRegistry{})
	for _, tc := range []struct {
		path        string
		contentType string
	}{
		{path: "/", contentType: "text/html; charset=utf-8"},
		{path: "/missing", contentType: "text/html; charset=utf-8"},
		{path: "/healthz", contentType: "text/plain; charset=utf-8"},
		{path: "/robots.txt", contentType: "text/plain; charset=utf-8"},
	} {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if got := rr.Header().Get("Content-Type"); got != tc.contentType {
			t.Fatalf("%s Content-Type = %q", tc.path, got)
		}
		wantHeaders := map[string]string{
			"Content-Security-Policy": contentSecurityPolicy,
			"X-Content-Type-Options":  "nosniff",
			"Referrer-Policy":         "no-referrer",
			"X-Frame-Options":         "DENY",
			"Permissions-Policy":      "camera=(), microphone=(), geolocation=(), payment=(), usb=()",
		}
		for key, want := range wantHeaders {
			if got := rr.Header().Get(key); got != want {
				t.Fatalf("%s %s = %q, want %q", tc.path, key, got, want)
			}
		}
	}
}

func TestRequestLogsExcludeSecrets(t *testing.T) {
	var output bytes.Buffer
	logger := log.New(&output, "", 0)
	registry := &fakeRegistry{err: errors.New("upstream-body-secret")}
	handler := newTestHandler(t, registry, Options{Logger: logger})
	req := httptest.NewRequest(http.MethodGet, "/search?q=query-secret", nil)
	req.Header.Set("Authorization", "Bearer header-secret")
	req.Header.Set("Cookie", "session=cookie-secret")
	req.Header.Set("X-Railway-Request-Id", "valid-prefix\nrequest-id-secret")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	logged := output.String()
	for _, secret := range []string{"query-secret", "header-secret", "cookie-secret", "upstream-body-secret", "request-id-secret"} {
		if strings.Contains(logged, secret) {
			t.Fatalf("log contains %q: %s", secret, logged)
		}
	}
	for _, want := range []string{"method=GET", `path="/search"`, "status=502", "duration=", `railway_request_id=""`} {
		if !strings.Contains(logged, want) {
			t.Fatalf("log missing %q: %s", want, logged)
		}
	}
}

func TestSafeRequestIDIsBoundedAndNewlineSafe(t *testing.T) {
	if got := safeRequestID(strings.Repeat("a", 200)); len(got) != 128 {
		t.Fatalf("request ID length = %d, want 128", len(got))
	}
	if got := safeRequestID("valid\nforged"); got != "" {
		t.Fatalf("newline request ID = %q, want empty", got)
	}
}

func TestNewRequiresRegistry(t *testing.T) {
	if handler, err := New(nil, Options{}); err == nil || handler != nil {
		t.Fatalf("New(nil) = %#v, %v", handler, err)
	}
}
