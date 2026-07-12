package api

import (
	"crypto/sha256"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	deviceBodyLimit      = 4 << 10
	devicePollDefault    = 5 * time.Second
	deviceSlowDown       = 5 * time.Second
	deviceDefaultExpiry  = 15 * time.Minute
	deviceStartWindow    = time.Minute
	limiterEntryCapacity = 10_000
)

type limitEntry struct {
	next     time.Time
	expires  time.Time
	interval time.Duration
}

type authLimiter struct {
	mu      sync.Mutex
	now     func() time.Time
	starts  map[string]limitEntry
	devices map[[sha256.Size]byte]limitEntry
}

func newAuthLimiter(now func() time.Time) *authLimiter {
	return &authLimiter{
		now:     now,
		starts:  make(map[string]limitEntry),
		devices: make(map[[sha256.Size]byte]limitEntry),
	}
}

func (l *authLimiter) allowStart(ip string) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.cleanup(now)
	if entry, ok := l.starts[ip]; ok && now.Before(entry.next) {
		return entry.next.Sub(now), false
	}
	l.makeStartRoom()
	l.starts[ip] = limitEntry{next: now.Add(deviceStartWindow), expires: now.Add(2 * deviceStartWindow)}
	return 0, true
}

func (l *authLimiter) registerDevice(code string, interval, expiry time.Duration) {
	initialDelay := interval
	if interval == 0 {
		interval = devicePollDefault
	} else if interval < 0 {
		interval = devicePollDefault
		initialDelay = devicePollDefault
	}
	if expiry <= 0 {
		expiry = deviceDefaultExpiry
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cleanup(now)
	l.makeDeviceRoom()
	l.devices[sha256.Sum256([]byte(code))] = limitEntry{
		next:     now.Add(initialDelay),
		expires:  now.Add(expiry),
		interval: interval,
	}
}

func (l *authLimiter) allowPoll(code string) (time.Duration, bool) {
	key := sha256.Sum256([]byte(code))
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.cleanup(now)
	entry, ok := l.devices[key]
	if !ok {
		l.makeDeviceRoom()
		entry = limitEntry{interval: devicePollDefault, expires: now.Add(deviceDefaultExpiry)}
	}
	if now.Before(entry.next) {
		return entry.next.Sub(now), false
	}
	entry.next = now.Add(entry.interval)
	l.devices[key] = entry
	return 0, true
}

func (l *authLimiter) slowDown(code string) {
	key := sha256.Sum256([]byte(code))
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	entry, ok := l.devices[key]
	if !ok {
		entry = limitEntry{interval: devicePollDefault, expires: now.Add(deviceDefaultExpiry)}
	}
	entry.interval += deviceSlowDown
	entry.next = now.Add(entry.interval)
	l.devices[key] = entry
}

func (l *authLimiter) forgetDevice(code string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.devices, sha256.Sum256([]byte(code)))
}

func (l *authLimiter) cleanup(now time.Time) {
	for key, entry := range l.starts {
		if !now.Before(entry.expires) {
			delete(l.starts, key)
		}
	}
	for key, entry := range l.devices {
		if !now.Before(entry.expires) {
			delete(l.devices, key)
		}
	}
}

func (l *authLimiter) makeStartRoom() {
	for len(l.starts) >= limiterEntryCapacity {
		var oldestKey string
		var oldestExpiry time.Time
		for key, entry := range l.starts {
			if oldestExpiry.IsZero() || entry.expires.Before(oldestExpiry) {
				oldestKey, oldestExpiry = key, entry.expires
			}
		}
		delete(l.starts, oldestKey)
	}
}

func (l *authLimiter) makeDeviceRoom() {
	for len(l.devices) >= limiterEntryCapacity {
		var oldestKey [sha256.Size]byte
		var oldestExpiry time.Time
		for key, entry := range l.devices {
			if oldestExpiry.IsZero() || entry.expires.Before(oldestExpiry) {
				oldestKey, oldestExpiry = key, entry.expires
			}
		}
		delete(l.devices, oldestKey)
	}
}

func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		forwarded := strings.TrimSpace(r.Header.Get("X-Real-IP"))
		if net.ParseIP(forwarded) != nil {
			return forwarded
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func writeRateLimited(w http.ResponseWriter, retry time.Duration) {
	seconds := int64((retry + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", fmt.Sprintf("%d", seconds))
	writeError(w, http.StatusTooManyRequests, "too many requests")
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *statusWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func (s *server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := s.now()
		wrapped := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(wrapped, r)
		status := wrapped.status
		if status == 0 {
			status = http.StatusOK
		}
		s.logger.Printf("request method=%s path=%q status=%d duration=%s railway_request_id=%q",
			r.Method, r.URL.Path, status, s.now().Sub(started), safeRequestID(r.Header.Get("X-Railway-Request-Id")))
	})
}

func safeRequestID(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 128 {
		value = value[:128]
	}
	for _, char := range value {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '-' && char != '_' {
			return ""
		}
	}
	return value
}
