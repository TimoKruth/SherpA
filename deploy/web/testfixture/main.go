package main

import (
	"fmt"
	"net/http"
	"os"
)

const repoURL = "https://registry.example/v1/stacks/alice/reviewer.git"

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /v1/search", handleSearch)
	mux.HandleFunc("GET /v1/stacks/alice/reviewer", handleStack)
	mux.HandleFunc("GET /v1/stacks/alice/reviewer/versions/2", handleVersion)
	addr := os.Getenv("SHERPA_WEB_FIXTURE_ADDR")
	if addr == "" {
		addr = ":8081"
	}
	if err := http.ListenAndServe(addr, mux); err != nil {
		panic(err)
	}
}

func handleSearch(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("limit") != "24" || r.URL.Query().Get("offset") != "0" {
		http.Error(w, "expected bounded search", http.StatusBadRequest)
		return
	}
	writeJSON(w, `{"stacks":[{"ref":"@alice/reviewer","name":"reviewer","owner":"alice","summary":"Security-focused code review","tags":["review","security"],"harness":"codex","version":2,"trust_tier":"linked","forked_from":"","repo_url":"`+repoURL+`"}]}`)
}

func handleStack(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("versions_limit") != "25" || r.URL.Query().Get("versions_offset") != "0" {
		http.Error(w, "expected bounded versions", http.StatusBadRequest)
		return
	}
	writeJSON(w, `{"name":"reviewer","owner":"alice","summary":"Security-focused code review","tags":["review","security"],"harness":"codex","forked_from":"","repo_url":"`+repoURL+`","versions":[{"version":2,"published_at":"2026-07-13T12:00:00Z","changelog":"Stricter review checks","scan_summary":"clean","trust_tier":"linked"}]}`)
}

func handleVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, `{"version":2,"git_tag":"v2","manifest":{"name":"reviewer","owner":"alice","version":2,"harness":"codex"},"scan_report":{"findings":[]},"changelog":"Stricter review checks","published_at":"2026-07-13T12:00:00Z","trust_tier":"linked","repo_url":"`+repoURL+`"}`)
}

func writeJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	_, _ = w.Write([]byte(body))
}
