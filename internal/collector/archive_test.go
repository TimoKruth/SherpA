package collector

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"sherpa/internal/recoveryarchive"
)

func TestIngestValidatesBeforeEncryption(t *testing.T) {
	fixture := newServiceFixture(t)
	encrypted := false
	fixture.service.ops.validateFile = func(context.Context, string, recoveryarchive.Limits) (recoveryarchive.Report, error) {
		return recoveryarchive.Report{}, errors.New("member secret")
	}
	fixture.service.ops.encryptFile = func(context.Context, string, string) (int64, error) {
		encrypted = true
		return 0, nil
	}

	_, err := fixture.service.Ingest(context.Background(), testReadCloser([]byte("invalid")), 7)
	if err == nil || err.Error() != "collector archive invalid" {
		t.Fatalf("Ingest error = %v", err)
	}
	if encrypted {
		t.Fatal("encryption ran before successful validation")
	}
}

func TestIngestDerivesObjectIDFromCompleteCompressedArchive(t *testing.T) {
	fixture := newServiceFixture(t)
	archive := validRecoveryArchive(t, "digest-canary")
	want := "sha256:" + digestHex(archive)

	result, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive)))
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if result.ObjectID != want || result.Status != ResultStored {
		t.Fatalf("result = %#v, want stored %q", result, want)
	}
	if !fixture.backend.has(want) {
		t.Fatal("backend does not contain exact object")
	}
}

func TestIngestRemovesPlaintextOnlyAfterDurableEncryptedRename(t *testing.T) {
	archive := validRecoveryArchive(t, "rename-order")
	for _, test := range []struct {
		name  string
		point serviceCrashPoint
		want  []string
	}{
		{"age partial", crashAfterAgePartial, []string{".plain.pending", ".age.partial"}},
		{"age rename", crashAfterAgeRename, []string{".plain.pending", ".age"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newServiceFixture(t)
			fixture.service.ops.crash = crashAt(test.point)
			_, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive)))
			if err == nil || err.Error() != "collector operation interrupted" {
				t.Fatalf("Ingest error = %v", err)
			}
			names := spoolEntryNames(t, fixture.spool.path)
			for _, suffix := range test.want {
				if !hasSuffix(names, suffix) {
					t.Fatalf("spool entries %v do not contain %q", names, suffix)
				}
			}
		})
	}
}

func TestIngestBackendFailureLeavesOnlyEncryptedRetryObject(t *testing.T) {
	fixture := newServiceFixture(t)
	fixture.backend.existsErr = errors.New("repository-private-canary")
	archive := validRecoveryArchive(t, "backend-failure")

	_, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive)))
	if err == nil || err.Error() != "collector backend unavailable" {
		t.Fatalf("Ingest error = %v", err)
	}
	names := spoolEntryNames(t, fixture.spool.path)
	if len(names) != 1 || !strings.HasSuffix(names[0], ".tar.gz.age") {
		t.Fatalf("spool entries = %v", names)
	}
	records, listErr := fixture.ledger.List()
	if listErr != nil || len(records) != 1 || records[0].RetryCount != 1 || records[0].LatestRetryClass != "backend_failed" {
		t.Fatalf("ledger records = %#v, err=%v", records, listErr)
	}
}

func TestIngestDuplicatePendingObjectReusesDurableAgeFile(t *testing.T) {
	fixture := newServiceFixture(t)
	archive := validRecoveryArchive(t, "duplicate-pending")
	encryptCalls := 0
	encryptFile := fixture.service.ops.encryptFile
	fixture.service.ops.encryptFile = func(ctx context.Context, sourcePath, partialPath string) (int64, error) {
		encryptCalls++
		return encryptFile(ctx, sourcePath, partialPath)
	}
	fixture.backend.existsErr = errors.New("offline")
	_, _ = fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive)))
	objects, err := fixture.spool.Discover()
	if err != nil || len(objects) != 1 {
		t.Fatalf("Discover: %#v, %v", objects, err)
	}
	before, err := os.Stat(objects[0].EncryptedPath)
	if err != nil {
		t.Fatal(err)
	}

	fixture.backend.existsErr = nil
	fixture.backend.set(objects[0].ObjectID, true)
	result, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive)))
	if err != nil {
		t.Fatalf("duplicate Ingest: %v", err)
	}
	if result.Status != ResultExisting {
		t.Fatalf("status = %q", result.Status)
	}
	after, err := os.Stat(objects[0].EncryptedPath)
	if !errors.Is(err, os.ErrNotExist) && (err != nil || !os.SameFile(before, after)) {
		t.Fatalf("durable object was overwritten: before=%v after=%v err=%v", before, after, err)
	}
	if fixture.backend.createCalls != 0 {
		t.Fatalf("Create calls = %d", fixture.backend.createCalls)
	}
	if encryptCalls != 1 {
		t.Fatalf("EncryptFile calls = %d, durable duplicate was re-encrypted", encryptCalls)
	}
}

func TestIngestRemotePresenceReturnsExisting(t *testing.T) {
	fixture := newServiceFixture(t)
	archive := validRecoveryArchive(t, "remote-existing")
	objectID := "sha256:" + digestHex(archive)
	fixture.backend.set(objectID, true)

	result, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive)))
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if result.Status != ResultExisting || fixture.backend.createCalls != 0 {
		t.Fatalf("result=%#v createCalls=%d", result, fixture.backend.createCalls)
	}
}

func TestIngestCreateRequiresExactRemotePresence(t *testing.T) {
	fixture := newServiceFixture(t)
	fixture.backend.createVisible = false
	archive := validRecoveryArchive(t, "missing-post-create")

	_, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive)))
	if err == nil || err.Error() != "collector remote verification failed" {
		t.Fatalf("Ingest error = %v", err)
	}
	if fixture.backend.createCalls != 1 || fixture.backend.existsCalls != 2 {
		t.Fatalf("calls: create=%d exists=%d", fixture.backend.createCalls, fixture.backend.existsCalls)
	}
	if len(spoolEntryNames(t, fixture.spool.path)) != 1 {
		t.Fatal("encrypted retry object was removed without proof")
	}
}

func TestIngestLostResponseRechecksPresenceBeforeCreate(t *testing.T) {
	fixture := newServiceFixture(t)
	archive := validRecoveryArchive(t, "lost-response")
	fixture.service.ops.crash = crashAt(crashAfterRemoteCreate)
	_, _ = fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive)))
	fixture.service.ops.crash = nil

	result, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive)))
	if err != nil {
		t.Fatalf("retry Ingest: %v", err)
	}
	if result.Status != ResultExisting || fixture.backend.createCalls != 1 {
		t.Fatalf("result=%#v createCalls=%d", result, fixture.backend.createCalls)
	}
}

func TestIngestNeverDeletesAgeFileBeforeVerifiedCommit(t *testing.T) {
	fixture := newServiceFixture(t)
	fixture.backend.createVisible = false
	archive := validRecoveryArchive(t, "retain-before-proof")
	_, _ = fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive)))
	objects, err := fixture.spool.Discover()
	if err != nil || len(objects) != 1 {
		t.Fatalf("Discover = %#v, %v", objects, err)
	}
	if _, err := os.Stat(objects[0].EncryptedPath); err != nil {
		t.Fatalf("encrypted object removed: %v", err)
	}
}

func TestIngestSerializesBorgOperationsWithWorker(t *testing.T) {
	fixture := newServiceFixture(t)
	fixture.backend.operationDelay = 5 * time.Millisecond
	first := createPendingFixture(t, fixture.spool, fixture.ledger, fixture.now.Add(-time.Hour), "worker-object")
	secondArchive := validRecoveryArchive(t, "request-object")

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = fixture.service.CommitPending(context.Background(), first)
	}()
	go func() {
		defer wg.Done()
		_, _ = fixture.service.Ingest(context.Background(), testReadCloser(secondArchive), int64(len(secondArchive)))
	}()
	wg.Wait()
	if fixture.backend.maxActive != 1 {
		t.Fatalf("maximum overlapping backend operations = %d", fixture.backend.maxActive)
	}
}

func TestIngestWritesStoredLedgerBeforeDeletingAge(t *testing.T) {
	fixture := newServiceFixture(t)
	archive := validRecoveryArchive(t, "ledger-before-delete")
	checked := false
	fixture.service.ops.crash = func(point serviceCrashPoint) error {
		if point != crashAfterLedgerUpdate {
			return nil
		}
		objects, err := fixture.spool.Discover()
		if err != nil || len(objects) != 1 {
			t.Fatalf("age missing before ledger boundary: %#v %v", objects, err)
		}
		record, found, err := fixture.ledger.Get(objects[0].ObjectID)
		if err != nil || !found || record.StoredAt == nil {
			t.Fatalf("stored ledger absent before delete: %#v %v %v", record, found, err)
		}
		checked = true
		return nil
	}
	if _, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive))); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if !checked {
		t.Fatal("ledger boundary was not observed")
	}
}

func TestIngestErrorsAreFixedAndRedacted(t *testing.T) {
	fixture := newServiceFixture(t)
	secret := fixture.spool.path + "/private-member repository-secret sha256:deadbeef"
	fixture.backend.existsErr = errors.New(secret)
	archive := validRecoveryArchive(t, "redaction")
	_, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive)))
	if err == nil || err.Error() != "collector backend unavailable" || strings.Contains(err.Error(), secret) {
		t.Fatalf("unsafe error = %v", err)
	}
}

func TestIngestRequiresOwnedClosableSource(t *testing.T) {
	fixture := newServiceFixture(t)
	archive := validRecoveryArchive(t, "closable")
	_, err := fixture.service.Ingest(context.Background(), bytes.NewReader(archive), int64(len(archive)))
	if err == nil || err.Error() != "collector upload source not closable" {
		t.Fatalf("Ingest error = %v", err)
	}
}

func TestServiceCrashWindowsRestartSafely(t *testing.T) {
	points := []serviceCrashPoint{
		crashAfterUploadPartial,
		crashAfterAgePartial,
		crashAfterAgeRename,
		crashAfterPlaintextRemoval,
		crashAfterRemoteCreate,
		crashAfterRemotePresence,
		crashAfterLedgerUpdate,
		crashAfterAgeRemoval,
	}
	for _, point := range points {
		t.Run(string(point), func(t *testing.T) {
			fixture := newServiceFixture(t)
			archive := validRecoveryArchive(t, string(point))
			fixture.service.ops.crash = crashAt(point)
			_, err := fixture.service.Ingest(context.Background(), testReadCloser(archive), int64(len(archive)))
			if err == nil || err.Error() != "collector operation interrupted" {
				t.Fatalf("Ingest error = %v", err)
			}
			if point != crashAfterUploadPartial {
				for _, name := range spoolEntryNames(t, fixture.spool.path) {
					if strings.HasSuffix(name, ".partial") {
						old := time.Unix(1, 0)
						if err := os.Chtimes(filepath.Join(fixture.spool.path, name), old, old); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			spoolPath, ledgerPath := fixture.spool.path, fixture.ledger.path
			if err := fixture.spool.Close(); err != nil {
				t.Fatal(err)
			}
			if err := fixture.ledger.Close(); err != nil {
				t.Fatal(err)
			}
			restartedSpool, err := OpenSpool(spoolPath, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = restartedSpool.Close() })
			restartedLedger, err := OpenLedger(ledgerPath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = restartedLedger.Close() })
			restartedService := newService(restartedSpool, restartedLedger, fixture.service.encryptor, fixture.backend, fixture.status, recoveryarchive.DefaultLimits(), serviceOps{now: fixture.clock})
			ctx, cancel := context.WithCancel(context.Background())
			worker := newWorker(restartedSpool, restartedLedger, restartedService, fixture.status, time.Minute, workerOps{
				now: fixture.clock,
				wait: func(context.Context, time.Duration) error {
					cancel()
					return context.Canceled
				},
			})
			if runErr := worker.Run(ctx); runErr != nil {
				t.Fatalf("restart Run: %v", runErr)
			}
			if point == crashAfterUploadPartial {
				objectID := "sha256:" + digestHex(archive)
				record, found, getErr := restartedLedger.Get(objectID)
				if getErr != nil || !found || !record.ReceivedAt.Equal(fixture.now) {
					t.Fatalf("upload crash receipt record=%#v found=%v err=%v", record, found, getErr)
				}
			}
			for _, name := range spoolEntryNames(t, fixture.spool.path) {
				if strings.HasSuffix(name, ".partial") {
					t.Fatalf("stale partial survived restart: %v", spoolEntryNames(t, fixture.spool.path))
				}
			}
		})
	}
}

type serviceFixture struct {
	spool   *Spool
	ledger  *Ledger
	backend *fakeBackend
	status  *StatusTracker
	service *Service
	now     time.Time
	clock   func() time.Time
}

func newServiceFixture(t testing.TB) *serviceFixture {
	t.Helper()
	spool := openTestSpool(t, defaultSpoolOps())
	ledger := openTestLedger(t, defaultLedgerOps())
	backend := &fakeBackend{objects: make(map[string]bool), createVisible: true}
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	status := NewStatusTracker(ReadinessConfig{StartedAt: now, StartupGrace: time.Hour, MaxRecoveryAge: 24 * time.Hour}, clock)
	status.SetSpoolWritable(true)
	identity := generateTestIdentity(t)
	service := newService(spool, ledger, NewEncryptor(identity.Recipient()), backend, status, recoveryarchive.DefaultLimits(), serviceOps{
		now:          clock,
		validateFile: recoveryarchive.ValidateFile,
	})
	return &serviceFixture{spool: spool, ledger: ledger, backend: backend, status: status, service: service, now: now, clock: clock}
}

type fakeBackend struct {
	mu             sync.Mutex
	objects        map[string]bool
	existsErr      error
	createErr      error
	createVisible  bool
	operationDelay time.Duration
	onCreate       func(PendingObject)
	existsCalls    int
	createCalls    int
	listCalls      int
	active         int
	maxActive      int
	calls          []string
}

func (b *fakeBackend) Exists(ctx context.Context, objectID string) (bool, error) {
	b.enter("exists")
	defer b.leave()
	if !sleepContext(ctx, b.operationDelay) {
		return false, context.Canceled
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.existsCalls++
	if b.existsErr != nil {
		return false, b.existsErr
	}
	return b.objects[objectID], nil
}

func (b *fakeBackend) Create(ctx context.Context, object PendingObject) error {
	b.enter("create")
	defer b.leave()
	if !sleepContext(ctx, b.operationDelay) {
		return context.Canceled
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.createCalls++
	if b.createVisible {
		b.objects[object.ObjectID] = true
	}
	if b.onCreate != nil {
		b.onCreate(object)
	}
	return b.createErr
}

func (b *fakeBackend) List(ctx context.Context) ([]ArchiveInfo, error) {
	b.enter("list")
	defer b.leave()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.listCalls++
	return nil, nil
}

func (b *fakeBackend) enter(call string) {
	b.mu.Lock()
	b.active++
	if b.active > b.maxActive {
		b.maxActive = b.active
	}
	b.calls = append(b.calls, call)
	b.mu.Unlock()
}

func (b *fakeBackend) leave() {
	b.mu.Lock()
	b.active--
	b.mu.Unlock()
}

func (b *fakeBackend) set(objectID string, present bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.objects[objectID] = present
}

func (b *fakeBackend) has(objectID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.objects[objectID]
}

func sleepContext(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return ctx == nil || ctx.Err() == nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func validRecoveryArchive(t testing.TB, postgres string) []byte {
	t.Helper()
	postgresBytes := []byte(postgres)
	hash := sha256.Sum256(postgresBytes)
	manifest, err := recoveryarchive.MarshalManifest(recoveryarchive.Manifest{Artifacts: []recoveryarchive.Artifact{{
		Path: "postgres.dump", SHA256: hex.EncodeToString(hash[:]), Size: int64(len(postgresBytes)),
	}}})
	if err != nil {
		t.Fatal(err)
	}
	var compressed bytes.Buffer
	gzipWriter := gzip.NewWriter(&compressed)
	tarWriter := tar.NewWriter(gzipWriter)
	for _, member := range []struct {
		name string
		body []byte
	}{{"postgres.dump", postgresBytes}, {"manifest.json", manifest}} {
		if err := tarWriter.WriteHeader(&tar.Header{Name: member.name, Mode: 0o600, Size: int64(len(member.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write(member.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return compressed.Bytes()
}

func digestHex(contents []byte) string {
	digest := sha256.Sum256(contents)
	return hex.EncodeToString(digest[:])
}

func crashAt(want serviceCrashPoint) func(serviceCrashPoint) error {
	return func(got serviceCrashPoint) error {
		if got == want {
			return errors.New("simulated crash")
		}
		return nil
	}
}

func spoolEntryNames(t testing.TB, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Name() == spoolLockName {
			continue
		}
		names = append(names, entry.Name())
	}
	return names
}

func hasSuffix(values []string, suffix string) bool {
	for _, value := range values {
		if strings.HasSuffix(value, suffix) {
			return true
		}
	}
	return false
}

func assertCalls(t testing.TB, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
}

var _ Backend = (*fakeBackend)(nil)
var _ io.ReadCloser = testReadCloser(nil)
