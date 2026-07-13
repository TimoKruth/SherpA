package web

import (
	"io/fs"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"sherpa/internal/web/registryclient"
)

const (
	searchPageSize  = 24
	versionPageSize = 25
	maxQueryBytes   = 200
	maxFilterBytes  = 64
)

func (s *server) route(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/login":
		if r.Method != http.MethodGet {
			s.renderError(w, http.StatusMethodNotAllowed)
			return
		}
		s.handleLogin(w, r)
		return
	case "/auth/callback":
		if r.Method != http.MethodGet {
			s.renderError(w, http.StatusMethodNotAllowed)
			return
		}
		s.handleAuthCallback(w, r)
		return
	case "/auth/error":
		if r.Method != http.MethodGet {
			s.renderError(w, http.StatusMethodNotAllowed)
			return
		}
		if err := s.renderer.render(w, http.StatusUnauthorized, pageData{Title: "Sign-in failed", Robots: "noindex", Auth: authView{Enabled: s.authRegistry != nil}, AuthError: true}); err != nil {
			writeFallbackError(w)
		}
		return
	case "/logout":
		if r.Method != http.MethodPost {
			s.renderError(w, http.StatusMethodNotAllowed)
			return
		}
		s.handleLogout(w, r)
		return
	case "/dashboard":
		if r.Method != http.MethodGet {
			s.renderError(w, http.StatusMethodNotAllowed)
			return
		}
		s.handleDashboard(w, r)
		return
	}
	if r.Method == http.MethodPost {
		segments := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
		switch {
		case len(segments) == 4 && segments[0] == "stacks" && (segments[3] == "follow" || segments[3] == "unfollow"):
			s.handleFollowMutation(w, r, segments[1], segments[2], segments[3] == "unfollow")
		case len(segments) == 4 && segments[0] == "dashboard" && segments[1] == "seen":
			s.handleSeenMutation(w, r, segments[2], segments[3])
		default:
			w.Header().Set("Allow", http.MethodGet)
			s.renderError(w, http.StatusMethodNotAllowed)
		}
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		s.renderError(w, http.StatusMethodNotAllowed)
		return
	}

	switch r.URL.Path {
	case "/":
		s.handleHome(w, r)
		return
	case "/search":
		s.handleSearch(w, r)
		return
	case "/healthz":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
		return
	case "/robots.txt":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte("User-agent: *\nAllow: /\n"))
		return
	case "/static/app.css":
		s.serveStatic(w, "static/app.css", "text/css; charset=utf-8")
		return
	case "/static/app.js":
		s.serveStatic(w, "static/app.js", "text/javascript; charset=utf-8")
		return
	case "/static/sherpa-mark.png":
		s.serveStatic(w, "static/sherpa-mark.png", "image/png")
		return
	case "/static/favicon.png":
		s.serveStatic(w, "static/favicon.png", "image/png")
		return
	}

	segments := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	switch {
	case len(segments) == 2 && segments[0] == "users":
		s.handleProfile(w, r, segments[1])
	case len(segments) == 3 && segments[0] == "stacks":
		s.handleStack(w, r, segments[1], segments[2])
	case len(segments) == 5 && segments[0] == "stacks" && segments[3] == "v":
		s.handleVersion(w, r, segments[1], segments[2], segments[4])
	default:
		s.renderError(w, http.StatusNotFound)
	}
}

func (s *server) serveStatic(w http.ResponseWriter, name, contentType string) {
	content, err := fs.ReadFile(staticFiles, name)
	if err != nil {
		s.logger.Printf("embedded asset unavailable asset=%q", name)
		s.renderError(w, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

func parseSearchQuery(values url.Values) (registryclient.SearchQuery, bool) {
	q, ok := boundedQueryValue(values, "q", maxQueryBytes)
	if !ok {
		return registryclient.SearchQuery{}, false
	}
	harness, ok := boundedQueryValue(values, "harness", maxFilterBytes)
	if !ok {
		return registryclient.SearchQuery{}, false
	}
	tag, ok := boundedQueryValue(values, "tag", maxFilterBytes)
	if !ok {
		return registryclient.SearchQuery{}, false
	}
	pageNumber, ok := parsePage(values, "page")
	if !ok {
		return registryclient.SearchQuery{}, false
	}
	page, ok := pageFromNumber(pageNumber, searchPageSize)
	if !ok {
		return registryclient.SearchQuery{}, false
	}
	return registryclient.SearchQuery{Q: q, Harness: harness, Tag: tag, Page: page}, true
}

func boundedQueryValue(values url.Values, key string, maxBytes int) (string, bool) {
	entries, exists := values[key]
	if !exists {
		return "", true
	}
	if len(entries) != 1 {
		return "", false
	}
	if len(entries[0]) > maxBytes {
		return "", false
	}
	value := strings.TrimSpace(entries[0])
	return value, true
}

func parsePage(values url.Values, key string) (int, bool) {
	entries, exists := values[key]
	if !exists {
		return 1, true
	}
	if len(entries) != 1 || entries[0] == "" || strings.TrimSpace(entries[0]) != entries[0] {
		return 0, false
	}
	return parsePositiveInteger(entries[0])
}

func parsePositiveInteger(value string) (int, bool) {
	if value == "" {
		return 0, false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return 0, false
		}
	}
	parsed, err := strconv.Atoi(value)
	return parsed, err == nil && parsed > 0
}

func pageFromNumber(pageNumber, limit int) (registryclient.Page, bool) {
	if pageNumber <= 0 || limit <= 0 || pageNumber-1 > math.MaxInt/limit {
		return registryclient.Page{}, false
	}
	return registryclient.Page{Limit: limit, Offset: (pageNumber - 1) * limit}, true
}

func validSegment(value string) bool {
	if value == "" || value == ".." || strings.HasPrefix(value, ".") || strings.ContainsAny(value, `/\`) {
		return false
	}
	for _, char := range value {
		switch {
		case char >= 'A' && char <= 'Z':
		case char >= 'a' && char <= 'z':
		case char >= '0' && char <= '9':
		case char == '_' || char == '-' || char == '.':
		default:
			return false
		}
	}
	return true
}
