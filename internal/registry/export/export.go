// Package export creates and uploads application-level disaster-recovery archives.
package export

import (
	"archive/tar"
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

	"sherpa/internal/registry/content"
)

const (
	archiveMode         = 0o600
	exportUploadTimeout = 10 * time.Minute
)

type manifest struct {
	Artifacts []artifact `json:"artifacts"`
}

type artifact struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type commandRunner func(ctx context.Context, dir, name string, args, env []string) error

var runCommand commandRunner = execCommand

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

	entries, err := artifactManifest(workspace)
	if err != nil {
		return err
	}
	manifestBytes, err := json.MarshalIndent(manifest{Artifacts: entries}, "", "  ")
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

func artifactManifest(root string) ([]artifact, error) {
	var entries []artifact
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		digest, size, err := hashFile(path)
		if err != nil {
			return err
		}
		entries = append(entries, artifact{Path: filepath.ToSlash(rel), SHA256: digest, Size: size})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("build artifact manifest: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, nil
}

func hashFile(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
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

// Upload sends a completed archive to an HTTPS collector. Plain HTTP is only
// accepted for loopback addresses so local integration tests remain practical.
func Upload(ctx context.Context, archivePath, collectorURL, token string) error {
	parsed, err := url.Parse(collectorURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && !isLoopbackHTTP(parsed)) {
		return errors.New("export collector must be an HTTPS URL")
	}
	file, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open export archive: %w", err)
	}
	defer file.Close()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, collectorURL, file)
	if err != nil {
		return errors.New("create export upload request")
	}
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
		return errors.New("export upload request failed")
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("export collector returned HTTP %d", response.StatusCode)
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

var runExport = Run
var uploadArchive = Upload

// ValidateSchedulerConfig checks scheduler settings without starting background
// work. This lets the registry fail startup on partial or unsafe configuration.
func ValidateSchedulerConfig(cfg SchedulerConfig) error {
	configured := cfg.CollectorURL != "" || cfg.Token != "" || cfg.Interval != 0 || cfg.ArchiveDir != ""
	if !configured {
		return nil
	}
	if cfg.CollectorURL == "" || cfg.Interval <= 0 {
		return errors.New("export collector URL and positive interval must be configured together")
	}
	parsed, err := url.Parse(cfg.CollectorURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && !isLoopbackHTTP(parsed)) {
		return errors.New("export collector must be an HTTPS URL")
	}
	archiveDir := cfg.ArchiveDir
	if archiveDir == "" {
		archiveDir = os.TempDir()
	}
	inside, err := pathWithin(cfg.ContentDir, archiveDir)
	if err != nil {
		return fmt.Errorf("validate export archive directory: %w", err)
	}
	if inside {
		return errors.New("export archive directory must be outside the content directory")
	}
	return nil
}

// StartScheduler blocks until ctx is cancelled. Empty collector and interval
// settings disable it; setting only one is a configuration error.
func StartScheduler(ctx context.Context, cfg SchedulerConfig) error {
	if err := ValidateSchedulerConfig(cfg); err != nil {
		return err
	}
	if cfg.CollectorURL == "" {
		return nil
	}
	archiveDir := cfg.ArchiveDir
	if archiveDir == "" {
		archiveDir = os.TempDir()
	}
	logger := cfg.Logger
	if logger == nil {
		logger = log.Default()
	}

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	var pendingArchive string
	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-ticker.C:
			if pendingArchive == "" {
				pendingArchive = filepath.Join(archiveDir, fmt.Sprintf("sherpa-%s-%d.tar.gz", now.UTC().Format("20060102T150405Z"), now.UnixNano()))
				if err := runExport(ctx, cfg.ContentDir, cfg.DatabaseURL, pendingArchive); err != nil {
					pendingArchive = ""
					if ctx.Err() != nil {
						return nil
					}
					logger.Printf("off-site export creation failed: %v", err)
					continue
				}
			}
			if err := uploadArchive(ctx, pendingArchive, cfg.CollectorURL, cfg.Token); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				logger.Printf("off-site export upload failed: %v", err)
				continue
			}
			if err := os.Remove(pendingArchive); err != nil {
				logger.Printf("remove uploaded export archive: %v", err)
				continue
			}
			pendingArchive = ""
		}
	}
}

func pathWithin(parent, child string) (bool, error) {
	parent, err := filepath.Abs(parent)
	if err != nil {
		return false, err
	}
	child, err = filepath.Abs(child)
	if err != nil {
		return false, err
	}
	relative, err := filepath.Rel(parent, child)
	if err != nil {
		return false, err
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))), nil
}
