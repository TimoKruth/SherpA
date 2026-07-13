package web

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strings"
	"time"
)

const (
	loginCookieName      = "__Host-sherpa_login"
	sessionCookieName    = "__Host-sherpa_session"
	csrfCookieName       = "__Host-sherpa_csrf"
	loginCookieTTL       = 10 * time.Minute
	webSessionCookieTTL  = 30 * 24 * time.Hour
	maxCookieHeaderBytes = 8 << 10
)

func randomWebToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
func validWebToken(value string) bool {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(raw) == 32 && base64.RawURLEncoding.EncodeToString(raw) == value
}
func validGrant(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}
func webChallenge(value string) string {
	sum := sha256.Sum256([]byte(value))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func setPrivateCookie(w http.ResponseWriter, name, value string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: int(ttl.Seconds()), Expires: time.Now().Add(ttl)})
}
func clearPrivateCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: -1, Expires: time.Unix(1, 0)})
}
func clearAuthCookies(w http.ResponseWriter) {
	clearPrivateCookie(w, loginCookieName)
	clearPrivateCookie(w, sessionCookieName)
	clearPrivateCookie(w, csrfCookieName)
}

func strictCookie(r *http.Request, name string, max int) (string, bool) {
	if len(strings.Join(r.Header.Values("Cookie"), ";")) > maxCookieHeaderBytes {
		return "", false
	}
	value := ""
	count := 0
	for _, cookie := range r.Cookies() {
		if cookie.Name == name {
			value = cookie.Value
			count++
		}
	}
	return value, count == 1 && value != "" && len(value) <= max
}
