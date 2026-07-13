package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	registryauth "sherpa/internal/registry/auth"
	"sherpa/internal/registry/store"
)

func (s *server) handleDeviceStart(w http.ResponseWriter, r *http.Request) {
	if retry, allowed := s.limiter.allowStart(clientIP(r, s.trustProxy)); !allowed {
		writeRateLimited(w, retry)
		return
	}
	code, err := s.github.StartDeviceFlow(r.Context())
	if err != nil {
		s.logger.Printf("auth device start failed error_type=%T", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	s.limiter.registerDevice(code.DeviceCode, time.Duration(code.Interval)*time.Second, time.Duration(code.ExpiresIn)*time.Second)
	writeJSON(w, http.StatusOK, code)
}

func (s *server) handleDevicePoll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceCode string `json:"device_code"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, deviceBodyLimit)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeError(w, http.StatusBadRequest, "device_code is required")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeError(w, http.StatusBadRequest, "device_code is required")
		return
	}
	if strings.TrimSpace(req.DeviceCode) == "" {
		writeError(w, http.StatusBadRequest, "device_code is required")
		return
	}
	retry, allowed, known := s.limiter.allowPoll(req.DeviceCode)
	if !known {
		// Never registered via /start on this instance (or already expired):
		// reject as a dead code WITHOUT contacting GitHub. This closes the
		// code-rotation abuse that would otherwise turn every unauthenticated
		// poll into an outbound GitHub token-poll.
		writeError(w, http.StatusGone, "device code expired")
		return
	}
	if !allowed {
		writeRateLimited(w, retry)
		return
	}

	githubToken, err := s.github.PollToken(r.Context(), req.DeviceCode)
	switch {
	case errors.Is(err, registryauth.ErrAuthPending):
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "pending"})
		return
	case errors.Is(err, registryauth.ErrSlowDown):
		s.limiter.slowDown(req.DeviceCode)
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "slow_down"})
		return
	case errors.Is(err, registryauth.ErrExpired):
		s.limiter.forgetDevice(req.DeviceCode)
		writeError(w, http.StatusGone, "device code expired")
		return
	case err != nil:
		s.logger.Printf("auth device poll failed error_type=%T", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	s.limiter.forgetDevice(req.DeviceCode)

	user, err := s.github.GetUser(r.Context(), githubToken)
	if err != nil {
		s.logger.Printf("auth GitHub user failed error_type=%T", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	userID, err := s.store.UpsertUserGitHub(r.Context(), user.Login, user.ID)
	if err != nil {
		s.logger.Printf("auth upsert user failed error_type=%T", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	token, hash, err := registryauth.NewSessionToken()
	if err != nil {
		s.logger.Printf("auth mint session failed error_type=%T", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	if err := s.store.CreateSession(r.Context(), userID, hash, store.SessionCLI, registryauth.SessionTTL); err != nil {
		s.logger.Printf("auth create session failed error_type=%T", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"access_token": token, "login": user.Login})
}
