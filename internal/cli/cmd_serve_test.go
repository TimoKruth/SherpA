package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sherpa/internal/state"
	"strings"
	"testing"
)

func testServer(t *testing.T) *localServer {
	return &localServer{home: t.TempDir(), host: "127.0.0.1:7331", token: "fixture-token"}
}
func request(t *testing.T, s *localServer, method, path, body, token, origin string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, "http://"+s.host+path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	if method == "POST" {
		r.Header.Set("Content-Type", "application/json")
	}
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}
func TestLocalServerRequiresPrivateTokenAndOwnOrigin(t *testing.T) {
	s := testServer(t)
	for _, path := range []string{"/api/state", "/api/comparisons", "/api/comparison?id=x", "/api/report?id=x"} {
		w := request(t, s, "GET", path, "", "wrong", "")
		if w.Code != 401 {
			t.Fatalf("%s returned %d", path, w.Code)
		}
	}
	w := request(t, s, "POST", "/api/action", `{"action":"use","name":"mine"}`, s.token, "https://malicious.example")
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
	r := httptest.NewRequest("GET", "http://malicious.example/api/state", nil)
	r.Header.Set("Authorization", "Bearer "+s.token)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("DNS rebinding host accepted")
	}
	w = request(t, s, "GET", "/", "", "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Side by side") || !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatal("app shell missing hardening")
	}
}
func TestLocalServerCreatesAndSwitchesVariantAndHidesLegacyData(t *testing.T) {
	s := testServer(t)
	st, _ := state.Load(s.home)
	dir := filepath.Join(s.home, "profiles", "mine")
	os.MkdirAll(dir, 0700)
	os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("baseline"), 0600)
	st.Profiles["mine"] = state.Profile{Name: "mine", Path: dir, Harness: "claude-code"}
	st.Active = "mine"
	st.Baselines["claude-code"] = "mine"
	st.Trials = json.RawMessage(`[{"notes":"private legacy notes"}]`)
	st.Save(s.home)
	w := request(t, s, "POST", "/api/action", `{"action":"create","name":"variant","from":"mine","instructions":"Be concise."}`, s.token, "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = request(t, s, "POST", "/api/action", `{"action":"use","name":"variant"}`, s.token, "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = request(t, s, "GET", "/api/state", "", s.token, "")
	if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte(`"active":"variant"`)) || bytes.Contains(w.Body.Bytes(), []byte("legacy notes")) {
		t.Fatal(w.Body.String())
	}
	w = request(t, s, "POST", "/api/action", `{"action":"create","name":"../escape","from":"mine"}`, s.token, "")
	if w.Code != 400 {
		t.Fatal("accepted unsafe name")
	}
}
func TestLocalServerRejectsInvalidJSONAndMethods(t *testing.T) {
	s := testServer(t)
	w := request(t, s, "POST", "/api/action", `{"action":"use","unknown":true}`, s.token, "")
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
	w = request(t, s, "DELETE", "/api/action", "", s.token, "")
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatal(w.Code)
	}
}

func TestLocalServerRejectsNewWorkDuringShutdown(t *testing.T) {
	s := testServer(t)
	s.stop()
	w := request(t, s, "POST", "/api/action", `{"action":"create","name":"variant","from":"mine"}`, s.token, "")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("shutdown accepted mutation: %d %s", w.Code, w.Body.String())
	}
}
