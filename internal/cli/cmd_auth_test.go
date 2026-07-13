package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoginLogoutAndTokenPrecedence(t *testing.T) {
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/auth/device/start":
			_ = json.NewEncoder(w).Encode(map[string]any{"device_code": "device", "user_code": "ABCD", "verification_uri": "https://github.com/login/device", "interval": 0, "expires_in": 900})
		case "/v1/auth/device/poll":
			polls++
			if polls == 1 {
				w.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(w).Encode(map[string]string{"status": "pending"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "sherpa-session", "login": "alice"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	home := t.TempDir()
	t.Setenv("SHERPA_REGISTRY_URL", srv.URL)
	t.Setenv("SHERPA_REGISTRY_TOKEN", "")
	var out bytes.Buffer
	ctx := &Ctx{Home: home, Stdout: &out, Stderr: &out, Stdin: strings.NewReader("")}
	if err := cmdLogin(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "ABCD") || !strings.Contains(out.String(), "alice") {
		t.Fatalf("output = %q", out.String())
	}
	info, err := os.Stat(filepath.Join(home, "registry-session.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("session mode = %v, err=%v", info.Mode().Perm(), err)
	}
	if token, err := registryToken(home, srv.URL+"/"); err != nil || token != "sherpa-session" {
		t.Fatalf("token = %q, %v", token, err)
	}
	var otherRequests int
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherRequests++
		t.Fatalf("session token request reached a different registry: %s", r.URL)
	}))
	defer other.Close()
	if token, err := registryToken(home, other.URL); err == nil || token != "" || !strings.Contains(err.Error(), "registry session belongs to") || !strings.Contains(err.Error(), "log in to") {
		t.Fatalf("cross-registry token = %q, %v", token, err)
	}
	profile := makeExpertRepo(t, true)
	if err := publishRegistryVersion(ctx, profile, other.URL, "v1"); err == nil {
		t.Fatal("cross-registry publish unexpectedly proceeded")
	}
	if otherRequests != 0 {
		t.Fatalf("different registry received %d requests", otherRequests)
	}
	t.Setenv("SHERPA_REGISTRY_TOKEN", "admin")
	if token, err := registryToken(home, "not a registry URL"); err != nil || token != "admin" {
		t.Fatalf("admin precedence = %q, %v", token, err)
	}
	if err := cmdLogout(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "registry-session.json")); !os.IsNotExist(err) {
		t.Fatalf("session still exists: %v", err)
	}
}

func TestLegacyRegistrySessionFailsClosed(t *testing.T) {
	home := t.TempDir()
	legacy := []byte(`{"access_token":"legacy-session","login":"alice"}`)
	if err := os.WriteFile(registrySessionPath(home), legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHERPA_REGISTRY_TOKEN", "")
	token, err := registryToken(home, "https://registry.example")
	if err == nil || token != "" || !strings.Contains(err.Error(), "no registry issuer; log in again") {
		t.Fatalf("legacy token = %q, err = %v", token, err)
	}
}

func TestRegistryUserSessionIsIssuerScopedAndIgnoresAdminToken(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SHERPA_REGISTRY_TOKEN", "admin-token")
	if _, err := registryUserSession(home, "https://registry.example"); err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("missing session error = %v", err)
	}
	if err := saveRegistrySession(home, "HTTPS://REGISTRY.EXAMPLE:443/", "user-token", "alice"); err != nil {
		t.Fatal(err)
	}
	session, err := registryUserSession(home, "https://registry.example")
	if err != nil || session.AccessToken != "user-token" || session.Login != "alice" || session.RegistryURL != "https://registry.example" {
		t.Fatalf("session = %#v, %v", session, err)
	}
	if _, err := registryUserSession(home, "https://other.example"); err == nil || !strings.Contains(err.Error(), "registry session belongs to") {
		t.Fatalf("issuer mismatch error = %v", err)
	}
}

func TestRegistryPublishForbiddenExplainsOwnerScope(t *testing.T) {
	var out bytes.Buffer
	printRegistryPublishError(&out, http.StatusForbidden, []byte(`{"error":"forbidden"}`))
	if got := out.String(); got != "you can only publish under @<your-github-login>\n" {
		t.Fatalf("output = %q", got)
	}
}
