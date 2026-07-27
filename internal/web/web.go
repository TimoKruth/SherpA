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
	GetUser(context.Context, string, registryclient.Page) (registryclient.UserProfile, error)
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
	PublicBaseURL     *url.URL
	RegistryPublicURL *url.URL
	Logger            *log.Logger
	// NoIndex serves noindex on every page and disallows all crawling in
	// robots.txt. Used for unlisted deployments such as a closed beta.
	NoIndex bool
}

type server struct {
	registry          Registry
	renderer          *renderer
	publicBaseURL     *url.URL
	logger            *log.Logger
	authRegistry      AuthRegistry
	registryPublicURL *url.URL
	noIndex           bool
}

func New(reg Registry, options Options) (http.Handler, error) {
	if reg == nil {
		return nil, errors.New("web registry client is required")
	}
	renderer, err := newRenderer(options.NoIndex)
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
	var registryPublicURL *url.URL
	if options.RegistryPublicURL != nil {
		copy := *options.RegistryPublicURL
		registryPublicURL = &copy
	}
	var authRegistry AuthRegistry
	if registryPublicURL != nil {
		if publicBaseURL == nil {
			return nil, errors.New("web auth public URLs must be configured together")
		}
		var ok bool
		authRegistry, ok = reg.(AuthRegistry)
		if !ok {
			return nil, errors.New("authenticated registry client is required")
		}
	}
	s := &server{
		registry:          reg,
		renderer:          renderer,
		publicBaseURL:     publicBaseURL,
		logger:            logger,
		authRegistry:      authRegistry,
		registryPublicURL: registryPublicURL,
		noIndex:           options.NoIndex,
	}
	return s.securityHeaders(s.logRequests(http.HandlerFunc(s.route))), nil
}
