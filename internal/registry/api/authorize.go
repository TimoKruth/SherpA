package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"

	registryauth "sherpa/internal/registry/auth"
	"sherpa/internal/registry/store"
)

var errUnauthorized = errors.New("unauthorized")
var errWrongOwner = errors.New("you can only publish under your GitHub login")

func (s *server) authorizePublish(r *http.Request, owner string) (string, string, int, error) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || strings.TrimSpace(token) == "" {
		return "", "", http.StatusUnauthorized, errUnauthorized
	}
	if s.adminToken != "" {
		got, want := sha256.Sum256([]byte(token)), sha256.Sum256([]byte(s.adminToken))
		if subtle.ConstantTimeCompare(got[:], want[:]) == 1 {
			return owner, "unreviewed", 0, nil
		}
	}
	login, err := s.store.SessionUser(r.Context(), registryauth.HashToken(token))
	if errors.Is(err, store.ErrNotFound) {
		return "", "", http.StatusUnauthorized, errUnauthorized
	}
	if err != nil {
		return "", "", http.StatusInternalServerError, err
	}
	if owner != login {
		return login, "", http.StatusForbidden, errWrongOwner
	}
	return login, "linked", 0, nil
}
