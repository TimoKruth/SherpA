package api

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	registryauth "sherpa/internal/registry/auth"
	"sherpa/internal/registry/store"
)

const (
	oauthCookieName = "__Host-sherpa_oauth"
	oauthCookieTTL  = 10 * time.Minute
	webGrantTTL     = 2 * time.Minute
	webSessionTTL   = 30 * 24 * time.Hour
)

type oauthCookiePayload struct {
	State            string `json:"s"`
	Verifier         string `json:"v"`
	HandoffChallenge string `json:"h"`
	Expires          int64  `json:"e"`
}

func newOAuthCookieKey() [32]byte {
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		panic("crypto/rand unavailable")
	}
	return key
}

func (s *server) webOAuthEnabled() bool { return s.publicBaseURL != "" && s.webPublicBaseURL != "" }

func (s *server) handleWebStart(w http.ResponseWriter, r *http.Request) {
	if !s.webOAuthEnabled() {
		http.NotFound(w, r)
		return
	}
	if !s.limiter.allowWebStart(clientIP(r, s.trustProxy)) {
		writeRateLimited(w, time.Minute)
		return
	}
	if len(r.URL.RawQuery) > 256 {
		writeError(w, http.StatusBadRequest, "invalid handoff challenge")
		return
	}
	query := r.URL.Query()
	values, ok := query["handoff_challenge"]
	if !ok || len(query) != 1 || len(values) != 1 || !validSHA256Challenge(values[0]) {
		writeError(w, http.StatusBadRequest, "invalid handoff challenge")
		return
	}
	state, err := randomBase64URL(32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	verifier, err := randomBase64URL(32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	callback := strings.TrimRight(s.publicBaseURL, "/") + "/v1/auth/web/callback"
	authorizeURL, err := s.github.WebAuthorizeURL(state, sha256Challenge(verifier), callback)
	if err != nil {
		s.logger.Printf("auth web authorize failed error_type=%T", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	payload := oauthCookiePayload{State: state, Verifier: verifier, HandoffChallenge: values[0], Expires: s.now().Add(oauthCookieTTL).Unix()}
	cookieValue, err := s.sealOAuthCookie(payload)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: oauthCookieName, Value: cookieValue, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: int(oauthCookieTTL.Seconds()), Expires: s.now().Add(oauthCookieTTL)})
	s.limiter.registerOAuthState(state, oauthCookieTTL)
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, authorizeURL, http.StatusSeeOther)
}

func (s *server) handleWebCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	s.clearOAuthCookie(w)
	if !s.webOAuthEnabled() {
		http.NotFound(w, r)
		return
	}
	if !s.limiter.allowWebCallback(clientIP(r, s.trustProxy)) {
		writeRateLimited(w, time.Minute)
		return
	}
	if len(r.URL.RawQuery) > 2048 {
		s.redirectWebAuthError(w, r, "oauth_invalid")
		return
	}
	payload, ok := s.oauthPayloadFromRequest(r)
	if !ok {
		s.redirectWebAuthError(w, r, "oauth_invalid")
		return
	}
	query := r.URL.Query()
	states := query["state"]
	if len(states) != 1 || len(states[0]) > 128 || hmac.Equal([]byte(states[0]), []byte(payload.State)) == false {
		s.redirectWebAuthError(w, r, "oauth_invalid")
		return
	}
	if !s.limiter.consumeOAuthState(payload.State) {
		s.redirectWebAuthError(w, r, "oauth_invalid")
		return
	}
	if denied := query["error"]; len(denied) == 1 && denied[0] != "" && len(denied[0]) <= 128 {
		s.redirectWebAuthError(w, r, "oauth_denied")
		return
	}
	codes := query["code"]
	if len(codes) != 1 || codes[0] == "" || len(codes[0]) > 512 {
		s.redirectWebAuthError(w, r, "oauth_invalid")
		return
	}
	callback := strings.TrimRight(s.publicBaseURL, "/") + "/v1/auth/web/callback"
	githubToken, err := s.github.ExchangeWebCode(r.Context(), codes[0], payload.Verifier, callback)
	if err != nil {
		s.logger.Printf("auth web exchange failed error_type=%T", err)
		s.redirectWebAuthError(w, r, "oauth_failed")
		return
	}
	user, err := s.github.GetUser(r.Context(), githubToken)
	if err != nil {
		s.logger.Printf("auth web user failed error_type=%T", err)
		s.redirectWebAuthError(w, r, "oauth_failed")
		return
	}
	userID, err := s.store.UpsertUserGitHub(r.Context(), user.Login, user.ID)
	if err != nil {
		s.logger.Printf("auth web identity failed error_type=%T", err)
		s.redirectWebAuthError(w, r, "identity_failed")
		return
	}
	grant, err := s.createWebGrant(r, userID, payload.HandoffChallenge)
	if err != nil {
		s.logger.Printf("auth web grant failed error_type=%T", err)
		s.redirectWebAuthError(w, r, "oauth_failed")
		return
	}
	destination := strings.TrimRight(s.webPublicBaseURL, "/") + "/auth/callback?" + url.Values{"grant": {grant}}.Encode()
	http.Redirect(w, r, destination, http.StatusSeeOther)
}

func (s *server) handleWebExchange(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.webOAuthEnabled() {
		http.NotFound(w, r)
		return
	}
	var request struct {
		Grant        string `json:"grant"`
		HandoffNonce string `json:"handoff_nonce"`
	}
	if err := decodeSocialJSON(w, r, &request); err != nil {
		return
	}
	if !validHexToken(request.Grant) || !validBase64URLBytes(request.HandoffNonce, 32) {
		writeError(w, http.StatusGone, "grant unavailable")
		return
	}
	if !s.limiter.allowWebExchange(clientIP(r, s.trustProxy), request.Grant) {
		writeRateLimited(w, time.Minute)
		return
	}
	challenge := sha256Challenge(request.HandoffNonce)
	var rawSession, sessionHash string
	var identity store.SessionIdentity
	var err error
	for range 3 {
		rawSession, sessionHash, err = registryauth.NewSessionToken()
		if err != nil {
			break
		}
		identity, err = s.store.ExchangeWebGrant(r.Context(), registryauth.HashToken(request.Grant), challenge, sessionHash, webSessionTTL)
		if !errors.Is(err, store.ErrSessionTokenConflict) {
			break
		}
	}
	if errors.Is(err, store.ErrWebGrantUnavailable) {
		writeError(w, http.StatusGone, "grant unavailable")
		return
	}
	if err != nil {
		s.logger.Printf("auth web grant exchange failed error_type=%T", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"access_token": rawSession, "login": identity.Login, "purpose": string(store.SessionWeb)})
}

func (s *server) createWebGrant(r *http.Request, userID int64, challenge string) (string, error) {
	for range 3 {
		raw, hash, err := registryauth.NewSessionToken()
		if err != nil {
			return "", err
		}
		if err := s.store.CreateWebGrant(r.Context(), userID, hash, challenge, webGrantTTL); err == nil {
			return raw, nil
		} else if !errors.Is(err, store.ErrWebGrantConflict) {
			return "", err
		}
	}
	return "", store.ErrWebGrantConflict
}

func (s *server) redirectWebAuthError(w http.ResponseWriter, r *http.Request, code string) {
	destination := strings.TrimRight(s.webPublicBaseURL, "/") + "/auth/callback?" + url.Values{"error": {code}}.Encode()
	http.Redirect(w, r, destination, http.StatusSeeOther)
}

func (s *server) clearOAuthCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: oauthCookieName, Value: "", Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: -1, Expires: time.Unix(1, 0)})
}

func (s *server) oauthPayloadFromRequest(r *http.Request) (oauthCookiePayload, bool) {
	if len(strings.Join(r.Header.Values("Cookie"), ";")) > 4096 {
		return oauthCookiePayload{}, false
	}
	var value string
	count := 0
	for _, cookie := range r.Cookies() {
		if cookie.Name == oauthCookieName {
			value = cookie.Value
			count++
		}
	}
	if count != 1 || len(value) > 2048 {
		return oauthCookiePayload{}, false
	}
	payload, err := s.openOAuthCookie(value)
	if err != nil || payload.Expires <= s.now().Unix() || !validSHA256Challenge(payload.HandoffChallenge) || !validBase64URLBytes(payload.State, 32) || !validBase64URLBytes(payload.Verifier, 32) {
		return oauthCookiePayload{}, false
	}
	return payload, true
}

func (s *server) sealOAuthCookie(payload oauthCookiePayload) (string, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	data := base64.RawURLEncoding.EncodeToString(encoded)
	mac := hmac.New(sha256.New, s.oauthCookieKey[:])
	_, _ = mac.Write([]byte(data))
	return data + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (s *server) openOAuthCookie(value string) (oauthCookiePayload, error) {
	data, signature, ok := strings.Cut(value, ".")
	if !ok {
		return oauthCookiePayload{}, errors.New("invalid cookie")
	}
	provided, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil {
		return oauthCookiePayload{}, errors.New("invalid cookie")
	}
	mac := hmac.New(sha256.New, s.oauthCookieKey[:])
	_, _ = mac.Write([]byte(data))
	if !hmac.Equal(provided, mac.Sum(nil)) {
		return oauthCookiePayload{}, errors.New("invalid cookie")
	}
	encoded, err := base64.RawURLEncoding.DecodeString(data)
	if err != nil {
		return oauthCookiePayload{}, errors.New("invalid cookie")
	}
	var payload oauthCookiePayload
	if err := json.Unmarshal(encoded, &payload); err != nil {
		return oauthCookiePayload{}, errors.New("invalid cookie")
	}
	return payload, nil
}

func randomBase64URL(size int) (string, error) {
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
func sha256Challenge(value string) string {
	sum := sha256.Sum256([]byte(value))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
func validSHA256Challenge(value string) bool { return validBase64URLBytes(value, sha256.Size) }
func validBase64URLBytes(value string, size int) bool {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(raw) == size && base64.RawURLEncoding.EncodeToString(raw) == value
}
func validHexToken(value string) bool {
	raw, err := hex.DecodeString(value)
	return err == nil && len(raw) == 32 && hex.EncodeToString(raw) == value
}
