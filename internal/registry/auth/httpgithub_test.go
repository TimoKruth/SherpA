package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
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

func TestHTTPGitHubWebOAuthWireFormat(t *testing.T) {
	var userCalls int
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		want := map[string]string{"client_id": "client-id", "client_secret": "client-secret", "code": "oauth-code", "code_verifier": "verifier", "redirect_uri": "https://registry.example/v1/auth/web/callback"}
		for key, value := range want {
			if r.Form.Get(key) != value {
				t.Errorf("form[%s] = %q", key, r.Form.Get(key))
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "github-token", "token_type": "bearer"})
	})
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, _ *http.Request) {
		userCalls++
		_ = json.NewEncoder(w).Encode(GitHubUser{ID: 42, Login: "alice"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	client := NewGitHubClientWithSecret("client-id", "client-secret", srv.URL, srv.URL)
	authorize, err := client.WebAuthorizeURL("state", "challenge", "https://registry.example/v1/auth/web/callback")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(authorize)
	if u.Path != "/login/oauth/authorize" || u.Query().Get("state") != "state" || u.Query().Get("code_challenge_method") != "S256" || u.Query().Get("scope") != "" || strings.Contains(authorize, "client-secret") {
		t.Fatalf("authorize URL = %q", authorize)
	}
	token, err := client.ExchangeWebCode(context.Background(), "oauth-code", "verifier", "https://registry.example/v1/auth/web/callback")
	if err != nil || token != "github-token" {
		t.Fatalf("ExchangeWebCode = %q, %v", token, err)
	}
	if _, err := client.GetUser(context.Background(), token); err != nil || userCalls != 1 {
		t.Fatalf("GetUser error=%v calls=%d", err, userCalls)
	}
}

func TestHTTPGitHubWebOAuthRejectsRedirectAndInvalidTokenType(t *testing.T) {
	targetCalls := 0
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetCalls++ }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	client := NewGitHubClientWithSecret("client", "secret", redirect.URL)
	if _, err := client.ExchangeWebCode(context.Background(), "code", "verifier", "https://registry.example/callback"); err == nil || targetCalls != 0 {
		t.Fatalf("redirect error=%v target calls=%d", err, targetCalls)
	}
	redirect.Close()
	invalid := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "token", "token_type": "mac"})
	}))
	defer invalid.Close()
	client = NewGitHubClientWithSecret("client", "secret", invalid.URL)
	if _, err := client.ExchangeWebCode(context.Background(), "code", "verifier", "https://registry.example/callback"); err == nil {
		t.Fatal("invalid token type accepted")
	}
}

func TestHTTPGitHubClientHasBoundedTimeout(t *testing.T) {
	client := NewGitHubClient("client-id")
	if client.client == http.DefaultClient || client.client.Timeout != 30*time.Second {
		t.Fatalf("HTTP client = %#v, want dedicated 30-second client", client.client)
	}
}

func TestHTTPGitHubClientDefaultsMissingPollInterval(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(DeviceCode{DeviceCode: "device"})
	}))
	defer srv.Close()

	client := NewGitHubClient("client-id", srv.URL)
	code, err := client.StartDeviceFlow(context.Background())
	if err != nil {
		t.Fatalf("StartDeviceFlow: %v", err)
	}
	if code.Interval != 5 {
		t.Fatalf("Interval = %d, want 5", code.Interval)
	}
}

func TestHTTPGitHubClientHonorsContextCancellation(t *testing.T) {
	entered := make(chan struct{})
	client := NewGitHubClient("client-id")
	client.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		close(entered)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := client.StartDeviceFlow(ctx)
		result <- err
	}()
	<-entered
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("StartDeviceFlow error = %v, want context canceled", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}
