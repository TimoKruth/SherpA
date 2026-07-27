package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"sherpa/internal/collector"
	"sherpa/internal/recoveryarchive"
	"sherpa/internal/recoveryarchive/testfixture"
)

func TestDispatchVerifyUsesSharedValidator(t *testing.T) {
	archivePath := testfixture.Write(t,
		testfixture.Repository{Path: "repos/acme/api.bundle", Contents: "api bundle"},
		testfixture.Repository{Path: "repos/acme/web.bundle", Contents: "web bundle"},
	)
	wantReport, err := recoveryarchive.ValidateFile(context.Background(), archivePath, recoveryarchive.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder

	handled, code := dispatch(context.Background(), []string{"verify", archivePath}, &stdout, &stderr)

	want := fmt.Sprintf("valid\nartifacts=%d\nverified_bytes=%d\nmanifest_final=%t\n", wantReport.ArtifactCount, wantReport.VerifiedBytes, wantReport.ManifestFinal)
	if !handled || code != 0 || stdout.String() != want || stderr.Len() != 0 {
		t.Fatalf("dispatch handled=%v code=%d stdout=%q stderr=%q, want stdout %q", handled, code, stdout.String(), stderr.String(), want)
	}
}

func TestDispatchVerifyPrintsOnlySafeSummary(t *testing.T) {
	memberCanary := "repos/private-owner-canary/private-repository-canary.bundle"
	contentsCanary := "private-bundle-contents-canary"
	archivePath := testfixture.Write(t, testfixture.Repository{Path: memberCanary, Contents: contentsCanary})
	var stdout, stderr strings.Builder

	handled, code := dispatch(context.Background(), []string{"verify", archivePath}, &stdout, &stderr)

	wantBytes := int64(len(testfixture.PostgresContents) + len(contentsCanary))
	want := fmt.Sprintf("valid\nartifacts=2\nverified_bytes=%d\nmanifest_final=true\n", wantBytes)
	if !handled || code != 0 || stdout.String() != want || stderr.Len() != 0 {
		t.Fatalf("dispatch handled=%v code=%d stdout=%q stderr=%q", handled, code, stdout.String(), stderr.String())
	}
	for _, canary := range []string{archivePath, filepath.Dir(archivePath), memberCanary, contentsCanary, "private-owner-canary", "private-repository-canary"} {
		if strings.Contains(stdout.String(), canary) || strings.Contains(stderr.String(), canary) {
			t.Fatalf("verification output exposed canary %q: stdout=%q stderr=%q", canary, stdout.String(), stderr.String())
		}
	}
}

func TestDispatchVerifyFailurePrintsOnlySafeClassification(t *testing.T) {
	memberCanary := "../private-member-path-canary"
	manifestCanary := "private-manifest-value-canary"
	archivePath := writeArchive(t, []archiveMember{
		{name: "postgres.dump", contents: "postgres-canary"},
		{name: memberCanary, contents: "bundle-canary"},
		{name: "manifest.json", contents: manifestCanary},
	})
	var stdout, stderr strings.Builder

	handled, code := dispatch(context.Background(), []string{"verify", archivePath}, &stdout, &stderr)

	if !handled || code != 1 || stdout.Len() != 0 || stderr.String() != "invalid\nclassification=unsafe_path\n" {
		t.Fatalf("dispatch handled=%v code=%d stdout=%q stderr=%q", handled, code, stdout.String(), stderr.String())
	}
	for _, canary := range []string{archivePath, filepath.Dir(archivePath), memberCanary, manifestCanary, "bundle-canary", "postgres-canary"} {
		if strings.Contains(stdout.String(), canary) || strings.Contains(stderr.String(), canary) {
			t.Fatalf("failure output exposed canary %q: stdout=%q stderr=%q", canary, stdout.String(), stderr.String())
		}
	}

	invalidManifestPath := writeArchive(t, []archiveMember{
		{name: "postgres.dump", contents: "postgres-canary"},
		{name: "manifest.json", contents: manifestCanary},
	})
	stdout.Reset()
	stderr.Reset()
	handled, code = dispatch(context.Background(), []string{"verify", invalidManifestPath}, &stdout, &stderr)
	if !handled || code != 1 || stdout.Len() != 0 || stderr.String() != "invalid\nclassification=invalid_manifest\n" {
		t.Fatalf("invalid manifest dispatch handled=%v code=%d stdout=%q stderr=%q", handled, code, stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), invalidManifestPath) || strings.Contains(stderr.String(), manifestCanary) {
		t.Fatalf("invalid-manifest output exposed private data: %q", stderr.String())
	}

	missingPath := filepath.Join(t.TempDir(), "private-archive-path-canary.tar.gz")
	stdout.Reset()
	stderr.Reset()
	handled, code = dispatch(context.Background(), []string{"verify", missingPath}, &stdout, &stderr)
	if !handled || code != 1 || stdout.Len() != 0 || stderr.String() != "invalid\nclassification=archive_validation_failed\n" {
		t.Fatalf("missing dispatch handled=%v code=%d stdout=%q stderr=%q", handled, code, stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), missingPath) || strings.Contains(stderr.String(), filepath.Dir(missingPath)) {
		t.Fatalf("missing-archive output exposed path: %q", stderr.String())
	}
}

func TestDispatchVerifyDoesNotRequireRuntimeSecrets(t *testing.T) {
	for _, key := range []string{
		"SHERPA_COLLECTOR_TOKEN_FILE",
		"SHERPA_COLLECTOR_AGE_RECIPIENT_FILE",
		"SHERPA_COLLECTOR_BORG_REPOSITORY_FILE",
		"SHERPA_COLLECTOR_BORG_SSH_KEY_FILE",
		"SHERPA_COLLECTOR_KNOWN_HOSTS_FILE",
	} {
		t.Setenv(key, filepath.Join(t.TempDir(), "must-not-be-read-canary"))
	}
	archivePath := testfixture.Write(t)
	var stdout, stderr strings.Builder

	handled, code := dispatch(context.Background(), []string{"verify", archivePath}, &stdout, &stderr)

	if !handled || code != 0 || stdout.String() != fmt.Sprintf("valid\nartifacts=1\nverified_bytes=%d\nmanifest_final=true\n", len(testfixture.PostgresContents)) || stderr.Len() != 0 {
		t.Fatalf("dispatch handled=%v code=%d stdout=%q stderr=%q", handled, code, stdout.String(), stderr.String())
	}
}

func TestDispatchRejectsInvalidVerifyUsage(t *testing.T) {
	for _, args := range [][]string{{"verify"}, {"verify", "one", "two"}} {
		var stdout, stderr strings.Builder
		handled, code := dispatch(context.Background(), args, &stdout, &stderr)
		if !handled || code != 2 || stdout.Len() != 0 || stderr.String() != "usage: collector verify <archive-path>\n" {
			t.Fatalf("args=%q handled=%v code=%d stdout=%q stderr=%q", args, handled, code, stdout.String(), stderr.String())
		}
	}

	handled, code := dispatch(context.Background(), []string{"unknown"}, bytes.NewBuffer(nil), bytes.NewBuffer(nil))
	if handled || code != 0 {
		t.Fatalf("unknown command handled=%v code=%d", handled, code)
	}
}

func TestRunBuildsDocumentedServerTimeouts(t *testing.T) {
	server := newCollectorServer("127.0.0.1:0", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	if server.ReadHeaderTimeout != collectorReadHeaderTimeout || server.ReadTimeout != collectorReadTimeout ||
		server.WriteTimeout != collectorWriteTimeout || server.IdleTimeout != collectorIdleTimeout ||
		server.MaxHeaderBytes != collectorMaxHeaderBytes {
		t.Fatalf("server timeouts = header %s read %s write %s idle %s headers %d",
			server.ReadHeaderTimeout, server.ReadTimeout, server.WriteTimeout, server.IdleTimeout, server.MaxHeaderBytes)
	}
	if collectorReadHeaderTimeout != 5*time.Second || collectorReadTimeout != 10*time.Minute ||
		collectorWriteTimeout != 11*time.Minute || collectorIdleTimeout != 2*time.Minute || collectorMaxHeaderBytes != 1<<20 {
		t.Fatalf("documented collector timeout constants changed")
	}
}

func TestRunAcquiresSpoolLockBeforeStartingWorker(t *testing.T) {
	var mu sync.Mutex
	var events []string
	record := func(event string) {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
	}
	ctx, cancel := context.WithCancel(context.Background())
	spool := &fakeSpoolResource{acquire: func() (func() error, error) {
		record("lock")
		return func() error { record("unlock"); return nil }, nil
	}, close: func() error { record("close-spool"); return nil }}
	ledger := &fakeLedgerResource{close: func() error { record("close-ledger"); return nil }}
	worker := workerFunc(func(ctx context.Context) error {
		record("worker")
		<-ctx.Done()
		record("worker-done")
		return nil
	})
	ops := collectorRunOps{
		loadConfig: func(func(string) string) (Config, error) {
			record("config")
			return Config{ListenAddr: "127.0.0.1:0", PartialMaxAge: time.Hour}, nil
		},
		openSpool:  func(string, time.Duration) (spoolResource, error) { record("spool"); return spool, nil },
		openLedger: func(string) (ledgerResource, error) { record("ledger"); return ledger, nil },
		build: func(Config, spoolResource, ledgerResource, time.Time, *log.Logger) (http.Handler, workerRunner, error) {
			record("build")
			return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }), worker, nil
		},
		listen: func(network, address string) (net.Listener, error) {
			record("listen")
			listener, err := net.Listen(network, address)
			cancel()
			return listener, err
		},
		now: time.Now,
	}

	if err := runCollectorWithOps(ctx, func(string) string { return "" }, log.New(io.Discard, "", 0), ops); err != nil {
		t.Fatalf("runCollectorWithOps: %v", err)
	}
	mu.Lock()
	got := append([]string(nil), events...)
	mu.Unlock()
	for _, pair := range [][2]string{{"spool", "lock"}, {"lock", "ledger"}, {"ledger", "build"}, {"build", "worker"}, {"worker", "listen"}, {"worker-done", "close-ledger"}, {"close-ledger", "close-spool"}, {"close-spool", "unlock"}} {
		if eventIndex(got, pair[0]) < 0 || eventIndex(got, pair[1]) < 0 || eventIndex(got, pair[0]) >= eventIndex(got, pair[1]) {
			t.Fatalf("events = %v, want %q before %q", got, pair[0], pair[1])
		}
	}
}

func TestRunFailsBeforeListenOnUnsafeConfiguration(t *testing.T) {
	var listened atomic.Bool
	ops := collectorRunOps{
		loadConfig: func(func(string) string) (Config, error) { return Config{}, errors.New("unsafe configuration canary") },
		listen: func(string, string) (net.Listener, error) {
			listened.Store(true)
			return nil, errors.New("must not listen")
		},
	}
	if err := runCollectorWithOps(context.Background(), func(string) string { return "" }, log.New(io.Discard, "", 0), ops); err == nil {
		t.Fatal("runCollectorWithOps succeeded")
	}
	if listened.Load() {
		t.Fatal("listener opened before configuration validation")
	}
}

func TestServeGracefullyStopsHTTPAndWorker(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	workerStarted := make(chan struct{})
	workerDone := make(chan struct{})
	spoolClosed := atomic.Bool{}
	ledgerClosed := atomic.Bool{}
	lockReleased := atomic.Bool{}
	spool := &fakeSpoolResource{acquire: func() (func() error, error) {
		return func() error { lockReleased.Store(true); return nil }, nil
	}, close: func() error { spoolClosed.Store(true); return nil }}
	ledger := &fakeLedgerResource{close: func() error { ledgerClosed.Store(true); return nil }}
	ops := collectorRunOps{
		loadConfig: func(func(string) string) (Config, error) {
			return Config{ListenAddr: listener.Addr().String(), PartialMaxAge: time.Hour}, nil
		},
		openSpool:  func(string, time.Duration) (spoolResource, error) { return spool, nil },
		openLedger: func(string) (ledgerResource, error) { return ledger, nil },
		build: func(Config, spoolResource, ledgerResource, time.Time, *log.Logger) (http.Handler, workerRunner, error) {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/healthz" {
						_, _ = io.WriteString(w, "ok\n")
						return
					}
					http.NotFound(w, r)
				}), workerFunc(func(ctx context.Context) error {
					close(workerStarted)
					<-ctx.Done()
					close(workerDone)
					return nil
				}), nil
		},
		listen: func(string, string) (net.Listener, error) { return listener, nil },
		now:    time.Now,
	}
	runDone := make(chan error, 1)
	go func() {
		runDone <- runCollectorWithOps(ctx, func(string) string { return "" }, log.New(io.Discard, "", 0), ops)
	}()
	<-workerStarted

	response, err := http.Get("http://" + listener.Addr().String() + "/healthz")
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		cancel()
		t.Fatalf("health status = %d", response.StatusCode)
	}
	cancel()
	if err := <-runDone; err != nil {
		t.Fatalf("runCollectorWithOps: %v", err)
	}
	select {
	case <-workerDone:
	default:
		t.Fatal("worker was not stopped")
	}
	if !ledgerClosed.Load() || !spoolClosed.Load() || !lockReleased.Load() {
		t.Fatalf("cleanup ledger=%v spool=%v lock=%v", ledgerClosed.Load(), spoolClosed.Load(), lockReleased.Load())
	}
}

func TestServeForcedShutdownCancelsInFlightRequestBeforeStorageCleanup(t *testing.T) {
	baseListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := &nonClosingListener{Listener: baseListener}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requestStarted := make(chan struct{})
	requestCanceled := make(chan struct{})
	workerDone := make(chan struct{})
	spoolClosed := atomic.Bool{}
	ledgerClosed := atomic.Bool{}
	lockReleased := atomic.Bool{}
	spool := &fakeSpoolResource{acquire: func() (func() error, error) {
		return func() error { lockReleased.Store(true); return nil }, nil
	}, close: func() error { spoolClosed.Store(true); return nil }}
	ledger := &fakeLedgerResource{close: func() error { ledgerClosed.Store(true); return nil }}
	ops := collectorRunOps{
		loadConfig: func(func(string) string) (Config, error) {
			return Config{ListenAddr: listener.Addr().String(), PartialMaxAge: time.Hour}, nil
		},
		openSpool:  func(string, time.Duration) (spoolResource, error) { return spool, nil },
		openLedger: func(string) (ledgerResource, error) { return ledger, nil },
		build: func(Config, spoolResource, ledgerResource, time.Time, *log.Logger) (http.Handler, workerRunner, error) {
			return http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
					close(requestStarted)
					<-request.Context().Done()
					close(requestCanceled)
				}), workerFunc(func(ctx context.Context) error {
					<-ctx.Done()
					close(workerDone)
					return nil
				}), nil
		},
		listen:          func(string, string) (net.Listener, error) { return listener, nil },
		now:             time.Now,
		shutdownTimeout: 25 * time.Millisecond,
	}
	runDone := make(chan error, 1)
	go func() {
		runDone <- runCollectorWithOps(ctx, func(string) string { return "" }, log.New(io.Discard, "", 0), ops)
	}()

	connection, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if _, err := fmt.Fprintf(connection, "GET /blocked HTTP/1.1\r\nHost: %s\r\n\r\n", listener.Addr().String()); err != nil {
		t.Fatal(err)
	}
	<-requestStarted
	cancel()

	select {
	case err := <-runDone:
		if err == nil || err.Error() != "collector HTTP shutdown failed" {
			t.Fatalf("runCollectorWithOps error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("forced shutdown did not cancel the active request")
	}
	select {
	case <-requestCanceled:
	default:
		t.Fatal("active request context was not canceled")
	}
	select {
	case <-workerDone:
	default:
		t.Fatal("worker was not stopped after active request cancellation")
	}
	if !ledgerClosed.Load() || !spoolClosed.Load() || !lockReleased.Load() {
		t.Fatalf("cleanup ledger=%v spool=%v lock=%v", ledgerClosed.Load(), spoolClosed.Load(), lockReleased.Load())
	}
}

func TestServeFailureShutsDownActiveHTTPWork(t *testing.T) {
	baseListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	failAccept := make(chan struct{})
	listener := &failAfterFirstAcceptListener{Listener: baseListener, fail: failAccept}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requestStarted := make(chan struct{})
	requestCanceled := make(chan struct{})
	workerDone := make(chan struct{})
	spoolClosed := atomic.Bool{}
	ledgerClosed := atomic.Bool{}
	lockReleased := atomic.Bool{}
	spool := &fakeSpoolResource{acquire: func() (func() error, error) {
		return func() error { lockReleased.Store(true); return nil }, nil
	}, close: func() error { spoolClosed.Store(true); return nil }}
	ledger := &fakeLedgerResource{close: func() error { ledgerClosed.Store(true); return nil }}
	ops := collectorRunOps{
		loadConfig: func(func(string) string) (Config, error) {
			return Config{ListenAddr: listener.Addr().String(), PartialMaxAge: time.Hour}, nil
		},
		openSpool:  func(string, time.Duration) (spoolResource, error) { return spool, nil },
		openLedger: func(string) (ledgerResource, error) { return ledger, nil },
		build: func(Config, spoolResource, ledgerResource, time.Time, *log.Logger) (http.Handler, workerRunner, error) {
			return http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
					close(requestStarted)
					<-request.Context().Done()
					close(requestCanceled)
				}), workerFunc(func(ctx context.Context) error {
					<-ctx.Done()
					close(workerDone)
					return nil
				}), nil
		},
		listen:          func(string, string) (net.Listener, error) { return listener, nil },
		now:             time.Now,
		shutdownTimeout: 25 * time.Millisecond,
	}
	runDone := make(chan error, 1)
	go func() {
		runDone <- runCollectorWithOps(ctx, func(string) string { return "" }, log.New(io.Discard, "", 0), ops)
	}()

	connection, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if _, err := fmt.Fprintf(connection, "GET /blocked HTTP/1.1\r\nHost: %s\r\n\r\n", listener.Addr().String()); err != nil {
		t.Fatal(err)
	}
	<-requestStarted
	close(failAccept)

	select {
	case err := <-runDone:
		if err == nil || err.Error() != "collector HTTP server failed" {
			t.Fatalf("runCollectorWithOps error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("unexpected Serve failure did not shut down active HTTP work")
	}
	select {
	case <-requestCanceled:
	default:
		t.Fatal("active request context was not canceled")
	}
	select {
	case <-workerDone:
	default:
		t.Fatal("worker was not stopped after active request cancellation")
	}
	if !ledgerClosed.Load() || !spoolClosed.Load() || !lockReleased.Load() {
		t.Fatalf("cleanup ledger=%v spool=%v lock=%v", ledgerClosed.Load(), spoolClosed.Load(), lockReleased.Load())
	}
}

func TestHealthcheckCommandCallsOnlyLocalHealthz(t *testing.T) {
	for _, rawURL := range []string{
		"https://127.0.0.1:8080/healthz",
		"http://example.com/healthz",
		"http://127.0.0.1:8080/readyz",
		"http://user@127.0.0.1:8080/healthz",
		"http://127.0.0.1:8080/healthz?private=canary",
		"http://127.0.0.1:8080/healthz#private-canary",
	} {
		t.Run(rawURL, func(t *testing.T) {
			doer := &recordingDoer{}
			var stdout, stderr strings.Builder
			if code := runHealthcheck(context.Background(), rawURL, &stdout, &stderr, doer); code != 2 {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if doer.calls.Load() != 0 {
				t.Fatal("healthcheck issued a request for rejected URL")
			}
			if strings.Contains(stderr.String(), rawURL) || strings.Contains(stderr.String(), "private-canary") {
				t.Fatalf("unsafe healthcheck error = %q", stderr.String())
			}
		})
	}

	doer := &recordingDoer{response: &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("ok\n")),
	}}
	var stdout, stderr strings.Builder
	if code := runHealthcheck(context.Background(), "http://127.0.0.1:8080/healthz", &stdout, &stderr, doer); code != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if stdout.String() != "healthy\n" || stderr.Len() != 0 || doer.request == nil {
		t.Fatalf("stdout=%q stderr=%q request=%v", stdout.String(), stderr.String(), doer.request)
	}
	if doer.request.Method != http.MethodGet || doer.request.URL.String() != "http://127.0.0.1:8080/healthz" || len(doer.request.Header.Values("Authorization")) != 0 {
		t.Fatalf("healthcheck request = %#v headers=%v", doer.request, doer.request.Header)
	}
}

type nonClosingListener struct{ net.Listener }

func (listener *nonClosingListener) Accept() (net.Conn, error) {
	connection, err := listener.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &nonClosingConn{Conn: connection}, nil
}

type nonClosingConn struct{ net.Conn }

func (*nonClosingConn) Close() error { return nil }

type failAfterFirstAcceptListener struct {
	net.Listener
	fail     <-chan struct{}
	accepted atomic.Bool
}

func (listener *failAfterFirstAcceptListener) Accept() (net.Conn, error) {
	if listener.accepted.CompareAndSwap(false, true) {
		connection, err := listener.Listener.Accept()
		if err != nil {
			return nil, err
		}
		return &nonClosingConn{Conn: connection}, nil
	}
	<-listener.fail
	return nil, errors.New("injected listener failure")
}

type fakeSpoolResource struct {
	acquire func() (func() error, error)
	close   func() error
}

func (resource *fakeSpoolResource) AcquireLock() (func() error, error) { return resource.acquire() }
func (resource *fakeSpoolResource) Close() error                       { return resource.close() }
func (*fakeSpoolResource) collectorSpool() *collector.Spool            { return nil }

type fakeLedgerResource struct{ close func() error }

func (resource *fakeLedgerResource) Close() error { return resource.close() }
func (*fakeLedgerResource) collectorLedger() *collector.Ledger {
	return nil
}

type workerFunc func(context.Context) error

func (fn workerFunc) Run(ctx context.Context) error { return fn(ctx) }

type recordingDoer struct {
	calls    atomic.Int32
	request  *http.Request
	response *http.Response
	err      error
}

func (doer *recordingDoer) Do(request *http.Request) (*http.Response, error) {
	doer.calls.Add(1)
	doer.request = request
	return doer.response, doer.err
}

func eventIndex(events []string, want string) int {
	for index, event := range events {
		if event == want {
			return index
		}
	}
	return -1
}

type archiveMember struct {
	name     string
	contents string
}

func writeArchive(t testing.TB, members []archiveMember) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "private-archive-name-canary.tar.gz")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	gzipWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzipWriter)
	for _, member := range members {
		if err := tarWriter.WriteHeader(&tar.Header{
			Name: member.name, Mode: 0o600, Size: int64(len(member.contents)), Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write([]byte(member.contents)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}
