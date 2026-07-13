package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type HTTPGitHubClient struct {
	clientID     string
	clientSecret string
	loginURL     string
	apiURL       string
	client       *http.Client
}

func NewGitHubClient(clientID string, httpBaseURLs ...string) *HTTPGitHubClient {
	return NewGitHubClientWithSecret(clientID, "", httpBaseURLs...)
}

func NewGitHubClientWithSecret(clientID, clientSecret string, httpBaseURLs ...string) *HTTPGitHubClient {
	loginURL := "https://github.com"
	apiURL := "https://api.github.com"
	if len(httpBaseURLs) > 0 {
		loginURL = strings.TrimRight(httpBaseURLs[0], "/")
	}
	if len(httpBaseURLs) > 1 {
		apiURL = strings.TrimRight(httpBaseURLs[1], "/")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	return &HTTPGitHubClient{clientID: clientID, clientSecret: clientSecret, loginURL: loginURL, apiURL: apiURL, client: &http.Client{
		Transport: transport, Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("GitHub redirect rejected") },
	}}
}

func (g *HTTPGitHubClient) WebAuthorizeURL(state, codeChallenge, callbackURL string) (string, error) {
	if strings.TrimSpace(g.clientID) == "" || state == "" || codeChallenge == "" || callbackURL == "" {
		return "", errors.New("GitHub web OAuth is not configured")
	}
	u, err := url.Parse(g.loginURL + "/login/oauth/authorize")
	if err != nil {
		return "", err
	}
	u.RawQuery = url.Values{
		"client_id": {g.clientID}, "redirect_uri": {callbackURL}, "state": {state},
		"code_challenge": {codeChallenge}, "code_challenge_method": {"S256"},
	}.Encode()
	return u.String(), nil
}

func (g *HTTPGitHubClient) ExchangeWebCode(ctx context.Context, code, codeVerifier, callbackURL string) (string, error) {
	if strings.TrimSpace(g.clientSecret) == "" {
		return "", errors.New("GitHub web OAuth is not configured")
	}
	var out struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		Error       string `json:"error"`
	}
	if err := g.postForm(ctx, g.loginURL+"/login/oauth/access_token", url.Values{
		"client_id": {g.clientID}, "client_secret": {g.clientSecret}, "code": {code},
		"redirect_uri": {callbackURL}, "code_verifier": {codeVerifier},
	}, &out); err != nil {
		return "", err
	}
	if out.Error != "" || out.AccessToken == "" || !strings.EqualFold(out.TokenType, "bearer") {
		return "", errors.New("GitHub OAuth token response was invalid")
	}
	return out.AccessToken, nil
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
	if err := decodeBoundedGitHubJSON(resp.Body, &user); err != nil {
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
	return decodeBoundedGitHubJSON(resp.Body, out)
}

func decodeBoundedGitHubJSON(body io.Reader, out any) error {
	encoded, err := io.ReadAll(io.LimitReader(body, (1<<20)+1))
	if err != nil {
		return err
	}
	if len(encoded) > 1<<20 {
		return errors.New("GitHub response too large")
	}
	if err := json.Unmarshal(encoded, out); err != nil {
		return errors.New("GitHub response was invalid")
	}
	return nil
}
