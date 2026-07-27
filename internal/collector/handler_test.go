package collector

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testUploadToken = "collector-upload-token"

type bodyReadSpy struct {
	reads  atomic.Int32
	closed atomic.Bool
}

func (body *bodyReadSpy) Read([]byte) (int, error) {
	body.reads.Add(1)
	return 0, io.EOF
}

func (body *bodyReadSpy) Close() error {
	body.closed.Store(true)
	return nil
}

type ingestFunc func(context.Context, io.Reader, int64) (Result, error)

func (fn ingestFunc) Ingest(ctx context.Context, source io.Reader, length int64) (Result, error) {
	return fn(ctx, source, length)
}

func TestUploadRejectsMissingInvalidAndDuplicateAuthorizationBeforeBodyRead(t *testing.T) {
	tests := []struct {
		name   string
		values []string
	}{
		{name: "missing"},
		{name: "blank", values: []string{""}},
		{name: "empty bearer", values: []string{"Bearer "}},
		{name: "wrong scheme", values: []string{"Basic " + testUploadToken}},
		{name: "wrong token", values: []string{"Bearer wrong-token"}},
		{name: "lowercase scheme", values: []string{"bearer " + testUploadToken}},
		{name: "leading space", values: []string{" Bearer " + testUploadToken}},
		{name: "trailing space", values: []string{"Bearer " + testUploadToken + " "}},
		{name: "embedded space", values: []string{"Bearer token value"}},
		{name: "comma combined", values: []string{"Bearer " + testUploadToken + ", Bearer " + testUploadToken}},
		{name: "duplicate", values: []string{"Bearer " + testUploadToken, "Bearer " + testUploadToken}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := &bodyReadSpy{}
			request := validUploadRequest(body)
			request.Header.Del("Authorization")
			for _, value := range test.values {
				request.Header.Add("Authorization", value)
			}
			response := httptest.NewRecorder()

			newHandlerForTest(t, rejectingIngestor(t)).ServeHTTP(response, request)

			if response.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
			}
			assertBodyUnread(t, body)
		})
	}
}

func TestUploadRejectsWrongMethodBeforeBodyRead(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			body := &bodyReadSpy{}
			request := validUploadRequest(body)
			request.Method = method
			response := httptest.NewRecorder()

			newHandlerForTest(t, rejectingIngestor(t)).ServeHTTP(response, request)

			if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != http.MethodPost {
				t.Fatalf("status=%d allow=%q", response.Code, response.Header().Get("Allow"))
			}
			assertBodyUnread(t, body)
		})
	}
}

func TestUploadRejectsMissingZeroNegativeAndUnknownLengthBeforeBodyRead(t *testing.T) {
	tests := []struct {
		name             string
		contentLength    int64
		transferEncoding []string
		wantStatus       int
	}{
		{name: "missing", contentLength: 0, wantStatus: http.StatusLengthRequired},
		{name: "zero", contentLength: 0, wantStatus: http.StatusLengthRequired},
		{name: "negative", contentLength: -2, wantStatus: http.StatusLengthRequired},
		{name: "unknown", contentLength: -1, wantStatus: http.StatusLengthRequired},
		{name: "chunked", contentLength: 12, transferEncoding: []string{"chunked"}, wantStatus: http.StatusBadRequest},
		{name: "identity transfer encoding", contentLength: 12, transferEncoding: []string{"identity"}, wantStatus: http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := &bodyReadSpy{}
			request := validUploadRequest(body)
			request.ContentLength = test.contentLength
			request.TransferEncoding = test.transferEncoding
			response := httptest.NewRecorder()

			newHandlerForTest(t, rejectingIngestor(t)).ServeHTTP(response, request)

			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
			assertBodyUnread(t, body)
		})
	}
}

func TestUploadRejectsOversizedLengthBeforeBodyRead(t *testing.T) {
	body := &bodyReadSpy{}
	request := validUploadRequest(body)
	request.ContentLength = 1025
	response := httptest.NewRecorder()

	newHandlerForTest(t, rejectingIngestor(t)).ServeHTTP(response, request)

	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusRequestEntityTooLarge)
	}
	assertBodyUnread(t, body)
}

func TestUploadRejectsWrongContentTypeBeforeBodyRead(t *testing.T) {
	for _, contentType := range []string{"", "application/octet-stream", "application/gzip; charset=binary", "Application/Gzip"} {
		t.Run(contentType, func(t *testing.T) {
			body := &bodyReadSpy{}
			request := validUploadRequest(body)
			request.Header.Set("Content-Type", contentType)
			response := httptest.NewRecorder()

			newHandlerForTest(t, rejectingIngestor(t)).ServeHTTP(response, request)

			if response.Code != http.StatusUnsupportedMediaType {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusUnsupportedMediaType)
			}
			assertBodyUnread(t, body)
		})
	}

	body := &bodyReadSpy{}
	request := validUploadRequest(body)
	request.Header["Content-Type"] = []string{"application/gzip", "application/gzip"}
	response := httptest.NewRecorder()
	newHandlerForTest(t, rejectingIngestor(t)).ServeHTTP(response, request)
	if response.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("duplicate content type status = %d, want %d", response.Code, http.StatusUnsupportedMediaType)
	}
	assertBodyUnread(t, body)
}

func TestUploadAllowsOnlyOneConcurrentRequest(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	ingestor := ingestFunc(func(_ context.Context, source io.Reader, length int64) (Result, error) {
		if source == nil || length != 12 {
			t.Fatalf("source=%v length=%d", source, length)
		}
		close(entered)
		<-release
		return Result{ObjectID: "sha256:" + strings.Repeat("a", 64), Status: ResultStored}, nil
	})
	handler := newHandlerForTest(t, ingestor)
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, validUploadRequest(&bodyReadSpy{}))
		firstDone <- response
	}()
	<-entered

	secondBody := &bodyReadSpy{}
	secondResponse := httptest.NewRecorder()
	handler.ServeHTTP(secondResponse, validUploadRequest(secondBody))

	if secondResponse.Code != http.StatusServiceUnavailable {
		t.Fatalf("second status = %d, want %d", secondResponse.Code, http.StatusServiceUnavailable)
	}
	assertBodyUnread(t, secondBody)
	close(release)
	if response := <-firstDone; response.Code != http.StatusCreated {
		t.Fatalf("first status = %d, want %d", response.Code, http.StatusCreated)
	}
}

func TestUploadMapsValidationFailureTo422(t *testing.T) {
	for _, message := range []string{"collector archive invalid", "collector upload invalid", "collector upload length mismatch"} {
		t.Run(message, func(t *testing.T) {
			response := serveUploadError(t, errors.New(message), nil)
			if response.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusUnprocessableEntity)
			}
		})
	}
}

func TestUploadMapsSpoolAdmissionFailureTo507(t *testing.T) {
	response := serveUploadError(t, errors.New("collector insufficient storage"), nil)
	if response.Code != http.StatusInsufficientStorage {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusInsufficientStorage)
	}
}

func TestUploadMapsPendingBackendFailureTo503(t *testing.T) {
	for _, message := range []string{
		"collector backend timeout",
		"collector backend unavailable",
		"collector backend cancelled",
		"collector remote verification failed",
		"collector ingestion canceled",
		"collector upload canceled",
	} {
		t.Run(message, func(t *testing.T) {
			response := serveUploadError(t, errors.New(message), nil)
			if response.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusServiceUnavailable)
			}
		})
	}
}

func TestUploadReturns201Stored(t *testing.T) {
	response := serveUploadResult(t, Result{ObjectID: "sha256:" + strings.Repeat("1", 64), Status: ResultStored})
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusCreated)
	}
}

func TestUploadReturns200Existing(t *testing.T) {
	response := serveUploadResult(t, Result{ObjectID: "sha256:" + strings.Repeat("2", 64), Status: ResultExisting})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
}

func TestUploadResponseUsesExactJSONContract(t *testing.T) {
	objectID := "sha256:" + strings.Repeat("3", 64)
	body := &bodyReadSpy{}
	ingestor := ingestFunc(func(_ context.Context, source io.Reader, length int64) (Result, error) {
		if source != body {
			t.Fatalf("source was wrapped: got %T %p, want %T %p", source, source, body, body)
		}
		if length != 12 {
			t.Fatalf("length = %d, want 12", length)
		}
		closer, ok := source.(io.Closer)
		if !ok {
			t.Fatal("source ownership lost: source is not closable")
		}
		if err := closer.Close(); err != nil {
			t.Fatal(err)
		}
		return Result{ObjectID: objectID, Status: ResultStored}, nil
	})
	response := httptest.NewRecorder()
	newHandlerForTest(t, ingestor).ServeHTTP(response, validUploadRequest(body))

	if response.Code != http.StatusCreated || response.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status=%d content-type=%q", response.Code, response.Header().Get("Content-Type"))
	}
	want := `{"object_id":"` + objectID + `","status":"stored"}` + "\n"
	if response.Body.String() != want {
		t.Fatalf("body = %q, want %q", response.Body.String(), want)
	}
	if !body.closed.Load() {
		t.Fatal("ingestor did not retain source close ownership")
	}

	invalid := serveUploadResult(t, Result{ObjectID: "private-object-id-canary", Status: ResultStatus("unexpected")})
	if invalid.Code != http.StatusInternalServerError || invalid.Body.String() != "collector upload failed\n" || strings.Contains(invalid.Body.String(), "private-object-id-canary") {
		t.Fatalf("invalid result response status=%d body=%q", invalid.Code, invalid.Body.String())
	}
}

func TestUploadResponsesAndLogsNeverExposeSecretsBackendOutputOrPaths(t *testing.T) {
	canaries := []string{
		testUploadToken,
		"repository-private-canary",
		"backend-output-private-canary",
		"/private/spool/path-canary",
		"sha256:" + strings.Repeat("f", 64),
		"uploaded-private-data-canary",
	}
	for _, failure := range []error{
		errors.New("collector archive invalid"),
		errors.New("collector insufficient storage"),
		errors.New("collector backend unavailable"),
		errors.New(strings.Join(canaries, " ")),
	} {
		var logs bytes.Buffer
		response := serveUploadError(t, failure, log.New(&logs, "", 0))
		combined := response.Body.String() + logs.String()
		for _, canary := range canaries {
			if strings.Contains(combined, canary) {
				t.Fatalf("failure %q exposed canary %q in %q", failure, canary, combined)
			}
		}
		if response.Code == http.StatusInternalServerError && response.Body.String() != "collector upload failed\n" {
			t.Fatalf("unexpected failure body = %q", response.Body.String())
		}
	}
}

func TestHealthzIsBackendIndependent(t *testing.T) {
	handler := newHandlerForTest(t, rejectingIngestor(t))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if response.Code != http.StatusOK || response.Body.String() != "ok\n" || response.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatalf("health response status=%d content-type=%q body=%q", response.Code, response.Header().Get("Content-Type"), response.Body.String())
	}

	unknown := httptest.NewRecorder()
	handler.ServeHTTP(unknown, httptest.NewRequest(http.MethodGet, "/private/object-canary", nil))
	if unknown.Code != http.StatusNotFound || strings.Contains(unknown.Body.String(), "private/object-canary") {
		t.Fatalf("unknown route status=%d body=%q", unknown.Code, unknown.Body.String())
	}
}

func TestReadyzUsesRecoveryReadinessWithoutDetails(t *testing.T) {
	digest := sha256.Sum256([]byte(testUploadToken))
	startedAt := time.Unix(100, 0)
	status := NewStatusTracker(ReadinessConfig{
		StartedAt: startedAt, StartupGrace: time.Hour, MaxRecoveryAge: 2 * time.Hour,
	}, func() time.Time { return startedAt.Add(time.Minute) })
	status.SetSpoolWritable(true)
	handler := NewHandler(
		HTTPConfig{TokenDigest: digest, MaxBytes: 1024}, rejectingIngestor(t), status,
		func() time.Time { return time.Unix(999999, 0) }, -time.Second, -time.Second, log.New(io.Discard, "", 0),
	)

	ready := httptest.NewRecorder()
	handler.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if ready.Code != http.StatusOK || ready.Body.String() != "ready\n" {
		t.Fatalf("ready response status=%d body=%q", ready.Code, ready.Body.String())
	}

	status.RecordPending("private-object-id-canary", startedAt.Add(-3*time.Hour))
	notReady := httptest.NewRecorder()
	handler.ServeHTTP(notReady, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if notReady.Code != http.StatusServiceUnavailable || notReady.Body.String() != "not ready\n" {
		t.Fatalf("not-ready response status=%d body=%q", notReady.Code, notReady.Body.String())
	}
	for _, detail := range []string{"private-object-id-canary", startedAt.String(), "terminal", "pending"} {
		if strings.Contains(strings.ToLower(notReady.Body.String()), strings.ToLower(detail)) {
			t.Fatalf("readiness exposed detail %q in %q", detail, notReady.Body.String())
		}
	}
}

func serveUploadResult(t *testing.T, result Result) *httptest.ResponseRecorder {
	t.Helper()
	ingestor := ingestFunc(func(_ context.Context, source io.Reader, _ int64) (Result, error) {
		closer, ok := source.(io.Closer)
		if !ok {
			t.Fatal("source is not closable")
		}
		if err := closer.Close(); err != nil {
			t.Fatal(err)
		}
		return result, nil
	})
	response := httptest.NewRecorder()
	newHandlerForTest(t, ingestor).ServeHTTP(response, validUploadRequest(&bodyReadSpy{}))
	return response
}

func serveUploadError(t *testing.T, ingestErr error, logger *log.Logger) *httptest.ResponseRecorder {
	t.Helper()
	ingestor := ingestFunc(func(_ context.Context, source io.Reader, _ int64) (Result, error) {
		closer, ok := source.(io.Closer)
		if !ok {
			t.Fatal("source is not closable")
		}
		if err := closer.Close(); err != nil {
			t.Fatal(err)
		}
		return Result{}, ingestErr
	})
	digest := sha256.Sum256([]byte(testUploadToken))
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	response := httptest.NewRecorder()
	NewHandler(HTTPConfig{TokenDigest: digest, MaxBytes: 1024}, ingestor, nil, nil, 0, 0, logger).
		ServeHTTP(response, validUploadRequest(&bodyReadSpy{}))
	return response
}

func newHandlerForTest(t *testing.T, ingestor Ingestor) http.Handler {
	t.Helper()
	digest := sha256.Sum256([]byte(testUploadToken))
	status := NewStatusTracker(ReadinessConfig{
		StartedAt: time.Unix(1, 0), StartupGrace: time.Hour, MaxRecoveryAge: time.Hour,
	}, func() time.Time { return time.Unix(2, 0) })
	status.SetSpoolWritable(true)
	return NewHandler(HTTPConfig{TokenDigest: digest, MaxBytes: 1024}, ingestor, status, time.Now, time.Hour, time.Hour, log.New(io.Discard, "", 0))
}

func validUploadRequest(body io.ReadCloser) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/v1/exports", body)
	request.ContentLength = 12
	request.Header.Set("Authorization", "Bearer "+testUploadToken)
	request.Header.Set("Content-Type", "application/gzip")
	return request
}

func rejectingIngestor(t *testing.T) Ingestor {
	t.Helper()
	return ingestFunc(func(context.Context, io.Reader, int64) (Result, error) {
		t.Fatal("Ingest called for rejected request")
		return Result{}, nil
	})
}

func assertBodyUnread(t *testing.T, body *bodyReadSpy) {
	t.Helper()
	if reads := body.reads.Load(); reads != 0 {
		t.Fatalf("body reads = %d, want zero", reads)
	}
}
