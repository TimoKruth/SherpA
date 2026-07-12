package api

import (
	"net/http"
	"os"
	"strings"
)

func (s *server) handleGit(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	repo := r.PathValue("repo")
	if !strings.HasSuffix(repo, ".git") {
		http.NotFound(w, r)
		return
	}

	name := strings.TrimSuffix(repo, ".git")
	repoPath := s.content.RepoPath(owner, name)
	info, err := os.Stat(repoPath)
	if err != nil || !info.IsDir() {
		http.NotFound(w, r)
		return
	}

	prefix := "/v1/stacks/" + owner + "/" + repo + "/"
	http.StripPrefix(prefix, http.FileServer(http.Dir(repoPath))).ServeHTTP(w, r)
}
