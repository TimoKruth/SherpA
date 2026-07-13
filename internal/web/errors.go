package web

import (
	"context"
	"errors"
	"net/http"

	"sherpa/internal/web/registryclient"
)

type publicError struct {
	Title   string
	Message string
}

var publicErrors = map[int]publicError{
	http.StatusBadRequest:          {Title: "Invalid request", Message: "The requested input is invalid."},
	http.StatusUnauthorized:        {Title: "Sign-in failed", Message: "The sign-in request could not be completed."},
	http.StatusForbidden:           {Title: "Request rejected", Message: "The request could not be verified."},
	http.StatusNotFound:            {Title: "Page not found", Message: "The requested page could not be found."},
	http.StatusMethodNotAllowed:    {Title: "Method not allowed", Message: "This page only accepts GET requests."},
	http.StatusBadGateway:          {Title: "Registry response error", Message: "The registry returned an invalid response."},
	http.StatusServiceUnavailable:  {Title: "Registry unavailable", Message: "The registry is temporarily unavailable."},
	http.StatusInternalServerError: {Title: "Page unavailable", Message: "The page could not be rendered."},
}

func (s *server) renderRegistryError(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	switch {
	case errors.Is(err, registryclient.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, registryclient.ErrBadGateway):
		status = http.StatusBadGateway
	case errors.Is(err, registryclient.ErrUnavailable), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		status = http.StatusServiceUnavailable
	}
	s.renderError(w, status)
}

func (s *server) renderError(w http.ResponseWriter, status int) {
	copy, ok := publicErrors[status]
	if !ok {
		status = http.StatusInternalServerError
		copy = publicErrors[status]
	}
	if status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "60")
	}
	data := pageData{
		Title:        copy.Title,
		Description:  copy.Message,
		Robots:       "noindex",
		ErrorTitle:   copy.Title,
		ErrorMessage: copy.Message,
	}
	if err := s.renderer.render(w, status, data); err != nil {
		s.logger.Printf("render error page failed status=%d error_type=%T", status, err)
		writeFallbackError(w)
	}
}

func (s *server) renderSuccess(w http.ResponseWriter, title string) {
	if err := s.renderer.render(w, http.StatusOK, pageData{Title: title}); err != nil {
		s.logger.Printf("render page failed error_type=%T", err)
		writeFallbackError(w)
	}
}
