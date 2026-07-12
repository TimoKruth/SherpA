package api

import (
	"encoding/json"
	"log"
	"net/http"
	"sync/atomic"
	"time"

	registryauth "sherpa/internal/registry/auth"
	"sherpa/internal/registry/content"
	"sherpa/internal/registry/store"
)

type server struct {
	store         store.Store
	content       content.ContentStore
	adminToken    string
	github        registryauth.GitHubClient
	limiter       *authLimiter
	trustProxy    bool
	publicBaseURL string
	now           func() time.Time
	logger        *log.Logger
}

type Options struct {
	TrustProxy    bool
	PublicBaseURL string
	Now           func() time.Time
	Logger        *log.Logger
}

func HealthHandler(ready *atomic.Bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

func New(st store.Store, cs content.ContentStore, adminToken string, github registryauth.GitHubClient) http.Handler {
	return NewWithOptions(st, cs, adminToken, github, Options{})
}

func NewWithOptions(st store.Store, cs content.ContentStore, adminToken string, github registryauth.GitHubClient, options Options) http.Handler {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	logger := options.Logger
	if logger == nil {
		logger = log.Default()
	}
	s := &server{
		store:         st,
		content:       cs,
		adminToken:    adminToken,
		github:        github,
		limiter:       newAuthLimiter(now),
		trustProxy:    options.TrustProxy,
		publicBaseURL: options.PublicBaseURL,
		now:           now,
		logger:        logger,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/auth/device/start", s.handleDeviceStart)
	mux.HandleFunc("POST /v1/auth/device/poll", s.handleDevicePoll)
	mux.HandleFunc("GET /v1/search", s.handleSearch)
	mux.HandleFunc("GET /v1/stacks/{owner}/{repo}/", s.handleGit)
	mux.HandleFunc("GET /v1/stacks/{owner}/{name}", s.handleStack)
	mux.HandleFunc("GET /v1/stacks/{owner}/{name}/versions/{version}", s.handleVersion)
	mux.HandleFunc("POST /v1/stacks/{owner}/{name}/versions", s.handlePublish)
	return s.logRequests(mux)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func internalServerError(w http.ResponseWriter, operation string, err error) {
	log.Printf("%s: %v", operation, err)
	writeError(w, http.StatusInternalServerError, "internal server error")
}
