package registry

import (
	"strings"
	"testing"
)

func TestLoadConfigCanonicalizesPublicBaseURL(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		want  string
	}{
		{name: "root slash", value: "https://Registry.Example/", want: "https://registry.example"},
		{name: "path prefix", value: "https://registry.example/sherpa//registry/", want: "https://registry.example/sherpa/registry"},
		{name: "http port", value: "http://Registry.Example:8080/", want: "http://registry.example:8080"},
		{name: "default HTTPS port", value: "https://Registry.Example:443/", want: "https://registry.example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://example/sherpa")
			t.Setenv("SHERPA_PUBLIC_BASE_URL", tc.value)
			cfg, err := LoadConfig()
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			if cfg.PublicBaseURL != tc.want {
				t.Fatalf("PublicBaseURL = %q, want %q", cfg.PublicBaseURL, tc.want)
			}
		})
	}
}

func TestLoadConfigRejectsInvalidPublicBaseURL(t *testing.T) {
	for _, value := range []string{
		"registry.example",
		"https:///registry",
		"ftp://registry.example",
		"https://user:password@registry.example",
		"https://registry.example?source=evil",
		"https://registry.example#fragment",
		"https://registry.example#",
	} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://example/sherpa")
			t.Setenv("SHERPA_PUBLIC_BASE_URL", value)
			_, err := LoadConfig()
			if err == nil || !strings.Contains(err.Error(), "SHERPA_PUBLIC_BASE_URL") {
				t.Fatalf("LoadConfig error = %v", err)
			}
		})
	}
}

func TestLoadConfigWebOAuthIsAllOrNothingAndPinned(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://example/sherpa")
	t.Setenv("SHERPA_GITHUB_CLIENT_ID", "client-id")
	t.Setenv("SHERPA_GITHUB_CLIENT_SECRET", "client-secret")
	t.Setenv("SHERPA_PUBLIC_BASE_URL", "https://registry.example/prefix/")
	t.Setenv("SHERPA_WEB_PUBLIC_BASE_URL", "https://web.example/")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GitHubClientSecret != "client-secret" || cfg.WebPublicBaseURL != "https://web.example" || cfg.PublicBaseURL != "https://registry.example/prefix" {
		t.Fatalf("config = %#v", cfg)
	}
	for _, tc := range []struct{ secret, web string }{{"secret", ""}, {"", "https://web.example"}} {
		t.Setenv("SHERPA_GITHUB_CLIENT_SECRET", tc.secret)
		t.Setenv("SHERPA_WEB_PUBLIC_BASE_URL", tc.web)
		if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "configured together") {
			t.Fatalf("partial web config error = %v", err)
		}
	}
}

func TestLoadConfigRequiresExportTokenWhenSchedulingEnabled(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://example/sherpa")
	t.Setenv("SHERPA_EXPORT_URL", "https://collector.example/upload")
	t.Setenv("SHERPA_EXPORT_TOKEN", "")
	t.Setenv("SHERPA_EXPORT_INTERVAL", "1h")
	t.Setenv("SHERPA_EXPORT_ARCHIVE_DIR", t.TempDir())

	_, err := LoadConfig()
	if err == nil || !strings.Contains(err.Error(), "SHERPA_EXPORT_TOKEN") {
		t.Fatalf("LoadConfig error = %v, want missing export token", err)
	}
}

func TestLoadConfigRejectsUnsafeWebOAuthOrigins(t *testing.T) {
	for _, tc := range []struct{ registry, web string }{
		{"http://registry.example", "https://web.example"},
		{"https://registry.example", "http://web.example"},
		{"https://same.example/a", "https://same.example/b"},
		{"https://registry.example", "https://user@web.example"},
	} {
		t.Setenv("DATABASE_URL", "postgres://example/sherpa")
		t.Setenv("SHERPA_GITHUB_CLIENT_ID", "client-id")
		t.Setenv("SHERPA_GITHUB_CLIENT_SECRET", "client-secret")
		t.Setenv("SHERPA_PUBLIC_BASE_URL", tc.registry)
		t.Setenv("SHERPA_WEB_PUBLIC_BASE_URL", tc.web)
		if _, err := LoadConfig(); err == nil {
			t.Fatalf("unsafe origins accepted: registry=%q web=%q", tc.registry, tc.web)
		}
	}
	t.Setenv("SHERPA_PUBLIC_BASE_URL", "http://127.0.0.1:8080")
	t.Setenv("SHERPA_WEB_PUBLIC_BASE_URL", "http://127.0.0.1:8081")
	if _, err := LoadConfig(); err != nil {
		t.Fatalf("loopback development origins rejected: %v", err)
	}
}
