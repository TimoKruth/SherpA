package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type registrySession struct {
	AccessToken string `json:"access_token"`
	Login       string `json:"login"`
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

func saveRegistrySession(home, token, login string) error {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(registrySession{AccessToken: token, Login: login})
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

func loadRegistrySession(home string) (string, string, error) {
	b, err := os.ReadFile(registrySessionPath(home))
	if err != nil {
		return "", "", err
	}
	var session registrySession
	if err := json.Unmarshal(b, &session); err != nil {
		return "", "", err
	}
	if session.AccessToken == "" {
		return "", "", errors.New("registry session has no access token")
	}
	return session.AccessToken, session.Login, nil
}

func registryToken(home string) (string, error) {
	if token := strings.TrimSpace(os.Getenv("SHERPA_REGISTRY_TOKEN")); token != "" {
		return token, nil
	}
	token, _, err := loadRegistrySession(home)
	if os.IsNotExist(err) {
		return "", nil
	}
	return token, err
}

func cmdLogin(ctx *Ctx, args []string) error {
	if len(args) != 0 {
		return errors.New("usage: sherpa login")
	}
	base := strings.TrimSpace(os.Getenv("SHERPA_REGISTRY_URL"))
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
		if err := saveRegistrySession(ctx.Home, session.AccessToken, session.Login); err != nil {
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
