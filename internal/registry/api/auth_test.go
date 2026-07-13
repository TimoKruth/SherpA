package api

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	registryauth "sherpa/internal/registry/auth"
)

type authTestClock struct {
	now time.Time
}

func (c *authTestClock) Now() time.Time { return c.now }
func (c *authTestClock) Advance(d time.Duration) {
	c.now = c.now.Add(d)
}

func TestDeviceFlowMintsHashedSherpaSession(t *testing.T) {
	st := newPublishSpyStore()
	gh := &registryauth.FakeGitHubClient{
		Device:      registryauth.DeviceCode{DeviceCode: "device", UserCode: "ABCD", VerificationURI: "https://github.com/login/device", Interval: 5, ExpiresIn: 900},
		PollResults: []registryauth.PollResult{{Err: registryauth.ErrAuthPending}, {AccessToken: "github-token"}},
		Users:       map[string]registryauth.GitHubUser{"github-token": {ID: 42, Login: "alice"}},
	}
	clock := &authTestClock{now: time.Unix(1_700_000_000, 0)}
	h := NewWithOptions(st, newPublishSpyContent(t), "admin", gh, Options{Now: clock.Now})
	start := httptest.NewRecorder()
	h.ServeHTTP(start, httptest.NewRequest(http.MethodPost, "/v1/auth/device/start", nil))
	if start.Code != http.StatusOK || !strings.Contains(start.Body.String(), `"user_code":"ABCD"`) {
		t.Fatalf("start = %d %s", start.Code, start.Body.String())
	}

	clock.Advance(5 * time.Second)
	pending := httptest.NewRecorder()
	h.ServeHTTP(pending, httptest.NewRequest(http.MethodPost, "/v1/auth/device/poll", strings.NewReader(`{"device_code":"device"}`)))
	if pending.Code != http.StatusAccepted || !strings.Contains(pending.Body.String(), `"status":"pending"`) {
		t.Fatalf("pending = %d %s", pending.Code, pending.Body.String())
	}

	clock.Advance(5 * time.Second)
	done := httptest.NewRecorder()
	h.ServeHTTP(done, httptest.NewRequest(http.MethodPost, "/v1/auth/device/poll", strings.NewReader(`{"device_code":"device"}`)))
	if done.Code != http.StatusOK {
		t.Fatalf("done = %d %s", done.Code, done.Body.String())
	}
	var body struct {
		AccessToken string `json:"access_token"`
		Login       string `json:"login"`
	}
	if err := json.Unmarshal(done.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.AccessToken == "" || body.Login != "alice" || strings.Contains(done.Body.String(), "github-token") {
		t.Fatalf("response = %s", done.Body.String())
	}
	if st.createdSessionHash == "" || st.createdSessionHash == body.AccessToken || st.createdSessionHash != registryauth.HashToken(body.AccessToken) {
		t.Fatalf("stored hash = %q", st.createdSessionHash)
	}
	if st.createdSessionTTL != registryauth.SessionTTL {
		t.Fatalf("TTL = %v", st.createdSessionTTL)
	}
}

func TestDevicePollRejectsOversizedBodyWithoutGitHubCall(t *testing.T) {
	gh := &registryauth.FakeGitHubClient{}
	h := New(newPublishSpyStore(), newPublishSpyContent(t), "", gh)
	body := `{"device_code":"` + strings.Repeat("x", deviceBodyLimit) + `"}`
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/auth/device/poll", strings.NewReader(body)))
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if gh.PollCalls != 0 {
		t.Fatalf("PollCalls = %d, want 0", gh.PollCalls)
	}
}

func TestDeviceStartRateLimitAndExpiry(t *testing.T) {
	clock := &authTestClock{now: time.Unix(1_700_000_000, 0)}
	gh := &registryauth.FakeGitHubClient{Device: registryauth.DeviceCode{DeviceCode: "device", Interval: 5, ExpiresIn: 900}}
	h := NewWithOptions(newPublishSpyStore(), newPublishSpyContent(t), "", gh, Options{Now: clock.Now})

	request := func() *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/auth/device/start", nil)
		req.RemoteAddr = "192.0.2.10:1234"
		h.ServeHTTP(rr, req)
		return rr
	}
	if rr := request(); rr.Code != http.StatusOK {
		t.Fatalf("first start = %d %s", rr.Code, rr.Body.String())
	}
	if rr := request(); rr.Code != http.StatusTooManyRequests || rr.Header().Get("Retry-After") == "" {
		t.Fatalf("limited start = %d, Retry-After=%q", rr.Code, rr.Header().Get("Retry-After"))
	}
	if gh.StartCalls != 1 {
		t.Fatalf("StartCalls = %d, want 1", gh.StartCalls)
	}
	clock.Advance(2 * deviceStartWindow)
	if rr := request(); rr.Code != http.StatusOK {
		t.Fatalf("start after expiry = %d %s", rr.Code, rr.Body.String())
	}
}

func TestDeviceLimiterExpiresPollEntries(t *testing.T) {
	clock := &authTestClock{now: time.Unix(1_700_000_000, 0)}
	limiter := newAuthLimiter(clock.Now)
	limiter.registerDevice("expired", time.Second, 2*time.Second)
	clock.Advance(2 * time.Second)
	limiter.registerDevice("current", time.Second, time.Minute)
	if len(limiter.devices) != 1 {
		t.Fatalf("device entries = %d, want expired entry cleaned", len(limiter.devices))
	}
}

func TestDeviceStartClientIPHonorsForwardedFor(t *testing.T) {
	newHandler := func(trustProxy bool) (http.Handler, *registryauth.FakeGitHubClient) {
		gh := &registryauth.FakeGitHubClient{Device: registryauth.DeviceCode{DeviceCode: "device", Interval: 5, ExpiresIn: 900}}
		return NewWithOptions(newPublishSpyStore(), newPublishSpyContent(t), "", gh, Options{TrustProxy: trustProxy}), gh
	}
	start := func(h http.Handler, headers map[string]string) int {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/auth/device/start", nil)
		req.RemoteAddr = "192.0.2.10:1234"
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		h.ServeHTTP(rr, req)
		return rr.Code
	}
	xff := func(v string) map[string]string { return map[string]string{"X-Forwarded-For": v} }

	// trustProxy=false: forwarded headers ignored; all requests share RemoteAddr's bucket.
	direct, directGitHub := newHandler(false)
	if first, second := start(direct, xff("198.51.100.1")), start(direct, xff("198.51.100.2")); first != http.StatusOK || second != http.StatusTooManyRequests {
		t.Fatalf("direct statuses = %d, %d", first, second)
	}
	if directGitHub.StartCalls != 1 {
		t.Fatalf("direct StartCalls = %d", directGitHub.StartCalls)
	}

	// trustProxy=true: the rightmost (proxy-appended) hop identifies the client,
	// so two distinct clients get independent buckets.
	proxied, proxiedGitHub := newHandler(true)
	if first, second := start(proxied, xff("198.51.100.1")), start(proxied, xff("198.51.100.2")); first != http.StatusOK || second != http.StatusOK {
		t.Fatalf("proxied statuses = %d, %d", first, second)
	}
	if proxiedGitHub.StartCalls != 2 {
		t.Fatalf("proxied StartCalls = %d", proxiedGitHub.StartCalls)
	}

	// Spoof resistance: prepended (left) entries are attacker-controlled and
	// must not create new buckets. Two requests whose RIGHTMOST hop is identical
	// share a bucket regardless of the differing prepended spoof.
	spoof, _ := newHandler(true)
	if first := start(spoof, xff("10.0.0.9, 203.0.113.7")); first != http.StatusOK {
		t.Fatalf("spoof first = %d", first)
	}
	if second := start(spoof, xff("10.0.0.250, 203.0.113.7")); second != http.StatusTooManyRequests {
		t.Fatalf("spoof second = %d, want limited (shared rightmost hop 203.0.113.7)", second)
	}

	// X-Real-IP is no longer trusted: two requests differing only in X-Real-IP
	// share RemoteAddr's bucket, so the second is limited.
	realIP, _ := newHandler(true)
	if first, second := start(realIP, map[string]string{"X-Real-IP": "198.51.100.77"}), start(realIP, map[string]string{"X-Real-IP": "198.51.100.88"}); first != http.StatusOK || second != http.StatusTooManyRequests {
		t.Fatalf("X-Real-IP trusted? statuses = %d, %d", first, second)
	}
}

func TestDevicePollRateLimitAndSlowDown(t *testing.T) {
	clock := &authTestClock{now: time.Unix(1_700_000_000, 0)}
	gh := &registryauth.FakeGitHubClient{
		Device:      registryauth.DeviceCode{DeviceCode: "device", Interval: 5, ExpiresIn: 900},
		PollResults: []registryauth.PollResult{{Err: registryauth.ErrSlowDown}, {Err: registryauth.ErrAuthPending}},
	}
	h := NewWithOptions(newPublishSpyStore(), newPublishSpyContent(t), "", gh, Options{Now: clock.Now})
	start := httptest.NewRecorder()
	h.ServeHTTP(start, httptest.NewRequest(http.MethodPost, "/v1/auth/device/start", nil))

	poll := func() *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/auth/device/poll", strings.NewReader(`{"device_code":"device"}`)))
		return rr
	}
	clock.Advance(4 * time.Second)
	if rr := poll(); rr.Code != http.StatusTooManyRequests || gh.PollCalls != 0 {
		t.Fatalf("early poll = %d, calls = %d", rr.Code, gh.PollCalls)
	}
	clock.Advance(time.Second)
	if rr := poll(); rr.Code != http.StatusAccepted || !strings.Contains(rr.Body.String(), "slow_down") {
		t.Fatalf("slow-down poll = %d %s", rr.Code, rr.Body.String())
	}
	clock.Advance(9 * time.Second)
	if rr := poll(); rr.Code != http.StatusTooManyRequests || gh.PollCalls != 1 {
		t.Fatalf("poll during slow-down = %d, calls = %d", rr.Code, gh.PollCalls)
	}
	clock.Advance(time.Second)
	if rr := poll(); rr.Code != http.StatusAccepted || gh.PollCalls != 2 {
		t.Fatalf("poll after slow-down = %d, calls = %d", rr.Code, gh.PollCalls)
	}
}

func TestRequestLoggerExcludesSecrets(t *testing.T) {
	var output bytes.Buffer
	logger := log.New(&output, "", 0)
	gh := &registryauth.FakeGitHubClient{
		Device:      registryauth.DeviceCode{DeviceCode: "github-token-session-token", ExpiresIn: 900},
		PollResults: []registryauth.PollResult{{Err: registryauth.ErrAuthPending}},
	}
	h := NewWithOptions(newPublishSpyStore(), newPublishSpyContent(t), "", gh, Options{Logger: logger})
	// Register the code via /start so the poll reaches the pending (202) path,
	// then assert the poll's log line carries no secrets.
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/auth/device/start", nil))
	output.Reset()
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/device/poll?token=query-secret", strings.NewReader(`{"device_code":"github-token-session-token"}`))
	req.Header.Set("Authorization", "Bearer bearer-secret")
	req.Header.Set("X-Railway-Request-Id", "railway-request-123")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	logged := output.String()
	for _, secret := range []string{"query-secret", "github-token", "session-token", "bearer-secret"} {
		if strings.Contains(logged, secret) {
			t.Fatalf("log contains %q: %s", secret, logged)
		}
	}
	for _, wanted := range []string{"method=POST", `path="/v1/auth/device/poll"`, "status=202", `railway_request_id="railway-request-123"`} {
		if !strings.Contains(logged, wanted) {
			t.Fatalf("log omitted %q: %s", wanted, logged)
		}
	}
}

func TestDevicePollRejectsUnregisteredCodeWithoutGitHubCall(t *testing.T) {
	gh := &registryauth.FakeGitHubClient{PollResults: []registryauth.PollResult{{AccessToken: "should-not-be-used"}}}
	h := New(newPublishSpyStore(), newPublishSpyContent(t), "", gh)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/auth/device/poll", strings.NewReader(`{"device_code":"never-registered"}`)))
	if rr.Code != http.StatusGone {
		t.Fatalf("status = %d, want 410 for an unregistered code", rr.Code)
	}
	if gh.PollCalls != 0 {
		t.Fatalf("PollCalls = %d, want 0 (no GitHub call for a code never registered via /start)", gh.PollCalls)
	}
}

func TestDevicePollStatusMapping(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
		body   string
	}{
		{"slow", registryauth.ErrSlowDown, http.StatusAccepted, `"status":"slow_down"`},
		{"expired", registryauth.ErrExpired, http.StatusGone, `"error":"device code expired"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := &authTestClock{now: time.Unix(1_700_000_000, 0)}
			st := newPublishSpyStore()
			gh := &registryauth.FakeGitHubClient{
				Device:      registryauth.DeviceCode{DeviceCode: "device", Interval: 5, ExpiresIn: 900},
				PollResults: []registryauth.PollResult{{Err: tc.err}},
			}
			h := NewWithOptions(st, newPublishSpyContent(t), "", gh, Options{Now: clock.Now})
			// Register the device via /start so the poll is for a known code, then
			// advance past the initial poll interval.
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/auth/device/start", nil))
			clock.Advance(5 * time.Second)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/auth/device/poll", strings.NewReader(`{"device_code":"device"}`)))
			if rr.Code != tc.status || !strings.Contains(rr.Body.String(), tc.body) {
				t.Fatalf("response = %d %s", rr.Code, rr.Body.String())
			}
		})
	}
}
