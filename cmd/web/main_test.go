package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	webapp "sherpa/internal/web"
)

func TestReadinessHandlerIsUnavailableUntilAppIsReady(t *testing.T) {
	gate := &readinessHandler{}
	request := httptest.NewRequest(http.MethodGet, "http://web.test/healthz", nil)
	before := httptest.NewRecorder()
	gate.ServeHTTP(before, request)
	if before.Code != http.StatusServiceUnavailable || before.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("before readiness status=%d headers=%v", before.Code, before.Header())
	}

	gate.setReady(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	after := httptest.NewRecorder()
	gate.ServeHTTP(after, request)
	if after.Code != http.StatusOK || after.Body.String() != "ok" {
		t.Fatalf("after readiness status=%d body=%q", after.Code, after.Body.String())
	}
}

func TestRunBuildsHardenedServerAndHealthIgnoresRegistryOutage(t *testing.T) {
	var upstreamCalls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls++
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	cfg := webapp.Config{
		Addr: "127.0.0.1:0", RegistryAPIURL: upstream.URL,
		PublicBaseURL: "https://www.example/catalog", UpstreamTimeout: time.Second,
	}
	server, err := run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if server.Addr != cfg.Addr || server.ReadHeaderTimeout != 5*time.Second || server.ReadTimeout != 10*time.Second || server.WriteTimeout != 15*time.Second || server.IdleTimeout != 60*time.Second || server.MaxHeaderBytes != 1<<20 {
		t.Fatalf("server settings = %#v", server)
	}
	assertHealthy(t, server.Handler)
	upstream.Close()
	assertHealthy(t, server.Handler)
	if upstreamCalls != 0 {
		t.Fatalf("health made %d registry calls", upstreamCalls)
	}
}

func TestRunRejectsInvalidURLsWithoutExposingCredentials(t *testing.T) {
	for _, cfg := range []webapp.Config{
		{Addr: ":8080", RegistryAPIURL: "https://user:planted-secret@registry.example", UpstreamTimeout: time.Second},
		{Addr: ":8080", RegistryAPIURL: "https://registry.example", PublicBaseURL: "javascript:planted-secret", UpstreamTimeout: time.Second},
	} {
		server, err := run(cfg)
		if err == nil || server != nil {
			t.Fatalf("run(%#v) = %#v, %v", cfg, server, err)
		}
		if contains(err.Error(), "planted-secret") {
			t.Fatalf("error %q exposes credentials", err)
		}
	}
}

func TestRunWiresPinnedPublicRegistryForLogin(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer upstream.Close()
	server, err := run(webapp.Config{Addr: "127.0.0.1:0", RegistryAPIURL: upstream.URL, RegistryPublicURL: "https://registry.example/prefix", PublicBaseURL: "https://web.example", UpstreamTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://evil.example/login", nil)
	req.Host = "evil.example"
	server.Handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther || !contains(rr.Header().Get("Location"), "https://registry.example/prefix/v1/auth/web/start?") {
		t.Fatalf("login=%d location=%q", rr.Code, rr.Header().Get("Location"))
	}
}

func TestServeOnListenerGracefullyDrainsActiveRequest(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var startOnce sync.Once
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			startOnce.Do(func() { close(started) })
			<-release
			w.WriteHeader(http.StatusNoContent)
		}),
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- serveOnListener(ctx, server, listener) }()

	requestDone := make(chan error, 1)
	go func() {
		response, err := http.Get("http://" + listener.Addr().String())
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			err = response.Body.Close()
		}
		requestDone <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case err := <-serveDone:
		t.Fatalf("server returned before request drained: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not drain promptly")
	}
	if err := <-requestDone; err != nil {
		t.Fatal(err)
	}
}

func assertHealthy(t *testing.T, handler http.Handler) {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://web.test/healthz", nil))
	if recorder.Code != http.StatusOK || recorder.Body.String() != "ok" {
		t.Fatalf("health status=%d body=%q", recorder.Code, recorder.Body.String())
	}
}

func contains(value, substring string) bool {
	for i := 0; i+len(substring) <= len(value); i++ {
		if value[i:i+len(substring)] == substring {
			return true
		}
	}
	return false
}
