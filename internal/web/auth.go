package web

import (
	"context"
	"crypto/subtle"
	"errors"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"sherpa/internal/web/registryclient"
)

type AuthRegistry interface {
	ExchangeWebGrant(context.Context, string, string) (registryclient.WebSession, error)
	Me(context.Context, string) (registryclient.Me, error)
	Revoke(context.Context, string) error
	Follow(context.Context, string, string, string) (registryclient.Follow, error)
	Unfollow(context.Context, string, string, string) error
	Follows(context.Context, string, int, string) (registryclient.FollowPage, error)
	Updates(context.Context, string, int, string) (registryclient.UpdatePage, error)
	MarkSeen(context.Context, string, string, string, int) (registryclient.Follow, error)
	PutTrial(context.Context, string, string, string, int, string) error
}

func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.authRegistry == nil || s.registryPublicURL == nil {
		s.renderError(w, http.StatusNotFound)
		return
	}
	nonce, err := randomWebToken()
	if err != nil {
		s.renderError(w, http.StatusInternalServerError)
		return
	}
	setPrivateCookie(w, loginCookieName, nonce, loginCookieTTL)
	destination := *s.registryPublicURL
	destination.Path = strings.TrimRight(destination.Path, "/") + "/v1/auth/web/start"
	destination.RawQuery = url.Values{"handoff_challenge": {webChallenge(nonce)}}.Encode()
	http.Redirect(w, r, destination.String(), http.StatusSeeOther)
}

func (s *server) handleAuthCallback(w http.ResponseWriter, r *http.Request) {
	clearPrivateCookie(w, loginCookieName)
	if s.authRegistry == nil {
		s.renderError(w, http.StatusNotFound)
		return
	}
	if len(r.URL.RawQuery) > 1024 {
		s.authCallbackError(w, r)
		return
	}
	values := r.URL.Query()
	if denied := values["error"]; len(denied) == 1 && len(values) == 1 && denied[0] != "" && len(denied[0]) <= 64 {
		s.authCallbackError(w, r)
		return
	}
	grants := values["grant"]
	if len(values) != 1 || len(grants) != 1 || !validGrant(grants[0]) {
		s.authCallbackError(w, r)
		return
	}
	nonce, ok := strictCookie(r, loginCookieName, 128)
	if !ok || !validWebToken(nonce) {
		s.authCallbackError(w, r)
		return
	}
	session, err := s.authRegistry.ExchangeWebGrant(r.Context(), grants[0], nonce)
	if err != nil {
		s.logger.Printf("web auth callback failed error_type=%T", err)
		s.authCallbackError(w, r)
		return
	}
	if session.AccessToken == "" || session.Login == "" || session.Purpose != "web" {
		s.authCallbackError(w, r)
		return
	}
	csrf, err := randomWebToken()
	if err != nil {
		s.renderError(w, http.StatusInternalServerError)
		return
	}
	setPrivateCookie(w, sessionCookieName, session.AccessToken, webSessionCookieTTL)
	setPrivateCookie(w, csrfCookieName, csrf, webSessionCookieTTL)
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

func (s *server) authCallbackError(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/auth/error", http.StatusSeeOther)
}

func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if !s.validMutation(w, r) {
		s.renderError(w, http.StatusForbidden)
		return
	}
	defer clearAuthCookies(w)
	if token, ok := strictCookie(r, sessionCookieName, 256); ok && s.authRegistry != nil {
		_ = s.authRegistry.Revoke(r.Context(), token)
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *server) validMutation(w http.ResponseWriter, r *http.Request) bool {
	if s.publicBaseURL == nil || r.URL.RawQuery != "" || r.ContentLength > 8<<10 {
		return false
	}
	origins := r.Header.Values("Origin")
	if len(origins) != 1 {
		return false
	}
	want := s.publicBaseURL.Scheme + "://" + s.publicBaseURL.Host
	if origins[0] != want {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/x-www-form-urlencoded" {
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		return false
	}
	formTokens := r.PostForm["csrf_token"]
	if len(formTokens) != 1 || len(formTokens[0]) > 128 {
		return false
	}
	cookie, ok := strictCookie(r, csrfCookieName, 128)
	return ok && subtle.ConstantTimeCompare([]byte(cookie), []byte(formTokens[0])) == 1
}

func (s *server) currentSession(w http.ResponseWriter, r *http.Request) (registryclient.Me, string, bool) {
	if s.authRegistry == nil {
		return registryclient.Me{}, "", false
	}
	token, ok := strictCookie(r, sessionCookieName, 256)
	if !ok {
		return registryclient.Me{}, "", false
	}
	identity, err := s.authRegistry.Me(r.Context(), token)
	if errors.Is(err, registryclient.ErrUnauthorized) {
		clearAuthCookies(w)
		return registryclient.Me{}, "", false
	}
	if err != nil {
		return registryclient.Me{}, "", false
	}
	if identity.Purpose != "web" {
		clearAuthCookies(w)
		return registryclient.Me{}, "", false
	}
	return identity, token, true
}

func csrfToken(r *http.Request) string {
	value, _ := strictCookie(r, csrfCookieName, 128)
	return value
}
