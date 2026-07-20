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
	"time"

	"sherpa/internal/recoveryarchive"
	"sherpa/internal/registry/content"
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
	parsed, err := url.Parse(collectorURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && !isLoopbackHTTP(parsed)) {
		return UploadResult{}, errors.New("export collector must be an HTTPS URL")
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

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, collectorURL, file)
	if err != nil {
		return UploadResult{}, errors.New("create export upload request")
	}
	// An *os.File body leaves ContentLength unset (chunked transfer), which a
	// collector that doesn't validate the manifest could store truncated. Set it
	// explicitly so the upload is a definite length.
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
	Path    string
	Base    string
	ModTime time.Time
	Size    int64
}

const staleExportPartialMaxAge = 24 * time.Hour

var runExport = Run
var uploadArchive = Upload

// ValidateSchedulerConfig checks scheduler settings without starting background
// work. This lets the registry fail startup on partial or unsafe configuration.
func ValidateSchedulerConfig(cfg SchedulerConfig) error {
	configured := strings.TrimSpace(cfg.CollectorURL) != "" || strings.TrimSpace(cfg.Token) != "" || cfg.Interval != 0 || strings.TrimSpace(cfg.ArchiveDir) != ""
	if !configured {
		return nil
	}
	if strings.TrimSpace(cfg.CollectorURL) == "" || strings.TrimSpace(cfg.Token) == "" || cfg.Interval <= 0 || strings.TrimSpace(cfg.ArchiveDir) == "" {
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

func discoverPendingArchives(archiveDir string) ([]pendingArchive, error) {
	entries, err := os.ReadDir(archiveDir)
	if err != nil {
		return nil, err
	}
	archives := make([]pendingArchive, 0, len(entries))
	for _, entry := range entries {
		if !completedArchiveName(entry.Name()) || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		archives = append(archives, pendingArchive{
			Path:    filepath.Join(archiveDir, entry.Name()),
			Base:    entry.Name(),
			ModTime: info.ModTime(),
			Size:    info.Size(),
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
	entries, err := os.ReadDir(archiveDir)
	if err != nil {
		return 0, err
	}
	cutoff := now.Add(-maxAge)
	removed := 0
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return removed, err
		}
		if !info.ModTime().Before(cutoff) {
			continue
		}
		path := filepath.Join(archiveDir, entry.Name())
		switch {
		case info.Mode().IsRegular() && generatedPartialArchiveName(entry.Name()):
			if err := os.Remove(path); err != nil {
				return removed, err
			}
			removed++
		case info.IsDir() && generatedWorkspaceName(entry.Name()):
			if err := os.RemoveAll(path); err != nil {
				return removed, err
			}
			removed++
		}
	}
	return removed, nil
}

func completedArchiveName(name string) bool {
	const prefix = "sherpa-"
	const suffix = ".tar.gz"
	return strings.HasPrefix(name, prefix) && strings.HasSuffix(name, suffix) && len(name) > len(prefix)+len(suffix)
}

func generatedPartialArchiveName(name string) bool {
	const prefix = ".sherpa-export-"
	const suffix = ".tar.gz"
	return strings.HasPrefix(name, prefix) && strings.HasSuffix(name, suffix) && len(name) > len(prefix)+len(suffix)
}

func generatedWorkspaceName(name string) bool {
	const prefix = ".sherpa-export-work-"
	return strings.HasPrefix(name, prefix) && len(name) > len(prefix)
}

func drainPendingArchives(ctx context.Context, cfg SchedulerConfig) (remaining int, err error) {
	archives, err := discoverPendingArchives(cfg.ArchiveDir)
	if err != nil {
		logSchedulerRetry(cfg, 0, "-", time.Time{}, "discover")
		return 0, errors.New("discover pending export archives")
	}
	for index, archive := range archives {
		remaining = len(archives) - index
		if err := ctx.Err(); err != nil {
			return remaining, err
		}
		info, err := os.Lstat(archive.Path)
		if err != nil {
			logSchedulerRetry(cfg, remaining, archive.Base, archive.ModTime, "queue")
			return remaining, errors.New("inspect pending export archive")
		}
		if !info.Mode().IsRegular() {
			logSchedulerRetry(cfg, remaining, archive.Base, archive.ModTime, "queue")
			return remaining, errors.New("pending export archive is not a regular file")
		}
		result, err := uploadArchive(ctx, archive.Path, cfg.CollectorURL, cfg.Token)
		if err != nil {
			if ctx.Err() != nil {
				return remaining, ctx.Err()
			}
			logSchedulerRetry(cfg, remaining, archive.Base, archive.ModTime, "upload")
			return remaining, errors.New("upload pending export archive")
		}
		if !validatedSchedulerUploadResult(result) {
			logSchedulerRetry(cfg, remaining, archive.Base, archive.ModTime, "response")
			return remaining, errors.New("unvalidated export upload result")
		}
		if err := os.Remove(archive.Path); err != nil && !os.IsNotExist(err) {
			logSchedulerRetry(cfg, remaining, archive.Base, archive.ModTime, "delete")
			return remaining, errors.New("remove uploaded export archive")
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
	if _, err := cleanupStaleExportPartials(cfg.ArchiveDir, now, staleExportPartialMaxAge); err != nil {
		logSchedulerRetry(cfg, 0, "-", time.Time{}, "cleanup")
		return errors.New("clean stale export partials")
	}
	archives, err := discoverPendingArchives(cfg.ArchiveDir)
	if err != nil {
		logSchedulerRetry(cfg, 0, "-", time.Time{}, "discover")
		return errors.New("discover pending export archives")
	}
	if len(archives) > 0 {
		_, err := drainPendingArchives(ctx, cfg)
		return err
	}
	archivePath := filepath.Join(cfg.ArchiveDir, fmt.Sprintf("sherpa-%s-%d.tar.gz", now.UTC().Format("20060102T150405Z"), now.UnixNano()))
	if err := runExport(ctx, cfg.ContentDir, cfg.DatabaseURL, archivePath); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		logSchedulerRetry(cfg, 0, filepath.Base(archivePath), now, "create")
		return errors.New("create export archive")
	}
	_, err = drainPendingArchives(ctx, cfg)
	return err
}

// StartScheduler blocks until ctx is cancelled. Empty scheduler settings disable
// it; otherwise URL, token, interval, and persistent archive directory are all required.
func StartScheduler(ctx context.Context, cfg SchedulerConfig) error {
	if err := ValidateSchedulerConfig(cfg); err != nil {
		return err
	}
	if cfg.CollectorURL == "" {
		return nil
	}
	if err := os.MkdirAll(cfg.ArchiveDir, 0o700); err != nil {
		return errors.New("prepare export archive directory")
	}
	if _, err := cleanupStaleExportPartials(cfg.ArchiveDir, time.Now(), staleExportPartialMaxAge); err != nil {
		return errors.New("clean stale export partials")
	}
	if archives, err := discoverPendingArchives(cfg.ArchiveDir); err != nil {
		return errors.New("discover pending export archives")
	} else if len(archives) > 0 {
		if _, err := drainPendingArchives(ctx, cfg); err != nil && ctx.Err() != nil {
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
			if err := runSchedulerCycle(ctx, cfg, now); err != nil && ctx.Err() != nil {
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
