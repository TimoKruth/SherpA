package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const (
	registrySocialTimeout = 10 * time.Second
	registrySocialBodyMax = 1 << 20
	registrySocialListMax = 50
)

var (
	errRegistryUnauthorized = errors.New("registry session is unauthorized; run `sherpa login`")
	errRegistryForbidden    = errors.New("registry request is forbidden")
	errRegistryNotFound     = errors.New("registry resource not found")
	errRegistryGone         = errors.New("registry credential is expired or already used")
	errRegistryRateLimited  = errors.New("registry request was rate limited")
	errRegistryUnavailable  = errors.New("registry is unavailable")
	errRegistryResponse     = errors.New("registry returned an invalid response")
)

type registrySocialClient struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

type registryFollow struct {
	Ref               string    `json:"ref"`
	Owner             string    `json:"owner"`
	Name              string    `json:"name"`
	Summary           string    `json:"summary"`
	Harness           string    `json:"harness"`
	Tags              []string  `json:"tags"`
	LatestVersion     int       `json:"latest_version"`
	LatestGitTag      string    `json:"latest_git_tag"`
	LatestTrustTier   string    `json:"latest_trust_tier"`
	LatestPublishedAt time.Time `json:"latest_published_at"`
	LastSeenVersion   int       `json:"last_seen_version"`
	FollowerCount     int       `json:"follower_count"`
	FollowedAt        time.Time `json:"followed_at"`
}

type registryUpdate struct {
	Ref         string    `json:"ref"`
	Owner       string    `json:"owner"`
	Name        string    `json:"name"`
	Version     int       `json:"version"`
	GitTag      string    `json:"git_tag"`
	Changelog   string    `json:"changelog"`
	TrustTier   string    `json:"trust_tier"`
	PublishedAt time.Time `json:"published_at"`
	SeenVersion int       `json:"seen_version"`
}

type registryFollowPage struct {
	Follows    []registryFollow `json:"follows"`
	NextCursor string           `json:"next_cursor"`
}

type registryUpdatePage struct {
	Updates    []registryUpdate `json:"updates"`
	NextCursor string           `json:"next_cursor"`
}

func newRegistrySocialClient(baseURL, token string) (*registrySocialClient, error) {
	baseURL, err := normalizeRegistryBase(baseURL)
	if err != nil {
		return nil, err
	}
	if token == "" {
		return nil, errRegistryUnauthorized
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	transport.TLSHandshakeTimeout = 5 * time.Second
	transport.ResponseHeaderTimeout = 5 * time.Second
	transport.IdleConnTimeout = 30 * time.Second
	return &registrySocialClient{
		baseURL: baseURL,
		token:   token,
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   registrySocialTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return errors.New("redirect rejected")
			},
		},
	}, nil
}

func (c *registrySocialClient) Follow(ctx context.Context, owner, name string) (registryFollow, error) {
	var follow registryFollow
	err := c.do(ctx, http.MethodPut, []string{"v1", "me", "follows", owner, name}, nil, nil, &follow, http.StatusOK)
	if err == nil && (follow.Owner != owner || follow.Name != name || !validRegistryFollow(follow)) {
		err = errRegistryResponse
	}
	return follow, err
}

func (c *registrySocialClient) Unfollow(ctx context.Context, owner, name string) error {
	return c.do(ctx, http.MethodDelete, []string{"v1", "me", "follows", owner, name}, nil, nil, nil, http.StatusNoContent)
}

func (c *registrySocialClient) ListFollows(ctx context.Context, limit int, cursor string) (registryFollowPage, error) {
	var page registryFollowPage
	if limit <= 0 || limit > registrySocialListMax {
		return page, errors.New("follow limit must be between 1 and 50")
	}
	query := url.Values{"limit": {strconv.Itoa(limit)}}
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	err := c.do(ctx, http.MethodGet, []string{"v1", "me", "follows"}, query, nil, &page, http.StatusOK)
	if err == nil {
		if len(page.Follows) > limit {
			err = errRegistryResponse
		} else {
			for _, follow := range page.Follows {
				if !validRegistryFollow(follow) {
					err = errRegistryResponse
					break
				}
			}
		}
	}
	return page, err
}

func (c *registrySocialClient) ListUpdates(ctx context.Context, limit int, cursor string) (registryUpdatePage, error) {
	var page registryUpdatePage
	if limit <= 0 || limit > registrySocialListMax {
		return page, errors.New("update limit must be between 1 and 50")
	}
	query := url.Values{"limit": {strconv.Itoa(limit)}}
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	err := c.do(ctx, http.MethodGet, []string{"v1", "me", "updates"}, query, nil, &page, http.StatusOK)
	if err == nil {
		if len(page.Updates) > limit {
			err = errRegistryResponse
		} else {
			for _, update := range page.Updates {
				if !validRegistryUpdate(update) {
					err = errRegistryResponse
					break
				}
			}
		}
	}
	return page, err
}

func (c *registrySocialClient) MarkSeen(ctx context.Context, owner, name string, version int) (registryFollow, error) {
	var follow registryFollow
	body := struct {
		Version int `json:"version"`
	}{Version: version}
	err := c.do(ctx, http.MethodPut, []string{"v1", "me", "follows", owner, name, "seen"}, nil, body, &follow, http.StatusOK)
	return follow, err
}

func (c *registrySocialClient) PutTrial(ctx context.Context, owner, name string, version int, verdict string) error {
	body := struct {
		Verdict string `json:"verdict"`
	}{Verdict: verdict}
	return c.do(ctx, http.MethodPut, []string{"v1", "me", "trials", owner, name, strconv.Itoa(version)}, nil, body, nil, http.StatusNoContent)
}

func (c *registrySocialClient) Revoke(ctx context.Context) error {
	return c.do(ctx, http.MethodDelete, []string{"v1", "me", "session"}, nil, nil, nil, http.StatusNoContent)
}

func (c *registrySocialClient) do(ctx context.Context, method string, pathParts []string, query url.Values, requestBody, responseBody any, expectedStatus int) error {
	endpoint, err := registryEndpoint(c.baseURL, pathParts...)
	if err != nil {
		return errRegistryResponse
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return errRegistryResponse
	}
	if query != nil {
		u.RawQuery = query.Encode()
	}
	var body io.Reader
	if requestBody != nil {
		encoded, err := json.Marshal(requestBody)
		if err != nil {
			return errRegistryResponse
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return errRegistryResponse
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	if requestBody != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errRegistryUnavailable
	}
	defer response.Body.Close()
	encoded, err := io.ReadAll(io.LimitReader(response.Body, registrySocialBodyMax+1))
	if err != nil {
		return errRegistryUnavailable
	}
	if len(encoded) > registrySocialBodyMax {
		return errRegistryResponse
	}
	if response.StatusCode != expectedStatus {
		return classifyRegistryStatus(response.StatusCode)
	}
	if responseBody == nil {
		return nil
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return errRegistryResponse
	}
	if err := json.Unmarshal(encoded, responseBody); err != nil {
		return errRegistryResponse
	}
	return nil
}

func classifyRegistryStatus(status int) error {
	switch status {
	case http.StatusUnauthorized:
		return errRegistryUnauthorized
	case http.StatusForbidden:
		return errRegistryForbidden
	case http.StatusNotFound:
		return errRegistryNotFound
	case http.StatusGone:
		return errRegistryGone
	case http.StatusTooManyRequests:
		return errRegistryRateLimited
	default:
		if status >= 500 {
			return errRegistryUnavailable
		}
		return errRegistryResponse
	}
}

func validRegistryFollow(follow registryFollow) bool {
	return len(follow.Owner) <= 100 && len(follow.Name) <= 100 &&
		validRegistrySegment(follow.Owner) && validRegistrySegment(follow.Name) &&
		follow.LatestVersion >= 0 && follow.LastSeenVersion >= 0 &&
		follow.LastSeenVersion <= follow.LatestVersion && follow.FollowerCount >= 0
}

func validRegistryUpdate(update registryUpdate) bool {
	return len(update.Owner) <= 100 && len(update.Name) <= 100 &&
		validRegistrySegment(update.Owner) && validRegistrySegment(update.Name) &&
		update.Version > 0 && update.SeenVersion >= 0 && update.SeenVersion < update.Version
}
