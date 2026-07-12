package api

import (
	"encoding/json"
	"net/http"

	registryauth "sherpa/internal/registry/auth"
	"sherpa/internal/registry/content"
	"sherpa/internal/registry/store"
)

type server struct {
	store   store.Store
	content content.ContentStore
	token   string
	github  registryauth.GitHubClient
}

func New(st store.Store, cs content.ContentStore, token string, github registryauth.GitHubClient) http.Handler {
	s := &server{
		store:   st,
		content: cs,
		token:   token,
		github:  github,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/auth/device/start", s.handleDeviceStart)
	mux.HandleFunc("POST /v1/auth/device/poll", s.handleDevicePoll)
	mux.HandleFunc("GET /v1/search", s.handleSearch)
	mux.HandleFunc("GET /v1/stacks/{owner}/{repo}/", s.handleGit)
	mux.HandleFunc("GET /v1/stacks/{owner}/{name}", s.handleStack)
	mux.HandleFunc("GET /v1/stacks/{owner}/{name}/versions/{version}", s.handleVersion)
	mux.HandleFunc("POST /v1/stacks/{owner}/{name}/versions", s.handlePublish)
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
