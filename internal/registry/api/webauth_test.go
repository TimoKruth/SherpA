package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	registryauth "sherpa/internal/registry/auth"
	"sherpa/internal/registry/store"
)

func TestWebOAuthStartCallbackAndGrantExchange(t *testing.T) {
	st := newPublishSpyStore()
	gh := &registryauth.FakeGitHubClient{
		AuthorizeURL: "https://github.example/login/oauth/authorize?fixed=1",
		WebToken:     "github-token",
		Users:        map[string]registryauth.GitHubUser{"github-token": {ID: 42, Login: "alice"}},
	}
	clock := &authTestClock{now: time.Unix(1_700_000_000, 0)}
	h := NewWithOptions(st, newPublishSpyContent(t), "admin", gh, Options{
		Now: clock.Now, PublicBaseURL: "https://registry.example/prefix", WebPublicBaseURL: "https://web.example/app",
	})
	nonce := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	challenge := sha256Challenge(nonce)
	start := httptest.NewRecorder()
	startReq := httptest.NewRequest(http.MethodGet, "http://attacker.invalid/v1/auth/web/start?handoff_challenge="+url.QueryEscape(challenge), nil)
	startReq.Header.Set("X-Forwarded-Host", "evil.example")
	h.ServeHTTP(start, startReq)
	if start.Code != http.StatusSeeOther || start.Header().Get("Location") != gh.AuthorizeURL {
		t.Fatalf("start = %d location=%q body=%s", start.Code, start.Header().Get("Location"), start.Body.String())
	}
	cookies := start.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %#v", cookies)
	}
	cookie := cookies[0]
	if cookie.Name != oauthCookieName || !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" || cookie.MaxAge != int(oauthCookieTTL.Seconds()) {
		t.Fatalf("OAuth cookie = %#v", cookie)
	}
	if gh.LastWebCallback != "https://registry.example/prefix/v1/auth/web/callback" || !validBase64URLBytes(gh.LastWebState, 32) || !validSHA256Challenge(gh.LastWebChallenge) {
		t.Fatalf("GitHub params state=%q challenge=%q callback=%q", gh.LastWebState, gh.LastWebChallenge, gh.LastWebCallback)
	}

	callback := httptest.NewRecorder()
	callbackReq := httptest.NewRequest(http.MethodGet, "http://evil.invalid/v1/auth/web/callback?code=oauth-code&state="+url.QueryEscape(gh.LastWebState)+"&return_to=https://evil.invalid", nil)
	callbackReq.Host = "evil.invalid"
	callbackReq.Header.Set("Forwarded", "host=evil.invalid")
	callbackReq.AddCookie(cookie)
	h.ServeHTTP(callback, callbackReq)
	if callback.Code != http.StatusSeeOther || callback.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("callback = %d headers=%v body=%s", callback.Code, callback.Header(), callback.Body.String())
	}
	location, err := url.Parse(callback.Header().Get("Location"))
	if err != nil || location.Scheme != "https" || location.Host != "web.example" || location.Path != "/app/auth/callback" || len(location.Query()["grant"]) != 1 {
		t.Fatalf("callback location = %q, err=%v", callback.Header().Get("Location"), err)
	}
	grant := location.Query().Get("grant")
	if !validHexToken(grant) || st.webGrantHash != registryauth.HashToken(grant) || st.webGrantHash == grant || st.webGrantChallenge != challenge || st.webGrantTTL != webGrantTTL {
		t.Fatalf("grant raw=%q hash=%q challenge=%q ttl=%v", grant, st.webGrantHash, st.webGrantChallenge, st.webGrantTTL)
	}
	if gh.LastWebCode != "oauth-code" || gh.LastWebVerifier == "" || gh.GetUserCalls != 1 || st.githubLogin != "alice" || st.githubUserID != 42 {
		t.Fatalf("GitHub exchange code=%q verifier=%q user calls=%d store identity=%s/%d", gh.LastWebCode, gh.LastWebVerifier, gh.GetUserCalls, st.githubLogin, st.githubUserID)
	}
	if gh.LastWebChallenge != sha256Challenge(gh.LastWebVerifier) {
		t.Fatalf("PKCE challenge = %q, verifier = %q", gh.LastWebChallenge, gh.LastWebVerifier)
	}
	assertOAuthCookieCleared(t, callback)
	replay := httptest.NewRecorder()
	replayReq := httptest.NewRequest(http.MethodGet, callbackReq.URL.String(), nil)
	replayReq.AddCookie(cookie)
	h.ServeHTTP(replay, replayReq)
	if replay.Header().Get("Location") != "https://web.example/app/auth/callback?error=oauth_invalid" || gh.WebExchangeCalls != 1 {
		t.Fatalf("callback replay = %d location=%q exchanges=%d", replay.Code, replay.Header().Get("Location"), gh.WebExchangeCalls)
	}

	st.webExchangeIdentity = store.SessionIdentity{UserID: 7, Login: "alice", Purpose: store.SessionWeb}
	exchange := httptest.NewRecorder()
	body := `{"grant":"` + grant + `","handoff_nonce":"` + nonce + `"}`
	exchangeReq := httptest.NewRequest(http.MethodPost, "/v1/auth/web/exchange", strings.NewReader(body))
	exchangeReq.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(exchange, exchangeReq)
	if exchange.Code != http.StatusOK || exchange.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("exchange = %d %s", exchange.Code, exchange.Body.String())
	}
	var response map[string]string
	if err := json.Unmarshal(exchange.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["access_token"] == "" || response["login"] != "alice" || response["purpose"] != "web" || strings.Contains(exchange.Body.String(), grant) || strings.Contains(exchange.Body.String(), nonce) {
		t.Fatalf("exchange response = %s", exchange.Body.String())
	}
	if st.webExchangeGrantHash != registryauth.HashToken(grant) || st.webExchangeChallenge != challenge || st.webExchangeSessionHash != registryauth.HashToken(response["access_token"]) || st.webExchangeTTL != webSessionTTL {
		t.Fatalf("exchange store hash=%q challenge=%q session=%q ttl=%v", st.webExchangeGrantHash, st.webExchangeChallenge, st.webExchangeSessionHash, st.webExchangeTTL)
	}
	st.webExchangeErr = store.ErrWebGrantUnavailable
	replayedExchange := httptest.NewRecorder()
	replayedRequest := httptest.NewRequest(http.MethodPost, "/v1/auth/web/exchange", strings.NewReader(body))
	replayedRequest.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(replayedExchange, replayedRequest)
	if replayedExchange.Code != http.StatusGone || strings.Contains(replayedExchange.Body.String(), grant) {
		t.Fatalf("grant replay = %d %s", replayedExchange.Code, replayedExchange.Body.String())
	}
}

func TestWebOAuthCallbackRejectsTamperingAndAlwaysClearsCookie(t *testing.T) {
	for _, mutation := range []string{"missing-cookie", "duplicate-cookie", "tampered-cookie", "wrong-state", "duplicate-state", "oversized-code"} {
		t.Run(mutation, func(t *testing.T) {
			st := newPublishSpyStore()
			gh := &registryauth.FakeGitHubClient{AuthorizeURL: "https://github.example/authorize", WebToken: "token", Users: map[string]registryauth.GitHubUser{"token": {ID: 1, Login: "alice"}}}
			h := NewWithOptions(st, newPublishSpyContent(t), "", gh, Options{PublicBaseURL: "https://registry.example", WebPublicBaseURL: "https://web.example"})
			start := httptest.NewRecorder()
			h.ServeHTTP(start, httptest.NewRequest(http.MethodGet, "/v1/auth/web/start?handoff_challenge="+sha256Challenge("nonce"), nil))
			cookie := start.Result().Cookies()[0]
			callbackURL := "/v1/auth/web/callback?code=code&state=" + gh.LastWebState
			switch mutation {
			case "tampered-cookie":
				cookie.Value += "x"
			case "wrong-state":
				callbackURL = "/v1/auth/web/callback?code=code&state=wrong"
			case "duplicate-state":
				callbackURL += "&state=second"
			case "oversized-code":
				callbackURL = "/v1/auth/web/callback?code=" + strings.Repeat("x", 513) + "&state=" + gh.LastWebState
			}
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, callbackURL, nil)
			if mutation != "missing-cookie" {
				req.AddCookie(cookie)
				if mutation == "duplicate-cookie" {
					req.AddCookie(cookie)
				}
			}
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "https://web.example/auth/callback?error=oauth_invalid" || gh.WebExchangeCalls != 0 {
				t.Fatalf("response=%d location=%q exchanges=%d", rr.Code, rr.Header().Get("Location"), gh.WebExchangeCalls)
			}
			assertOAuthCookieCleared(t, rr)
		})
	}
}

func TestWebOAuthStartRejectsMalformedChallengesBeforeGitHub(t *testing.T) {
	gh := &registryauth.FakeGitHubClient{AuthorizeURL: "https://github.example/authorize"}
	h := NewWithOptions(newPublishSpyStore(), newPublishSpyContent(t), "", gh, Options{PublicBaseURL: "https://registry.example", WebPublicBaseURL: "https://web.example"})
	valid := sha256Challenge("nonce")
	for _, target := range []string{
		"/v1/auth/web/start", "/v1/auth/web/start?handoff_challenge=bad",
		"/v1/auth/web/start?handoff_challenge=" + valid + "&handoff_challenge=" + valid,
		"/v1/auth/web/start?handoff_challenge=" + valid + "&return_to=https://evil.example",
		"/v1/auth/web/start?handoff_challenge=" + strings.Repeat("x", 300),
	} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, target, nil))
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d", target, rr.Code)
		}
	}
	if gh.WebAuthorizeCalls != 0 {
		t.Fatalf("GitHub authorize calls = %d", gh.WebAuthorizeCalls)
	}
}

func TestWebGrantExchangeGoneOnBadReplayAndStrictBody(t *testing.T) {
	st := newPublishSpyStore()
	st.webExchangeErr = store.ErrWebGrantUnavailable
	h := NewWithOptions(st, newPublishSpyContent(t), "", &registryauth.FakeGitHubClient{}, Options{PublicBaseURL: "https://registry.example", WebPublicBaseURL: "https://web.example"})
	grant := strings.Repeat("a", 64)
	nonce := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32))
	for _, tc := range []struct {
		body, contentType string
		want              int
	}{
		{`{"grant":"` + grant + `","handoff_nonce":"` + nonce + `"}`, "application/json", http.StatusGone},
		{`{"grant":"bad","handoff_nonce":"` + nonce + `"}`, "application/json", http.StatusGone},
		{`{"grant":"` + grant + `","handoff_nonce":"bad"}`, "application/json", http.StatusGone},
		{`{"grant":"` + grant + `","handoff_nonce":"` + nonce + `","extra":true}`, "application/json", http.StatusBadRequest},
		{`{}`, "text/plain", http.StatusUnsupportedMediaType},
	} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/auth/web/exchange", strings.NewReader(tc.body))
		req.Header.Set("Content-Type", tc.contentType)
		h.ServeHTTP(rr, req)
		if rr.Code != tc.want || strings.Contains(rr.Body.String(), grant) || strings.Contains(rr.Body.String(), nonce) {
			t.Fatalf("body=%s response=%d %s", tc.body, rr.Code, rr.Body.String())
		}
	}
}

func TestWebOAuthDenialAndLogsDoNotExposeQuerySecrets(t *testing.T) {
	var logs bytes.Buffer
	logger := log.New(&logs, "", 0)
	gh := &registryauth.FakeGitHubClient{AuthorizeURL: "https://github.example/authorize"}
	h := NewWithOptions(newPublishSpyStore(), newPublishSpyContent(t), "", gh, Options{PublicBaseURL: "https://registry.example", WebPublicBaseURL: "https://web.example", Logger: logger})
	start := httptest.NewRecorder()
	h.ServeHTTP(start, httptest.NewRequest(http.MethodGet, "/v1/auth/web/start?handoff_challenge="+sha256Challenge("nonce"), nil))
	cookie := start.Result().Cookies()[0]
	logs.Reset()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/auth/web/callback?error=access_denied&error_description=SECRET_DESCRIPTION&state="+gh.LastWebState, nil)
	req.AddCookie(cookie)
	h.ServeHTTP(rr, req)
	if rr.Header().Get("Location") != "https://web.example/auth/callback?error=oauth_denied" {
		t.Fatalf("location = %q", rr.Header().Get("Location"))
	}
	for _, secret := range []string{"SECRET_DESCRIPTION", gh.LastWebState, cookie.Value} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("logs contain secret %q: %s", secret, logs.String())
		}
	}
}

func assertOAuthCookieCleared(t *testing.T, rr *httptest.ResponseRecorder) {
	t.Helper()
	for _, cookie := range rr.Result().Cookies() {
		if cookie.Name == oauthCookieName && cookie.MaxAge < 0 && cookie.Secure && cookie.HttpOnly && cookie.SameSite == http.SameSiteLaxMode && cookie.Path == "/" {
			return
		}
	}
	t.Fatalf("OAuth cookie was not securely cleared: %v", rr.Header().Values("Set-Cookie"))
}
