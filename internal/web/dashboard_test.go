package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"sherpa/internal/web/registryclient"
)

func TestStackRendersFollowStateWithoutExposingSession(t *testing.T) {
	fake := &fakeAuthRegistry{
		meResult: registryclient.Me{Login: "alice", Purpose: "web"},
		stackResult: registryclient.Stack{
			Owner: "alice", Name: "reviewer", FollowerCount: 12,
			RepoURL: "https://registry.example/v1/stacks/alice/reviewer.git",
		},
		isFollowing: true,
	}
	handler := newAuthHandler(t, fake, nil)
	csrf := strings.Repeat("c", 32)
	req := authenticatedRequest(http.MethodGet, "https://web.example/stacks/alice/reviewer", "web-session-secret", csrf, "")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{"12", "Followers", "Following", `/stacks/alice/reviewer/unfollow`, `value="` + csrf + `"`, "alice"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, "web-session-secret") {
		t.Fatal("session token reached HTML")
	}
	if len(fake.isFollowingCalls) != 1 || fake.isFollowingCalls[0].owner != "alice" || fake.isFollowingCalls[0].name != "reviewer" {
		t.Fatalf("following calls=%v", fake.isFollowingCalls)
	}
}

func TestStackSignedOutAndExpiredSessionFallback(t *testing.T) {
	for _, expired := range []bool{false, true} {
		fake := &fakeAuthRegistry{stackResult: registryclient.Stack{
			Owner: "alice", Name: "reviewer", FollowerCount: 7,
			RepoURL: "https://registry.example/v1/stacks/alice/reviewer.git",
		}}
		if expired {
			fake.meErr = registryclient.ErrUnauthorized
		}
		handler := newAuthHandler(t, fake, nil)
		req := httptest.NewRequest(http.MethodGet, "https://web.example/stacks/alice/reviewer", nil)
		if expired {
			req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "expired"})
		}
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "Sign in to follow") || strings.Contains(rr.Body.String(), ">Follow</button>") {
			t.Fatalf("expired=%v response=%d body=%s", expired, rr.Code, rr.Body.String())
		}
		if expired && len(rr.Header().Values("Set-Cookie")) == 0 {
			t.Fatal("expired browser session was not cleared")
		}
	}
}

func TestDashboardRendersBoundedPagesWithoutImplicitSeen(t *testing.T) {
	fake := &fakeAuthRegistry{
		meResult: registryclient.Me{Login: "bob", Purpose: "web"},
		followsResult: registryclient.FollowPage{
			Follows:    []registryclient.Follow{{Owner: "alice", Name: "reviewer", Summary: "Review safely", LatestVersion: 2, FollowerCount: 9}},
			NextCursor: "follow+next",
		},
		updatesResult: registryclient.UpdatePage{
			Updates:    []registryclient.Update{{Owner: "alice", Name: "reviewer", Version: 2, SeenVersion: 1, Changelog: "Safer checks", PublishedAt: time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)}},
			NextCursor: "update/next",
		},
	}
	handler := newAuthHandler(t, fake, nil)
	req := authenticatedRequest(http.MethodGet, "https://web.example/dashboard?follows_cursor=old-follow&updates_cursor=old-update", "browser-token", "csrf-value", "")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || rr.Header().Get("Cache-Control") != "no-store" || rr.Header().Get("X-Robots-Tag") != "noindex" {
		t.Fatalf("status=%d headers=%v body=%s", rr.Code, rr.Header(), rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{"Pending updates", "Safer checks", "Mark reviewed", "Review safely", "More follows", "More updates", `name="robots" content="noindex,nofollow"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, "browser-token") || len(fake.seenCalls) != 0 {
		t.Fatalf("token exposed or GET marked seen: calls=%v", fake.seenCalls)
	}
	if len(fake.followsCursors) != 1 || fake.followsCursors[0] != "old-follow" || len(fake.updatesCursors) != 1 || fake.updatesCursors[0] != "old-update" {
		t.Fatalf("cursors follows=%v updates=%v", fake.followsCursors, fake.updatesCursors)
	}
}

func TestDashboardEmptyState(t *testing.T) {
	fake := &fakeAuthRegistry{meResult: registryclient.Me{Login: "bob", Purpose: "web"}}
	handler := newAuthHandler(t, fake, nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, authenticatedRequest(http.MethodGet, "https://web.example/dashboard", "token", "csrf", ""))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "No pending updates.") || !strings.Contains(rr.Body.String(), "You are not following any stacks.") || !strings.Contains(rr.Body.String(), "sherpa updates") {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestDashboardAuthFailuresClearOnlyInvalidSession(t *testing.T) {
	for _, tc := range []struct {
		name       string
		meErr      error
		updatesErr error
		status     int
		location   string
		clear      bool
	}{
		{name: "unauthorized", meErr: registryclient.ErrUnauthorized, status: http.StatusSeeOther, location: "/login", clear: true},
		{name: "registry unavailable", updatesErr: registryclient.ErrUnavailable, status: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeAuthRegistry{meResult: registryclient.Me{Login: "bob", Purpose: "web"}, meErr: tc.meErr, updatesErr: tc.updatesErr}
			handler := newAuthHandler(t, fake, nil)
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, authenticatedRequest(http.MethodGet, "https://web.example/dashboard", "token", "csrf", ""))
			if rr.Code != tc.status || rr.Header().Get("Location") != tc.location {
				t.Fatalf("status=%d location=%q body=%s", rr.Code, rr.Header().Get("Location"), rr.Body.String())
			}
			if (len(rr.Header().Values("Set-Cookie")) > 0) != tc.clear {
				t.Fatalf("cookies=%v", rr.Header().Values("Set-Cookie"))
			}
		})
	}
}

func TestSocialMutationsRequireOriginCSRFAndPOST(t *testing.T) {
	fake := &fakeAuthRegistry{meResult: registryclient.Me{Login: "bob", Purpose: "web"}}
	handler := newAuthHandler(t, fake, nil)
	csrf := "csrf-value"
	for _, tc := range []struct {
		path string
		body string
	}{
		{path: "/stacks/alice/reviewer/follow", body: "csrf_token=" + url.QueryEscape(csrf)},
		{path: "/stacks/alice/reviewer/unfollow", body: "csrf_token=" + url.QueryEscape(csrf)},
		{path: "/dashboard/seen/alice/reviewer", body: "csrf_token=" + url.QueryEscape(csrf) + "&version=2"},
	} {
		bad := authenticatedRequest(http.MethodPost, "https://web.example"+tc.path, "token", csrf, tc.body)
		bad.Header.Set("Origin", "https://evil.example")
		badRR := httptest.NewRecorder()
		handler.ServeHTTP(badRR, bad)
		if badRR.Code != http.StatusForbidden {
			t.Fatalf("bad mutation %s status=%d", tc.path, badRR.Code)
		}

		good := authenticatedRequest(http.MethodPost, "https://web.example"+tc.path, "token", csrf, tc.body)
		good.Header.Set("Origin", "https://web.example")
		goodRR := httptest.NewRecorder()
		handler.ServeHTTP(goodRR, good)
		if goodRR.Code != http.StatusSeeOther {
			t.Fatalf("mutation %s status=%d body=%s", tc.path, goodRR.Code, goodRR.Body.String())
		}
	}
	if len(fake.followCalls) != 1 || len(fake.unfollowCalls) != 1 || len(fake.seenCalls) != 1 || fake.seenCalls[0].version != 2 {
		t.Fatalf("calls follow=%v unfollow=%v seen=%v", fake.followCalls, fake.unfollowCalls, fake.seenCalls)
	}
}

func authenticatedRequest(method, target, token, csrf, body string) *http.Request {
	var reader *strings.Reader
	reader = strings.NewReader(body)
	req := httptest.NewRequest(method, target, reader)
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: csrf})
	return req
}
