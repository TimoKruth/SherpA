// Package export creates and uploads application-level disaster-recovery archives.
package export

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"sherpa/internal/recoveryarchive"
	"sherpa/internal/registry/content"

	"golang.org/x/sys/unix"
)

const (
	archiveMode                  = 0o600
	exportUploadTimeout          = 10 * time.Minute
	maxCollectorResponseBodySize = 64 << 10
)

type commandRunner func(ctx context.Context, dir, name string, args, env []string) error

type archiveValidator func(context.Context, string, recoveryarchive.Limits) (recoveryarchive.Report, error)

var (
	runCommand              commandRunner    = execCommand
	validateRecoveryArchive archiveValidator = recoveryarchive.ValidateFile
)

func execCommand(ctx context.Context, dir, name string, args, env []string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = commandEnvironment(env)
	_, err := cmd.CombinedOutput()
	if err != nil {
		// Command output can contain connection details. Keep operational errors secret-free.
		return fmt.Errorf("%s failed: %w", filepath.Base(name), err)
	}
	return nil
}

func commandEnvironment(overrides []string) []string {
	overridden := make(map[string]struct{}, len(overrides))
	for _, value := range overrides {
		key, _, _ := strings.Cut(value, "=")
		overridden[key] = struct{}{}
	}
	environment := make([]string, 0, len(os.Environ())+len(overrides))
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		if _, exists := overridden[key]; !exists {
			environment = append(environment, value)
		}
	}
	return append(environment, overrides...)
}

// Run creates an atomic archive at archivePath. The database is dumped before
// repository enumeration so metadata cannot refer to a publish omitted from the
// repository portion merely because it completed during the dump.
func Run(ctx context.Context, contentDir, dbURL, archivePath string) error {
	return run(ctx, contentDir, dbURL, archivePath, true)
}

func runScheduledExport(ctx context.Context, contentDir, dbURL, archivePath string) error {
	return run(ctx, contentDir, dbURL, archivePath, false)
}

func run(ctx context.Context, contentDir, dbURL, archivePath string, createParent bool) (err error) {
	if strings.TrimSpace(dbURL) == "" {
		return errors.New("database URL is required")
	}
	archivePath, err = filepath.Abs(archivePath)
	if err != nil {
		return fmt.Errorf("resolve archive path: %w", err)
	}
	parent := filepath.Dir(archivePath)
	if createParent {
		if err := os.MkdirAll(parent, 0o755); err != nil {
			return fmt.Errorf("create archive directory: %w", err)
		}
	}

	workspace, err := os.MkdirTemp(parent, ".sherpa-export-work-*")
	if err != nil {
		return fmt.Errorf("create export workspace: %w", err)
	}
	defer os.RemoveAll(workspace)

	tempArchive, err := os.CreateTemp(parent, ".sherpa-export-*.tar.gz")
	if err != nil {
		return fmt.Errorf("create temporary archive: %w", err)
	}
	tempName := tempArchive.Name()
	defer func() {
		tempArchive.Close()
		if err != nil {
			_ = os.Remove(tempName)
		}
	}()
	if err := tempArchive.Chmod(archiveMode); err != nil {
		return fmt.Errorf("secure temporary archive: %w", err)
	}

	dumpPath := filepath.Join(workspace, "postgres.dump")
	databaseEnv, err := databaseEnvironment(dbURL)
	if err != nil {
		return err
	}
	if err := runCommand(ctx, workspace, "pg_dump", []string{"--format=custom", "--file", dumpPath}, databaseEnv); err != nil {
		return fmt.Errorf("dump database: %w", err)
	}
	if err := requireRegularFile(dumpPath); err != nil {
		return fmt.Errorf("validate database dump: %w", err)
	}

	store := content.NewBareGit(contentDir)
	repositories, err := store.ListRepositories()
	if err != nil {
		return fmt.Errorf("list repositories: %w", err)
	}
	for _, repository := range repositories {
		rel := filepath.Join("repos", repository.Owner, repository.Name+".bundle")
		destination := filepath.Join(workspace, rel)
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return fmt.Errorf("create bundle directory: %w", err)
		}
		if err := runCommand(ctx, workspace, "git", []string{"-C", store.RepoPath(repository.Owner, repository.Name), "bundle", "create", destination, "--all"}, nil); err != nil {
			return fmt.Errorf("bundle repository %s/%s: %w", repository.Owner, repository.Name, err)
		}
		if err := requireRegularFile(destination); err != nil {
			return fmt.Errorf("validate repository bundle %s/%s: %w", repository.Owner, repository.Name, err)
		}
	}

	archiveManifest, err := recoveryarchive.BuildManifest(workspace)
	if err != nil {
		return err
	}
	manifestBytes, err := json.MarshalIndent(archiveManifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	manifestBytes = append(manifestBytes, '\n')
	if err := os.WriteFile(filepath.Join(workspace, "manifest.json"), manifestBytes, archiveMode); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}

	if err := writeArchive(ctx, tempArchive, workspace); err != nil {
		return err
	}
	if err := tempArchive.Sync(); err != nil {
		return fmt.Errorf("sync archive: %w", err)
	}
	if err := tempArchive.Close(); err != nil {
		return fmt.Errorf("close archive: %w", err)
	}
	if _, err := validateRecoveryArchive(ctx, tempName, recoveryarchive.DefaultLimits()); err != nil {
		return fmt.Errorf("validate completed recovery archive: %s", recoveryarchive.Classify(err))
	}
	if err := os.Rename(tempName, archivePath); err != nil {
		return fmt.Errorf("publish archive: %w", err)
	}
	if err := syncDirectory(parent); err != nil {
		return fmt.Errorf("sync archive directory: %w", err)
	}
	return nil
}

func databaseEnvironment(databaseURL string) ([]string, error) {
	parsed, err := url.Parse(databaseURL)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
		return nil, errors.New("DATABASE_URL must be a valid postgres or postgresql URL")
	}
	if parsed.Opaque != "" || parsed.Hostname() == "" || parsed.User == nil || parsed.User.Username() == "" || parsed.Fragment != "" {
		return nil, errors.New("DATABASE_URL must include a host, user, and database")
	}
	database := strings.TrimPrefix(parsed.Path, "/")
	if database == "" || strings.Contains(database, "/") {
		return nil, errors.New("DATABASE_URL must include one database name")
	}
	port := parsed.Port()
	if port == "" {
		port = "5432"
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return nil, errors.New("DATABASE_URL has an invalid port")
	}

	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil {
		return nil, errors.New("DATABASE_URL contains invalid connection options")
	}
	for key, values := range query {
		if key != "sslmode" || len(values) != 1 || values[0] == "" {
			return nil, errors.New("DATABASE_URL contains unsupported connection options")
		}
	}
	sslMode := query.Get("sslmode")
	if sslMode == "" {
		sslMode = "prefer"
	}
	if !validSSLMode(sslMode) {
		return nil, errors.New("DATABASE_URL has an invalid sslmode")
	}
	password, _ := parsed.User.Password()
	return []string{
		"PGHOST=" + parsed.Hostname(),
		"PGPORT=" + port,
		"PGUSER=" + parsed.User.Username(),
		"PGPASSWORD=" + password,
		"PGDATABASE=" + database,
		"PGSSLMODE=" + sslMode,
	}, nil
}

func validSSLMode(mode string) bool {
	switch mode {
	case "disable", "allow", "prefer", "require", "verify-ca", "verify-full":
		return true
	default:
		return false
	}
}

func requireRegularFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return errors.New("artifact is empty or not a regular file")
	}
	return nil
}

func writeArchive(ctx context.Context, destination io.Writer, root string) error {
	gzipWriter := gzip.NewWriter(destination)
	tarWriter := tar.NewWriter(gzipWriter)
	closeWriters := func() error {
		if err := tarWriter.Close(); err != nil {
			_ = gzipWriter.Close()
			return err
		}
		return gzipWriter.Close()
	}

	var files []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("list export artifacts: %w", err)
	}
	sort.SliceStable(files, func(i, j int) bool {
		// The manifest is deliberately the final tar member.
		if filepath.Base(files[i]) == "manifest.json" {
			return false
		}
		if filepath.Base(files[j]) == "manifest.json" {
			return true
		}
		return files[i] < files[j]
	})
	for _, path := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("stat archive artifact: %w", err)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(rel)
		header.Mode = archiveMode
		header.ModTime = time.Unix(0, 0)
		header.AccessTime = time.Time{}
		header.ChangeTime = time.Time{}
		if err := tarWriter.WriteHeader(header); err != nil {
			return fmt.Errorf("write archive header: %w", err)
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tarWriter, file)
		closeErr := file.Close()
		if copyErr != nil {
			return fmt.Errorf("write archive artifact: %w", copyErr)
		}
		if closeErr != nil {
			return closeErr
		}
	}
	if err := closeWriters(); err != nil {
		return fmt.Errorf("finalize archive: %w", err)
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

type UploadResult struct {
	ObjectID string `json:"object_id"`
	Status   string `json:"status"`
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.reader.Read(p)
	if contextErr := r.ctx.Err(); contextErr != nil {
		return n, contextErr
	}
	return n, err
}

func hashArchive(ctx context.Context, reader io.Reader) (string, error) {
	hash := sha256.New()
	if _, err := io.Copy(hash, contextReader{ctx: ctx, reader: reader}); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

// Upload sends a completed archive to an HTTPS collector. Plain HTTP is only
// accepted for loopback addresses so local integration tests remain practical.
func Upload(ctx context.Context, archivePath, collectorURL, token string) (UploadResult, error) {
	if err := validateCollectorURL(collectorURL); err != nil {
		return UploadResult{}, err
	}
	file, err := os.Open(archivePath)
	if err != nil {
		return UploadResult{}, fmt.Errorf("open export archive: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return UploadResult{}, fmt.Errorf("stat export archive: %w", err)
	}
	return uploadArchiveFile(ctx, file, info, collectorURL, token)
}

func uploadArchiveFile(ctx context.Context, file *os.File, info os.FileInfo, collectorURL, token string) (UploadResult, error) {
	if err := validateCollectorURL(collectorURL); err != nil {
		return UploadResult{}, err
	}
	objectID, err := hashArchive(ctx, file)
	if err != nil {
		if ctx.Err() != nil {
			return UploadResult{}, ctx.Err()
		}
		return UploadResult{}, errors.New("hash export archive")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return UploadResult{}, errors.New("rewind export archive")
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, collectorURL, io.NopCloser(file))
	if err != nil {
		return UploadResult{}, errors.New("create export upload request")
	}
	// The non-closing wrapper keeps descriptor ownership with the caller while
	// the explicit length prevents chunked transfer and truncated storage.
	request.ContentLength = info.Size()
	request.Header.Set("Content-Type", "application/gzip")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{
		Timeout: exportUploadTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := client.Do(request)
	if err != nil {
		return UploadResult{}, errors.New("export upload request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxCollectorResponseBodySize))
		return UploadResult{}, fmt.Errorf("export collector returned HTTP %d", response.StatusCode)
	}
	if response.ContentLength > maxCollectorResponseBodySize {
		return UploadResult{}, errors.New("export collector response is too large")
	}
	encoded, err := io.ReadAll(io.LimitReader(response.Body, maxCollectorResponseBodySize+1))
	if err != nil {
		return UploadResult{}, errors.New("read export collector response")
	}
	if len(encoded) > maxCollectorResponseBodySize {
		return UploadResult{}, errors.New("export collector response is too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var result UploadResult
	if err := decoder.Decode(&result); err != nil {
		return UploadResult{}, errors.New("export collector returned malformed JSON")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return UploadResult{}, errors.New("export collector returned malformed JSON")
	}
	validStatus := (response.StatusCode == http.StatusCreated && result.Status == "stored") ||
		(response.StatusCode == http.StatusOK && result.Status == "existing")
	if !validStatus {
		return UploadResult{}, errors.New("export collector response status mismatch")
	}
	if result.ObjectID != objectID {
		return UploadResult{}, errors.New("export collector object identity mismatch")
	}
	return result, nil
}

func validateCollectorURL(collectorURL string) error {
	parsed, err := url.Parse(collectorURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && !isLoopbackHTTP(parsed)) {
		return errors.New("export collector must be an HTTPS URL")
	}
	return nil
}

func isLoopbackHTTP(parsed *url.URL) bool {
	if parsed.Scheme != "http" {
		return false
	}
	host := parsed.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// SchedulerConfig configures periodic exports inside the volume-owning process.
type SchedulerConfig struct {
	ContentDir   string
	DatabaseURL  string
	ArchiveDir   string
	CollectorURL string
	Token        string
	Interval     time.Duration
	Logger       *log.Logger
}

type pendingArchive struct {
	Path      string
	Base      string
	EntryBase string
	ModTime   time.Time
	Size      int64
	info      os.FileInfo
}

type archiveQueue struct {
	path      string
	directory *os.File
}

type schedulerOps struct {
	runExport      func(context.Context, string, string, string) error
	upload         func(context.Context, *os.File, os.FileInfo, string, string) (UploadResult, error)
	remove         func(*archiveQueue, string) error
	syncQueue      func(*archiveQueue) error
	now            func() time.Time
	wait           func(context.Context, time.Duration) error
	jitter         func(time.Duration) time.Duration
	readLastCycle  func(*archiveQueue) (time.Time, bool, error)
	writeLastCycle func(*archiveQueue, time.Time) error
	effectiveUID   uint32
	effectiveGID   uint32
}

func defaultSchedulerOps() schedulerOps {
	return schedulerOps{
		runExport: runScheduledExport,
		upload:    uploadArchiveFile,
		remove: func(queue *archiveQueue, entryBase string) error {
			return unix.Unlinkat(int(queue.directory.Fd()), entryBase, 0)
		},
		syncQueue: func(queue *archiveQueue) error {
			return queue.directory.Sync()
		},
		now:            time.Now,
		wait:           waitFor,
		jitter:         randomJitter,
		readLastCycle:  readLastCycleMarker,
		writeLastCycle: writeLastCycleMarker,
		effectiveUID:   uint32(os.Geteuid()),
		effectiveGID:   uint32(os.Getegid()),
	}
}

// waitFor sleeps for d, returning early if ctx is cancelled.
func waitFor(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func randomJitter(window time.Duration) time.Duration {
	if window <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(window)))
}

// readLastCycleMarker reports when the scheduler last ran a cycle. A missing or
// unparseable marker reports no recorded cycle rather than failing: the worst
// consequence is one extra export, whereas refusing to start would stop backups
// entirely.
func readLastCycleMarker(queue *archiveQueue) (time.Time, bool, error) {
	file, err := queue.openNoFollow(exportLastCycleMarkerName)
	if err != nil {
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ELOOP) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return time.Time{}, false, err
	}
	if !info.Mode().IsRegular() || info.Size() > 128 {
		return time.Time{}, false, nil
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return time.Time{}, false, err
	}
	recorded, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(data)))
	if err != nil {
		return time.Time{}, false, nil
	}
	return recorded.UTC(), true, nil
}

// writeLastCycleMarker replaces the marker atomically and syncs the queue
// directory, so a crash cannot leave a truncated timestamp behind.
func writeLastCycleMarker(queue *archiveQueue, at time.Time) error {
	temp, err := os.CreateTemp(queue.path, exportLastCycleMarkerName+"-*")
	if err != nil {
		return err
	}
	tempName := filepath.Base(temp.Name())
	cleanup := func() {
		_ = temp.Close()
		_ = unix.Unlinkat(int(queue.directory.Fd()), tempName, 0)
	}
	if _, err := temp.WriteString(at.UTC().Format(time.RFC3339Nano)); err != nil {
		cleanup()
		return err
	}
	if err := temp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := temp.Close(); err != nil {
		_ = unix.Unlinkat(int(queue.directory.Fd()), tempName, 0)
		return err
	}
	if err := unix.Renameat(int(queue.directory.Fd()), tempName, int(queue.directory.Fd()), exportLastCycleMarkerName); err != nil {
		_ = unix.Unlinkat(int(queue.directory.Fd()), tempName, 0)
		return err
	}
	return queue.directory.Sync()
}

const staleExportPartialMaxAge = 24 * time.Hour

// exportLastCycleMarkerName records when the scheduler last ran a cycle, so a
// restart resumes the existing schedule instead of starting a fresh interval.
// The name deliberately avoids the ".sherpa-export-" prefix used by partial
// archives and workspaces so queue discovery and stale-partial cleanup ignore it.
const exportLastCycleMarkerName = ".sherpa-last-export"

// schedulerStartupJitter bounds the delay before an overdue cycle runs. Without
// it a crash-looping registry would export on every restart.
const schedulerStartupJitter = 30 * time.Second

var errSyncExportArchiveDirectory = errors.New("sync export archive directory")

// ValidateSchedulerConfig checks scheduler settings without starting background
// work. This lets the registry fail startup on partial or unsafe configuration.
func ValidateSchedulerConfig(cfg SchedulerConfig) error {
	return validateNormalizedSchedulerConfig(normalizeSchedulerConfig(cfg))
}

func normalizeSchedulerConfig(cfg SchedulerConfig) SchedulerConfig {
	cfg.ContentDir = strings.TrimSpace(cfg.ContentDir)
	cfg.DatabaseURL = strings.TrimSpace(cfg.DatabaseURL)
	cfg.ArchiveDir = strings.TrimSpace(cfg.ArchiveDir)
	cfg.CollectorURL = strings.TrimSpace(cfg.CollectorURL)
	cfg.Token = strings.TrimSpace(cfg.Token)
	return cfg
}

func validateNormalizedSchedulerConfig(cfg SchedulerConfig) error {
	configured := cfg.CollectorURL != "" || cfg.Token != "" || cfg.Interval != 0 || cfg.ArchiveDir != ""
	if !configured {
		return nil
	}
	if cfg.CollectorURL == "" || cfg.Token == "" || cfg.Interval <= 0 || cfg.ArchiveDir == "" {
		return errors.New("export collector URL, token, positive interval, and archive directory must be configured together")
	}
	parsed, err := url.Parse(cfg.CollectorURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && !isLoopbackHTTP(parsed)) {
		return errors.New("export collector must be an HTTPS URL")
	}
	inside, err := pathWithin(cfg.ContentDir, cfg.ArchiveDir)
	if err != nil {
		return fmt.Errorf("validate export archive directory: %w", err)
	}
	if inside {
		return errors.New("export archive directory must be outside the content directory")
	}
	return nil
}

func openDirectoryNoFollow(path string) (*os.File, error) {
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		return nil, errors.New("directory path must be absolute")
	}
	currentPath := string(filepath.Separator)
	current, err := os.Open(currentPath)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator))
	for _, part := range parts {
		if part == "" {
			continue
		}
		fd, err := unix.Openat(
			int(current.Fd()),
			part,
			unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_DIRECTORY,
			0,
		)
		if err != nil {
			_ = current.Close()
			return nil, err
		}
		currentPath = filepath.Join(currentPath, part)
		next := os.NewFile(uintptr(fd), currentPath)
		if err := current.Close(); err != nil {
			_ = next.Close()
			return nil, err
		}
		current = next
	}
	return current, nil
}

func openArchiveQueue(path string) (*archiveQueue, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	absolute = filepath.Clean(absolute)
	parentPath, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return nil, err
	}
	parent, err := openDirectoryNoFollow(parentPath)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	fd, err := unix.Openat(
		int(parent.Fd()),
		filepath.Base(absolute),
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_DIRECTORY,
		0,
	)
	if err != nil {
		return nil, err
	}
	directory := os.NewFile(uintptr(fd), absolute)
	if directory == nil {
		_ = unix.Close(fd)
		return nil, errors.New("open export archive directory")
	}
	return &archiveQueue{path: absolute, directory: directory}, nil
}

func openValidatedArchiveQueue(cfg SchedulerConfig, ops schedulerOps) (*archiveQueue, error) {
	inside, err := pathWithin(cfg.ContentDir, cfg.ArchiveDir)
	if err != nil {
		return nil, errors.New("validate export archive directory")
	}
	if inside {
		return nil, errors.New("export archive directory must be outside the content directory")
	}
	queue, err := openArchiveQueue(cfg.ArchiveDir)
	if err != nil {
		return nil, errors.New("open export archive directory")
	}
	if err := validateArchiveQueue(cfg, queue, ops); err != nil {
		_ = queue.close()
		return nil, err
	}
	if err := unix.Flock(int(queue.directory.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = queue.close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, errors.New("export archive directory is already in use")
		}
		return nil, errors.New("lock export archive directory")
	}
	return queue, nil
}

func validateArchiveQueue(cfg SchedulerConfig, queue *archiveQueue, ops schedulerOps) error {
	var stat unix.Stat_t
	if err := unix.Fstat(int(queue.directory.Fd()), &stat); err != nil {
		return errors.New("stat export archive directory")
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return errors.New("export archive path is not a directory")
	}
	if stat.Mode&0o7777 != 0o700 {
		return errors.New("export archive directory must have mode 0700")
	}
	if stat.Uid != ops.effectiveUID || stat.Gid != ops.effectiveGID {
		return errors.New("export archive directory has wrong ownership")
	}
	return validateArchiveQueuePath(cfg, queue)
}

func validateArchiveQueuePath(cfg SchedulerConfig, queue *archiveQueue) error {
	inside, err := pathWithin(cfg.ContentDir, cfg.ArchiveDir)
	if err != nil {
		return errors.New("validate export archive directory")
	}
	if inside {
		return errors.New("export archive directory must be outside the content directory")
	}
	openedInfo, err := queue.directory.Stat()
	if err != nil {
		return errors.New("stat export archive directory")
	}
	pathInfo, err := os.Lstat(cfg.ArchiveDir)
	if err != nil || pathInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(openedInfo, pathInfo) {
		return errors.New("export archive directory changed")
	}
	return nil
}

func (queue *archiveQueue) close() error {
	return queue.directory.Close()
}

func (queue *archiveQueue) readDir() ([]os.DirEntry, error) {
	if _, err := queue.directory.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return queue.directory.ReadDir(-1)
}

func (queue *archiveQueue) openNoFollow(entryBase string) (*os.File, error) {
	fd, err := unix.Openat(
		int(queue.directory.Fd()),
		entryBase,
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK,
		0,
	)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), filepath.Join(queue.path, entryBase)), nil
}

func discoverPendingArchives(archiveDir string) ([]pendingArchive, error) {
	queue, err := openArchiveQueue(archiveDir)
	if err != nil {
		return nil, err
	}
	defer queue.close()
	return discoverPendingArchivesIn(queue)
}

func discoverPendingArchivesIn(queue *archiveQueue) ([]pendingArchive, error) {
	entries, err := queue.readDir()
	if err != nil {
		return nil, err
	}
	archives := make([]pendingArchive, 0, len(entries))
	for _, entry := range entries {
		if !completedArchiveName(entry.Name()) || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		file, err := queue.openNoFollow(entry.Name())
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOENT) {
			continue
		}
		if err != nil {
			return nil, err
		}
		info, statErr := file.Stat()
		closeErr := file.Close()
		if statErr != nil {
			return nil, statErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if !info.Mode().IsRegular() {
			continue
		}
		archives = append(archives, pendingArchive{
			Path:      filepath.Join(queue.path, entry.Name()),
			Base:      entry.Name(),
			EntryBase: entry.Name(),
			ModTime:   info.ModTime(),
			Size:      info.Size(),
			info:      info,
		})
	}
	sort.Slice(archives, func(i, j int) bool {
		if archives[i].ModTime.Equal(archives[j].ModTime) {
			return archives[i].Base < archives[j].Base
		}
		return archives[i].ModTime.Before(archives[j].ModTime)
	})
	return archives, nil
}

func cleanupStaleExportPartials(archiveDir string, now time.Time, maxAge time.Duration) (int, error) {
	queue, err := openArchiveQueue(archiveDir)
	if err != nil {
		return 0, err
	}
	defer queue.close()
	return cleanupStaleExportPartialsIn(queue, now, maxAge)
}

func cleanupStaleExportPartialsIn(queue *archiveQueue, now time.Time, maxAge time.Duration) (int, error) {
	entries, err := queue.readDir()
	if err != nil {
		return 0, err
	}
	cutoff := now.Add(-maxAge)
	removed := 0
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		isPartial := generatedPartialArchiveName(entry.Name())
		isWorkspace := generatedWorkspaceName(entry.Name())
		if !isPartial && !isWorkspace {
			continue
		}
		file, err := queue.openCleanupEntry(entry.Name(), isWorkspace)
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ENOENT) {
			continue
		}
		if err != nil {
			return removed, err
		}
		info, statErr := file.Stat()
		if statErr != nil {
			_ = file.Close()
			return removed, statErr
		}
		if !info.ModTime().Before(cutoff) || (isPartial && !info.Mode().IsRegular()) || (isWorkspace && !info.IsDir()) {
			_ = file.Close()
			continue
		}
		if isWorkspace {
			err = removeDirectoryContents(queue.directory, entry.Name(), file)
		} else {
			err = unix.Unlinkat(int(queue.directory.Fd()), entry.Name(), 0)
			closeErr := file.Close()
			if err == nil {
				err = closeErr
			}
		}
		if err != nil {
			return removed, err
		}
		removed++
	}
	if removed > 0 {
		if err := queue.directory.Sync(); err != nil {
			return removed, err
		}
	}
	return removed, nil
}

func (queue *archiveQueue) openCleanupEntry(entryBase string, directory bool) (*os.File, error) {
	flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	if directory {
		flags |= unix.O_DIRECTORY
	}
	fd, err := unix.Openat(int(queue.directory.Fd()), entryBase, flags, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), filepath.Join(queue.path, entryBase)), nil
}

func removeDirectoryContents(parent *os.File, entryBase string, directory *os.File) error {
	entries, err := directory.ReadDir(-1)
	if err != nil {
		_ = directory.Close()
		return err
	}
	for _, entry := range entries {
		if entry.Type().IsDir() && entry.Type()&os.ModeSymlink == 0 {
			childFD, openErr := unix.Openat(
				int(directory.Fd()),
				entry.Name(),
				unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_DIRECTORY,
				0,
			)
			if openErr != nil {
				_ = directory.Close()
				return openErr
			}
			child := os.NewFile(uintptr(childFD), entry.Name())
			if err := removeDirectoryContents(directory, entry.Name(), child); err != nil {
				_ = directory.Close()
				return err
			}
			continue
		}
		if err := unix.Unlinkat(int(directory.Fd()), entry.Name(), 0); err != nil {
			_ = directory.Close()
			return err
		}
	}
	if err := directory.Close(); err != nil {
		return err
	}
	return unix.Unlinkat(int(parent.Fd()), entryBase, unix.AT_REMOVEDIR)
}

const completedArchiveTimestampLayout = "20060102T150405Z"

func completedArchiveBase(created time.Time) string {
	return fmt.Sprintf("sherpa-%s-%d.tar.gz", created.UTC().Format(completedArchiveTimestampLayout), created.UnixNano())
}

func completedArchiveName(name string) bool {
	const prefix = "sherpa-"
	const suffix = ".tar.gz"
	body, ok := strings.CutPrefix(name, prefix)
	if !ok {
		return false
	}
	body, ok = strings.CutSuffix(body, suffix)
	if !ok || len(body) <= len(completedArchiveTimestampLayout) || body[len(completedArchiveTimestampLayout)] != '-' {
		return false
	}
	timestampText := body[:len(completedArchiveTimestampLayout)]
	nanosecondsText := body[len(completedArchiveTimestampLayout)+1:]
	timestamp, err := time.Parse(completedArchiveTimestampLayout, timestampText)
	if err != nil || timestamp.Format(completedArchiveTimestampLayout) != timestampText {
		return false
	}
	nanoseconds, err := strconv.ParseInt(nanosecondsText, 10, 64)
	if err != nil || strconv.FormatInt(nanoseconds, 10) != nanosecondsText {
		return false
	}
	return timestamp.Equal(time.Unix(0, nanoseconds).UTC().Truncate(time.Second))
}

func generatedPartialArchiveName(name string) bool {
	return generatedTempName(name, ".sherpa-export-", ".tar.gz")
}

func generatedWorkspaceName(name string) bool {
	return generatedTempName(name, ".sherpa-export-work-", "")
}

func generatedTempName(name, prefix, suffix string) bool {
	random, ok := strings.CutPrefix(name, prefix)
	if !ok {
		return false
	}
	random, ok = strings.CutSuffix(random, suffix)
	if !ok || random == "" {
		return false
	}
	value, err := strconv.ParseUint(random, 10, 32)
	return err == nil && strconv.FormatUint(value, 10) == random
}

func drainPendingArchives(ctx context.Context, cfg SchedulerConfig) (remaining int, err error) {
	return drainPendingArchivesWithOps(ctx, cfg, defaultSchedulerOps())
}

func drainPendingArchivesWithOps(ctx context.Context, cfg SchedulerConfig, ops schedulerOps) (remaining int, err error) {
	cfg = normalizeSchedulerConfig(cfg)
	queue, err := openValidatedArchiveQueue(cfg, ops)
	if err != nil {
		logSchedulerRetry(cfg, 0, "-", time.Time{}, "discover")
		return 0, errors.New("discover pending export archives")
	}
	defer queue.close()
	return drainPendingArchivesIn(ctx, cfg, queue, ops)
}

func drainPendingArchivesIn(ctx context.Context, cfg SchedulerConfig, queue *archiveQueue, ops schedulerOps) (remaining int, err error) {
	archives, err := discoverPendingArchivesIn(queue)
	if err != nil {
		logSchedulerRetry(cfg, 0, "-", time.Time{}, "discover")
		return 0, errors.New("discover pending export archives")
	}
	for index, archive := range archives {
		remaining = len(archives) - index
		if err := ctx.Err(); err != nil {
			return remaining, err
		}
		file, err := queue.openNoFollow(archive.EntryBase)
		if err != nil {
			logSchedulerRetry(cfg, remaining, archive.Base, archive.ModTime, "queue")
			return remaining, errors.New("pending export archive changed")
		}
		openedInfo, err := file.Stat()
		if err != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(archive.info, openedInfo) {
			_ = file.Close()
			logSchedulerRetry(cfg, remaining, archive.Base, archive.ModTime, "queue")
			return remaining, errors.New("pending export archive changed")
		}
		result, uploadErr := ops.upload(ctx, file, openedInfo, cfg.CollectorURL, cfg.Token)
		if uploadErr != nil {
			_ = file.Close()
			if ctx.Err() != nil {
				return remaining, ctx.Err()
			}
			logSchedulerRetry(cfg, remaining, archive.Base, archive.ModTime, "upload")
			return remaining, errors.New("upload pending export archive")
		}
		if !validatedSchedulerUploadResult(result) {
			_ = file.Close()
			logSchedulerRetry(cfg, remaining, archive.Base, archive.ModTime, "response")
			return remaining, errors.New("unvalidated export upload result")
		}
		current, err := queue.openNoFollow(archive.EntryBase)
		if err != nil {
			_ = file.Close()
			logSchedulerRetry(cfg, remaining, archive.Base, archive.ModTime, "queue")
			return remaining, errors.New("pending export archive changed")
		}
		currentInfo, statErr := current.Stat()
		currentCloseErr := current.Close()
		if statErr != nil || currentCloseErr != nil || !currentInfo.Mode().IsRegular() || !os.SameFile(openedInfo, currentInfo) {
			_ = file.Close()
			logSchedulerRetry(cfg, remaining, archive.Base, archive.ModTime, "queue")
			return remaining, errors.New("pending export archive changed")
		}
		removeErr := ops.remove(queue, archive.EntryBase)
		if removeErr != nil {
			_ = file.Close()
			logSchedulerRetry(cfg, remaining, archive.Base, archive.ModTime, "delete")
			return remaining, errors.New("remove uploaded export archive")
		}
		if err := ops.syncQueue(queue); err != nil {
			_ = file.Close()
			logSchedulerRetry(cfg, remaining, archive.Base, archive.ModTime, "delete")
			return remaining, errSyncExportArchiveDirectory
		}
		if err := file.Close(); err != nil {
			logSchedulerRetry(cfg, remaining, archive.Base, archive.ModTime, "queue")
			return remaining, errors.New("close pending export archive")
		}
		remaining--
	}
	return remaining, nil
}

func validatedSchedulerUploadResult(result UploadResult) bool {
	if result.Status != "stored" && result.Status != "existing" {
		return false
	}
	const prefix = "sha256:"
	if !strings.HasPrefix(result.ObjectID, prefix) || len(result.ObjectID) != len(prefix)+sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(result.ObjectID, prefix))
	return err == nil
}

func runSchedulerCycle(ctx context.Context, cfg SchedulerConfig, now time.Time) error {
	ops := defaultSchedulerOps()
	cfg = normalizeSchedulerConfig(cfg)
	queue, err := openValidatedArchiveQueue(cfg, ops)
	if err != nil {
		return err
	}
	defer queue.close()
	return runSchedulerCycleIn(ctx, cfg, now, queue, ops)
}

func runSchedulerCycleIn(ctx context.Context, cfg SchedulerConfig, now time.Time, queue *archiveQueue, ops schedulerOps) error {
	if _, err := cleanupStaleExportPartialsIn(queue, now, staleExportPartialMaxAge); err != nil {
		logSchedulerRetry(cfg, 0, "-", time.Time{}, "cleanup")
		return errors.New("clean stale export partials")
	}
	archives, err := discoverPendingArchivesIn(queue)
	if err != nil {
		logSchedulerRetry(cfg, 0, "-", time.Time{}, "discover")
		return errors.New("discover pending export archives")
	}
	if len(archives) > 0 {
		_, err := drainPendingArchivesIn(ctx, cfg, queue, ops)
		return err
	}
	if err := validateArchiveQueuePath(cfg, queue); err != nil {
		return err
	}
	archivePath := filepath.Join(cfg.ArchiveDir, completedArchiveBase(now))
	if err := ops.runExport(ctx, cfg.ContentDir, cfg.DatabaseURL, archivePath); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		logSchedulerRetry(cfg, 0, filepath.Base(archivePath), now, "create")
		return errors.New("create export archive")
	}
	_, err = drainPendingArchivesIn(ctx, cfg, queue, ops)
	return err
}

// PreparedScheduler owns a validated export queue and its cooperative lifetime
// lock until Close is called. Run performs scheduler work but never releases
// that ownership.
type PreparedScheduler struct {
	cfg   SchedulerConfig
	queue *archiveQueue
	ops   schedulerOps

	mu        sync.Mutex
	started   bool
	running   bool
	closed    bool
	runDone   chan struct{}
	closeOnce sync.Once
	closeErr  error
}

// PrepareScheduler validates an enabled scheduler's physical queue and acquires
// its lifetime lock synchronously. Empty scheduler settings return a nil scheduler.
func PrepareScheduler(cfg SchedulerConfig) (*PreparedScheduler, error) {
	return prepareScheduler(cfg, defaultSchedulerOps())
}

func prepareScheduler(cfg SchedulerConfig, ops schedulerOps) (*PreparedScheduler, error) {
	cfg = normalizeSchedulerConfig(cfg)
	if err := validateNormalizedSchedulerConfig(cfg); err != nil {
		return nil, err
	}
	if cfg.CollectorURL == "" {
		return nil, nil
	}
	queue, err := openValidatedArchiveQueue(cfg, ops)
	if err != nil {
		return nil, err
	}
	return &PreparedScheduler{cfg: cfg, queue: queue, ops: ops}, nil
}

// Run blocks until ctx is cancelled or scheduler work terminates. It may be
// called once and does not release the prepared queue lock; the owner must call
// Close after Run has returned.
func (scheduler *PreparedScheduler) Run(ctx context.Context) error {
	if err := scheduler.beginRun(); err != nil {
		return err
	}
	defer scheduler.finishRun()
	if _, err := cleanupStaleExportPartialsIn(scheduler.queue, scheduler.ops.now(), staleExportPartialMaxAge); err != nil {
		return errors.New("clean stale export partials")
	}
	if archives, err := discoverPendingArchivesIn(scheduler.queue); err != nil {
		return errors.New("discover pending export archives")
	} else if len(archives) > 0 {
		if _, err := drainPendingArchivesIn(ctx, scheduler.cfg, scheduler.queue, scheduler.ops); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, errSyncExportArchiveDirectory) {
				return err
			}
		}
	}

	// The schedule is anchored to the recorded cycle time rather than to process
	// start. A ticker restarts its interval on every boot, so a registry that is
	// redeployed more often than its interval never exports at all.
	lastCycle, hasCycle, err := scheduler.ops.readLastCycle(scheduler.queue)
	if err != nil {
		return errors.New("read export cycle marker")
	}
	for {
		delay := scheduler.nextDelay(lastCycle, hasCycle)
		if err := scheduler.ops.wait(ctx, delay); err != nil {
			return nil
		}
		if ctx.Err() != nil {
			return nil
		}
		now := scheduler.ops.now()
		cycleErr := runSchedulerCycleIn(ctx, scheduler.cfg, now, scheduler.queue, scheduler.ops)
		// The marker records the last attempt, not the last success, so a
		// persistently failing export retries on the normal interval instead of
		// spinning. Failure visibility is the monitoring layer's job.
		if markErr := scheduler.ops.writeLastCycle(scheduler.queue, now); markErr != nil {
			return errors.New("record export cycle marker")
		}
		lastCycle, hasCycle = now, true
		if cycleErr != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(cycleErr, errSyncExportArchiveDirectory) {
				return cycleErr
			}
		}
	}
}

// nextDelay returns how long to wait before the next cycle. An overdue or
// never-run schedule waits only a jittered moment; otherwise it waits out the
// remainder of the interval so a restart does not re-export.
func (scheduler *PreparedScheduler) nextDelay(lastCycle time.Time, hasCycle bool) time.Duration {
	if !hasCycle {
		return scheduler.ops.jitter(scheduler.jitterWindow())
	}
	due := lastCycle.Add(scheduler.cfg.Interval)
	now := scheduler.ops.now()
	if remaining := due.Sub(now); remaining > 0 {
		return remaining
	}
	return scheduler.ops.jitter(scheduler.jitterWindow())
}

// jitterWindow bounds startup jitter by the interval. A fixed window would
// dominate any schedule shorter than itself, delaying an export far beyond its
// configured interval.
func (scheduler *PreparedScheduler) jitterWindow() time.Duration {
	if scheduler.cfg.Interval < schedulerStartupJitter {
		return scheduler.cfg.Interval
	}
	return schedulerStartupJitter
}

func (scheduler *PreparedScheduler) beginRun() error {
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	if scheduler.closed {
		return errors.New("export scheduler is closed")
	}
	if scheduler.started {
		return errors.New("export scheduler has already run")
	}
	scheduler.started = true
	scheduler.running = true
	scheduler.runDone = make(chan struct{})
	return nil
}

func (scheduler *PreparedScheduler) finishRun() {
	scheduler.mu.Lock()
	done := scheduler.runDone
	scheduler.running = false
	scheduler.runDone = nil
	scheduler.mu.Unlock()
	close(done)
}

// Close waits for a running scheduler to return, then idempotently releases the
// prepared queue descriptor and cooperative lifetime lock. Close never performs
// queue discovery, cleanup, upload, or export work.
func (scheduler *PreparedScheduler) Close() error {
	for {
		scheduler.mu.Lock()
		if scheduler.running {
			done := scheduler.runDone
			scheduler.mu.Unlock()
			<-done
			continue
		}
		scheduler.closed = true
		scheduler.mu.Unlock()
		scheduler.closeOnce.Do(func() {
			scheduler.closeErr = scheduler.queue.close()
		})
		return scheduler.closeErr
	}
}

// StartScheduler prepares the scheduler synchronously, then runs it until ctx is
// cancelled. Empty scheduler settings disable it.
func StartScheduler(ctx context.Context, cfg SchedulerConfig) error {
	return startScheduler(ctx, cfg, defaultSchedulerOps())
}

func startScheduler(ctx context.Context, cfg SchedulerConfig, ops schedulerOps) error {
	scheduler, err := prepareScheduler(cfg, ops)
	if err != nil || scheduler == nil {
		return err
	}
	runErr := scheduler.Run(ctx)
	closeErr := scheduler.Close()
	if runErr != nil {
		return runErr
	}
	if closeErr != nil {
		return errors.New("close export scheduler")
	}
	return nil
}

func logSchedulerRetry(cfg SchedulerConfig, queueCount int, base string, modTime time.Time, retryClass string) {
	logger := cfg.Logger
	if logger == nil {
		logger = log.Default()
	}
	age := time.Duration(0)
	if !modTime.IsZero() {
		age = time.Since(modTime)
		if age < 0 {
			age = 0
		}
	}
	logger.Printf("off-site export retry queue_count=%d archive=%s age=%s retry_class=%s", queueCount, base, age.Round(time.Second), retryClass)
}

func pathWithin(parent, child string) (bool, error) {
	parent, err := resolvePath(parent)
	if err != nil {
		return false, err
	}
	child, err = resolvePath(child)
	if err != nil {
		return false, err
	}
	relative, err := filepath.Rel(parent, child)
	if err != nil {
		return false, err
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))), nil
}

func resolvePath(path string) (string, error) {
	current, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for index := len(missing) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, missing[index])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}
