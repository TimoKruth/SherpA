package web

import (
	"bytes"
	"encoding/json"
	"image/png"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"sherpa/internal/web/registryclient"
)

func TestEmbeddedStaticRoutes(t *testing.T) {
	handler := newTestHandler(t, &fakeRegistry{})
	for _, test := range []struct {
		path        string
		contentType string
		pngSize     int
	}{
		{path: "/static/app.css", contentType: "text/css; charset=utf-8"},
		{path: "/static/app.js", contentType: "text/javascript; charset=utf-8"},
		{path: "/static/sherpa-mark.png", contentType: "image/png", pngSize: 96},
		{path: "/static/favicon.png", contentType: "image/png", pngSize: 32},
	} {
		t.Run(test.path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, test.path, nil))
			if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Type") != test.contentType {
				t.Fatalf("response = %d content-type=%q", recorder.Code, recorder.Header().Get("Content-Type"))
			}
			if recorder.Header().Get("Cache-Control") != "public, max-age=3600" || recorder.Header().Get("Content-Security-Policy") != contentSecurityPolicy {
				t.Fatalf("asset headers = %v", recorder.Header())
			}
			if recorder.Body.Len() == 0 {
				t.Fatal("empty embedded asset")
			}
			if test.pngSize > 0 {
				image, err := png.Decode(bytes.NewReader(recorder.Body.Bytes()))
				if err != nil {
					t.Fatalf("decode PNG: %v", err)
				}
				if image.Bounds().Dx() != test.pngSize || image.Bounds().Dy() != test.pngSize {
					t.Fatalf("PNG bounds = %v", image.Bounds())
				}
			}
		})
	}
}

func TestPageShellProvidesKeyboardSkipLink(t *testing.T) {
	handler := newTestHandler(t, &fakeRegistry{})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	body := recorder.Body.String()
	if !strings.Contains(body, `<a class="skip-link" href="#main-content">Skip to content</a>`) || !strings.Contains(body, `<main id="main-content" tabindex="-1">`) {
		t.Fatalf("keyboard skip target missing: %s", body)
	}
}

func TestDetailMetadataUsesOnlyPinnedPublicBase(t *testing.T) {
	publicBase := &url.URL{Scheme: "https", Host: "discover.example", Path: "/catalog/"}
	registry := &fakeRegistry{versionResult: registryclient.Version{
		Version: 4, Manifest: json.RawMessage(`{}`), RepoURL: testRepoURL,
	}}
	handler := newTestHandler(t, registry, Options{PublicBaseURL: publicBase, Logger: log.Default()})
	request := httptest.NewRequest(http.MethodGet, "http://attacker.example/stacks/alice/reviewer/v/4", nil)
	request.Host = "attacker.example"
	request.Header.Set("Forwarded", "host=forwarded-attacker.example")
	request.Header.Set("X-Forwarded-Host", "xfh-attacker.example")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	body := recorder.Body.String()
	for _, expected := range []string{
		`name="robots" content="index,follow"`,
		`rel="canonical" href="https://discover.example/catalog/stacks/alice/reviewer/v/4"`,
		`property="og:url" content="https://discover.example/catalog/stacks/alice/reviewer/v/4"`,
		`property="og:title" content="@alice/reviewer version 4 · SherpA"`,
		`property="og:description" content="Inspect @alice/reviewer version 4 manifest, trust, and registry scan."`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("metadata omitted %q: %s", expected, body)
		}
	}
	for _, forbidden := range []string{"attacker.example", "forwarded-attacker.example", "xfh-attacker.example"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("request host %q reached output: %s", forbidden, body)
		}
	}
}

func TestLocalDetailOmitsCanonicalAndOpenGraphURL(t *testing.T) {
	handler := newTestHandler(t, &fakeRegistry{stackResult: registryclient.Stack{RepoURL: testRepoURL}})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/stacks/alice/reviewer", nil))
	body := recorder.Body.String()
	if strings.Contains(body, `rel="canonical"`) || strings.Contains(body, `property="og:url"`) {
		t.Fatalf("unpinned URL metadata rendered: %s", body)
	}
}
