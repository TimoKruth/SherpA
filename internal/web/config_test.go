package web

import (
	"strings"
	"testing"
	"time"
)

func TestLoadConfigDefaultsAndPortPrecedence(t *testing.T) {
	setBaseConfigEnv(t)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != "[::]:8080" || cfg.UpstreamTimeout != 5*time.Second {
		t.Fatalf("defaults = %#v", cfg)
	}

	t.Setenv("PORT", "9090")
	cfg, err = LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != "[::]:9090" {
		t.Fatalf("PORT address = %q", cfg.Addr)
	}

	t.Setenv("PORT", "not-a-port")
	t.Setenv("SHERPA_WEB_ADDR", "127.0.0.1:7777")
	cfg, err = LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != "127.0.0.1:7777" {
		t.Fatalf("SHERPA_WEB_ADDR = %q", cfg.Addr)
	}
}

func TestLoadConfigNormalizesURLsAndTimeout(t *testing.T) {
	setBaseConfigEnv(t)
	t.Setenv("SHERPA_REGISTRY_API_URL", "HTTPS://REGISTRY.EXAMPLE/api//v1/../")
	t.Setenv("SHERPA_WEB_PUBLIC_BASE_URL", "https://WWW.EXAMPLE/catalog/")
	t.Setenv("SHERPA_WEB_UPSTREAM_TIMEOUT", "1250ms")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RegistryAPIURL != "https://registry.example/api" {
		t.Fatalf("RegistryAPIURL = %q", cfg.RegistryAPIURL)
	}
	if cfg.PublicBaseURL != "https://www.example/catalog" {
		t.Fatalf("PublicBaseURL = %q", cfg.PublicBaseURL)
	}
	if cfg.UpstreamTimeout != 1250*time.Millisecond {
		t.Fatalf("UpstreamTimeout = %s", cfg.UpstreamTimeout)
	}
}

func TestLoadConfigRequiresRegistryURL(t *testing.T) {
	setBaseConfigEnv(t)
	t.Setenv("SHERPA_REGISTRY_API_URL", "")
	_, err := LoadConfig()
	if err == nil || !strings.Contains(err.Error(), "SHERPA_REGISTRY_API_URL") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadConfigRejectsInvalidPortsAndAddresses(t *testing.T) {
	for _, port := range []string{"0", "65536", "-1", "abc", "80.5"} {
		t.Run("port_"+port, func(t *testing.T) {
			setBaseConfigEnv(t)
			t.Setenv("PORT", port)
			if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "PORT") {
				t.Fatalf("PORT %q error = %v", port, err)
			}
		})
	}
	for _, addr := range []string{"localhost", ":0", "localhost:65536", "[::]", "https://localhost:8080"} {
		t.Run("addr_"+addr, func(t *testing.T) {
			setBaseConfigEnv(t)
			t.Setenv("SHERPA_WEB_ADDR", addr)
			if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "SHERPA_WEB_ADDR") {
				t.Fatalf("address %q error = %v", addr, err)
			}
		})
	}
}

func TestLoadConfigRejectsUnsafeURLsWithoutEchoingSecrets(t *testing.T) {
	invalid := []string{
		"registry.example", "ftp://registry.example", "https:///missing-host",
		"https://user:planted-secret@registry.example", "https://registry.example?token=planted-secret",
		"https://registry.example/#planted-secret", "https://registry.example/#",
	}
	for _, key := range []string{"SHERPA_REGISTRY_API_URL", "SHERPA_WEB_PUBLIC_BASE_URL"} {
		for _, raw := range invalid {
			t.Run(key+"_"+raw, func(t *testing.T) {
				setBaseConfigEnv(t)
				t.Setenv(key, raw)
				_, err := LoadConfig()
				if err == nil {
					t.Fatalf("%s=%q accepted", key, raw)
				}
				if strings.Contains(err.Error(), "planted-secret") || strings.Contains(err.Error(), raw) {
					t.Fatalf("error %q exposes URL", err)
				}
			})
		}
	}
}

func TestLoadConfigValidatesUpstreamTimeoutRange(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want time.Duration
		ok   bool
	}{
		{raw: "100ms", want: 100 * time.Millisecond, ok: true},
		{raw: "30s", want: 30 * time.Second, ok: true},
		{raw: "99ms"}, {raw: "30.001s"}, {raw: "0"}, {raw: "forever"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			setBaseConfigEnv(t)
			t.Setenv("SHERPA_WEB_UPSTREAM_TIMEOUT", tc.raw)
			cfg, err := LoadConfig()
			if tc.ok {
				if err != nil || cfg.UpstreamTimeout != tc.want {
					t.Fatalf("LoadConfig = %#v, %v", cfg, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "SHERPA_WEB_UPSTREAM_TIMEOUT") {
				t.Fatalf("timeout %q error = %v", tc.raw, err)
			}
		})
	}
}

func setBaseConfigEnv(t *testing.T) {
	t.Helper()
	t.Setenv("SHERPA_WEB_ADDR", "")
	t.Setenv("PORT", "")
	t.Setenv("SHERPA_REGISTRY_API_URL", "https://registry.example")
	t.Setenv("SHERPA_WEB_PUBLIC_BASE_URL", "")
	t.Setenv("SHERPA_WEB_UPSTREAM_TIMEOUT", "")
}
