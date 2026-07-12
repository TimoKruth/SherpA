package registry

import (
	"fmt"
	"os"
	"strings"
)

type Config struct {
	Port           string
	DatabaseURL    string
	Token          string
	ContentDir     string
	GitHubClientID string
}

func LoadConfig() (Config, error) {
	cfg := Config{
		Port:           valueOrDefault("PORT", "8080"),
		DatabaseURL:    strings.TrimSpace(os.Getenv("DATABASE_URL")),
		Token:          strings.TrimSpace(os.Getenv("SHERPA_REGISTRY_TOKEN")),
		ContentDir:     valueOrDefault("SHERPA_CONTENT_DIR", "./registry-content"),
		GitHubClientID: strings.TrimSpace(os.Getenv("SHERPA_GITHUB_CLIENT_ID")),
	}

	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
	}
	return cfg, nil
}

func valueOrDefault(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}
