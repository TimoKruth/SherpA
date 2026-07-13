package web

import (
	"bytes"
	"encoding/json"
	"html"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"sherpa/internal/web/registryclient"
)

const testRepoURL = "https://registry.example/v1/stacks/alice/reviewer.git"

func TestStackRendersMetadataCommandsVersionsAndPagination(t *testing.T) {
	next := 75
	registry := &fakeRegistry{stackResult: registryclient.Stack{
		Summary:    "Strict review",
		Harness:    "codex",
		Tags:       []string{"review", "security"},
		ForkedFrom: "@origin/base@v2",
		RepoURL:    testRepoURL,
		Versions: []registryclient.VersionSummary{
			{Version: 3, PublishedAt: time.Date(2026, 7, 13, 12, 30, 0, 0, time.FixedZone("EEST", 3*60*60)), Changelog: "Third", ScanSummary: "clean", TrustTier: "linked"},
			{Version: 2, PublishedAt: time.Date(2026, 7, 12, 9, 0, 0, 0, time.UTC), Changelog: "Second", ScanSummary: "1 finding", TrustTier: "unreviewed"},
		},
		NextVersionsOffset: &next,
	}}
	handler := newTestHandler(t, registry)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/stacks/alice/reviewer?versions_page=2", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if len(registry.stackCalls) != 1 || registry.stackCalls[0].page != (registryclient.Page{Limit: 25, Offset: 25}) {
		t.Fatalf("stack calls = %#v", registry.stackCalls)
	}
	body := html.UnescapeString(rr.Body.String())
	for _, want := range []string{
		"@alice/reviewer", "Strict review", "codex", "review", "security", "linked",
		`href="/stacks/origin/base"`, "@origin/base@v2",
		"sherpa try " + testRepoURL, "sherpa clone " + testRepoURL,
		`href="/stacks/alice/reviewer/v/3"`, "Third", "clean",
		`datetime="2026-07-13T09:30:00Z"`, "2026-07-13 09:30 UTC",
		`href="/stacks/alice/reviewer" rel="prev"`,
		`href="/stacks/alice/reviewer?versions_page=3" rel="next"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("stack omitted %q: %s", want, body)
		}
	}
	if strings.Index(body, "Version 3") > strings.Index(body, "Version 2") {
		t.Fatalf("version order changed: %s", body)
	}
}

func TestStackHandlesEmptyVersions(t *testing.T) {
	handler := newTestHandler(t, &fakeRegistry{stackResult: registryclient.Stack{RepoURL: testRepoURL}})
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/stacks/alice/reviewer", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "No versions published.") {
		t.Fatalf("response = %d %s", rr.Code, rr.Body.String())
	}
}

func TestCommandsUseOnlyAPIRepoURL(t *testing.T) {
	publicBase := &url.URL{Scheme: "https", Host: "website.example", Path: "/prefix"}
	registry := &fakeRegistry{stackResult: registryclient.Stack{RepoURL: testRepoURL}}
	handler := newTestHandler(t, registry, Options{PublicBaseURL: publicBase, Logger: log.New(io.Discard, "", 0)})
	req := httptest.NewRequest(http.MethodGet, "http://attacker.example/stacks/alice/reviewer", nil)
	req.Host = "attacker.example"
	req.Header.Set("Host", "attacker.example")
	req.Header.Set("Forwarded", "host=attacker.example")
	req.Header.Set("X-Forwarded-Host", "attacker.example")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	body := rr.Body.String()
	if rr.Code != http.StatusOK || !strings.Contains(body, "sherpa try "+testRepoURL) || !strings.Contains(body, "sherpa clone "+testRepoURL) {
		t.Fatalf("response = %d %s", rr.Code, body)
	}
	if strings.Contains(body, "attacker.example") || strings.Contains(body, "website.example") {
		t.Fatalf("request/website host altered command: %s", body)
	}
}

func TestUnsafeRepoURLBecomesBadGateway(t *testing.T) {
	for _, repoURL := range []string{
		"/relative/repo.git",
		"javascript:alert(1)",
		"https://user:password@registry.example/repo.git",
		"https://registry.example/repo.git?ref=v2",
		"https://registry.example/repo.git#v2",
		"https://registry.example/repo.git;touch-pwned",
	} {
		t.Run(repoURL, func(t *testing.T) {
			handler := newTestHandler(t, &fakeRegistry{stackResult: registryclient.Stack{RepoURL: repoURL}})
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/stacks/alice/reviewer", nil))
			if rr.Code != http.StatusBadGateway || strings.Contains(rr.Body.String(), repoURL) || strings.Contains(rr.Body.String(), "sherpa try") {
				t.Fatalf("response = %d %s", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestVersionRendersManifestScanAndLatestCommandsWithoutExcerpt(t *testing.T) {
	excerpt := "scan-excerpt-secret-should-never-render"
	manifest := json.RawMessage(`{
      "name":"reviewer","owner":"@alice","version":2,"harness":"codex","summary":"Review code",
      "tags":["review"],
      "executes":{"hooks":[{"path":"hooks/check.sh","event":"pre-tool","purpose":"Check"}],"mcp_servers":[{"name":"docs","transport":"stdio","command":"serve-docs","purpose":"Docs"}]},
      "parameters":[{"name":"strict","description":"Strict mode","required":true}],
      "future":"<script>unknown manifest field</script>"
    }`)
	registry := &fakeRegistry{versionResult: registryclient.Version{
		Version:     2,
		GitTag:      "v2",
		Manifest:    manifest,
		ScanReport:  registryclient.ScanReport{Findings: []registryclient.ScanFinding{{File: `<script>file</script>`, Line: 7, Kind: `secret<script>`, Excerpt: excerpt}}},
		Changelog:   `<img src=x onerror=alert(1)>`,
		PublishedAt: time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC),
		TrustTier:   "linked",
		RepoURL:     testRepoURL,
	}}
	var logs bytes.Buffer
	handler := newTestHandler(t, registry, Options{Logger: log.New(&logs, "", 0)})
	req := httptest.NewRequest(http.MethodGet, "/stacks/alice/reviewer/v/2", nil)
	req.Host = "attacker.example"
	req.Header.Set("X-Forwarded-Host", "attacker.example")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{
		"@alice/reviewer version 2", "v2", "linked", "2026-07-13 10:00 UTC",
		"Try latest", "sherpa try " + testRepoURL, "sherpa clone " + testRepoURL,
		"reviewer", "@alice", "codex", "Review code", "hooks/check.sh", "pre-tool",
		"MCP servers", "serve-docs", "Parameters", "strict", "required", "Additional fields",
		"1 finding(s)", `&lt;script&gt;file&lt;/script&gt;`, `secret&lt;script&gt;`,
		`&lt;img src=x onerror=alert(1)&gt;`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("version omitted %q: %s", want, body)
		}
	}
	for _, forbidden := range []string{excerpt, "attacker.example", testRepoURL + "@v2", testRepoURL + "?ref=", testRepoURL + "v2"} {
		if strings.Contains(body, forbidden) || strings.Contains(logs.String(), forbidden) {
			t.Fatalf("forbidden value %q rendered/logged: body=%s logs=%s", forbidden, body, logs.String())
		}
	}
	if strings.Contains(body, "<script>") || strings.Contains(body, "<img") {
		t.Fatalf("live publisher markup rendered: %s", body)
	}
	if _, exists := reflect.TypeOf(ScanFindingView{}).FieldByName("Excerpt"); exists {
		t.Fatal("ScanFindingView exposes Excerpt")
	}
}

func TestVersionEmptyScanAndMalformedManifest(t *testing.T) {
	valid := &fakeRegistry{versionResult: registryclient.Version{
		Version: 1, Manifest: json.RawMessage(`{}`), RepoURL: testRepoURL,
	}}
	handler := newTestHandler(t, valid)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/stacks/alice/reviewer/v/1", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "Registry scan passed with no findings.") {
		t.Fatalf("empty scan = %d %s", rr.Code, rr.Body.String())
	}

	for _, manifest := range []json.RawMessage{nil, json.RawMessage(`{"name":`), json.RawMessage(`[]`)} {
		handler := newTestHandler(t, &fakeRegistry{versionResult: registryclient.Version{
			Version: 1, Manifest: manifest, RepoURL: testRepoURL,
		}})
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/stacks/alice/reviewer/v/1", nil))
		if rr.Code != http.StatusBadGateway || (len(manifest) > 0 && strings.Contains(rr.Body.String(), string(manifest))) {
			t.Fatalf("malformed manifest = %d %s", rr.Code, rr.Body.String())
		}
	}
}

func TestVersionMalformedScanResponseMapsToBadGateway(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
          "version":1,"git_tag":"v1","manifest":{},
          "scan_report":{"findings":"not-an-array"},
          "published_at":"2026-07-13T10:00:00Z","trust_tier":"linked",
          "repo_url":"https://registry.example/v1/stacks/alice/reviewer.git"
        }`))
	}))
	defer upstream.Close()
	client, err := registryclient.New(upstream.URL, time.Second)
	if err != nil {
		t.Fatalf("registry client: %v", err)
	}
	handler := newTestHandler(t, client)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/stacks/alice/reviewer/v/1", nil))
	if rr.Code != http.StatusBadGateway || !strings.Contains(rr.Body.String(), "Registry response error") {
		t.Fatalf("response = %d %s", rr.Code, rr.Body.String())
	}
}
