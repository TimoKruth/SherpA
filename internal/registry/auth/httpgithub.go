package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type HTTPGitHubClient struct {
	clientID string
	loginURL string
	apiURL   string
	client   *http.Client
}

func NewGitHubClient(clientID string, httpBaseURLs ...string) *HTTPGitHubClient {
	loginURL := "https://github.com"
	apiURL := "https://api.github.com"
	if len(httpBaseURLs) > 0 {
		loginURL = strings.TrimRight(httpBaseURLs[0], "/")
	}
	if len(httpBaseURLs) > 1 {
		apiURL = strings.TrimRight(httpBaseURLs[1], "/")
	}
	return &HTTPGitHubClient{clientID: clientID, loginURL: loginURL, apiURL: apiURL, client: &http.Client{Timeout: 30 * time.Second}}
}

func (g *HTTPGitHubClient) StartDeviceFlow(ctx context.Context) (DeviceCode, error) {
	if strings.TrimSpace(g.clientID) == "" {
		return DeviceCode{}, fmt.Errorf("SHERPA_GITHUB_CLIENT_ID is required for login")
	}
	var out DeviceCode
	err := g.postForm(ctx, g.loginURL+"/login/device/code", url.Values{"client_id": {g.clientID}}, &out)
	if err == nil && out.Interval <= 0 {
		out.Interval = 5
	}
	return out, err
}

func (g *HTTPGitHubClient) PollToken(ctx context.Context, deviceCode string) (string, error) {
	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	err := g.postForm(ctx, g.loginURL+"/login/oauth/access_token", url.Values{
		"client_id":   {g.clientID},
		"device_code": {deviceCode},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
	}, &out)
	if err != nil {
		return "", err
	}
	switch out.Error {
	case "authorization_pending":
		return "", ErrAuthPending
	case "slow_down":
		return "", ErrSlowDown
	case "expired_token":
		return "", ErrExpired
	case "":
		if out.AccessToken == "" {
			return "", fmt.Errorf("GitHub token response omitted access_token")
		}
		return out.AccessToken, nil
	default:
		return "", fmt.Errorf("GitHub device flow: %s", out.Error)
	}
}

func (g *HTTPGitHubClient) GetUser(ctx context.Context, accessToken string) (GitHubUser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.apiURL+"/user", nil)
	if err != nil {
		return GitHubUser{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := g.client.Do(req)
	if err != nil {
		return GitHubUser{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return GitHubUser{}, fmt.Errorf("GitHub user endpoint: HTTP %d", resp.StatusCode)
	}
	var user GitHubUser
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		return GitHubUser{}, err
	}
	if user.ID == 0 || strings.TrimSpace(user.Login) == "" {
		return GitHubUser{}, fmt.Errorf("GitHub user response omitted id or login")
	}
	return user, nil
}

func (g *HTTPGitHubClient) postForm(ctx context.Context, endpoint string, values url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("GitHub endpoint: HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
