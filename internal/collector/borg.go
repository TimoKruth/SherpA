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
	"sync"
	"sync/atomic"
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
	state       borgStateIdentity
	system      borgSystem
}

func NewBorgBackend(config BorgConfig) (*BorgBackend, error) {
	return newBorgBackend(config, time.Local)
}

func newBorgBackend(config BorgConfig, location *time.Location) (*BorgBackend, error) {
	return newBorgBackendWithSystem(config, location, defaultBorgSystem())
}

func newBorgBackendWithSystem(config BorgConfig, location *time.Location, system borgSystem) (*BorgBackend, error) {
	if !validBorgConfig(config) || location == nil || !system.valid() {
		return nil, errors.New("collector backend unavailable")
	}
	workFD, workPath, err := openPrivateDirectory(config.WorkDir)
	if err != nil || workPath != config.WorkDir {
		return nil, errors.New("collector backend unavailable")
	}
	state, stateErr := initializeBorgState(workFD, system)
	closeErr := unix.Close(workFD)
	if stateErr != nil || closeErr != nil {
		return nil, errors.New("collector backend unavailable")
	}
	environment := append(nonBorgEnvironment(os.Environ()),
		"BORG_REPO="+config.Repository,
		"BORG_RSH="+borgSSHCommand(config.SSHKeyFile, config.KnownHostsFile),
		"BORG_CACHE_DIR="+borgFDPath(3),
		"BORG_CONFIG_DIR="+borgFDPath(4),
		"BORG_SECURITY_DIR="+borgFDPath(5),
		"BORG_UNKNOWN_UNENCRYPTED_REPO_ACCESS_IS_OK=yes",
	)
	return &BorgBackend{config: config, environment: environment, location: location, state: state, system: system}, nil
}

func (b *BorgBackend) Create(ctx context.Context, object PendingObject) (retErr error) {
	if b == nil {
		return errors.New("collector backend failed")
	}
	if err := classifyBorgContext(ctx); err != nil {
		return err
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
	createCtx, cancel := context.WithTimeout(ctx, b.config.CreateTimeout)
	defer cancel()
	stage, err := b.stageInput(createCtx, held)
	if err != nil {
		if contextErr := classifyBorgRunContext(ctx, createCtx); contextErr != nil {
			return contextErr
		}
		return errors.New("collector backend failed")
	}
	defer func() {
		if stage.cleanup() != nil {
			retErr = errors.New("collector backend failed")
		}
	}()
	if !held.valid() || !stage.valid() {
		return errors.New("collector backend failed")
	}
	_, createErr := b.run(createCtx, b.config.CreateTimeout, stage, "create", "--compression", "none", "::"+held.archiveName, held.name)
	if !held.valid() || !stage.valid() {
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
	if !held.valid() || !stage.valid() {
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
	output, err := b.run(ctx, b.config.QueryTimeout, nil, "list", "--json")
	if err != nil {
		return nil, err
	}
	archives, err := parseBorgList(output, b.location)
	if err != nil {
		return nil, errors.New("collector backend failed")
	}
	return archives, nil
}

func (b *BorgBackend) run(parent context.Context, timeout time.Duration, stage *borgStage, args ...string) ([]byte, error) {
	if parent == nil || b == nil || timeout <= 0 {
		return nil, errors.New("collector backend failed")
	}
	if err := classifyBorgContext(parent); err != nil {
		return nil, err
	}
	binding, err := b.bindCommand(stage)
	if err != nil {
		return nil, errors.New("collector backend failed")
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := exec.Command(b.config.Binary, args...)
	cmd.Env = append([]string(nil), b.environment...)
	cmd.ExtraFiles = append([]*os.File(nil), binding.files...)
	if stage != nil {
		cmd.Dir = borgFDPath(3 + borgStateFileCount)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = borgWaitDelay
	overflow := newBorgOverflowSignal()
	stdout := newBoundedBorgBuffer(borgOutputLimit, overflow)
	stderr := newBoundedBorgBuffer(borgOutputLimit, overflow)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := b.system.beforeStart(cmd); err != nil {
		_ = binding.close()
		return nil, errors.New("collector backend failed")
	}
	prepared, err := prepareBorgCommand(cmd, binding, stage)
	if err != nil {
		_ = binding.close()
		return nil, errors.New("collector backend failed")
	}
	if err := b.system.beforeExecStart(cmd); err != nil {
		_ = binding.close()
		return nil, errors.New("collector backend failed")
	}
	if err := classifyBorgContext(ctx); err != nil {
		_ = binding.close()
		return nil, err
	}
	if !binding.valid() || (stage != nil && !stage.valid()) {
		_ = binding.close()
		return nil, errors.New("collector backend failed")
	}
	if !prepared.valid() {
		_ = binding.close()
		return nil, errors.New("collector backend failed")
	}
	if err := cmd.Start(); err != nil {
		_ = binding.close()
		if contextErr := classifyBorgContext(parent); contextErr != nil {
			return nil, contextErr
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, errors.New("collector backend timeout")
		}
		return nil, errors.New("collector backend failed")
	}
	postStartErr := b.system.afterStart(cmd)
	postStartValid := prepared.valid()
	noReap := make(chan error, 1)
	go func() { noReap <- b.system.waitNoReap(cmd.Process.Pid) }()
	var noReapErr, killErr error
	if postStartErr != nil || !postStartValid {
		killErr = b.system.killProcessGroup(cmd.Process.Pid)
		noReapErr = <-noReap
	} else {
		select {
		case noReapErr = <-noReap:
			killErr = b.system.killProcessGroup(cmd.Process.Pid)
		case <-ctx.Done():
			killErr = b.system.killProcessGroup(cmd.Process.Pid)
			noReapErr = <-noReap
		case <-overflow.channel:
			killErr = b.system.killProcessGroup(cmd.Process.Pid)
			noReapErr = <-noReap
		}
	}
	waitErr := b.system.waitProcess(cmd)
	bindingValid := binding.valid()
	preparedValid := prepared.valid()
	closeErr := binding.close()
	if contextErr := classifyBorgContext(parent); contextErr != nil {
		return nil, contextErr
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, errors.New("collector backend timeout")
	}
	if postStartErr != nil || !postStartValid || noReapErr != nil || killErr != nil || waitErr != nil || closeErr != nil || !bindingValid || !preparedValid || stdout.overflow.Load() || stderr.overflow.Load() {
		return nil, errors.New("collector backend failed")
	}
	return append([]byte(nil), stdout.Bytes()...), nil
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

func classifyBorgRunContext(parent, run context.Context) error {
	if err := classifyBorgContext(parent); err != nil {
		return err
	}
	if errors.Is(run.Err(), context.DeadlineExceeded) {
		return errors.New("collector backend timeout")
	}
	if run.Err() != nil {
		return errors.New("collector backend cancelled")
	}
	return nil
}

type borgOverflowSignal struct {
	channel chan struct{}
	once    sync.Once
}

func newBorgOverflowSignal() *borgOverflowSignal {
	return &borgOverflowSignal{channel: make(chan struct{})}
}

func (s *borgOverflowSignal) notify() {
	s.once.Do(func() { close(s.channel) })
}

type boundedBorgBuffer struct {
	buffer   bytes.Buffer
	limit    int
	signal   *borgOverflowSignal
	overflow atomic.Bool
}

func newBoundedBorgBuffer(limit int, signal *borgOverflowSignal) *boundedBorgBuffer {
	return &boundedBorgBuffer{limit: limit, signal: signal}
}

func (b *boundedBorgBuffer) Write(p []byte) (int, error) {
	remaining := b.limit - b.buffer.Len()
	if remaining <= 0 {
		b.markOverflow()
		return 0, errBorgOutputLimit
	}
	if len(p) > remaining {
		_, _ = b.buffer.Write(p[:remaining])
		b.markOverflow()
		return remaining, errBorgOutputLimit
	}
	return b.buffer.Write(p)
}

func (b *boundedBorgBuffer) Bytes() []byte {
	return b.buffer.Bytes()
}

func (b *boundedBorgBuffer) markOverflow() {
	b.overflow.Store(true)
	b.signal.notify()
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
	return config.Binary != "" && config.Repository != "" && validBorgSSHPath(config.SSHKeyFile) && validBorgSSHPath(config.KnownHostsFile) &&
		validAbsoluteCleanPath(config.WorkDir) && config.CreateTimeout > 0 && config.QueryTimeout > 0 &&
		!containsNUL(config.Binary) && !containsNUL(config.Repository)
}

func validAbsoluteCleanPath(path string) bool {
	return path != "" && filepath.IsAbs(path) && filepath.Clean(path) == path && !containsNUL(path)
}

func validBorgSSHPath(path string) bool {
	if !validAbsoluteCleanPath(path) {
		return false
	}
	for i := 0; i < len(path); i++ {
		value := path[i]
		if value == '/' || value == '.' || value == '_' || value == '-' || value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9' {
			continue
		}
		return false
	}
	return true
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
		if unix.Fstatat(parentFD, name, &before, unix.AT_SYMLINK_NOFOLLOW) != nil || !ownedBorgDirectoryMetadata(&before) {
			return errors.New("unsafe Borg state directory")
		}
	} else if err != nil || !safeBorgDirectoryMetadata(&before) {
		return errors.New("unsafe Borg state directory")
	}
	fd, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	var opened unix.Stat_t
	if unix.Fstat(fd, &opened) != nil || !ownedBorgDirectoryMetadata(&opened) || !sameInode(&before, &opened) {
		return errors.New("unsafe Borg state directory")
	}
	if created {
		if err := unix.Fchmod(fd, 0o700); err != nil {
			return err
		}
	}
	if unix.Fstat(fd, &opened) != nil || !safeBorgDirectoryMetadata(&opened) {
		return errors.New("unsafe Borg state directory")
	}
	var current unix.Stat_t
	if unix.Fstatat(parentFD, name, &current, unix.AT_SYMLINK_NOFOLLOW) != nil || !sameInode(&opened, &current) || !safeBorgDirectoryMetadata(&current) {
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
	return "ssh -i " + identity +
		" -o IdentitiesOnly=yes -o UserKnownHostsFile=" + knownHosts +
		" -o StrictHostKeyChecking=yes -p 23"
}

func containsNUL(value string) bool { return strings.IndexByte(value, 0) >= 0 }
