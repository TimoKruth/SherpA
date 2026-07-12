package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	registryauth "sherpa/internal/registry/auth"
)

func TestDeviceFlowMintsHashedSherpaSession(t *testing.T) {
	st := newPublishSpyStore()
	gh := &registryauth.FakeGitHubClient{
		Device:      registryauth.DeviceCode{DeviceCode: "device", UserCode: "ABCD", VerificationURI: "https://github.com/login/device", Interval: 5, ExpiresIn: 900},
		PollResults: []registryauth.PollResult{{Err: registryauth.ErrAuthPending}, {AccessToken: "github-token"}},
		Users:       map[string]registryauth.GitHubUser{"github-token": {ID: 42, Login: "alice"}},
	}
	h := New(st, newPublishSpyContent(t), "admin", gh)
	start := httptest.NewRecorder()
	h.ServeHTTP(start, httptest.NewRequest(http.MethodPost, "/v1/auth/device/start", nil))
	if start.Code != http.StatusOK || !strings.Contains(start.Body.String(), `"user_code":"ABCD"`) {
		t.Fatalf("start = %d %s", start.Code, start.Body.String())
	}

	pending := httptest.NewRecorder()
	h.ServeHTTP(pending, httptest.NewRequest(http.MethodPost, "/v1/auth/device/poll", strings.NewReader(`{"device_code":"device"}`)))
	if pending.Code != http.StatusAccepted || !strings.Contains(pending.Body.String(), `"status":"pending"`) {
		t.Fatalf("pending = %d %s", pending.Code, pending.Body.String())
	}

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
			st := newPublishSpyStore()
			gh := &registryauth.FakeGitHubClient{PollResults: []registryauth.PollResult{{Err: tc.err}}}
			h := New(st, newPublishSpyContent(t), "", gh)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/auth/device/poll", strings.NewReader(`{"device_code":"device"}`)))
			if rr.Code != tc.status || !strings.Contains(rr.Body.String(), tc.body) {
				t.Fatalf("response = %d %s", rr.Code, rr.Body.String())
			}
		})
	}
}
