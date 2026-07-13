package registryclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewValidatesBaseURLWithoutEchoingInput(t *testing.T) {
	for _, raw := range []string{
		"", "registry.example", "ftp://registry.example", "https:///missing-host",
		"https://user:planted-secret@registry.example", "https://registry.example?planted=secret",
		"https://registry.example/#planted-secret", "https://registry.example/#", "://planted-secret",
	} {
		t.Run(raw, func(t *testing.T) {
			client, err := New(raw, time.Second)
			if err == nil || client != nil {
				t.Fatalf("New(%q) = %#v, %v; want error", raw, client, err)
			}
			if strings.Contains(err.Error(), "planted-secret") || strings.Contains(err.Error(), raw) && raw != "" {
				t.Fatalf("error %q exposes base URL", err)
			}
		})
	}
	if client, err := New("https://registry.example/api/", 0); err == nil || client != nil {
		t.Fatalf("New accepted unbounded timeout: %#v, %v", client, err)
	}
}

func TestSearchEncodesQueryPreservesPrefixAndForwardsNoBrowserHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/prefix/v1/search" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		want := url.Values{
			"q": {"security & review"}, "harness": {"claude/code"}, "tag": {"a+b"},
			"limit": {"24"}, "offset": {"48"},
		}
		if r.URL.Query().Encode() != want.Encode() {
			t.Errorf("query = %q, want %q", r.URL.RawQuery, want.Encode())
		}
		if r.Header.Get("Accept") != "application/json" {
			t.Errorf("Accept = %q", r.Header.Get("Accept"))
		}
		for _, name := range []string{"Cookie", "Authorization", "Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"} {
			if value := r.Header.Get(name); value != "" {
				t.Errorf("forwarded header %s = %q", name, value)
			}
		}
		writeJSON(t, w, `{"stacks":[{"ref":"@alice/reviewer","name":"reviewer","owner":"alice","summary":"safe","tags":[],"harness":"codex","version":2,"trust_tier":"linked","forked_from":"","repo_url":"https://registry.example/v1/stacks/alice/reviewer.git"}],"next_offset":72}`)
	}))
	defer server.Close()

	client := mustClient(t, server.URL+"/api/prefix/", time.Second)
	result, err := client.Search(context.Background(), SearchQuery{
		Q: "security & review", Harness: "claude/code", Tag: "a+b", Page: Page{Limit: 24, Offset: 48},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Stacks) != 1 || result.NextOffset == nil || *result.NextOffset != 72 {
		t.Fatalf("result = %#v", result)
	}
}

func TestAuthenticatedClientMethodsScopeBearerAndUseFixedRoutes(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		if r.URL.Path != "/root/v1/auth/web/exchange" && r.Header.Get("Authorization") != "Bearer web-session" {
			t.Errorf("authorization on %s = %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		if r.URL.Path == "/root/v1/auth/web/exchange" && r.Header.Get("Authorization") != "" {
			t.Errorf("exchange authorization = %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/root/v1/auth/web/exchange":
			writeJSON(t, w, `{"access_token":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","login":"alice","purpose":"web"}`)
		case "/root/v1/me":
			writeJSON(t, w, `{"login":"alice","purpose":"web"}`)
		case "/root/v1/me/follows/alice/reviewer":
			if r.Method == http.MethodDelete {
				w.WriteHeader(http.StatusNoContent)
			} else {
				writeJSON(t, w, `{"ref":"@alice/reviewer","owner":"alice","name":"reviewer","latest_version":2,"followed_at":"2026-07-13T12:00:00Z"}`)
			}
		case "/root/v1/me/follows":
			writeJSON(t, w, `{"follows":[]}`)
		case "/root/v1/me/updates":
			writeJSON(t, w, `{"updates":[]}`)
		case "/root/v1/me/follows/alice/reviewer/seen":
			writeJSON(t, w, `{"ref":"@alice/reviewer","owner":"alice","name":"reviewer","latest_version":2,"last_seen_version":2,"followed_at":"2026-07-13T12:00:00Z"}`)
		case "/root/v1/me/trials/alice/reviewer/2", "/root/v1/me/session":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := mustClient(t, server.URL+"/root", time.Second)
	ctx := context.Background()
	session, err := client.ExchangeWebGrant(ctx, strings.Repeat("a", 64), strings.Repeat("n", 43))
	if err != nil || session.Purpose != "web" {
		t.Fatalf("exchange=%#v %v", session, err)
	}
	if _, err := client.Me(ctx, "web-session"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Follow(ctx, "web-session", "alice", "reviewer"); err != nil {
		t.Fatal(err)
	}
	if following, err := client.IsFollowing(ctx, "web-session", "alice", "reviewer"); err != nil || !following {
		t.Fatalf("following=%v err=%v", following, err)
	}
	if err := client.Unfollow(ctx, "web-session", "alice", "reviewer"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Follows(ctx, "web-session", 25, "cursor"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Updates(ctx, "web-session", 25, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := client.MarkSeen(ctx, "web-session", "alice", "reviewer", 2); err != nil {
		t.Fatal(err)
	}
	if err := client.PutTrial(ctx, "web-session", "alice", "reviewer", 2, "keep"); err != nil {
		t.Fatal(err)
	}
	if err := client.Revoke(ctx, "web-session"); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 10 {
		t.Fatalf("paths = %v", paths)
	}
	if _, err := client.Me(ctx, ""); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("empty token error = %v", err)
	}
}

func TestAuthenticatedClientMapsErrorsAndRejectsRedirects(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   error
	}{{401, ErrUnauthorized}, {403, ErrForbidden}, {404, ErrNotFound}, {410, ErrGone}, {429, ErrRateLimited}, {500, ErrUnavailable}, {418, ErrBadGateway}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(tc.status) }))
		client := mustClient(t, srv.URL, time.Second)
		_, err := client.Me(context.Background(), "secret-token")
		srv.Close()
		if !errors.Is(err, tc.want) || strings.Contains(err.Error(), "secret-token") {
			t.Fatalf("status %d error=%v", tc.status, err)
		}
	}
	targetCalls := 0
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetCalls++ }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer redirect.Close()
	client := mustClient(t, redirect.URL, time.Second)
	if _, err := client.Me(context.Background(), "token"); !errors.Is(err, ErrBadGateway) || targetCalls != 0 {
		t.Fatalf("redirect error=%v target=%d", err, targetCalls)
	}
}

func TestSocialAndProfileResponsesFailClosed(t *testing.T) {
	validStack := `{"ref":"@alice/reviewer","name":"reviewer","owner":"alice","version":2,"repo_url":"https://registry.example/v1/stacks/alice/reviewer.git"}`
	for _, tc := range []struct {
		name string
		body string
		call func(*Client) error
	}{
		{
			name: "profile negative count",
			body: `{"handle":"alice","total_stack_follows":-1,"stacks":[]}`,
			call: func(c *Client) error { _, err := c.GetUser(context.Background(), "alice", Page{Limit: 24}); return err },
		},
		{
			name: "profile malformed stack",
			body: `{"handle":"alice","total_stack_follows":1,"stacks":[` + strings.Replace(validStack, `"version":2`, `"version":0`, 1) + `]}`,
			call: func(c *Client) error { _, err := c.GetUser(context.Background(), "alice", Page{Limit: 24}); return err },
		},
		{
			name: "follow mismatched ref",
			body: `{"ref":"@mallory/reviewer","owner":"alice","name":"reviewer","latest_version":2}`,
			call: func(c *Client) error {
				_, err := c.Follow(context.Background(), "token", "alice", "reviewer")
				return err
			},
		},
		{
			name: "follow impossible seen version",
			body: `{"follows":[{"ref":"@alice/reviewer","owner":"alice","name":"reviewer","latest_version":2,"last_seen_version":3}]}`,
			call: func(c *Client) error { _, err := c.Follows(context.Background(), "token", 25, ""); return err },
		},
		{
			name: "oversized next cursor",
			body: `{"follows":[],"next_cursor":"` + strings.Repeat("x", 513) + `"}`,
			call: func(c *Client) error { _, err := c.Follows(context.Background(), "token", 25, ""); return err },
		},
		{
			name: "non-pending update",
			body: `{"updates":[{"ref":"@alice/reviewer","owner":"alice","name":"reviewer","version":2,"seen_version":2}]}`,
			call: func(c *Client) error { _, err := c.Updates(context.Background(), "token", 25, ""); return err },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, tc.body)
			}))
			defer server.Close()
			if err := tc.call(mustClient(t, server.URL, time.Second)); !errors.Is(err, ErrBadGateway) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestGetUserUsesBoundedPublicRoute(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/root/v1/users/alice" || r.URL.Query().Get("limit") != "24" || r.URL.Query().Get("offset") != "24" || r.Header.Get("Authorization") != "" {
			t.Errorf("request=%s %s?%s auth=%q", r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"))
		}
		writeJSON(t, w, `{"handle":"alice","total_stack_follows":4,"stacks":[{"ref":"@alice/reviewer","name":"reviewer","owner":"alice","version":2,"repo_url":"https://registry.example/v1/stacks/alice/reviewer.git"}],"next_offset":48}`)
	}))
	defer server.Close()
	profile, err := mustClient(t, server.URL+"/root", time.Second).GetUser(context.Background(), "alice", Page{Limit: 24, Offset: 24})
	if err != nil || profile.Handle != "alice" || len(profile.Stacks) != 1 || profile.NextOffset == nil || *profile.NextOffset != 48 {
		t.Fatalf("profile=%#v err=%v", profile, err)
	}
}

func TestIsFollowingTreatsOnlyNotFoundAsFalse(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   error
	}{
		{status: http.StatusNotFound},
		{status: http.StatusUnauthorized, want: ErrUnauthorized},
		{status: http.StatusServiceUnavailable, want: ErrUnavailable},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(tc.status) }))
		following, err := mustClient(t, server.URL, time.Second).IsFollowing(context.Background(), "token", "alice", "reviewer")
		server.Close()
		if following || !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
			t.Fatalf("status=%d following=%v err=%v", tc.status, following, err)
		}
	}
}

func TestDetailRequestsAndTypedResponses(t *testing.T) {
	published := "2026-07-13T08:09:10Z"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/root/v1/stacks/alice.smith/reviewer-1":
			if got := r.URL.Query(); got.Get("versions_limit") != "25" || got.Get("versions_offset") != "25" {
				t.Errorf("stack query = %q", got.Encode())
			}
			writeJSON(t, w, fmt.Sprintf(`{"name":"reviewer-1","owner":"alice.smith","summary":"summary","tags":["review"],"harness":"codex","forked_from":"@base/original","repo_url":"https://registry.example/v1/stacks/alice.smith/reviewer-1.git","versions":[{"version":2,"published_at":%q,"changelog":"changes","scan_summary":"clean","trust_tier":"linked"}],"next_versions_offset":50}`, published))
		case "/root/v1/stacks/alice.smith/reviewer-1/versions/2":
			writeJSON(t, w, fmt.Sprintf(`{"version":2,"git_tag":"v2","manifest":{"name":"reviewer-1"},"scan_report":{"findings":[{"file":"a.txt","line":3,"kind":"secret","excerpt":"client-only"}]},"changelog":"changes","published_at":%q,"trust_tier":"linked","repo_url":"https://registry.example/v1/stacks/alice.smith/reviewer-1.git"}`, published))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := mustClient(t, server.URL+"/root", time.Second)

	stack, err := client.GetStack(context.Background(), "alice.smith", "reviewer-1", Page{Limit: 25, Offset: 25})
	if err != nil {
		t.Fatal(err)
	}
	wantTime, _ := time.Parse(time.RFC3339, published)
	if len(stack.Versions) != 1 || !stack.Versions[0].PublishedAt.Equal(wantTime) || stack.NextVersionsOffset == nil || *stack.NextVersionsOffset != 50 {
		t.Fatalf("stack = %#v", stack)
	}
	version, err := client.GetVersion(context.Background(), "alice.smith", "reviewer-1", 2)
	if err != nil {
		t.Fatal(err)
	}
	if !version.PublishedAt.Equal(wantTime) || len(version.ScanReport.Findings) != 1 || version.ScanReport.Findings[0].Excerpt != "client-only" || string(version.Manifest) != `{"name":"reviewer-1"}` {
		t.Fatalf("version = %#v", version)
	}
}

func TestInvalidInputsMakeNoRequest(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	client := mustClient(t, server.URL, time.Second)

	invalidPages := []Page{{}, {Limit: -1}, {Limit: 51}, {Limit: 1, Offset: -1}}
	for _, page := range invalidPages {
		if _, err := client.Search(context.Background(), SearchQuery{Page: page}); !errors.Is(err, ErrBadGateway) {
			t.Errorf("Search page %#v error = %v", page, err)
		}
		if _, err := client.GetStack(context.Background(), "alice", "stack", page); !errors.Is(err, ErrBadGateway) {
			t.Errorf("GetStack page %#v error = %v", page, err)
		}
	}
	for _, pair := range [][2]string{{"", "name"}, {"..", "name"}, {".hidden", "name"}, {"a/b", "name"}, {"owner", `a\b`}, {"owner", "snowman-☃"}} {
		if _, err := client.GetStack(context.Background(), pair[0], pair[1], Page{Limit: 1}); !errors.Is(err, ErrBadGateway) {
			t.Errorf("GetStack(%q,%q) error = %v", pair[0], pair[1], err)
		}
		if _, err := client.GetVersion(context.Background(), pair[0], pair[1], 1); !errors.Is(err, ErrBadGateway) {
			t.Errorf("GetVersion(%q,%q) error = %v", pair[0], pair[1], err)
		}
	}
	if _, err := client.GetVersion(context.Background(), "alice", "stack", 0); !errors.Is(err, ErrBadGateway) {
		t.Errorf("GetVersion zero error = %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid inputs made %d requests", calls.Load())
	}
}

func TestResponseClassification(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		contentType string
		body        string
		want        error
	}{
		{name: "not found", status: 404, contentType: "text/plain", body: "planted-credential", want: ErrNotFound},
		{name: "other 4xx", status: 400, contentType: "application/json", body: `{}`, want: ErrBadGateway},
		{name: "server error", status: 503, contentType: "text/plain", body: "planted-credential", want: ErrUnavailable},
		{name: "wrong content type", status: 200, contentType: "text/html", body: `{}`, want: ErrBadGateway},
		{name: "malformed JSON", status: 200, contentType: "application/json", body: `{"stacks":`, want: ErrBadGateway},
		{name: "oversized", status: 200, contentType: "application/json", body: strings.Repeat("x", maxResponseBytes+1), want: ErrBadGateway},
		{name: "invalid repo URL", status: 200, contentType: "application/json", body: `{"stacks":[{"repo_url":"https://user:planted-credential@registry.example/repo.git"}]}`, want: ErrBadGateway},
		{name: "shell-dangerous repo URL", status: 200, contentType: "application/json", body: `{"stacks":[{"repo_url":"https://registry.example/repo.git;planted-credential"}]}`, want: ErrBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			client := mustClient(t, server.URL, time.Second)
			_, err := client.Search(context.Background(), SearchQuery{Page: Page{Limit: 1}})
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			for _, planted := range []string{server.URL, "planted-credential", "stacks"} {
				if strings.Contains(err.Error(), planted) {
					t.Fatalf("public error %q exposes %q", err, planted)
				}
			}
		})
	}
}

func TestGetStackRejectsMismatchedIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, `{"name":"other","owner":"mallory","repo_url":"https://registry.example/v1/stacks/mallory/other.git","versions":[]}`)
	}))
	defer server.Close()
	client := mustClient(t, server.URL, time.Second)
	if _, err := client.GetStack(context.Background(), "alice", "reviewer", Page{Limit: 25}); !errors.Is(err, ErrBadGateway) {
		t.Fatalf("mismatched stack identity error = %v", err)
	}
}

func TestTransportTimeoutAndCancellation(t *testing.T) {
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	client := mustClient(t, closed.URL, time.Second)
	closed.Close()
	if _, err := client.Search(context.Background(), SearchQuery{Page: Page{Limit: 1}}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("transport error = %v", err)
	}

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		writeJSON(t, w, `{"stacks":[]}`)
	}))
	defer slow.Close()
	client = mustClient(t, slow.URL, 10*time.Millisecond)
	if _, err := client.Search(context.Background(), SearchQuery{Page: Page{Limit: 1}}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("timeout error = %v", err)
	}

	started := make(chan struct{})
	cancelled := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer cancelled.Close()
	client = mustClient(t, cancelled.URL, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := client.Search(ctx, SearchQuery{Page: Page{Limit: 1}})
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
}

func TestRedirectsAreRejectedWithoutReachingTarget(t *testing.T) {
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetCalls.Add(1)
		writeJSON(t, w, `{"stacks":[]}`)
	}))
	defer target.Close()

	for _, location := range []string{"/redirected?planted-credential=value", target.URL + "/cross?planted-credential=value"} {
		redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", location)
			w.WriteHeader(http.StatusTemporaryRedirect)
		}))
		client := mustClient(t, redirect.URL, time.Second)
		_, err := client.Search(context.Background(), SearchQuery{Page: Page{Limit: 1}})
		redirect.Close()
		if !errors.Is(err, ErrBadGateway) {
			t.Fatalf("redirect %q error = %v", location, err)
		}
		if strings.Contains(err.Error(), "planted-credential") || strings.Contains(err.Error(), redirect.URL) {
			t.Fatalf("redirect error %q leaks request data", err)
		}
	}
	if targetCalls.Load() != 0 {
		t.Fatalf("redirect target received %d calls", targetCalls.Load())
	}
}

func mustClient(t *testing.T, baseURL string, timeout time.Duration) *Client {
	t.Helper()
	client, err := New(baseURL, timeout)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func writeJSON(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write([]byte(body))
}
