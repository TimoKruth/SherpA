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
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
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
func Run(ctx context.Context, contentDir, dbURL, archivePath string) (err error) {
	if strings.TrimSpace(dbURL) == "" {
		return errors.New("database URL is required")
	}
	archivePath, err = filepath.Abs(archivePath)
	if err != nil {
		return fmt.Errorf("resolve archive path: %w", err)
	}
	parent := filepath.Dir(archivePath)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create archive directory: %w", err)
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

const staleExportPartialMaxAge = 24 * time.Hour

var runExport = Run
var beforePrepareArchiveDirectory = func(string) {}
var beforeOpenArchiveQueue = func(string) {}
var afterArchiveQueueValidated = func() {}
var beforeOpenPendingArchive = func(string) {}
var afterOpenPendingArchive = func(string) {}
var beforeClaimPendingArchive = func(string) {}
var beforePendingClaimRename = func(string) error { return nil }
var beforeClaimStaleExportEntry = func(string) {}
var uploadPendingArchive = uploadArchiveFile
var removeClaimedPendingArchive = func(queue *archiveQueue, entryBase string) error {
	return unix.Unlinkat(int(queue.directory.Fd()), entryBase, 0)
}

var queueClaimCounter atomic.Uint64

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

func prepareArchiveDirectory(path string, mode os.FileMode) error {
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		return errors.New("export archive directory must be absolute")
	}
	current, err := os.Open(string(filepath.Separator))
	if err != nil {
		return err
	}
	parts := strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator))
	for _, part := range parts {
		if part == "" {
			continue
		}
		fd, openErr := unix.Openat(
			int(current.Fd()),
			part,
			unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_DIRECTORY,
			0,
		)
		if errors.Is(openErr, unix.ENOENT) {
			if err := unix.Mkdirat(int(current.Fd()), part, uint32(mode.Perm())); err != nil && !errors.Is(err, unix.EEXIST) {
				_ = current.Close()
				return err
			}
			fd, openErr = unix.Openat(
				int(current.Fd()),
				part,
				unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_DIRECTORY,
				0,
			)
		}
		if openErr != nil {
			_ = current.Close()
			return openErr
		}
		next := os.NewFile(uintptr(fd), part)
		if err := current.Close(); err != nil {
			_ = next.Close()
			return err
		}
		current = next
	}
	return current.Close()
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

func openValidatedArchiveQueue(cfg SchedulerConfig) (*archiveQueue, error) {
	inside, err := pathWithin(cfg.ContentDir, cfg.ArchiveDir)
	if err != nil {
		return nil, errors.New("validate export archive directory")
	}
	if inside {
		return nil, errors.New("export archive directory must be outside the content directory")
	}
	queue, err := openArchiveQueue(cfg.ArchiveDir)
	if err != nil {
		return nil, err
	}
	if err := validateArchiveQueuePath(cfg, queue); err != nil {
		_ = queue.close()
		return nil, err
	}
	afterArchiveQueueValidated()
	return queue, nil
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
	pathInfo, err := os.Stat(cfg.ArchiveDir)
	if err != nil || !os.SameFile(openedInfo, pathInfo) {
		return errors.New("export archive directory changed")
	}
	return nil
}

func openArchiveQueue(path string) (*archiveQueue, error) {
	beforeOpenArchiveQueue(path)
	resolved, err := resolvePath(path)
	if err != nil {
		return nil, err
	}
	directory, err := openDirectoryNoFollow(resolved)
	if err != nil {
		return nil, err
	}
	info, err := directory.Stat()
	if err != nil || !info.IsDir() {
		_ = directory.Close()
		if err != nil {
			return nil, err
		}
		return nil, errors.New("export archive path is not a directory")
	}
	return &archiveQueue{path: path, directory: directory}, nil
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
		base, ok := pendingArchiveBase(entry.Name())
		if !ok || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		file, err := queue.openNoFollow(entry.Name())
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
			Base:      base,
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
		originalName, recoveredClaim, ok := cleanupEntryOriginalName(entry.Name())
		if !ok {
			continue
		}
		expectWorkspace := generatedWorkspaceName(originalName)
		file, err := queue.openClaimedCleanupEntry(entry.Name(), expectWorkspace)
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			continue
		}
		if err != nil {
			return removed, err
		}
		info, statErr := file.Stat()
		closeErr := file.Close()
		if statErr != nil {
			return removed, statErr
		}
		if closeErr != nil {
			return removed, closeErr
		}
		isPartial := info.Mode().IsRegular() && generatedPartialArchiveName(originalName)
		isWorkspace := info.IsDir() && expectWorkspace
		if (!isPartial && !isWorkspace) || (!recoveredClaim && !info.ModTime().Before(cutoff)) {
			continue
		}
		beforeClaimStaleExportEntry(filepath.Join(queue.path, entry.Name()))
		claimBase := fmt.Sprintf(
			".sherpa-cleanup-claim-%d-%d-%s",
			os.Getpid(),
			queueClaimCounter.Add(1),
			originalName,
		)
		if err := renameNoReplace(int(queue.directory.Fd()), entry.Name(), int(queue.directory.Fd()), claimBase); err != nil {
			return removed, err
		}
		claimedFile, err := queue.openClaimedCleanupEntry(claimBase, isWorkspace)
		if err != nil {
			_ = renameNoReplace(int(queue.directory.Fd()), claimBase, int(queue.directory.Fd()), entry.Name())
			return removed, err
		}
		claimedInfo, statErr := claimedFile.Stat()
		unchanged := statErr == nil && os.SameFile(info, claimedInfo) &&
			(recoveredClaim || claimedInfo.ModTime().Before(cutoff)) &&
			((isPartial && claimedInfo.Mode().IsRegular()) || (isWorkspace && claimedInfo.IsDir()))
		if !unchanged {
			_ = claimedFile.Close()
			_ = renameNoReplace(int(queue.directory.Fd()), claimBase, int(queue.directory.Fd()), entry.Name())
			continue
		}
		if isWorkspace {
			err = removeClaimedDirectory(queue, claimBase, claimedFile)
		} else {
			err = unix.Unlinkat(int(queue.directory.Fd()), claimBase, 0)
			closeErr := claimedFile.Close()
			if err == nil {
				err = closeErr
			}
		}
		if err != nil {
			_ = renameNoReplace(int(queue.directory.Fd()), claimBase, int(queue.directory.Fd()), entry.Name())
			return removed, err
		}
		removed++
	}
	return removed, nil
}

func (queue *archiveQueue) openClaimedCleanupEntry(entryBase string, directory bool) (*os.File, error) {
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

func removeClaimedDirectory(queue *archiveQueue, entryBase string, directory *os.File) error {
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
	return unix.Unlinkat(int(queue.directory.Fd()), entryBase, unix.AT_REMOVEDIR)
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

func pendingArchiveBase(name string) (string, bool) {
	if completedArchiveName(name) {
		return name, true
	}
	const prefix = ".sherpa-queue-claim-"
	claim, ok := strings.CutPrefix(name, prefix)
	if !ok {
		return "", false
	}
	firstDash := strings.IndexByte(claim, '-')
	if firstDash <= 0 {
		return "", false
	}
	if !canonicalPositiveUint(claim[:firstDash]) {
		return "", false
	}
	claim = claim[firstDash+1:]
	secondDash := strings.Index(claim, "-sherpa-")
	if secondDash <= 0 {
		return "", false
	}
	if !canonicalPositiveUint(claim[:secondDash]) {
		return "", false
	}
	base := claim[secondDash+1:]
	return base, completedArchiveName(base)
}

func canonicalPositiveUint(value string) bool {
	parsed, err := strconv.ParseUint(value, 10, 64)
	return err == nil && parsed > 0 && strconv.FormatUint(parsed, 10) == value
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

func cleanupEntryOriginalName(name string) (originalName string, recoveredClaim bool, ok bool) {
	if generatedPartialArchiveName(name) || generatedWorkspaceName(name) {
		return name, false, true
	}
	const prefix = ".sherpa-cleanup-claim-"
	claim, ok := strings.CutPrefix(name, prefix)
	if !ok {
		return "", false, false
	}
	firstDash := strings.IndexByte(claim, '-')
	if firstDash <= 0 || !canonicalPositiveUint(claim[:firstDash]) {
		return "", false, false
	}
	claim = claim[firstDash+1:]
	secondDash := strings.IndexByte(claim, '-')
	if secondDash <= 0 || !canonicalPositiveUint(claim[:secondDash]) {
		return "", false, false
	}
	originalName = claim[secondDash+1:]
	ok = generatedPartialArchiveName(originalName) || generatedWorkspaceName(originalName)
	return originalName, true, ok
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

func (queue *archiveQueue) claimPendingArchive(archive pendingArchive, openedInfo os.FileInfo) (string, *os.File, error) {
	beforeClaimPendingArchive(archive.Path)
	var claimBase string
	for {
		claimBase = fmt.Sprintf(
			".sherpa-queue-claim-%d-%d-%s",
			os.Getpid(),
			queueClaimCounter.Add(1),
			archive.Base,
		)
		var stat unix.Stat_t
		err := unix.Fstatat(int(queue.directory.Fd()), claimBase, &stat, unix.AT_SYMLINK_NOFOLLOW)
		if errors.Is(err, unix.ENOENT) {
			break
		}
		if err != nil {
			return "", nil, err
		}
	}
	if err := beforePendingClaimRename(filepath.Join(queue.path, claimBase)); err != nil {
		return "", nil, err
	}
	if err := renameNoReplace(int(queue.directory.Fd()), archive.EntryBase, int(queue.directory.Fd()), claimBase); err != nil {
		return "", nil, err
	}
	claimedFile, err := queue.openNoFollow(claimBase)
	if err != nil {
		_ = renameNoReplace(int(queue.directory.Fd()), claimBase, int(queue.directory.Fd()), archive.EntryBase)
		return "", nil, err
	}
	claimedInfo, err := claimedFile.Stat()
	if err != nil || !claimedInfo.Mode().IsRegular() || !os.SameFile(openedInfo, claimedInfo) {
		_ = claimedFile.Close()
		_ = renameNoReplace(int(queue.directory.Fd()), claimBase, int(queue.directory.Fd()), archive.EntryBase)
		return "", nil, errors.New("pending export archive changed")
	}
	return claimBase, claimedFile, nil
}

func drainPendingArchives(ctx context.Context, cfg SchedulerConfig) (remaining int, err error) {
	queue, err := openArchiveQueue(cfg.ArchiveDir)
	if err != nil {
		logSchedulerRetry(cfg, 0, "-", time.Time{}, "discover")
		return 0, errors.New("discover pending export archives")
	}
	defer queue.close()
	return drainPendingArchivesIn(ctx, cfg, queue)
}

func drainPendingArchivesIn(ctx context.Context, cfg SchedulerConfig, queue *archiveQueue) (remaining int, err error) {
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
		beforeOpenPendingArchive(archive.Path)
		file, err := queue.openNoFollow(archive.EntryBase)
		if err != nil {
			logSchedulerRetry(cfg, remaining, archive.Base, archive.ModTime, "queue")
			return remaining, errors.New("pending export archive changed")
		}
		afterOpenPendingArchive(archive.Path)
		openedInfo, err := file.Stat()
		if err != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(archive.info, openedInfo) {
			_ = file.Close()
			logSchedulerRetry(cfg, remaining, archive.Base, archive.ModTime, "queue")
			return remaining, errors.New("pending export archive changed")
		}
		result, uploadErr := uploadPendingArchive(ctx, file, openedInfo, cfg.CollectorURL, cfg.Token)
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
		claimBase, claimedFile, claimErr := queue.claimPendingArchive(archive, openedInfo)
		if claimErr != nil {
			_ = file.Close()
			logSchedulerRetry(cfg, remaining, archive.Base, archive.ModTime, "queue")
			return remaining, errors.New("pending export archive changed")
		}
		removeErr := removeClaimedPendingArchive(queue, claimBase)
		claimedCloseErr := claimedFile.Close()
		closeErr := file.Close()
		if removeErr != nil {
			logSchedulerRetry(cfg, remaining, archive.Base, archive.ModTime, "delete")
			return remaining, errors.New("remove uploaded export archive")
		}
		if claimedCloseErr != nil || closeErr != nil {
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
	queue, err := openValidatedArchiveQueue(cfg)
	if err != nil {
		return err
	}
	defer queue.close()
	return runSchedulerCycleIn(ctx, cfg, now, queue)
}

func runSchedulerCycleIn(ctx context.Context, cfg SchedulerConfig, now time.Time, queue *archiveQueue) error {
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
		_, err := drainPendingArchivesIn(ctx, cfg, queue)
		return err
	}
	if err := validateArchiveQueuePath(cfg, queue); err != nil {
		return err
	}
	archivePath := filepath.Join(cfg.ArchiveDir, completedArchiveBase(now))
	if err := runExport(ctx, cfg.ContentDir, cfg.DatabaseURL, archivePath); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		logSchedulerRetry(cfg, 0, filepath.Base(archivePath), now, "create")
		return errors.New("create export archive")
	}
	_, err = drainPendingArchivesIn(ctx, cfg, queue)
	return err
}

// StartScheduler blocks until ctx is cancelled. Empty scheduler settings disable
// it; otherwise URL, token, interval, and persistent archive directory are all required.
func StartScheduler(ctx context.Context, cfg SchedulerConfig) error {
	cfg = normalizeSchedulerConfig(cfg)
	if err := validateNormalizedSchedulerConfig(cfg); err != nil {
		return err
	}
	if cfg.CollectorURL == "" {
		return nil
	}
	preparedPath, err := resolvePath(cfg.ArchiveDir)
	if err != nil {
		return errors.New("prepare export archive directory")
	}
	beforePrepareArchiveDirectory(cfg.ArchiveDir)
	if err := prepareArchiveDirectory(preparedPath, 0o700); err != nil {
		return errors.New("prepare export archive directory")
	}
	queue, err := openValidatedArchiveQueue(cfg)
	if err != nil {
		return err
	}
	defer queue.close()
	if _, err := cleanupStaleExportPartialsIn(queue, time.Now(), staleExportPartialMaxAge); err != nil {
		return errors.New("clean stale export partials")
	}
	if archives, err := discoverPendingArchivesIn(queue); err != nil {
		return errors.New("discover pending export archives")
	} else if len(archives) > 0 {
		if _, err := drainPendingArchivesIn(ctx, cfg, queue); err != nil && ctx.Err() != nil {
			return nil
		}
	}

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-ticker.C:
			if err := runSchedulerCycleIn(ctx, cfg, now, queue); err != nil && ctx.Err() != nil {
				return nil
			}
		}
	}
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
