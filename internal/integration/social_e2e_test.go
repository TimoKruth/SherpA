package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"sherpa/internal/registry/api"
	registryauth "sherpa/internal/registry/auth"
	"sherpa/internal/registry/content"
	"sherpa/internal/registry/store"
	webapp "sherpa/internal/web"
	webclient "sherpa/internal/web/registryclient"
)

func TestSocialLifecycleAcrossCLIRegistryAndWeb(t *testing.T) {
	root := t.TempDir()
	aliceHome := filepath.Join(root, "alice-home")
	bobHome := filepath.Join(root, "bob-home")
	gh := &registryauth.FakeGitHubClient{
		Device:       registryauth.DeviceCode{DeviceCode: "alice-device", UserCode: "ALICE", VerificationURI: "https://github.example/device", ExpiresIn: 60},
		PollResults:  []registryauth.PollResult{{AccessToken: "alice-github-token"}},
		Users:        map[string]registryauth.GitHubUser{"alice-github-token": {ID: 41, Login: "alice"}, "alice-web-token": {ID: 41, Login: "alice"}},
		AuthorizeURL: "https://github.example/authorize",
		WebToken:     "alice-web-token",
	}
	serverURL, st := startSocialRegistryServer(t, gh, "admin-token")
	t.Setenv("SHERPA_REGISTRY_URL", serverURL)
	t.Setenv("SHERPA_REGISTRY_TOKEN", "")
	t.Setenv("SHERPA_HOME", aliceHome)
	if out, errOut, code := runCLI(t, nil, "login"); code != 0 || !strings.Contains(out, "alice") {
		t.Fatalf("alice device login code=%d out=%s err=%s", code, out, errOut)
	}

	ctx := context.Background()
	if _, err := st.UpsertUser(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	bobID, err := st.UpsertUser(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	bobCLIToken, bobCLIHash, err := registryauth.NewSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, bobID, bobCLIHash, store.SessionCLI, time.Hour); err != nil {
		t.Fatal(err)
	}
	writeRegistrySession(t, bobHome, serverURL, bobCLIToken, "bob")
	t.Setenv("SHERPA_HOME", bobHome)

	stackID, err := st.UpsertStack(ctx, store.Stack{Owner: "alice", Name: "reviewer", Summary: "Security review", Harness: "codex", Tags: []string{"review"}})
	if err != nil {
		t.Fatal(err)
	}
	insertSocialVersion(t, st, stackID, 1, "initial")
	if out, errOut, code := runCLI(t, nil, "follow", "@alice/reviewer"); code != 0 || !strings.Contains(out, "at v1") {
		t.Fatalf("follow code=%d out=%s err=%s", code, out, errOut)
	}
	if out, errOut, code := runCLI(t, nil, "updates"); code != 0 || !strings.Contains(out, "no pending updates") {
		t.Fatalf("initial updates code=%d out=%s err=%s", code, out, errOut)
	}

	insertSocialVersion(t, st, stackID, 2, "stricter checks")
	if out, errOut, code := runCLI(t, nil, "updates"); code != 0 || !strings.Contains(out, "@alice/reviewer") || !strings.Contains(out, "v1 -> v2") || !strings.Contains(out, "stricter checks") {
		t.Fatalf("pending updates code=%d out=%s err=%s", code, out, errOut)
	}

	bobWebToken, bobWebHash, err := registryauth.NewSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, bobID, bobWebHash, store.SessionWeb, time.Hour); err != nil {
		t.Fatal(err)
	}
	webHandler := newSocialWebHandler(t, serverURL)
	dashboard := serveWeb(t, webHandler, http.MethodGet, "https://web.example/dashboard", bobWebToken, "csrf-token", "")
	if dashboard.Code != http.StatusOK || !strings.Contains(dashboard.Body.String(), "stricter checks") || strings.Contains(dashboard.Body.String(), bobWebToken) {
		t.Fatalf("dashboard status=%d expected_update=%v token_exposed=%v", dashboard.Code, strings.Contains(dashboard.Body.String(), "stricter checks"), strings.Contains(dashboard.Body.String(), bobWebToken))
	}
	dashboard = serveWeb(t, webHandler, http.MethodGet, "https://web.example/dashboard", bobWebToken, "csrf-token", "")
	if dashboard.Code != http.StatusOK {
		t.Fatalf("second dashboard status=%d", dashboard.Code)
	}
	updates, err := st.ListUpdates(ctx, bobID, store.UpdatePage{Limit: 10})
	if err != nil || len(updates) != 1 || updates[0].Version != 2 {
		t.Fatalf("dashboard GET changed pending state: %#v, %v", updates, err)
	}

	seen := serveWeb(t, webHandler, http.MethodPost, "https://web.example/dashboard/seen/alice/reviewer", bobWebToken, "csrf-token", "csrf_token=csrf-token&version=2")
	if seen.Code != http.StatusSeeOther || seen.Header().Get("Location") != "/dashboard" {
		t.Fatalf("seen status=%d location=%q", seen.Code, seen.Header().Get("Location"))
	}
	updates, err = st.ListUpdates(ctx, bobID, store.UpdatePage{Limit: 10})
	if err != nil || len(updates) != 0 {
		t.Fatalf("seen did not clear pending state: %#v, %v", updates, err)
	}
	if out, errOut, code := runCLI(t, nil, "unfollow", "@alice/reviewer"); code != 0 || !strings.Contains(out, "unfollowed") {
		t.Fatalf("unfollow code=%d out=%s err=%s", code, out, errOut)
	}
	insertSocialVersion(t, st, stackID, 3, "after unfollow")
	if out, errOut, code := runCLI(t, nil, "updates"); code != 0 || !strings.Contains(out, "no pending updates") {
		t.Fatalf("post-unfollow updates code=%d out=%s err=%s", code, out, errOut)
	}

	aliceWebToken := exchangeRealWebGrant(t, serverURL, gh)
	response := registryRequest(t, http.MethodPut, serverURL+"/v1/me/follows/alice/reviewer", aliceWebToken, "")
	responseBody := readResponse(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("web follow status=%d body=%s", response.StatusCode, responseBody)
	}
	response = registryRequest(t, http.MethodPost, serverURL+"/v1/stacks/alice/reviewer/versions", aliceWebToken, "")
	responseBody = readResponse(t, response)
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("web publish status=%d body=%s", response.StatusCode, responseBody)
	}
	response = registryRequest(t, http.MethodGet, serverURL+"/v1/me", "admin-token", "")
	responseBody = readResponse(t, response)
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("admin personal status=%d body=%s", response.StatusCode, responseBody)
	}
	response = registryRequest(t, http.MethodDelete, serverURL+"/v1/me/session", aliceWebToken, "")
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("web revoke status=%d body=%s", response.StatusCode, readResponse(t, response))
	}
	response.Body.Close()
	response = registryRequest(t, http.MethodGet, serverURL+"/v1/me", aliceWebToken, "")
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked session status=%d body=%s", response.StatusCode, readResponse(t, response))
	}
}

func startSocialRegistryServer(t *testing.T, gh registryauth.GitHubClient, adminToken string) (string, *store.PostgresStore) {
	t.Helper()
	st, err := store.OpenPostgres(context.Background(), store.StartPostgres(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	handler := api.NewWithOptions(st, content.NewBareGit(t.TempDir()), adminToken, gh, api.Options{
		PublicBaseURL: "https://registry.example", WebPublicBaseURL: "https://web.example",
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server.URL, st
}

func writeRegistrySession(t *testing.T, home, registryURL, token, login string) {
	t.Helper()
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(map[string]string{"access_token": token, "login": login, "registry_url": registryURL})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "registry-session.json"), append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func insertSocialVersion(t *testing.T, st *store.PostgresStore, stackID int64, version int, changelog string) {
	t.Helper()
	if err := st.InsertVersion(context.Background(), store.Version{StackID: stackID, Version: version, GitTag: "v" + strconv.Itoa(version), Changelog: changelog, TrustTier: "linked"}); err != nil {
		t.Fatal(err)
	}
}

func newSocialWebHandler(t *testing.T, registryURL string) http.Handler {
	t.Helper()
	client, err := webclient.New(registryURL, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	webBase, _ := url.Parse("https://web.example")
	registryBase, _ := url.Parse(registryURL)
	handler, err := webapp.New(client, webapp.Options{PublicBaseURL: webBase, RegistryPublicURL: registryBase})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func serveWeb(t *testing.T, handler http.Handler, method, target, token, csrf, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.AddCookie(&http.Cookie{Name: "__Host-sherpa_session", Value: token})
	request.AddCookie(&http.Cookie{Name: "__Host-sherpa_csrf", Value: csrf})
	if method == http.MethodPost {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.Header.Set("Origin", "https://web.example")
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func exchangeRealWebGrant(t *testing.T, serverURL string, gh *registryauth.FakeGitHubClient) string {
	t.Helper()
	nonce := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	sum := sha256.Sum256([]byte(nonce))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	start, err := client.Get(serverURL + "/v1/auth/web/start?handoff_challenge=" + url.QueryEscape(challenge))
	if err != nil {
		t.Fatal(err)
	}
	start.Body.Close()
	if start.StatusCode != http.StatusSeeOther || len(start.Cookies()) != 1 || gh.LastWebState == "" {
		t.Fatalf("web start status=%d cookie_count=%d state_present=%v", start.StatusCode, len(start.Cookies()), gh.LastWebState != "")
	}
	callbackURL := serverURL + "/v1/auth/web/callback?code=web-code&state=" + url.QueryEscape(gh.LastWebState)
	request, _ := http.NewRequest(http.MethodGet, callbackURL, nil)
	request.AddCookie(start.Cookies()[0])
	callback, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	callback.Body.Close()
	location, err := url.Parse(callback.Header.Get("Location"))
	if err != nil || callback.StatusCode != http.StatusSeeOther || location.Host != "web.example" {
		t.Fatalf("callback status=%d pinned_host=%v parse_error=%v", callback.StatusCode, location != nil && location.Host == "web.example", err)
	}
	grant := location.Query().Get("grant")
	body, _ := json.Marshal(map[string]string{"grant": grant, "handoff_nonce": nonce})
	exchange, err := http.Post(serverURL+"/v1/auth/web/exchange", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var session struct {
		AccessToken string `json:"access_token"`
	}
	if exchange.StatusCode != http.StatusOK {
		t.Fatalf("exchange=%d body=%s", exchange.StatusCode, readResponse(t, exchange))
	}
	if err := json.NewDecoder(exchange.Body).Decode(&session); err != nil {
		t.Fatal(err)
	}
	exchange.Body.Close()
	replay, err := http.Post(serverURL+"/v1/auth/web/exchange", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if replay.StatusCode != http.StatusGone || strings.Contains(readResponse(t, replay), grant) || session.AccessToken == "" {
		t.Fatalf("replay=%d", replay.StatusCode)
	}
	return session.AccessToken
}

func registryRequest(t *testing.T, method, target, token, body string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(method, target, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func readResponse(t *testing.T, response *http.Response) string {
	t.Helper()
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
