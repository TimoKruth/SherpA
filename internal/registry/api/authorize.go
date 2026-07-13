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
var errWebSessionPublish = errors.New("web sessions cannot publish")

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
	identity, err := s.store.SessionIdentity(r.Context(), registryauth.HashToken(token))
	if errors.Is(err, store.ErrNotFound) {
		return "", "", http.StatusUnauthorized, errUnauthorized
	}
	if err != nil {
		return "", "", http.StatusInternalServerError, err
	}
	if identity.Purpose != store.SessionCLI {
		return identity.Login, "", http.StatusForbidden, errWebSessionPublish
	}
	if owner != identity.Login {
		return identity.Login, "", http.StatusForbidden, errWrongOwner
	}
	return identity.Login, "linked", 0, nil
}
