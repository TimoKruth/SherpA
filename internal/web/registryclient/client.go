package registryclient

import (
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
	ErrNotFound    = errors.New("registry resource not found")
	ErrBadGateway  = errors.New("invalid registry response")
	ErrUnavailable = errors.New("registry unavailable")
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
	if !validRepoURL(stack.RepoURL) || (stack.NextVersionsOffset != nil && *stack.NextVersionsOffset < 0) {
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
