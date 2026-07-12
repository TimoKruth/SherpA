package api

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	registryauth "sherpa/internal/registry/auth"
)

func (s *server) handleDeviceStart(w http.ResponseWriter, r *http.Request) {
	code, err := s.github.StartDeviceFlow(r.Context())
	if err != nil {
		log.Printf("auth device start: %v", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSON(w, http.StatusOK, code)
}

func (s *server) handleDevicePoll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceCode string `json:"device_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.DeviceCode) == "" {
		writeError(w, http.StatusBadRequest, "device_code is required")
		return
	}

	githubToken, err := s.github.PollToken(r.Context(), req.DeviceCode)
	switch {
	case errors.Is(err, registryauth.ErrAuthPending):
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "pending"})
		return
	case errors.Is(err, registryauth.ErrSlowDown):
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "slow_down"})
		return
	case errors.Is(err, registryauth.ErrExpired):
		writeError(w, http.StatusGone, "device code expired")
		return
	case err != nil:
		log.Printf("auth device poll: %v", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	user, err := s.github.GetUser(r.Context(), githubToken)
	if err != nil {
		log.Printf("auth GitHub user: %v", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	userID, err := s.store.UpsertUserGitHub(r.Context(), user.Login, user.ID)
	if err != nil {
		log.Printf("auth upsert user: %v", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	token, hash, err := registryauth.NewSessionToken()
	if err != nil {
		log.Printf("auth mint session: %v", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	if err := s.store.CreateSession(r.Context(), userID, hash, registryauth.SessionTTL); err != nil {
		log.Printf("auth create session: %v", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"access_token": token, "login": user.Login})
}
