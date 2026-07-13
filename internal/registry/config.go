package registry

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Port               string
	DatabaseURL        string
	Token              string
	ContentDir         string
	GitHubClientID     string
	GitHubClientSecret string
	PublicBaseURL      string
	WebPublicBaseURL   string
	TrustProxy         bool
	DBMaxConns         int
	ExportURL          string
	ExportToken        string
	ExportInterval     time.Duration
	ExportArchiveDir   string
}

func LoadConfig() (Config, error) {
	cfg := Config{
		Port:               valueOrDefault("PORT", "8080"),
		DatabaseURL:        strings.TrimSpace(os.Getenv("DATABASE_URL")),
		Token:              strings.TrimSpace(os.Getenv("SHERPA_REGISTRY_TOKEN")),
		ContentDir:         valueOrDefault("SHERPA_CONTENT_DIR", "./registry-content"),
		GitHubClientID:     strings.TrimSpace(os.Getenv("SHERPA_GITHUB_CLIENT_ID")),
		GitHubClientSecret: strings.TrimSpace(os.Getenv("SHERPA_GITHUB_CLIENT_SECRET")),
		PublicBaseURL:      strings.TrimSpace(os.Getenv("SHERPA_PUBLIC_BASE_URL")),
		WebPublicBaseURL:   strings.TrimSpace(os.Getenv("SHERPA_WEB_PUBLIC_BASE_URL")),
		ExportURL:          strings.TrimSpace(os.Getenv("SHERPA_EXPORT_URL")),
		ExportToken:        strings.TrimSpace(os.Getenv("SHERPA_EXPORT_TOKEN")),
		ExportArchiveDir:   strings.TrimSpace(os.Getenv("SHERPA_EXPORT_ARCHIVE_DIR")),
	}

	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
	}
	if cfg.PublicBaseURL != "" {
		var err error
		cfg.PublicBaseURL, err = canonicalPublicBaseURL(cfg.PublicBaseURL)
		if err != nil {
			return Config{}, fmt.Errorf("SHERPA_PUBLIC_BASE_URL: %w", err)
		}
	}
	webConfigured := cfg.GitHubClientSecret != "" || cfg.WebPublicBaseURL != ""
	if webConfigured && (cfg.GitHubClientSecret == "" || cfg.WebPublicBaseURL == "") {
		return Config{}, fmt.Errorf("SHERPA_GITHUB_CLIENT_SECRET and SHERPA_WEB_PUBLIC_BASE_URL must be configured together")
	}
	if webConfigured {
		if cfg.GitHubClientID == "" || cfg.PublicBaseURL == "" {
			return Config{}, fmt.Errorf("SHERPA_GITHUB_CLIENT_ID and SHERPA_PUBLIC_BASE_URL are required when web OAuth is enabled")
		}
		var err error
		cfg.WebPublicBaseURL, err = canonicalPublicBaseURL(cfg.WebPublicBaseURL)
		if err != nil {
			return Config{}, fmt.Errorf("SHERPA_WEB_PUBLIC_BASE_URL: %w", err)
		}
		registryURL, _ := url.Parse(cfg.PublicBaseURL)
		websiteURL, _ := url.Parse(cfg.WebPublicBaseURL)
		if registryURL.Scheme == websiteURL.Scheme && registryURL.Host == websiteURL.Host {
			return Config{}, fmt.Errorf("registry and website public origins must be distinct")
		}
		if !secureOrLoopback(registryURL) || !secureOrLoopback(websiteURL) {
			return Config{}, fmt.Errorf("web OAuth public URLs must use HTTPS outside loopback development")
		}
	}

	var err error
	if cfg.TrustProxy, err = optionalBool("SHERPA_TRUST_PROXY"); err != nil {
		return Config{}, err
	}
	if cfg.DBMaxConns, err = optionalNonNegativeInt("SHERPA_DB_MAX_CONNS"); err != nil {
		return Config{}, err
	}
	if cfg.ExportInterval, err = optionalPositiveDuration("SHERPA_EXPORT_INTERVAL"); err != nil {
		return Config{}, err
	}
	exportConfigured := cfg.ExportURL != "" || cfg.ExportToken != "" || cfg.ExportInterval != 0 || cfg.ExportArchiveDir != ""
	if exportConfigured && (cfg.ExportURL == "" || cfg.ExportInterval == 0) {
		return Config{}, fmt.Errorf("SHERPA_EXPORT_URL and SHERPA_EXPORT_INTERVAL must be configured together")
	}
	return cfg, nil
}

func secureOrLoopback(parsed *url.URL) bool {
	if parsed.Scheme == "https" {
		return true
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func canonicalPublicBaseURL(value string) (string, error) {
	parsed, err := url.Parse(value)
	if err != nil {
		return "", fmt.Errorf("must be a valid URL: %w", err)
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("scheme must be http or https")
	}
	if parsed.Opaque != "" || parsed.Host == "" || parsed.Hostname() == "" {
		return "", fmt.Errorf("scheme and host are required")
	}
	if parsed.User != nil {
		return "", fmt.Errorf("credentials are not allowed")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery {
		return "", fmt.Errorf("query is not allowed")
	}
	if parsed.Fragment != "" || strings.Contains(value, "#") {
		return "", fmt.Errorf("fragment is not allowed")
	}

	hostname := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	if (parsed.Scheme == "https" && port == "443") || (parsed.Scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		parsed.Host = net.JoinHostPort(hostname, port)
	} else if strings.Contains(hostname, ":") {
		parsed.Host = "[" + hostname + "]"
	} else {
		parsed.Host = hostname
	}
	parsed.RawPath = ""
	parsed.RawFragment = ""
	parsed.Path = path.Clean(parsed.Path)
	if parsed.Path == "." || parsed.Path == "/" {
		parsed.Path = ""
	}
	return parsed.String(), nil
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

func optionalPositiveDuration(key string) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return 0, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", key)
	}
	return duration, nil
}

func valueOrDefault(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}
