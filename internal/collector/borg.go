package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	borgOutputLimit  = 1 << 20
	borgArchiveLimit = 10000
	borgWaitDelay    = 2 * time.Second
)

var errBorgOutputLimit = errors.New("borg output limit exceeded")

type ArchiveInfo struct {
	Name      string
	StartedAt time.Time
}

type Backend interface {
	Create(context.Context, PendingObject) error
	Exists(context.Context, string) (bool, error)
	List(context.Context) ([]ArchiveInfo, error)
}

type BorgBackend struct {
	config      BorgConfig
	environment []string
	location    *time.Location
}

func NewBorgBackend(config BorgConfig) (*BorgBackend, error) {
	return newBorgBackend(config, time.Local)
}

func newBorgBackend(config BorgConfig, location *time.Location) (*BorgBackend, error) {
	if !validBorgConfig(config) || location == nil {
		return nil, errors.New("collector backend unavailable")
	}
	workFD, workPath, err := openPrivateDirectory(config.WorkDir)
	if err != nil || workPath != config.WorkDir {
		return nil, errors.New("collector backend unavailable")
	}
	defer unix.Close(workFD)
	for _, name := range []string{"cache", "config", "security"} {
		if err := ensureBorgStateDirectory(workFD, name); err != nil {
			return nil, errors.New("collector backend unavailable")
		}
	}
	cacheDir := filepath.Join(config.WorkDir, "cache")
	configDir := filepath.Join(config.WorkDir, "config")
	securityDir := filepath.Join(config.WorkDir, "security")
	environment := append(nonBorgEnvironment(os.Environ()),
		"BORG_REPO="+config.Repository,
		"BORG_RSH="+borgSSHCommand(config.SSHKeyFile, config.KnownHostsFile),
		"BORG_CACHE_DIR="+cacheDir,
		"BORG_CONFIG_DIR="+configDir,
		"BORG_SECURITY_DIR="+securityDir,
		"BORG_UNKNOWN_UNENCRYPTED_REPO_ACCESS_IS_OK=yes",
	)
	return &BorgBackend{config: config, environment: environment, location: location}, nil
}

func (b *BorgBackend) Create(ctx context.Context, object PendingObject) (retErr error) {
	if b == nil {
		return errors.New("collector backend failed")
	}
	held, err := openBorgInput(object)
	if err != nil {
		return errors.New("collector backend failed")
	}
	defer func() {
		if held.close() != nil && retErr == nil {
			retErr = errors.New("collector backend failed")
		}
	}()

	if !held.valid() {
		return errors.New("collector backend failed")
	}
	_, createErr := b.run(ctx, b.config.CreateTimeout, held.dir, "create", "--compression", "none", "::"+held.archiveName, held.name)
	if !held.valid() {
		return errors.New("collector backend failed")
	}
	if contextErr := classifyBorgContext(ctx); contextErr != nil {
		return contextErr
	}

	archives, listErr := b.list(ctx)
	if listErr != nil {
		if contextErr := classifyBorgContext(ctx); contextErr != nil {
			return contextErr
		}
		return errors.New("collector remote verification failed")
	}
	found := false
	for _, archive := range archives {
		if archive.Name == held.archiveName {
			found = true
			break
		}
	}
	if !found {
		if createErr != nil {
			return createErr
		}
		return errors.New("collector remote verification failed")
	}
	if !held.valid() {
		return errors.New("collector backend failed")
	}
	return nil
}

func (b *BorgBackend) Exists(ctx context.Context, objectID string) (bool, error) {
	if b == nil {
		return false, errors.New("collector backend failed")
	}
	digest, ok := canonicalDigest(objectID)
	if !ok {
		return false, errors.New("collector backend failed")
	}
	archives, err := b.list(ctx)
	if err != nil {
		return false, err
	}
	want := "sherpa-" + digest
	for _, archive := range archives {
		if archive.Name == want {
			return true, nil
		}
	}
	return false, nil
}

func (b *BorgBackend) List(ctx context.Context) ([]ArchiveInfo, error) {
	if b == nil {
		return nil, errors.New("collector backend failed")
	}
	return b.list(ctx)
}

func (b *BorgBackend) list(ctx context.Context) ([]ArchiveInfo, error) {
	output, err := b.run(ctx, b.config.QueryTimeout, "", "list", "--json")
	if err != nil {
		return nil, err
	}
	archives, err := parseBorgList(output, b.location)
	if err != nil {
		return nil, errors.New("collector backend failed")
	}
	return archives, nil
}

func (b *BorgBackend) run(parent context.Context, timeout time.Duration, dir string, args ...string) ([]byte, error) {
	if parent == nil || b == nil || timeout <= 0 {
		return nil, errors.New("collector backend failed")
	}
	if err := classifyBorgContext(parent); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.config.Binary, args...)
	cmd.Dir = dir
	cmd.Env = append([]string(nil), b.environment...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = borgWaitDelay
	cmd.Cancel = func() error { return terminateBorgProcessGroup(cmd) }
	stdout := &boundedBorgBuffer{limit: borgOutputLimit}
	stderr := &boundedBorgBuffer{limit: borgOutputLimit}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	runErr := cmd.Run()
	cleanupErr := terminateBorgProcessGroup(cmd)
	if err := classifyBorgContext(parent); err != nil {
		return nil, err
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, errors.New("collector backend timeout")
	}
	if cleanupErr != nil || runErr != nil || stdout.overflow || stderr.overflow {
		return nil, errors.New("collector backend failed")
	}
	return append([]byte(nil), stdout.Bytes()...), nil
}

func terminateBorgProcessGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func classifyBorgContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("collector backend failed")
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return errors.New("collector backend timeout")
	}
	if ctx.Err() != nil {
		return errors.New("collector backend cancelled")
	}
	return nil
}

type boundedBorgBuffer struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (b *boundedBorgBuffer) Write(p []byte) (int, error) {
	remaining := b.limit - b.Len()
	if remaining <= 0 {
		b.overflow = true
		return 0, errBorgOutputLimit
	}
	if len(p) > remaining {
		_, _ = b.Buffer.Write(p[:remaining])
		b.overflow = true
		return remaining, errBorgOutputLimit
	}
	return b.Buffer.Write(p)
}

func parseBorgList(output []byte, location *time.Location) ([]ArchiveInfo, error) {
	if location == nil {
		return nil, errors.New("invalid Borg list")
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	var document map[string]json.RawMessage
	if err := decoder.Decode(&document); err != nil || document == nil {
		return nil, errors.New("invalid Borg list")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("invalid Borg list")
	}
	rawArchives, ok := document["archives"]
	if !ok {
		return nil, errors.New("invalid Borg list")
	}
	var rawItems []json.RawMessage
	if err := json.Unmarshal(rawArchives, &rawItems); err != nil || rawItems == nil || len(rawItems) > borgArchiveLimit {
		return nil, errors.New("invalid Borg list")
	}
	archives := make([]ArchiveInfo, 0, len(rawItems))
	for _, rawItem := range rawItems {
		var item map[string]json.RawMessage
		if err := json.Unmarshal(rawItem, &item); err != nil || item == nil {
			return nil, errors.New("invalid Borg list")
		}
		var name, start string
		if err := json.Unmarshal(item["name"], &name); err != nil || name == "" {
			return nil, errors.New("invalid Borg list")
		}
		if err := json.Unmarshal(item["start"], &start); err != nil || start == "" {
			return nil, errors.New("invalid Borg list")
		}
		startedAt, err := parseBorgTime(start, location)
		if err != nil {
			return nil, errors.New("invalid Borg list")
		}
		archives = append(archives, ArchiveInfo{Name: name, StartedAt: startedAt})
	}
	sort.Slice(archives, func(i, j int) bool {
		if archives[i].StartedAt.Equal(archives[j].StartedAt) {
			return archives[i].Name < archives[j].Name
		}
		return archives[i].StartedAt.Before(archives[j].StartedAt)
	})
	return archives, nil
}

func parseBorgTime(value string, location *time.Location) (time.Time, error) {
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return parsed, nil
	}
	for _, layout := range []string{"2006-01-02T15:04:05.999999999", "2006-01-02T15:04:05"} {
		if parsed, err := time.ParseInLocation(layout, value, location); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, errors.New("invalid Borg timestamp")
}

type heldBorgInput struct {
	dirFD       int
	fileFD      int
	dir         string
	name        string
	archiveName string
	size        int64
	stat        unix.Stat_t
}

func openBorgInput(object PendingObject) (*heldBorgInput, error) {
	digest, dir, name, ok := validBorgObject(object)
	if !ok || object.EncryptedSize < 0 {
		return nil, errors.New("invalid Borg input")
	}
	dirFD, cleanDir, err := openPrivateDirectory(dir)
	if err != nil || cleanDir != dir {
		return nil, errors.New("invalid Borg input")
	}
	fileFD, err := unix.Openat(dirFD, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		_ = unix.Close(dirFD)
		return nil, errors.New("invalid Borg input")
	}
	held := &heldBorgInput{dirFD: dirFD, fileFD: fileFD, dir: dir, name: name, archiveName: "sherpa-" + digest, size: object.EncryptedSize}
	if unix.Fstat(fileFD, &held.stat) != nil || !safeRegularMetadata(&held.stat) || held.stat.Size != held.size || !sameDirectoryEntry(dirFD, name, &held.stat) {
		_ = held.close()
		return nil, errors.New("invalid Borg input")
	}
	return held, nil
}

func (h *heldBorgInput) valid() bool {
	if h == nil || h.dirFD < 0 || h.fileFD < 0 {
		return false
	}
	var dirStat unix.Stat_t
	if unix.Fstat(h.dirFD, &dirStat) != nil || !safeBorgDirectoryMetadata(&dirStat) {
		return false
	}
	var fileStat unix.Stat_t
	if unix.Fstat(h.fileFD, &fileStat) != nil || !safeRegularMetadata(&fileStat) || fileStat.Size != h.size || !sameInode(&h.stat, &fileStat) {
		return false
	}
	var entryStat unix.Stat_t
	if unix.Fstatat(h.dirFD, h.name, &entryStat, unix.AT_SYMLINK_NOFOLLOW) != nil || !safeRegularMetadata(&entryStat) || entryStat.Size != h.size || !sameInode(&h.stat, &entryStat) {
		return false
	}
	return true
}

func (h *heldBorgInput) close() error {
	if h == nil {
		return nil
	}
	var closeErr error
	if h.fileFD >= 0 {
		if err := unix.Close(h.fileFD); err != nil {
			closeErr = err
		}
		h.fileFD = -1
	}
	if h.dirFD >= 0 {
		if err := unix.Close(h.dirFD); err != nil && closeErr == nil {
			closeErr = err
		}
		h.dirFD = -1
	}
	return closeErr
}

func validBorgObject(object PendingObject) (digest, dir, name string, ok bool) {
	digest, ok = canonicalDigest(object.ObjectID)
	if !ok || object.DigestHex != digest || object.ArchiveName != "sherpa-"+digest {
		return "", "", "", false
	}
	path := object.EncryptedPath
	name = digest + ".tar.gz.age"
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Base(path) != name {
		return "", "", "", false
	}
	dir = filepath.Dir(path)
	if dir == path || dir == "." || dir == string(os.PathSeparator) {
		return "", "", "", false
	}
	return digest, dir, name, true
}

func validBorgConfig(config BorgConfig) bool {
	return config.Binary != "" && config.Repository != "" && validAbsoluteCleanPath(config.SSHKeyFile) && validAbsoluteCleanPath(config.KnownHostsFile) &&
		validAbsoluteCleanPath(config.WorkDir) && config.CreateTimeout > 0 && config.QueryTimeout > 0 &&
		!containsNUL(config.Binary) && !containsNUL(config.Repository)
}

func validAbsoluteCleanPath(path string) bool {
	return path != "" && filepath.IsAbs(path) && filepath.Clean(path) == path && !containsNUL(path)
}

func ensureBorgStateDirectory(parentFD int, name string) error {
	var before unix.Stat_t
	err := unix.Fstatat(parentFD, name, &before, unix.AT_SYMLINK_NOFOLLOW)
	created := false
	if errors.Is(err, unix.ENOENT) {
		if err := unix.Mkdirat(parentFD, name, 0o700); err != nil {
			return err
		}
		created = true
	} else if err != nil {
		return err
	}
	fd, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if created {
		if err := unix.Fchmod(fd, 0o700); err != nil {
			return err
		}
	}
	var opened unix.Stat_t
	if unix.Fstat(fd, &opened) != nil || !safeBorgDirectoryMetadata(&opened) {
		return errors.New("unsafe Borg state directory")
	}
	var current unix.Stat_t
	if unix.Fstatat(parentFD, name, &current, unix.AT_SYMLINK_NOFOLLOW) != nil || !sameInode(&opened, &current) || !safeBorgDirectoryMetadata(&current) {
		return errors.New("unsafe Borg state directory")
	}
	if !created && !sameInode(&before, &opened) {
		return errors.New("unsafe Borg state directory")
	}
	if created {
		if err := unix.Fsync(parentFD); err != nil {
			return err
		}
	}
	return nil
}

func safeBorgDirectoryMetadata(stat *unix.Stat_t) bool {
	return stat != nil && stat.Mode&unix.S_IFMT == unix.S_IFDIR && stat.Mode&0o7777 == 0o700 && stat.Uid == uint32(os.Geteuid()) && stat.Gid == uint32(os.Getegid())
}

func nonBorgEnvironment(environment []string) []string {
	filtered := make([]string, 0, len(environment))
	for _, item := range environment {
		key, _, ok := strings.Cut(item, "=")
		if ok && strings.HasPrefix(key, "BORG_") {
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered
}

func borgSSHCommand(identity, knownHosts string) string {
	return "ssh -i " + quoteBorgSSHArgument(identity) +
		" -o IdentitiesOnly=yes -o UserKnownHostsFile=" + quoteBorgSSHArgument(knownHosts) +
		" -o StrictHostKeyChecking=yes -p 23"
}

func quoteBorgSSHArgument(value string) string {
	if value != "" && strings.IndexFunc(value, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("/_.,:@%+=-", r))
	}) == -1 {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func containsNUL(value string) bool { return strings.IndexByte(value, 0) >= 0 }
