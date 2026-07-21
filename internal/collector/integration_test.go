package collector

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"sherpa/internal/recoveryarchive"
	registryexport "sherpa/internal/registry/export"
)

func TestCollectorHTTPUploadThroughRealServer(t *testing.T) {
	fixture := newServiceFixture(t)
	token := "real-server-upload-token"
	server := httptest.NewServer(integrationHandler(token, fixture.service, fixture.status))
	defer server.Close()
	archive := validRecoveryArchive(t, "real-http-upload")
	archivePath := filepath.Join(t.TempDir(), "recovery.tar.gz")
	if err := os.WriteFile(archivePath, archive, 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := registryexport.Upload(context.Background(), archivePath, server.URL+"/v1/exports", token)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	wantID := "sha256:" + digestHex(archive)
	if result.ObjectID != wantID || result.Status != "stored" || !fixture.backend.has(wantID) {
		t.Fatalf("result=%#v remote=%v", result, fixture.backend.has(wantID))
	}
}

func TestCollectorRestartDrainsEncryptedPendingObject(t *testing.T) {
	fixture := newServiceFixture(t)
	fixture.backend.existsErr = errors.New("backend-private-canary")
	token := "restart-upload-token"
	server := httptest.NewServer(integrationHandler(token, fixture.service, fixture.status))
	archive := validRecoveryArchive(t, "restart-pending")
	objectID := "sha256:" + digestHex(archive)

	response, err := integrationUpload(context.Background(), server.URL+"/v1/exports", token, archive)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	server.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("initial status = %d, want %d", response.StatusCode, http.StatusServiceUnavailable)
	}
	if objects, err := fixture.spool.Discover(); err != nil || len(objects) != 1 || objects[0].ObjectID != objectID {
		t.Fatalf("pending objects = %#v, %v", objects, err)
	}

	spoolPath := fixture.spool.path
	ledgerPath := fixture.ledger.path
	if err := fixture.ledger.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fixture.spool.Close(); err != nil {
		t.Fatal(err)
	}
	restartedSpool, err := OpenSpool(spoolPath, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer restartedSpool.Close()
	restartedLedger, err := OpenLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	defer restartedLedger.Close()
	fixture.backend.existsErr = nil
	restartedNow := fixture.now.Add(time.Minute)
	restartedStatus := NewStatusTracker(ReadinessConfig{
		StartedAt: restartedNow, StartupGrace: time.Hour, MaxRecoveryAge: 24 * time.Hour,
	}, func() time.Time { return restartedNow })
	restartedService := newService(
		restartedSpool, restartedLedger, nil, fixture.backend, restartedStatus, recoveryarchive.DefaultLimits(),
		serviceOps{now: func() time.Time { return restartedNow }},
	)
	ctx, cancel := context.WithCancel(context.Background())
	drained := make(chan struct{})
	var once sync.Once
	worker := newWorker(restartedSpool, restartedLedger, restartedService, restartedStatus, time.Nanosecond, workerOps{
		now: func() time.Time { return restartedNow },
		wait: func(context.Context, time.Duration) error {
			if fixture.backend.has(objectID) {
				once.Do(func() { close(drained) })
				cancel()
				return context.Canceled
			}
			return nil
		},
	})
	workerDone := make(chan error, 1)
	go func() { workerDone <- worker.Run(ctx) }()
	<-drained
	if err := <-workerDone; err != nil {
		t.Fatalf("worker: %v", err)
	}
	if objects, err := restartedSpool.Discover(); err != nil || len(objects) != 0 {
		t.Fatalf("remaining objects = %#v, %v", objects, err)
	}
	record, found, err := restartedLedger.Get(objectID)
	if err != nil || !found || record.StoredAt == nil {
		t.Fatalf("record=%#v found=%v err=%v", record, found, err)
	}
}

func TestCollectorDuplicateAfterLostResponseReturnsSameObjectID(t *testing.T) {
	fixture := newServiceFixture(t)
	token := "lost-response-upload-token"
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tracking := &connectionTrackingListener{Listener: listener}
	requestDone := make(chan struct{})
	handler := integrationHandler(token, fixture.service, fixture.status)
	server := &http.Server{Handler: http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		defer close(requestDone)
		handler.ServeHTTP(response, request)
	})}
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(tracking) }()
	fixture.backend.onCreate = func(PendingObject) { tracking.closeActive() }
	archive := validRecoveryArchive(t, "lost-response")
	objectID := "sha256:" + digestHex(archive)

	response, requestErr := integrationUpload(context.Background(), "http://"+listener.Addr().String()+"/v1/exports", token, archive)
	if response != nil {
		_ = response.Body.Close()
	}
	if requestErr == nil {
		t.Fatal("first upload unexpectedly received a response")
	}
	<-requestDone
	fixture.backend.onCreate = nil
	if !fixture.backend.has(objectID) {
		t.Fatal("remote object was not durably published before response loss")
	}

	retryServer := httptest.NewServer(integrationHandler(token, fixture.service, fixture.status))
	defer retryServer.Close()
	retry, err := integrationUpload(context.Background(), retryServer.URL+"/v1/exports", token, archive)
	if err != nil {
		t.Fatal(err)
	}
	defer retry.Body.Close()
	var result struct {
		ObjectID string `json:"object_id"`
		Status   string `json:"status"`
	}
	decoder := json.NewDecoder(retry.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		t.Fatal(err)
	}
	if retry.StatusCode != http.StatusOK || result.ObjectID != objectID || result.Status != "existing" {
		t.Fatalf("retry status=%d result=%#v", retry.StatusCode, result)
	}

	_ = server.Close()
	serveErr := <-serverDone
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		t.Fatalf("Serve: %v", serveErr)
	}
}

func integrationHandler(token string, ingestor Ingestor, status *StatusTracker) http.Handler {
	digest := sha256.Sum256([]byte(token))
	return NewHandler(
		HTTPConfig{TokenDigest: digest, MaxBytes: recoveryarchive.DefaultLimits().MaxCompressedBytes},
		ingestor, status, time.Now, time.Hour, 24*time.Hour, log.New(io.Discard, "", 0),
	)
}

func integrationUpload(ctx context.Context, endpoint, token string, archive []byte) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/gzip")
	client := &http.Client{
		Transport:     &http.Transport{DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return client.Do(request)
}

type connectionTrackingListener struct {
	net.Listener
	mu     sync.Mutex
	active net.Conn
}

func (listener *connectionTrackingListener) Accept() (net.Conn, error) {
	connection, err := listener.Listener.Accept()
	if err != nil {
		return nil, err
	}
	listener.mu.Lock()
	listener.active = connection
	listener.mu.Unlock()
	return connection, nil
}

func (listener *connectionTrackingListener) closeActive() {
	listener.mu.Lock()
	connection := listener.active
	listener.mu.Unlock()
	if connection != nil {
		_ = connection.Close()
	}
}
