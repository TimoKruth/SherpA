package api

import (
	"encoding/json"
	"net/http"

	"sherpa/internal/registry/content"
	"sherpa/internal/registry/store"
)

type server struct {
	store   store.Store
	content content.ContentStore
	token   string
}

func New(st store.Store, cs content.ContentStore, token string) http.Handler {
	s := &server{
		store:   st,
		content: cs,
		token:   token,
	}

	mux := http.NewServeMux()
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
