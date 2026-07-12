package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPGitHubDeviceFlowWireFormat(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login/device/code", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.Form.Get("client_id") != "client-id" {
			t.Fatalf("start form = %#v, err=%v", r.Form, err)
		}
		_ = json.NewEncoder(w).Encode(DeviceCode{DeviceCode: "device", UserCode: "ABCD-EFGH", VerificationURI: "https://github.com/login/device", Interval: 5, ExpiresIn: 900})
	})
	polls := 0
	mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		polls++
		if err := r.ParseForm(); err != nil || r.Form.Get("device_code") != "device" || r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" {
			t.Fatalf("poll form = %#v, err=%v", r.Form, err)
		}
		switch polls {
		case 1:
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending"})
		case 2:
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "slow_down"})
		case 3:
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "expired_token"})
		default:
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "github-token", "token_type": "bearer"})
		}
	})
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer github-token" {
			t.Fatalf("Authorization = %q", r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(GitHubUser{ID: 42, Login: "alice"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := NewGitHubClient("client-id", srv.URL, srv.URL)
	ctx := context.Background()
	code, err := client.StartDeviceFlow(ctx)
	if err != nil || code.DeviceCode != "device" || code.Interval != 5 {
		t.Fatalf("StartDeviceFlow = %#v, %v", code, err)
	}
	if _, err := client.PollToken(ctx, code.DeviceCode); !errors.Is(err, ErrAuthPending) {
		t.Fatalf("first PollToken error = %v", err)
	}
	if _, err := client.PollToken(ctx, code.DeviceCode); !errors.Is(err, ErrSlowDown) {
		t.Fatalf("second PollToken error = %v", err)
	}
	if _, err := client.PollToken(ctx, code.DeviceCode); !errors.Is(err, ErrExpired) {
		t.Fatalf("third PollToken error = %v", err)
	}
	token, err := client.PollToken(ctx, code.DeviceCode)
	if err != nil || token != "github-token" {
		t.Fatalf("fourth PollToken = %q, %v", token, err)
	}
	user, err := client.GetUser(ctx, token)
	if err != nil || user != (GitHubUser{ID: 42, Login: "alice"}) {
		t.Fatalf("GetUser = %#v, %v", user, err)
	}
}
