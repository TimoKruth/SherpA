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
