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
	Addr              string
	RegistryAPIURL    string
	RegistryPublicURL string
	PublicBaseURL     string
	UpstreamTimeout   time.Duration
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
	registryPublicURL, err := normalizedHTTPURL("SHERPA_REGISTRY_PUBLIC_URL", strings.TrimSpace(os.Getenv("SHERPA_REGISTRY_PUBLIC_URL")), false)
	if err != nil {
		return Config{}, err
	}
	if (publicURL == "") != (registryPublicURL == "") {
		return Config{}, errors.New("SHERPA_WEB_PUBLIC_BASE_URL and SHERPA_REGISTRY_PUBLIC_URL must be configured together")
	}
	if publicURL != "" {
		webParsed, _ := url.Parse(publicURL)
		registryParsed, _ := url.Parse(registryPublicURL)
		if webParsed.Scheme == registryParsed.Scheme && webParsed.Host == registryParsed.Host {
			return Config{}, errors.New("website and registry public origins must be distinct")
		}
		if !webSecureOrLoopback(webParsed) || !webSecureOrLoopback(registryParsed) {
			return Config{}, errors.New("public URLs must use HTTPS outside loopback development")
		}
	}
	timeout, err := loadUpstreamTimeout()
	if err != nil {
		return Config{}, err
	}
	return Config{
		Addr: addr, RegistryAPIURL: registryURL, RegistryPublicURL: registryPublicURL, PublicBaseURL: publicURL, UpstreamTimeout: timeout,
	}, nil
}

func webSecureOrLoopback(parsed *url.URL) bool {
	if parsed.Scheme == "https" {
		return true
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
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
	for _, char := range port {
		if char < '0' || char > '9' {
			return errors.New("invalid port")
		}
	}
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
