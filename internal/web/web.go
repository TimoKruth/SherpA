package web

import (
	"context"
	"embed"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"

	"sherpa/internal/web/registryclient"
)

//go:embed static/*
var staticFiles embed.FS

type Registry interface {
	Search(context.Context, registryclient.SearchQuery) (registryclient.SearchResult, error)
	GetStack(context.Context, string, string, registryclient.Page) (registryclient.Stack, error)
	GetVersion(context.Context, string, string, int) (registryclient.Version, error)
}

func (s *server) canonicalURL(routePath string) string {
	if s.publicBaseURL == nil {
		return ""
	}
	canonical := *s.publicBaseURL
	canonical.Path = strings.TrimRight(canonical.Path, "/") + routePath
	canonical.RawPath = ""
	canonical.RawQuery = ""
	canonical.Fragment = ""
	return canonical.String()
}

type Options struct {
	PublicBaseURL *url.URL
	Logger        *log.Logger
}

type server struct {
	registry      Registry
	renderer      *renderer
	publicBaseURL *url.URL
	logger        *log.Logger
}

func New(reg Registry, options Options) (http.Handler, error) {
	if reg == nil {
		return nil, errors.New("web registry client is required")
	}
	renderer, err := newRenderer()
	if err != nil {
		return nil, err
	}
	logger := options.Logger
	if logger == nil {
		logger = log.Default()
	}
	var publicBaseURL *url.URL
	if options.PublicBaseURL != nil {
		copy := *options.PublicBaseURL
		publicBaseURL = &copy
	}
	s := &server{
		registry:      reg,
		renderer:      renderer,
		publicBaseURL: publicBaseURL,
		logger:        logger,
	}
	return s.securityHeaders(s.logRequests(http.HandlerFunc(s.route))), nil
}
