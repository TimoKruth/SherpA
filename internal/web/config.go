package web

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
)

const (
	defaultWebAddr         = "[::]:8080"
	defaultUpstreamTimeout = 5 * time.Second
	minimumUpstreamTimeout = 100 * time.Millisecond
	maximumUpstreamTimeout = 30 * time.Second
)

type Config struct {
	Addr            string
	RegistryAPIURL  string
	PublicBaseURL   string
	UpstreamTimeout time.Duration
}

func LoadConfig() (Config, error) {
	addr, err := loadAddr()
	if err != nil {
		return Config{}, err
	}
	registryURL, err := normalizedHTTPURL("SHERPA_REGISTRY_API_URL", strings.TrimSpace(os.Getenv("SHERPA_REGISTRY_API_URL")), true)
	if err != nil {
		return Config{}, err
	}
	publicURL, err := normalizedHTTPURL("SHERPA_WEB_PUBLIC_BASE_URL", strings.TrimSpace(os.Getenv("SHERPA_WEB_PUBLIC_BASE_URL")), false)
	if err != nil {
		return Config{}, err
	}
	timeout, err := loadUpstreamTimeout()
	if err != nil {
		return Config{}, err
	}
	return Config{
		Addr: addr, RegistryAPIURL: registryURL, PublicBaseURL: publicURL, UpstreamTimeout: timeout,
	}, nil
}

func loadAddr() (string, error) {
	if addr := strings.TrimSpace(os.Getenv("SHERPA_WEB_ADDR")); addr != "" {
		if err := validateListenAddr(addr); err != nil {
			return "", fmt.Errorf("SHERPA_WEB_ADDR must be a valid TCP listen address")
		}
		return addr, nil
	}
	port := strings.TrimSpace(os.Getenv("PORT"))
	if port == "" {
		return defaultWebAddr, nil
	}
	if err := validatePort(port); err != nil {
		return "", errors.New("PORT must be an integer between 1 and 65535")
	}
	return net.JoinHostPort("::", port), nil
}

func validateListenAddr(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	return validatePort(port)
}

func validatePort(port string) error {
	value, err := strconv.Atoi(port)
	if err != nil || value < 1 || value > 65535 {
		return errors.New("invalid port")
	}
	return nil
}

func normalizedHTTPURL(key, raw string, required bool) (string, error) {
	if raw == "" {
		if required {
			return "", fmt.Errorf("%s is required", key)
		}
		return "", nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" || parsed.Hostname() == "" {
		return "", fmt.Errorf("%s must be an absolute HTTP(S) URL", key)
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("%s must be an absolute HTTP(S) URL", key)
	}
	if parsed.Opaque != "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || strings.Contains(raw, "#") {
		return "", fmt.Errorf("%s must not contain credentials, query, or fragment", key)
	}
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Path = path.Clean(parsed.Path)
	if parsed.Path == "." || parsed.Path == "/" {
		parsed.Path = ""
	}
	parsed.RawPath = ""
	return parsed.String(), nil
}

func loadUpstreamTimeout() (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv("SHERPA_WEB_UPSTREAM_TIMEOUT"))
	if raw == "" {
		return defaultUpstreamTimeout, nil
	}
	timeout, err := time.ParseDuration(raw)
	if err != nil || timeout < minimumUpstreamTimeout || timeout > maximumUpstreamTimeout {
		return 0, errors.New("SHERPA_WEB_UPSTREAM_TIMEOUT must be between 100ms and 30s")
	}
	return timeout, nil
}
