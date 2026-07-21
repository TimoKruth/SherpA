package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"sherpa/internal/collector"
	"sherpa/internal/recoveryarchive"
)

const (
	collectorUsage = "usage: collector [serve | healthcheck <http://loopback/healthz> | verify <archive-path>]"
	verifyUsage    = "usage: collector verify <archive-path>"

	collectorReadHeaderTimeout = 5 * time.Second
	collectorReadTimeout       = 10 * time.Minute
	collectorWriteTimeout      = 11 * time.Minute
	collectorIdleTimeout       = 2 * time.Minute
	collectorShutdownTimeout   = 30 * time.Second
	collectorHealthTimeout     = 3 * time.Second
	collectorMaxHeaderBytes    = 1 << 20
	collectorHealthBodyLimit   = 4 << 10
)

type Config = collector.Config

type spoolResource interface {
	AcquireLock() (func() error, error)
	Close() error
	collectorSpool() *collector.Spool
}

type ledgerResource interface {
	Close() error
	collectorLedger() *collector.Ledger
}

type workerRunner interface {
	Run(context.Context) error
}

type observedContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func newObservedContext(ctx context.Context) *observedContext {
	return &observedContext{Context: ctx, entered: make(chan struct{})}
}

func (ctx *observedContext) observe() { ctx.once.Do(func() { close(ctx.entered) }) }

func (ctx *observedContext) Deadline() (time.Time, bool) {
	ctx.observe()
	return ctx.Context.Deadline()
}
func (ctx *observedContext) Done() <-chan struct{} {
	ctx.observe()
	return ctx.Context.Done()
}
func (ctx *observedContext) Err() error {
	ctx.observe()
	return ctx.Context.Err()
}
func (ctx *observedContext) Value(key any) any {
	ctx.observe()
	return ctx.Context.Value(key)
}

type concreteSpoolResource struct{ spool *collector.Spool }

func (resource *concreteSpoolResource) AcquireLock() (func() error, error) {
	return resource.spool.AcquireLock()
}
func (resource *concreteSpoolResource) Close() error { return resource.spool.Close() }
func (resource *concreteSpoolResource) collectorSpool() *collector.Spool {
	return resource.spool
}

type concreteLedgerResource struct{ ledger *collector.Ledger }

func (resource *concreteLedgerResource) Close() error { return resource.ledger.Close() }
func (resource *concreteLedgerResource) collectorLedger() *collector.Ledger {
	return resource.ledger
}

type collectorRunOps struct {
	loadConfig func(func(string) string) (Config, error)
	openSpool  func(string, time.Duration) (spoolResource, error)
	openLedger func(string) (ledgerResource, error)
	build      func(Config, spoolResource, ledgerResource, time.Time, *log.Logger) (http.Handler, workerRunner, error)
	listen     func(string, string) (net.Listener, error)
	now        func() time.Time
}

func defaultCollectorRunOps() collectorRunOps {
	return collectorRunOps{
		loadConfig: collector.LoadConfig,
		openSpool: func(path string, age time.Duration) (spoolResource, error) {
			spool, err := collector.OpenSpool(path, age)
			if err != nil {
				return nil, err
			}
			return &concreteSpoolResource{spool: spool}, nil
		},
		openLedger: func(path string) (ledgerResource, error) {
			ledger, err := collector.OpenLedger(path)
			if err != nil {
				return nil, err
			}
			return &concreteLedgerResource{ledger: ledger}, nil
		},
		build:  buildCollectorRuntime,
		listen: net.Listen,
		now:    time.Now,
	}
}

func buildCollectorRuntime(config Config, spoolResource spoolResource, ledgerResource ledgerResource, startedAt time.Time, logger *log.Logger) (http.Handler, workerRunner, error) {
	spool := spoolResource.collectorSpool()
	ledger := ledgerResource.collectorLedger()
	if spool == nil || ledger == nil || startedAt.IsZero() {
		return nil, nil, errors.New("collector runtime unavailable")
	}
	backend, err := collector.NewBorgBackend(config.Borg)
	if err != nil {
		return nil, nil, errors.New("collector backend unavailable")
	}
	status := collector.NewStatusTracker(collector.ReadinessConfig{
		StartedAt: startedAt, StartupGrace: config.StartupGrace, MaxRecoveryAge: config.MaxRecoveryAge,
	}, time.Now)
	encryptor := collector.NewEncryptor(config.AgeRecipient)
	service := collector.NewService(spool, ledger, encryptor, backend, status, config.Limits)
	worker := collector.NewWorker(spool, ledger, service, status, config.RetryInterval)
	handler := collector.NewHandler(
		collector.HTTPConfig{TokenDigest: config.TokenDigest, MaxBytes: config.Limits.MaxCompressedBytes},
		service, status, time.Now, config.StartupGrace, config.MaxRecoveryAge, logger,
	)
	return handler, worker, nil
}

type trackedHandler struct {
	handler http.Handler
	active  sync.WaitGroup
}

func (tracker *trackedHandler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	tracker.active.Add(1)
	defer tracker.active.Done()
	tracker.handler.ServeHTTP(response, request)
}

func newCollectorServer(address string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              address,
		Handler:           handler,
		ReadHeaderTimeout: collectorReadHeaderTimeout,
		ReadTimeout:       collectorReadTimeout,
		WriteTimeout:      collectorWriteTimeout,
		IdleTimeout:       collectorIdleTimeout,
		MaxHeaderBytes:    collectorMaxHeaderBytes,
	}
}

func runCollector(ctx context.Context, getenv func(string) string, logger *log.Logger) error {
	return runCollectorWithOps(ctx, getenv, logger, defaultCollectorRunOps())
}

func runCollectorWithOps(ctx context.Context, getenv func(string) string, logger *log.Logger, ops collectorRunOps) error {
	if ctx == nil || getenv == nil || logger == nil || ops.loadConfig == nil {
		return errors.New("collector runtime unavailable")
	}
	config, err := ops.loadConfig(getenv)
	if err != nil {
		return errors.New("collector configuration invalid")
	}
	if ops.openSpool == nil || ops.openLedger == nil || ops.build == nil || ops.listen == nil || ops.now == nil {
		return errors.New("collector runtime unavailable")
	}

	spool, err := ops.openSpool(config.SpoolDir, config.PartialMaxAge)
	if err != nil || spool == nil {
		return errors.New("collector spool unavailable")
	}
	var cleanupOnce sync.Once
	var ledger ledgerResource
	var releaseLock func() error
	cleanupStorage := func() {
		cleanupOnce.Do(func() {
			if ledger != nil {
				if err := ledger.Close(); err != nil {
					logger.Print("collector cleanup failed class=ledger")
				}
			}
			if err := spool.Close(); err != nil {
				logger.Print("collector cleanup failed class=spool")
			}
			if releaseLock != nil {
				if err := releaseLock(); err != nil {
					logger.Print("collector cleanup failed class=lock")
				}
			}
		})
	}

	releaseLock, err = spool.AcquireLock()
	if err != nil || releaseLock == nil {
		cleanupStorage()
		return errors.New("collector spool lock unavailable")
	}
	ledger, err = ops.openLedger(config.StateDir)
	if err != nil || ledger == nil {
		cleanupStorage()
		return errors.New("collector ledger unavailable")
	}
	startedAt := ops.now()
	handler, worker, err := ops.build(config, spool, ledger, startedAt, logger)
	if err != nil || handler == nil || worker == nil {
		cleanupStorage()
		return errors.New("collector runtime unavailable")
	}
	tracker := &trackedHandler{handler: handler}
	server := newCollectorServer(config.ListenAddr, tracker)

	workerContext, cancelWorker := context.WithCancel(ctx)
	observedWorkerContext := newObservedContext(workerContext)
	workerDone := make(chan error, 1)
	go func() {
		workerDone <- worker.Run(observedWorkerContext)
	}()
	select {
	case <-observedWorkerContext.entered:
	case workerErr := <-workerDone:
		cancelWorker()
		cleanupStorage()
		if workerErr == nil {
			return errors.New("collector worker stopped unexpectedly")
		}
		return errors.New("collector worker failed")
	}

	listener, err := ops.listen("tcp", config.ListenAddr)
	if err != nil {
		cancelWorker()
		<-workerDone
		cleanupStorage()
		return errors.New("collector listener unavailable")
	}
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(listener) }()

	var runErr error
	workerFinished := false
	serverFinished := false
	select {
	case <-ctx.Done():
	case workerErr := <-workerDone:
		workerFinished = true
		if ctx.Err() != nil && workerErr == nil {
			runErr = nil
		} else if workerErr == nil {
			runErr = errors.New("collector worker stopped unexpectedly")
		} else {
			runErr = errors.New("collector worker failed")
		}
	case serveErr := <-serverDone:
		serverFinished = true
		if !errors.Is(serveErr, http.ErrServerClosed) {
			runErr = errors.New("collector HTTP server failed")
		}
	}

	if !serverFinished {
		shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), collectorShutdownTimeout)
		shutdownErr := server.Shutdown(shutdownContext)
		cancelShutdown()
		if shutdownErr != nil {
			_ = server.Close()
			if runErr == nil {
				runErr = errors.New("collector HTTP shutdown failed")
			}
		}
		serveErr := <-serverDone
		if !errors.Is(serveErr, http.ErrServerClosed) && runErr == nil {
			runErr = errors.New("collector HTTP server failed")
		}
	}
	tracker.active.Wait()
	cancelWorker()
	if !workerFinished {
		workerErr := <-workerDone
		if workerErr != nil && runErr == nil && ctx.Err() == nil {
			runErr = errors.New("collector worker failed")
		}
	}
	cleanupStorage()
	return runErr
}

type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

func defaultHealthClient() *http.Client {
	return &http.Client{
		Timeout: collectorHealthTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func runHealthcheck(ctx context.Context, rawURL string, stdout, stderr io.Writer, client httpDoer) int {
	if ctx == nil || stdout == nil || stderr == nil || client == nil || !validHealthcheckURL(rawURL) {
		fmt.Fprintln(stderr, "invalid healthcheck target")
		return 2
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		fmt.Fprintln(stderr, "invalid healthcheck target")
		return 2
	}
	response, err := client.Do(request)
	if err != nil {
		fmt.Fprintln(stderr, "unhealthy")
		return 1
	}
	defer response.Body.Close()
	_, drainErr := io.Copy(io.Discard, io.LimitReader(response.Body, collectorHealthBodyLimit+1))
	if drainErr != nil || response.StatusCode != http.StatusOK {
		fmt.Fprintln(stderr, "unhealthy")
		return 1
	}
	fmt.Fprintln(stdout, "healthy")
	return 0
}

func validHealthcheckURL(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" || parsed.Path != "/healthz" {
		return false
	}
	if parsed.Opaque != "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || strings.Contains(rawURL, "#") {
		return false
	}
	host := parsed.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	handled, code := dispatch(ctx, os.Args[1:], os.Stdout, os.Stderr)
	if !handled {
		fmt.Fprintln(os.Stderr, collectorUsage)
		code = 2
	}
	os.Exit(code)
}

func dispatch(ctx context.Context, args []string, stdout, stderr io.Writer) (bool, int) {
	if len(args) == 0 {
		if err := runCollector(ctx, os.Getenv, log.Default()); err != nil {
			fmt.Fprintln(stderr, "collector serve failed")
			return true, 1
		}
		return true, 0
	}
	switch args[0] {
	case "serve":
		if len(args) != 1 {
			fmt.Fprintln(stderr, "usage: collector serve")
			return true, 2
		}
		if err := runCollector(ctx, os.Getenv, log.Default()); err != nil {
			fmt.Fprintln(stderr, "collector serve failed")
			return true, 1
		}
		return true, 0
	case "healthcheck":
		if len(args) != 2 {
			fmt.Fprintln(stderr, "usage: collector healthcheck <http://loopback/healthz>")
			return true, 2
		}
		return true, runHealthcheck(ctx, args[1], stdout, stderr, defaultHealthClient())
	case "verify":
		if len(args) != 2 {
			fmt.Fprintln(stderr, verifyUsage)
			return true, 2
		}
		return true, runVerify(ctx, args[1], stdout, stderr)
	default:
		return false, 0
	}
}

func runVerify(ctx context.Context, archivePath string, stdout, stderr io.Writer) int {
	report, err := recoveryarchive.ValidateFile(ctx, archivePath, recoveryarchive.DefaultLimits())
	if err != nil {
		classification := recoveryarchive.Classify(err)
		if classification == "" {
			classification = "archive_validation_failed"
		}
		fmt.Fprintln(stderr, "invalid")
		fmt.Fprintf(stderr, "classification=%s\n", classification)
		return 1
	}
	fmt.Fprintln(stdout, "valid")
	fmt.Fprintf(stdout, "artifacts=%d\n", report.ArtifactCount)
	fmt.Fprintf(stdout, "verified_bytes=%d\n", report.VerifiedBytes)
	fmt.Fprintf(stdout, "manifest_final=%t\n", report.ManifestFinal)
	return 0
}
