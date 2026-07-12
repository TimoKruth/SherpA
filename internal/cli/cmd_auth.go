package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

type registrySession struct {
	AccessToken string `json:"access_token"`
	Login       string `json:"login"`
	RegistryURL string `json:"registry_url"`
}

type deviceStart struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	Interval        int    `json:"interval"`
	ExpiresIn       int    `json:"expires_in"`
}

func init() {
	register("login", cmdLogin)
	register("logout", cmdLogout)
}

func registrySessionPath(home string) string {
	return filepath.Join(home, "registry-session.json")
}

func saveRegistrySession(home, registryURL, token, login string) error {
	registryURL, err := normalizeRegistryBase(registryURL)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(registrySession{AccessToken: token, Login: login, RegistryURL: registryURL})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(home, ".registry-session-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, registrySessionPath(home))
}

func loadRegistrySession(home string) (string, string, string, error) {
	b, err := os.ReadFile(registrySessionPath(home))
	if err != nil {
		return "", "", "", err
	}
	var session registrySession
	if err := json.Unmarshal(b, &session); err != nil {
		return "", "", "", err
	}
	if session.AccessToken == "" {
		return "", "", "", errors.New("registry session has no access token")
	}
	if session.RegistryURL == "" {
		return "", "", "", errors.New("registry session has no registry issuer; log in again")
	}
	registryURL, err := normalizeRegistryBase(session.RegistryURL)
	if err != nil {
		return "", "", "", fmt.Errorf("registry session has invalid registry issuer: %w", err)
	}
	return session.AccessToken, session.Login, registryURL, nil
}

func registryToken(home, targetRegistryURL string) (string, error) {
	if token := strings.TrimSpace(os.Getenv("SHERPA_REGISTRY_TOKEN")); token != "" {
		return token, nil
	}
	targetRegistryURL, err := normalizeRegistryBase(targetRegistryURL)
	if err != nil {
		return "", err
	}
	token, _, sessionRegistryURL, err := loadRegistrySession(home)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if sessionRegistryURL != targetRegistryURL {
		return "", fmt.Errorf("registry session belongs to %s; log in to %s before publishing", sessionRegistryURL, targetRegistryURL)
	}
	return token, nil
}

func normalizeRegistryBase(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("registry URL is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("registry URL: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", errors.New("registry URL must include scheme and host")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", errors.New("registry URL must not include credentials, query, or fragment")
	}

	u.Scheme = strings.ToLower(u.Scheme)
	hostname := strings.ToLower(u.Hostname())
	port := u.Port()
	if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		port = ""
	}
	if port != "" {
		u.Host = net.JoinHostPort(hostname, port)
	} else if strings.Contains(hostname, ":") {
		u.Host = "[" + hostname + "]"
	} else {
		u.Host = hostname
	}
	u.Path = path.Clean("/" + strings.Trim(u.Path, "/"))
	if u.Path == "/" {
		u.Path = ""
	}
	u.RawPath = ""
	return u.String(), nil
}

func cmdLogin(ctx *Ctx, args []string) error {
	if len(args) != 0 {
		return errors.New("usage: sherpa login")
	}
	base, err := normalizeRegistryBase(os.Getenv("SHERPA_REGISTRY_URL"))
	if err != nil {
		return err
	}
	startURL, err := registryEndpoint(base, "v1", "auth", "device", "start")
	if err != nil {
		return err
	}
	startResp, err := http.Post(startURL, "application/json", bytes.NewReader(nil))
	if err != nil {
		return err
	}
	defer startResp.Body.Close()
	if startResp.StatusCode != http.StatusOK {
		return fmt.Errorf("registry login start: %s", startResp.Status)
	}
	var start deviceStart
	if err := json.NewDecoder(startResp.Body).Decode(&start); err != nil {
		return err
	}
	fmt.Fprintf(ctx.Stdout, "Open %s and enter code %s\n", start.VerificationURI, start.UserCode)
	interval := time.Duration(start.Interval) * time.Second
	pollURL, err := registryEndpoint(base, "v1", "auth", "device", "poll")
	if err != nil {
		return err
	}
	deadline := time.Now().Add(time.Duration(start.ExpiresIn) * time.Second)
	for time.Now().Before(deadline) {
		if interval > 0 {
			time.Sleep(interval)
		}
		body, _ := json.Marshal(map[string]string{"device_code": start.DeviceCode})
		resp, err := http.Post(pollURL, "application/json", bytes.NewReader(body))
		if err != nil {
			return err
		}
		responseBody, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return readErr
		}
		if resp.StatusCode == http.StatusAccepted {
			var waiting struct {
				Status string `json:"status"`
			}
			if err := json.Unmarshal(responseBody, &waiting); err != nil {
				return err
			}
			if waiting.Status == "slow_down" {
				interval += 5 * time.Second
			}
			continue
		}
		if resp.StatusCode == http.StatusGone {
			return errors.New("GitHub device code expired")
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("registry login poll: HTTP %d", resp.StatusCode)
		}
		var session registrySession
		if err := json.Unmarshal(responseBody, &session); err != nil {
			return err
		}
		if err := saveRegistrySession(ctx.Home, base, session.AccessToken, session.Login); err != nil {
			return err
		}
		fmt.Fprintf(ctx.Stdout, "logged in as %s\n", session.Login)
		return nil
	}
	return errors.New("GitHub device code expired")
}

func cmdLogout(ctx *Ctx, args []string) error {
	if len(args) != 0 {
		return errors.New("usage: sherpa logout")
	}
	err := os.Remove(registrySessionPath(ctx.Home))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Fprintln(ctx.Stdout, "logged out")
	return nil
}
