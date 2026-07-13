package registryclient

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
	"path"
	"strconv"
	"strings"
	"time"
)

var (
	ErrNotFound     = errors.New("registry resource not found")
	ErrBadGateway   = errors.New("invalid registry response")
	ErrUnavailable  = errors.New("registry unavailable")
	ErrUnauthorized = errors.New("registry session unauthorized")
	ErrForbidden    = errors.New("registry request forbidden")
	ErrGone         = errors.New("registry grant unavailable")
	ErrRateLimited  = errors.New("registry rate limited")
)

const (
	maxResponseBytes = 1 << 20
	maxPageLimit     = 50
	transportBound   = 5 * time.Second
)

type Client struct {
	base             *url.URL
	http             *http.Client
	maxResponseBytes int64
}

func (c *Client) ExchangeWebGrant(ctx context.Context, grant, nonce string) (WebSession, error) {
	var result WebSession
	err := c.authRequest(ctx, http.MethodPost, "/v1/auth/web/exchange", nil, map[string]string{"grant": grant, "handoff_nonce": nonce}, "", &result, http.StatusOK)
	if err == nil && (!validSessionToken(result.AccessToken) || !validSegment(result.Login) || result.Purpose != "web") {
		err = ErrBadGateway
	}
	return result, err
}

func validSessionToken(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

func (c *Client) Me(ctx context.Context, token string) (Me, error) {
	var result Me
	err := c.authRequest(ctx, http.MethodGet, "/v1/me", nil, nil, token, &result, http.StatusOK)
	if err == nil && (result.Login == "" || (result.Purpose != "cli" && result.Purpose != "web")) {
		err = ErrBadGateway
	}
	return result, err
}

func (c *Client) Revoke(ctx context.Context, token string) error {
	return c.authRequest(ctx, http.MethodDelete, "/v1/me/session", nil, nil, token, nil, http.StatusNoContent)
}
func (c *Client) Follow(ctx context.Context, token, owner, name string) (Follow, error) {
	var out Follow
	if !validSegment(owner) || !validSegment(name) {
		return out, ErrBadGateway
	}
	err := c.authRequest(ctx, http.MethodPut, "/v1/me/follows/"+url.PathEscape(owner)+"/"+url.PathEscape(name), nil, nil, token, &out, http.StatusOK)
	return out, err
}
func (c *Client) Unfollow(ctx context.Context, token, owner, name string) error {
	if !validSegment(owner) || !validSegment(name) {
		return ErrBadGateway
	}
	return c.authRequest(ctx, http.MethodDelete, "/v1/me/follows/"+url.PathEscape(owner)+"/"+url.PathEscape(name), nil, nil, token, nil, http.StatusNoContent)
}
func (c *Client) Follows(ctx context.Context, token string, limit int, cursor string) (FollowPage, error) {
	var out FollowPage
	if limit < 1 || limit > maxPageLimit || len(cursor) > 512 {
		return out, ErrBadGateway
	}
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	err := c.authRequest(ctx, http.MethodGet, "/v1/me/follows", q, nil, token, &out, http.StatusOK)
	if err == nil && len(out.Follows) > limit {
		err = ErrBadGateway
	}
	return out, err
}
func (c *Client) Updates(ctx context.Context, token string, limit int, cursor string) (UpdatePage, error) {
	var out UpdatePage
	if limit < 1 || limit > maxPageLimit || len(cursor) > 512 {
		return out, ErrBadGateway
	}
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	err := c.authRequest(ctx, http.MethodGet, "/v1/me/updates", q, nil, token, &out, http.StatusOK)
	if err == nil && len(out.Updates) > limit {
		err = ErrBadGateway
	}
	return out, err
}
func (c *Client) MarkSeen(ctx context.Context, token, owner, name string, version int) (Follow, error) {
	var out Follow
	if !validSegment(owner) || !validSegment(name) || version < 1 {
		return out, ErrBadGateway
	}
	err := c.authRequest(ctx, http.MethodPut, "/v1/me/follows/"+url.PathEscape(owner)+"/"+url.PathEscape(name)+"/seen", nil, map[string]int{"version": version}, token, &out, http.StatusOK)
	return out, err
}
func (c *Client) PutTrial(ctx context.Context, token, owner, name string, version int, verdict string) error {
	if !validSegment(owner) || !validSegment(name) || version < 1 {
		return ErrBadGateway
	}
	return c.authRequest(ctx, http.MethodPut, "/v1/me/trials/"+url.PathEscape(owner)+"/"+url.PathEscape(name)+"/"+strconv.Itoa(version), nil, map[string]string{"verdict": verdict}, token, nil, http.StatusNoContent)
}

func (c *Client) GetUser(ctx context.Context, handle string, page Page) (UserProfile, error) {
	var out UserProfile
	if !validSegment(handle) || !validPage(page) {
		return out, ErrBadGateway
	}
	q := url.Values{"limit": {strconv.Itoa(page.Limit)}, "offset": {strconv.Itoa(page.Offset)}}
	if err := c.get(ctx, "/v1/users/"+url.PathEscape(handle), q, &out); err != nil {
		return out, err
	}
	if out.Handle != handle || out.TotalStackFollows < 0 || len(out.Stacks) > page.Limit {
		return UserProfile{}, ErrBadGateway
	}
	return out, nil
}

func (c *Client) authRequest(ctx context.Context, method, endpoint string, query url.Values, requestBody any, token string, destination any, expected int) error {
	if endpoint != "/v1/auth/web/exchange" && token == "" {
		return ErrUnauthorized
	}
	requestURL := *c.base
	requestURL.Path = strings.TrimSuffix(c.base.Path, "/") + endpoint
	if query != nil {
		requestURL.RawQuery = query.Encode()
	}
	var body io.Reader
	if requestBody != nil {
		encoded, err := json.Marshal(requestBody)
		if err != nil {
			return ErrBadGateway
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, requestURL.String(), body)
	if err != nil {
		return ErrBadGateway
	}
	request.Header.Set("Accept", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if requestBody != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrUnavailable
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusUnauthorized:
		return ErrUnauthorized
	case http.StatusForbidden:
		return ErrForbidden
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusGone:
		return ErrGone
	case http.StatusTooManyRequests:
		return ErrRateLimited
	}
	if response.StatusCode >= 500 {
		return ErrUnavailable
	}
	if response.StatusCode != expected {
		return ErrBadGateway
	}
	encoded, err := io.ReadAll(io.LimitReader(response.Body, c.maxResponseBytes+1))
	if err != nil {
		return ErrUnavailable
	}
	if int64(len(encoded)) > c.maxResponseBytes {
		return ErrBadGateway
	}
	if destination == nil {
		return nil
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return ErrBadGateway
	}
	if err := json.Unmarshal(encoded, destination); err != nil {
		return ErrBadGateway
	}
	return nil
}

func New(rawBaseURL string, timeout time.Duration) (*Client, error) {
	base, err := validateBaseURL(rawBaseURL)
	if err != nil {
		return nil, err
	}
	if timeout <= 0 {
		return nil, errors.New("registry timeout must be positive")
	}
	responseHeaderTimeout := timeout
	if responseHeaderTimeout > transportBound {
		responseHeaderTimeout = transportBound
	}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: transportBound, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          16,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   transportBound,
		ResponseHeaderTimeout: responseHeaderTimeout,
	}
	return &Client{
		base: base,
		http: &http.Client{
			Transport: transport,
			Timeout:   timeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		maxResponseBytes: maxResponseBytes,
	}, nil
}

func validateBaseURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" || parsed.Hostname() == "" {
		return nil, errors.New("invalid registry base URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("invalid registry base URL")
	}
	if parsed.Opaque != "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || strings.Contains(raw, "#") {
		return nil, errors.New("invalid registry base URL")
	}
	parsed.Path = path.Clean(parsed.Path)
	if parsed.Path == "." || parsed.Path == "/" {
		parsed.Path = ""
	}
	parsed.RawPath = ""
	return parsed, nil
}

func (c *Client) Search(ctx context.Context, query SearchQuery) (SearchResult, error) {
	if !validPage(query.Page) {
		return SearchResult{}, ErrBadGateway
	}
	values := url.Values{}
	values.Set("limit", strconv.Itoa(query.Page.Limit))
	values.Set("offset", strconv.Itoa(query.Page.Offset))
	if query.Q != "" {
		values.Set("q", query.Q)
	}
	if query.Harness != "" {
		values.Set("harness", query.Harness)
	}
	if query.Tag != "" {
		values.Set("tag", query.Tag)
	}
	var result SearchResult
	if err := c.get(ctx, "/v1/search", values, &result); err != nil {
		return SearchResult{}, err
	}
	if result.NextOffset != nil && *result.NextOffset < 0 {
		return SearchResult{}, ErrBadGateway
	}
	for _, stack := range result.Stacks {
		if !validRepoURL(stack.RepoURL) {
			return SearchResult{}, ErrBadGateway
		}
	}
	return result, nil
}

func (c *Client) GetStack(ctx context.Context, owner, name string, page Page) (Stack, error) {
	if !validSegment(owner) || !validSegment(name) || !validPage(page) {
		return Stack{}, ErrBadGateway
	}
	values := url.Values{}
	values.Set("versions_limit", strconv.Itoa(page.Limit))
	values.Set("versions_offset", strconv.Itoa(page.Offset))
	var stack Stack
	path := "/v1/stacks/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
	if err := c.get(ctx, path, values, &stack); err != nil {
		return Stack{}, err
	}
	if stack.Owner != owner || stack.Name != name || !validRepoURL(stack.RepoURL) || (stack.NextVersionsOffset != nil && *stack.NextVersionsOffset < 0) {
		return Stack{}, ErrBadGateway
	}
	return stack, nil
}

func (c *Client) GetVersion(ctx context.Context, owner, name string, version int) (Version, error) {
	if !validSegment(owner) || !validSegment(name) || version <= 0 {
		return Version{}, ErrBadGateway
	}
	var result Version
	path := "/v1/stacks/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/versions/" + strconv.Itoa(version)
	if err := c.get(ctx, path, nil, &result); err != nil {
		return Version{}, err
	}
	if !validRepoURL(result.RepoURL) {
		return Version{}, ErrBadGateway
	}
	return result, nil
}

func (c *Client) get(ctx context.Context, endpoint string, query url.Values, destination any) error {
	requestURL := *c.base
	requestURL.Path = strings.TrimSuffix(c.base.Path, "/") + endpoint
	requestURL.RawPath = ""
	requestURL.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
	if err != nil {
		return ErrBadGateway
	}
	request.Header.Set("Accept", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrUnavailable
	}
	defer response.Body.Close()

	switch {
	case response.StatusCode == http.StatusNotFound:
		return ErrNotFound
	case response.StatusCode >= 500:
		return ErrUnavailable
	case response.StatusCode != http.StatusOK:
		return ErrBadGateway
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return ErrBadGateway
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, c.maxResponseBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrUnavailable
	}
	if int64(len(body)) > c.maxResponseBytes {
		return ErrBadGateway
	}
	if err := json.Unmarshal(body, destination); err != nil {
		return ErrBadGateway
	}
	return nil
}

func validPage(page Page) bool {
	return page.Limit >= 1 && page.Limit <= maxPageLimit && page.Offset >= 0
}

func validSegment(segment string) bool {
	if segment == "" || segment == ".." || strings.HasPrefix(segment, ".") || strings.ContainsAny(segment, `/\`) {
		return false
	}
	for _, r := range segment {
		switch {
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '-' || r == '.':
		default:
			return false
		}
	}
	return true
}

func validRepoURL(raw string) bool {
	if raw == "" || strings.ContainsAny(raw, " \t\r\n\"'`\\;&|<>()$!*?[]{}#") {
		return false
	}
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" || parsed.Hostname() == "" {
		return false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return false
	}
	return parsed.Opaque == "" && parsed.User == nil && parsed.RawQuery == "" && !parsed.ForceQuery && parsed.Fragment == ""
}
