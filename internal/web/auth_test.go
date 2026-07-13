package web

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"sherpa/internal/web/registryclient"
)

type fakeAuthRegistry struct {
	exchangeGrant  string
	exchangeNonce  string
	exchangeResult registryclient.WebSession
	exchangeErr    error
	meResult       registryclient.Me
	meErr          error
	meToken        string
	revokeToken    string
	revokeErr      error
	revokeCalls    int
}

func (*fakeAuthRegistry) Search(context.Context, registryclient.SearchQuery) (registryclient.SearchResult, error) {
	return registryclient.SearchResult{}, nil
}
func (*fakeAuthRegistry) GetStack(context.Context, string, string, registryclient.Page) (registryclient.Stack, error) {
	return registryclient.Stack{}, registryclient.ErrNotFound
}
func (*fakeAuthRegistry) GetVersion(context.Context, string, string, int) (registryclient.Version, error) {
	return registryclient.Version{}, registryclient.ErrNotFound
}
func (f *fakeAuthRegistry) ExchangeWebGrant(_ context.Context, g, n string) (registryclient.WebSession, error) {
	f.exchangeGrant, f.exchangeNonce = g, n
	return f.exchangeResult, f.exchangeErr
}
func (f *fakeAuthRegistry) Me(_ context.Context, t string) (registryclient.Me, error) {
	f.meToken = t
	return f.meResult, f.meErr
}
func (f *fakeAuthRegistry) Revoke(_ context.Context, t string) error {
	f.revokeCalls++
	f.revokeToken = t
	return f.revokeErr
}
func (*fakeAuthRegistry) Follow(context.Context, string, string, string) (registryclient.Follow, error) {
	return registryclient.Follow{}, nil
}
func (*fakeAuthRegistry) Unfollow(context.Context, string, string, string) error { return nil }
func (*fakeAuthRegistry) Follows(context.Context, string, int, string) (registryclient.FollowPage, error) {
	return registryclient.FollowPage{}, nil
}
func (*fakeAuthRegistry) Updates(context.Context, string, int, string) (registryclient.UpdatePage, error) {
	return registryclient.UpdatePage{}, nil
}
func (*fakeAuthRegistry) MarkSeen(context.Context, string, string, string, int) (registryclient.Follow, error) {
	return registryclient.Follow{}, nil
}
func (*fakeAuthRegistry) PutTrial(context.Context, string, string, string, int, string) error {
	return nil
}

func TestLoginUsesPinnedRegistryAndExactPrivateCookie(t *testing.T) {
	fake := &fakeAuthRegistry{}
	handler := newAuthHandler(t, fake, nil)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://evil.test/login?return_to=https://evil.test", nil)
	req.Host = "evil.test"
	req.Header.Set("X-Forwarded-Host", "evil.test")
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	location, _ := url.Parse(rr.Header().Get("Location"))
	if location.Scheme != "https" || location.Host != "registry.example" || location.Path != "/registry/v1/auth/web/start" || !validWebToken(location.Query().Get("handoff_challenge")) {
		t.Fatalf("location=%q", location.String())
	}
	cookie := cookieNamed(t, rr, loginCookieName)
	assertPrivateCookie(t, cookie, int(loginCookieTTL.Seconds()))
}

func TestAuthCallbackExchangesNonceSetsSessionAndClearsQuery(t *testing.T) {
	var logs bytes.Buffer
	fake := &fakeAuthRegistry{exchangeResult: registryclient.WebSession{AccessToken: strings.Repeat("a", 64), Login: "alice", Purpose: "web"}}
	handler := newAuthHandler(t, fake, log.New(&logs, "", 0))
	nonce, _ := randomWebToken()
	grant := strings.Repeat("a", 64)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "https://web.example/auth/callback?grant="+grant, nil)
	req.AddCookie(&http.Cookie{Name: loginCookieName, Value: nonce})
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/dashboard" || fake.exchangeGrant != grant || fake.exchangeNonce != nonce {
		t.Fatalf("response=%d location=%q exchange=%q/%q", rr.Code, rr.Header().Get("Location"), fake.exchangeGrant, fake.exchangeNonce)
	}
	assertClearedCookie(t, rr, loginCookieName)
	session := cookieNamed(t, rr, sessionCookieName)
	csrf := cookieNamed(t, rr, csrfCookieName)
	assertPrivateCookie(t, session, int(webSessionCookieTTL.Seconds()))
	assertPrivateCookie(t, csrf, int(webSessionCookieTTL.Seconds()))
	if session.Value != strings.Repeat("a", 64) || !validWebToken(csrf.Value) {
		t.Fatalf("session=%#v csrf=%#v", session, csrf)
	}
	for _, secret := range []string{grant, nonce, strings.Repeat("a", 64)} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("logs contain %q: %s", secret, logs.String())
		}
	}
}

func TestAuthCallbackRejectsMalformedOrDuplicateCookiesWithoutExchange(t *testing.T) {
	for _, tc := range []string{"missing", "duplicate", "bad-nonce", "bad-grant", "duplicate-grant", "registry-error"} {
		t.Run(tc, func(t *testing.T) {
			fake := &fakeAuthRegistry{exchangeResult: registryclient.WebSession{AccessToken: strings.Repeat("a", 64), Login: "alice", Purpose: "web"}}
			if tc == "registry-error" {
				fake.exchangeErr = registryclient.ErrGone
			}
			handler := newAuthHandler(t, fake, nil)
			nonce, _ := randomWebToken()
			target := "https://web.example/auth/callback?grant=" + strings.Repeat("a", 64)
			if tc == "bad-grant" {
				target = "https://web.example/auth/callback?grant=bad"
			}
			if tc == "duplicate-grant" {
				target += "&grant=" + strings.Repeat("b", 64)
			}
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, target, nil)
			if tc != "missing" {
				value := nonce
				if tc == "bad-nonce" {
					value = "bad"
				}
				req.AddCookie(&http.Cookie{Name: loginCookieName, Value: value})
				if tc == "duplicate" {
					req.AddCookie(&http.Cookie{Name: loginCookieName, Value: value})
				}
			}
			handler.ServeHTTP(rr, req)
			if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/auth/error" {
				t.Fatalf("response=%d location=%q", rr.Code, rr.Header().Get("Location"))
			}
			assertClearedCookie(t, rr, loginCookieName)
			if tc != "registry-error" && fake.exchangeGrant != "" {
				t.Fatalf("exchange called with %q", fake.exchangeGrant)
			}
		})
	}
}

func TestLogoutRequiresExactOriginAndCSRFThenAlwaysClears(t *testing.T) {
	fake := &fakeAuthRegistry{revokeErr: registryclient.ErrUnavailable}
	handler := newAuthHandler(t, fake, nil)
	csrf, _ := randomWebToken()
	valid := func(origin, formToken string, duplicate bool) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		body := "csrf_token=" + url.QueryEscape(formToken)
		req := httptest.NewRequest(http.MethodPost, "https://web.example/logout", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if origin != "" {
			req.Header.Add("Origin", origin)
			if duplicate {
				req.Header.Add("Origin", origin)
			}
		}
		req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: csrf})
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "web-session"})
		handler.ServeHTTP(rr, req)
		return rr
	}
	for _, rr := range []*httptest.ResponseRecorder{valid("", csrf, false), valid("https://evil.example", csrf, false), valid("https://web.example", "wrong", false), valid("https://web.example", csrf, true)} {
		if rr.Code != http.StatusForbidden {
			t.Fatalf("invalid logout status=%d", rr.Code)
		}
		if len(rr.Header().Values("Set-Cookie")) != 0 {
			t.Fatalf("invalid logout cleared cookies: %v", rr.Header().Values("Set-Cookie"))
		}
	}
	rr := valid("https://web.example", csrf, false)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/" || fake.revokeCalls != 1 || fake.revokeToken != "web-session" {
		t.Fatalf("logout=%d location=%q revoke=%d/%q", rr.Code, rr.Header().Get("Location"), fake.revokeCalls, fake.revokeToken)
	}
	for _, name := range []string{loginCookieName, sessionCookieName, csrfCookieName} {
		assertClearedCookie(t, rr, name)
	}
}

func TestCurrentSessionClearsOnlyUnauthorizedCredentials(t *testing.T) {
	for _, tc := range []struct {
		err   error
		clear bool
	}{{registryclient.ErrUnauthorized, true}, {registryclient.ErrUnavailable, false}} {
		fake := &fakeAuthRegistry{meErr: tc.err}
		handlerServer := newAuthServer(t, fake, nil)
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "token"})
		_, _, ok := handlerServer.currentSession(rr, req)
		if ok {
			t.Fatal("invalid session accepted")
		}
		if (len(rr.Header().Values("Set-Cookie")) > 0) != tc.clear {
			t.Fatalf("error=%v cookies=%v", tc.err, rr.Header().Values("Set-Cookie"))
		}
	}
	fake := &fakeAuthRegistry{meResult: registryclient.Me{Login: "alice", Purpose: "cli"}}
	s := newAuthServer(t, fake, nil)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "cli-token"})
	if _, _, ok := s.currentSession(rr, req); ok || len(rr.Header().Values("Set-Cookie")) == 0 {
		t.Fatalf("CLI-purpose browser cookie accepted or retained")
	}
}

func newAuthHandler(t *testing.T, fake *fakeAuthRegistry, logger *log.Logger) http.Handler {
	t.Helper()
	s := newAuthServer(t, fake, logger)
	return s.securityHeaders(s.logRequests(http.HandlerFunc(s.route)))
}
func newAuthServer(t *testing.T, fake *fakeAuthRegistry, logger *log.Logger) *server {
	t.Helper()
	renderer, err := newRenderer()
	if err != nil {
		t.Fatal(err)
	}
	web, _ := url.Parse("https://web.example")
	registry, _ := url.Parse("https://registry.example/registry")
	if logger == nil {
		logger = log.New(&bytes.Buffer{}, "", 0)
	}
	return &server{registry: fake, authRegistry: fake, renderer: renderer, publicBaseURL: web, registryPublicURL: registry, logger: logger}
}
func cookieNamed(t *testing.T, rr *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, c := range rr.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("cookie %s missing: %v", name, rr.Header().Values("Set-Cookie"))
	return nil
}
func assertPrivateCookie(t *testing.T, c *http.Cookie, maxAge int) {
	t.Helper()
	if !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.MaxAge != maxAge || c.Domain != "" {
		t.Fatalf("cookie=%#v", c)
	}
}
func assertClearedCookie(t *testing.T, rr *httptest.ResponseRecorder, name string) {
	t.Helper()
	for _, header := range rr.Header().Values("Set-Cookie") {
		if strings.HasPrefix(header, name+"=") && strings.Contains(header, "Path=/") && strings.Contains(header, "Max-Age=0") && strings.Contains(header, "HttpOnly") && strings.Contains(header, "Secure") && strings.Contains(header, "SameSite=Lax") {
			return
		}
	}
	t.Fatalf("cookie %s was not securely cleared: %v", name, rr.Header().Values("Set-Cookie"))
}
