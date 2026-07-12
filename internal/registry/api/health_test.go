package api

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestHealthHandlerReadyGate(t *testing.T) {
	var ready atomic.Bool
	handler := HealthHandler(&ready)

	before := httptest.NewRecorder()
	handler.ServeHTTP(before, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if before.Code != http.StatusServiceUnavailable {
		t.Fatalf("pre-ready status = %d, want %d", before.Code, http.StatusServiceUnavailable)
	}

	ready.Store(true)
	after := httptest.NewRecorder()
	handler.ServeHTTP(after, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if after.Code != http.StatusOK || after.Body.String() != "ok" {
		t.Fatalf("post-ready status = %d, body = %q", after.Code, after.Body.String())
	}
}
