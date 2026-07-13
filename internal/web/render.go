package web

import (
	"bytes"
	"embed"
	"errors"
	"html/template"
	"net/http"
)

//go:embed templates/*.html
var templateFiles embed.FS

type pageData struct {
	Title        string
	ErrorTitle   string
	ErrorMessage string
	NoIndex      bool
	SearchPage   *searchPageView
	StackPage    *stackPageView
	VersionPage  *versionPageView
}

type renderer struct {
	templates *template.Template
}

func newRenderer() (*renderer, error) {
	templates, err := template.New("pages").ParseFS(templateFiles, "templates/*.html")
	if err != nil {
		return nil, errors.New("parse web templates")
	}
	return &renderer{templates: templates}, nil
}

func (r *renderer) render(w http.ResponseWriter, status int, data pageData) error {
	var body bytes.Buffer
	if err := r.templates.ExecuteTemplate(&body, "base", data); err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if status < 200 || status >= 300 {
		w.Header().Set("X-Robots-Tag", "noindex")
	}
	w.WriteHeader(status)
	_, err := w.Write(body.Bytes())
	return err
}

func writeFallbackError(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Robots-Tag", "noindex")
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = w.Write([]byte("<!doctype html><html lang=en><head><meta charset=utf-8><meta name=robots content=noindex><title>Page unavailable</title></head><body><main><h1>Page unavailable</h1><p>The page could not be rendered.</p></main></body></html>"))
}
