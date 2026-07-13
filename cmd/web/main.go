package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	webapp "sherpa/internal/web"
	"sherpa/internal/web/registryclient"
)

const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 10 * time.Second
	writeTimeout      = 15 * time.Second
	idleTimeout       = 60 * time.Second
	shutdownTimeout   = 10 * time.Second
	maxHeaderBytes    = 1 << 20
)

type readinessHandler struct {
	ready atomic.Bool
	app   http.Handler
}

func (h *readinessHandler) setReady(app http.Handler) {
	h.app = app
	h.ready.Store(true)
}

func (h *readinessHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.ready.Load() || h.app == nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	h.app.ServeHTTP(w, r)
}

func run(cfg webapp.Config) (*http.Server, error) {
	registry, err := registryclient.New(cfg.RegistryAPIURL, cfg.UpstreamTimeout)
	if err != nil {
		return nil, errors.New("configure registry client")
	}
	publicBaseURL, err := parsePublicBaseURL(cfg.PublicBaseURL)
	if err != nil {
		return nil, err
	}
	gate := &readinessHandler{}
	handler, err := webapp.New(registry, webapp.Options{PublicBaseURL: publicBaseURL, Logger: log.Default()})
	if err != nil {
		return nil, errors.New("initialize web handler")
	}
	gate.setReady(handler)
	return &http.Server{
		Addr:              cfg.Addr,
		Handler:           gate,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
	}, nil
}

func parsePublicBaseURL(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" || parsed.Hostname() == "" {
		return nil, errors.New("configure public web URL")
	}
	if parsed.Opaque != "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || strings.Contains(raw, "#") {
		return nil, errors.New("configure public web URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("configure public web URL")
	}
	return parsed, nil
}

func serve(ctx context.Context, cfg webapp.Config) error {
	server, err := run(cfg)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen for web requests: %w", err)
	}
	return serveOnListener(ctx, server, listener)
}

func serveOnListener(ctx context.Context, server *http.Server, listener net.Listener) error {
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- server.Serve(listener)
	}()

	select {
	case err := <-serverErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve web requests: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		err := server.Shutdown(shutdownCtx)
		cancel()
		if err != nil {
			_ = server.Close()
			<-serverErr
			return fmt.Errorf("shutdown web server: %w", err)
		}
		if err := <-serverErr; !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve web requests: %w", err)
		}
		return nil
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	cfg, err := webapp.LoadConfig()
	if err != nil {
		log.Fatal(err)
	}
	if err := serve(ctx, cfg); err != nil {
		log.Fatal(err)
	}
}
