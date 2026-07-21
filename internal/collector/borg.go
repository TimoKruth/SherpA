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
}

func NewBorgBackend(config BorgConfig) (*BorgBackend, error) {
	if !validBorgConfig(config) {
		return nil, errors.New("collector backend unavailable")
	}
	config.WorkDir = filepath.Clean(config.WorkDir)
	cacheDir := filepath.Join(config.WorkDir, "cache")
	configDir := filepath.Join(config.WorkDir, "config")
	securityDir := filepath.Join(config.WorkDir, "security")
	for _, dir := range []string{config.WorkDir, cacheDir, configDir, securityDir} {
		if err := ensureBorgDirectory(dir); err != nil {
			return nil, errors.New("collector backend unavailable")
		}
	}
	environment := append(nonBorgEnvironment(os.Environ()),
		"BORG_REPO="+config.Repository,
		"BORG_RSH="+borgSSHCommand(config.SSHKeyFile, config.KnownHostsFile),
		"BORG_CACHE_DIR="+cacheDir,
		"BORG_CONFIG_DIR="+configDir,
		"BORG_SECURITY_DIR="+securityDir,
		"BORG_UNKNOWN_UNENCRYPTED_REPO_ACCESS_IS_OK=yes",
	)
	return &BorgBackend{config: config, environment: environment}, nil
}

func (b *BorgBackend) Create(ctx context.Context, object PendingObject) error {
	digest, dir, name, ok := validBorgObject(object)
	if !ok || b == nil {
		return errors.New("collector backend failed")
	}
	if _, err := b.run(ctx, b.config.CreateTimeout, dir, "create", "--compression", "none", "::sherpa-"+digest, name); err != nil {
		return err
	}
	archives, err := b.list(ctx)
	if err != nil {
		return err
	}
	want := "sherpa-" + digest
	for _, archive := range archives {
		if archive.Name == want {
			return nil
		}
	}
	return errors.New("collector remote verification failed")
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
	archives, err := parseBorgList(output)
	if err != nil {
		return nil, errors.New("collector backend failed")
	}
	return archives, nil
}

func (b *BorgBackend) run(parent context.Context, timeout time.Duration, dir string, args ...string) ([]byte, error) {
	if parent == nil || b == nil || timeout <= 0 {
		return nil, errors.New("collector backend failed")
	}
	if errors.Is(parent.Err(), context.DeadlineExceeded) {
		return nil, errors.New("collector backend timeout")
	}
	if parent.Err() != nil {
		return nil, errors.New("collector backend cancelled")
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.config.Binary, args...)
	cmd.Dir = dir
	cmd.Env = append([]string(nil), b.environment...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = borgWaitDelay
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if err == nil || errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return cmd.Process.Kill()
	}
	stdout := &boundedBorgBuffer{limit: borgOutputLimit}
	stderr := &boundedBorgBuffer{limit: borgOutputLimit}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	runErr := cmd.Run()
	if errors.Is(parent.Err(), context.DeadlineExceeded) {
		return nil, errors.New("collector backend timeout")
	}
	if parent.Err() != nil {
		return nil, errors.New("collector backend cancelled")
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, errors.New("collector backend timeout")
	}
	if runErr != nil || stdout.overflow || stderr.overflow {
		return nil, errors.New("collector backend failed")
	}
	return append([]byte(nil), stdout.Bytes()...), nil
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

func parseBorgList(output []byte) ([]ArchiveInfo, error) {
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
		startedAt, err := parseBorgTime(start)
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

func parseBorgTime(value string) (time.Time, error) {
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return parsed.UTC(), nil
	}
	for _, layout := range []string{"2006-01-02T15:04:05.999999999", "2006-01-02T15:04:05"} {
		if parsed, err := time.ParseInLocation(layout, value, time.UTC); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, errors.New("invalid Borg timestamp")
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
	return config.Binary != "" && config.Repository != "" && config.SSHKeyFile != "" && config.KnownHostsFile != "" &&
		config.WorkDir != "" && filepath.IsAbs(config.WorkDir) && filepath.Clean(config.WorkDir) == config.WorkDir &&
		config.CreateTimeout > 0 && config.QueryTimeout > 0 &&
		!containsNUL(config.Binary) && !containsNUL(config.Repository) && !containsNUL(config.SSHKeyFile) &&
		!containsNUL(config.KnownHostsFile) && !containsNUL(config.WorkDir)
}

func ensureBorgDirectory(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err = os.MkdirAll(path, 0o700); err != nil {
			return err
		}
		info, err = os.Lstat(path)
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("unsafe Borg directory")
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return err
	}
	info, err = os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("unsafe Borg directory")
	}
	return nil
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
