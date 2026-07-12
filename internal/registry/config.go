package registry

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Port           string
	DatabaseURL    string
	Token          string
	ContentDir     string
	GitHubClientID string
	PublicBaseURL  string
	TrustProxy     bool
	DBMaxConns     int
}

func LoadConfig() (Config, error) {
	cfg := Config{
		Port:           valueOrDefault("PORT", "8080"),
		DatabaseURL:    strings.TrimSpace(os.Getenv("DATABASE_URL")),
		Token:          strings.TrimSpace(os.Getenv("SHERPA_REGISTRY_TOKEN")),
		ContentDir:     valueOrDefault("SHERPA_CONTENT_DIR", "./registry-content"),
		GitHubClientID: strings.TrimSpace(os.Getenv("SHERPA_GITHUB_CLIENT_ID")),
		PublicBaseURL:  strings.TrimSpace(os.Getenv("SHERPA_PUBLIC_BASE_URL")),
	}

	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
	}

	var err error
	if cfg.TrustProxy, err = optionalBool("SHERPA_TRUST_PROXY"); err != nil {
		return Config{}, err
	}
	if cfg.DBMaxConns, err = optionalNonNegativeInt("SHERPA_DB_MAX_CONNS"); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func optionalBool(key string) (bool, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return false, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean: %w", key, err)
	}
	return parsed, nil
}

func optionalNonNegativeInt(key string) (int, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return 0, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer", key)
	}
	return parsed, nil
}

func valueOrDefault(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}
