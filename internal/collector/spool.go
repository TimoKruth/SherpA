package collector

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const (
	spoolSafetyReserve  int64 = 1 << 30
	spoolLockName             = ".sherpa-spool.lock"
	spoolCopyBufferSize       = 32 * 1024
)

var (
	objectIDPattern      = regexp.MustCompile(`^sha256:([0-9a-f]{64})$`)
	completeAgePattern   = regexp.MustCompile(`^([0-9a-f]{64})\.tar\.gz\.age$`)
	partialAgePattern    = regexp.MustCompile(`^[0-9a-f]{64}\.tar\.gz\.age\.partial$`)
	uploadPartialPattern = regexp.MustCompile(`^\.sherpa-upload-[0-9a-f]+\.upload\.partial$`)
)

type PendingObject struct {
	ObjectID      string
	DigestHex     string
	ArchiveName   string
	EncryptedPath string
	EncryptedSize int64
	ReceivedAt    time.Time
}

type spoolOps struct {
	statfs          func(int, *unix.Statfs_t) error
	fsync           func(int) error
	renameNoReplace func(int, string, int, string) error
	random          io.Reader
	now             func() time.Time
}

func defaultSpoolOps() spoolOps {
	return spoolOps{
		statfs:          unix.Fstatfs,
		fsync:           unix.Fsync,
		renameNoReplace: renameAtNoReplace,
		random:          rand.Reader,
		now:             time.Now,
	}
}

type Spool struct {
	dirFD         int
	path          string
	partialMaxAge time.Duration
	ops           spoolOps
	closeOnce     sync.Once
	closeErr      error
}

func OpenSpool(dir string, partialMaxAge time.Duration) (*Spool, error) {
	return openSpool(dir, partialMaxAge, defaultSpoolOps())
}

func openSpool(dir string, partialMaxAge time.Duration, ops spoolOps) (*Spool, error) {
	if partialMaxAge <= 0 || ops.statfs == nil || ops.fsync == nil || ops.renameNoReplace == nil || ops.random == nil || ops.now == nil {
		return nil, errors.New("collector spool configuration invalid")
	}
	fd, cleanPath, err := openPrivateDirectory(dir)
	if err != nil {
		return nil, errors.New("collector spool directory unsafe")
	}
	return &Spool{dirFD: fd, path: cleanPath, partialMaxAge: partialMaxAge, ops: ops}, nil
}

func (s *Spool) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		if s.dirFD >= 0 && unix.Close(s.dirFD) != nil {
			s.closeErr = errors.New("collector spool close failed")
		}
		s.dirFD = -1
	})
	return s.closeErr
}

func (s *Spool) AcquireLock() (func() error, error) {
	if !s.usable() {
		return nil, errors.New("collector spool unavailable")
	}
	fd, err := unix.Openat(s.dirFD, spoolLockName, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, errors.New("collector spool lock unavailable")
	}
	if err := forceRegularMode0600(fd); err != nil {
		_ = unix.Close(fd)
		return nil, errors.New("collector spool lock unsafe")
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = unix.Close(fd)
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, errors.New("collector spool already in use")
		}
		return nil, errors.New("collector spool lock unavailable")
	}
	var once sync.Once
	var releaseErr error
	release := func() error {
		once.Do(func() {
			if err := unix.Close(fd); err != nil {
				releaseErr = errors.New("collector spool lock release failed")
			}
		})
		return releaseErr
	}
	return release, nil
}

func (s *Spool) Admit(contentLength int64) error {
	if !s.usable() || contentLength <= 0 || contentLength > (math.MaxInt64-spoolSafetyReserve)/2 {
		return errors.New("collector insufficient storage")
	}
	required := spoolSafetyReserve + 2*contentLength
	var stat unix.Statfs_t
	if err := s.ops.statfs(s.dirFD, &stat); err != nil {
		return errors.New("collector insufficient storage")
	}
	blockSize := uint64(stat.Bsize)
	if blockSize == 0 || stat.Bavail > math.MaxUint64/blockSize {
		return errors.New("collector insufficient storage")
	}
	available := stat.Bavail * blockSize
	if available < uint64(required) {
		return errors.New("collector insufficient storage")
	}
	return nil
}

func (s *Spool) Receive(ctx context.Context, source io.Reader, contentLength int64) (string, string, int64, error) {
	if ctx == nil || source == nil {
		return "", "", 0, errors.New("collector upload invalid")
	}
	if err := s.Admit(contentLength); err != nil {
		return "", "", 0, err
	}
	name, fd, err := s.createRandomFile(".sherpa-upload-", ".upload.partial")
	if err != nil {
		return "", "", 0, errors.New("collector upload unavailable")
	}
	file := os.NewFile(uintptr(fd), "collector-spool-upload")
	if file == nil {
		_ = unix.Close(fd)
		return "", "", 0, errors.New("collector upload unavailable")
	}
	closed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
	}()

	hash := sha256.New()
	writer := io.MultiWriter(file, hash)
	buffer := make([]byte, spoolCopyBufferSize)
	written, copyErr := io.CopyBuffer(writer, io.LimitReader(&contextReader{ctx: ctx, reader: source}, contentLength), buffer)
	if copyErr != nil {
		if ctx.Err() != nil {
			return "", "", 0, errors.New("collector upload canceled")
		}
		return "", "", 0, errors.New("collector upload read failed")
	}
	if written != contentLength {
		return "", "", 0, errors.New("collector upload length mismatch")
	}
	var extra [1]byte
	n, extraErr := io.ReadFull(&contextReader{ctx: ctx, reader: source}, extra[:])
	if ctx.Err() != nil {
		return "", "", 0, errors.New("collector upload canceled")
	}
	if n != 0 || !errors.Is(extraErr, io.EOF) {
		return "", "", 0, errors.New("collector upload length mismatch")
	}
	if err := s.ops.fsync(fd); err != nil {
		return "", "", 0, errors.New("collector upload synchronization failed")
	}
	closed = true
	if err := file.Close(); err != nil {
		return "", "", 0, errors.New("collector upload close failed")
	}
	return filepath.Join(s.path, name), "sha256:" + hex.EncodeToString(hash.Sum(nil)), written, nil
}

func (s *Spool) EncryptedPaths(objectID string) (string, string, error) {
	digest, ok := canonicalDigest(objectID)
	if !s.usable() || !ok {
		return "", "", errors.New("collector object ID invalid")
	}
	base := digest + ".tar.gz.age"
	return filepath.Join(s.path, base+".partial"), filepath.Join(s.path, base), nil
}

func (s *Spool) CommitEncrypted(partial, final string) error {
	if !s.usable() {
		return errors.New("collector spool unavailable")
	}
	partialName, finalName, ok := s.validEncryptedPair(partial, final)
	if !ok {
		return errors.New("collector encrypted path invalid")
	}
	fd, err := unix.Openat(s.dirFD, partialName, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return errors.New("collector encrypted partial unavailable")
	}
	if err := verifyRegularMode0600(fd); err != nil {
		_ = unix.Close(fd)
		return errors.New("collector encrypted partial unsafe")
	}
	var openedStat unix.Stat_t
	if err := unix.Fstat(fd, &openedStat); err != nil {
		_ = unix.Close(fd)
		return errors.New("collector encrypted partial unsafe")
	}
	if err := s.ops.fsync(fd); err != nil {
		_ = unix.Close(fd)
		return errors.New("collector encrypted synchronization failed")
	}
	if err := unix.Close(fd); err != nil {
		return errors.New("collector encrypted close failed")
	}
	if !sameDirectoryEntry(s.dirFD, partialName, &openedStat) {
		return errors.New("collector encrypted partial unsafe")
	}
	if err := s.ops.renameNoReplace(s.dirFD, partialName, s.dirFD, finalName); err != nil {
		return errors.New("collector encrypted commit failed")
	}
	if err := s.ops.fsync(s.dirFD); err != nil {
		return errors.New("collector spool synchronization failed")
	}
	return nil
}

func (s *Spool) RemovePlaintext(path string) error {
	if !s.usable() {
		return errors.New("collector spool unavailable")
	}
	name, ok := s.exactChild(path)
	if !ok || !validUploadPartialName(name) {
		return errors.New("collector plaintext path invalid")
	}
	return s.removeRegular(name, "collector plaintext removal failed")
}

func (s *Spool) RemoveEncrypted(path string) error {
	if !s.usable() {
		return errors.New("collector spool unavailable")
	}
	name, ok := s.exactChild(path)
	if !ok || !completeAgePattern.MatchString(name) {
		return errors.New("collector encrypted path invalid")
	}
	return s.removeRegular(name, "collector encrypted removal failed")
}

func (s *Spool) removeRegular(name, failure string) error {
	fd, err := unix.Openat(s.dirFD, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return errors.New(failure)
	}
	if err := verifyRegularMode0600(fd); err != nil {
		_ = unix.Close(fd)
		return errors.New(failure)
	}
	var openedStat unix.Stat_t
	if err := unix.Fstat(fd, &openedStat); err != nil {
		_ = unix.Close(fd)
		return errors.New(failure)
	}
	if err := unix.Close(fd); err != nil {
		return errors.New(failure)
	}
	if !sameDirectoryEntry(s.dirFD, name, &openedStat) {
		return errors.New(failure)
	}
	if err := unix.Unlinkat(s.dirFD, name, 0); err != nil {
		return errors.New(failure)
	}
	if err := s.ops.fsync(s.dirFD); err != nil {
		return errors.New("collector spool synchronization failed")
	}
	return nil
}

func (s *Spool) Discover() ([]PendingObject, error) {
	if !s.usable() {
		return nil, errors.New("collector spool unavailable")
	}
	entries, err := readDirectoryNames(s.dirFD)
	if err != nil {
		return nil, errors.New("collector spool discovery failed")
	}
	objects := make([]PendingObject, 0)
	for _, name := range entries {
		match := completeAgePattern.FindStringSubmatch(name)
		if match == nil {
			continue
		}
		fd, openErr := unix.Openat(s.dirFD, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		if openErr != nil {
			if errors.Is(openErr, unix.ELOOP) || errors.Is(openErr, unix.ENOENT) || errors.Is(openErr, unix.ENXIO) {
				continue
			}
			return nil, errors.New("collector spool object unavailable")
		}
		if verifyRegularMode0600(fd) != nil {
			_ = unix.Close(fd)
			continue
		}
		var openedStat unix.Stat_t
		if err := unix.Fstat(fd, &openedStat); err != nil {
			_ = unix.Close(fd)
			return nil, errors.New("collector spool object unavailable")
		}
		file := os.NewFile(uintptr(fd), "collector-spool-object")
		info, statErr := file.Stat()
		closeErr := file.Close()
		if statErr != nil || closeErr != nil || !info.Mode().IsRegular() || !sameDirectoryEntry(s.dirFD, name, &openedStat) {
			return nil, errors.New("collector spool object unavailable")
		}
		digest := match[1]
		objects = append(objects, PendingObject{
			ObjectID: "sha256:" + digest, DigestHex: digest, ArchiveName: "sherpa-" + digest,
			EncryptedPath: filepath.Join(s.path, name), EncryptedSize: info.Size(), ReceivedAt: info.ModTime(),
		})
	}
	sort.Slice(objects, func(i, j int) bool {
		if objects[i].ReceivedAt.Equal(objects[j].ReceivedAt) {
			return objects[i].DigestHex < objects[j].DigestHex
		}
		return objects[i].ReceivedAt.Before(objects[j].ReceivedAt)
	})
	return objects, nil
}

func (s *Spool) CleanupStalePartials() (int, error) { return s.cleanupStalePartialsAt(s.ops.now()) }

func (s *Spool) cleanupStalePartialsAt(now time.Time) (int, error) {
	if !s.usable() {
		return 0, errors.New("collector spool unavailable")
	}
	entries, err := readDirectoryNames(s.dirFD)
	if err != nil {
		return 0, errors.New("collector spool cleanup failed")
	}
	cutoff := now.Add(-s.partialMaxAge)
	removed := 0
	for _, name := range entries {
		if !validUploadPartialName(name) && !partialAgePattern.MatchString(name) {
			continue
		}
		fd, openErr := unix.Openat(s.dirFD, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		if openErr != nil {
			continue
		}
		if verifyRegularMode0600(fd) != nil {
			_ = unix.Close(fd)
			continue
		}
		var openedStat unix.Stat_t
		if err := unix.Fstat(fd, &openedStat); err != nil {
			_ = unix.Close(fd)
			return removed, errors.New("collector spool cleanup failed")
		}
		file := os.NewFile(uintptr(fd), "collector-spool-partial")
		info, statErr := file.Stat()
		closeErr := file.Close()
		if statErr != nil || closeErr != nil {
			return removed, errors.New("collector spool cleanup failed")
		}
		if !info.ModTime().Before(cutoff) {
			continue
		}
		if !sameDirectoryEntry(s.dirFD, name, &openedStat) {
			return removed, errors.New("collector spool cleanup failed")
		}
		if err := unix.Unlinkat(s.dirFD, name, 0); err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue
			}
			return removed, errors.New("collector spool cleanup failed")
		}
		removed++
	}
	if removed > 0 && s.ops.fsync(s.dirFD) != nil {
		return removed, errors.New("collector spool synchronization failed")
	}
	return removed, nil
}

func (s *Spool) CheckWritable() error {
	if !s.usable() {
		return errors.New("collector spool unavailable")
	}
	name, fd, err := s.createRandomFile(".sherpa-probe-", ".tmp")
	if err != nil {
		return errors.New("collector spool not writable")
	}
	if err := s.ops.fsync(fd); err != nil {
		_ = unix.Close(fd)
		_ = unix.Unlinkat(s.dirFD, name, 0)
		return errors.New("collector spool not writable")
	}
	if err := unix.Close(fd); err != nil {
		_ = unix.Unlinkat(s.dirFD, name, 0)
		return errors.New("collector spool not writable")
	}
	if err := unix.Unlinkat(s.dirFD, name, 0); err != nil {
		return errors.New("collector spool not writable")
	}
	if err := s.ops.fsync(s.dirFD); err != nil {
		return errors.New("collector spool not writable")
	}
	return nil
}

func (s *Spool) createRandomFile(prefix, suffix string) (string, int, error) {
	for attempts := 0; attempts < 8; attempts++ {
		var randomBytes [16]byte
		if _, err := io.ReadFull(s.ops.random, randomBytes[:]); err != nil {
			return "", -1, err
		}
		name := prefix + hex.EncodeToString(randomBytes[:]) + suffix
		fd, err := unix.Openat(s.dirFD, name, unix.O_CREAT|unix.O_EXCL|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o600)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return "", -1, err
		}
		if err := forceRegularMode0600(fd); err != nil {
			_ = unix.Close(fd)
			return "", -1, err
		}
		return name, fd, nil
	}
	return "", -1, errors.New("random file collision")
}

func (s *Spool) validEncryptedPair(partial, final string) (string, string, bool) {
	partialName, partialOK := s.exactChild(partial)
	finalName, finalOK := s.exactChild(final)
	if !partialOK || !finalOK || !partialAgePattern.MatchString(partialName) || !completeAgePattern.MatchString(finalName) {
		return "", "", false
	}
	return partialName, finalName, partialName == finalName+".partial"
}
func (s *Spool) exactChild(path string) (string, bool) {
	if path == "" || filepath.Clean(path) != path || filepath.Dir(path) != s.path {
		return "", false
	}
	name := filepath.Base(path)
	if name == "." || name == string(os.PathSeparator) || strings.Contains(name, string(os.PathSeparator)) {
		return "", false
	}
	return name, true
}
func (s *Spool) usable() bool { return s != nil && s.dirFD >= 0 }

func canonicalDigest(objectID string) (string, bool) {
	match := objectIDPattern.FindStringSubmatch(objectID)
	if match == nil {
		return "", false
	}
	return match[1], true
}
func validUploadPartialName(name string) bool { return uploadPartialPattern.MatchString(name) }

func openPrivateDirectory(path string) (int, string, error) {
	start, components, err := safePathComponents(path)
	if err != nil {
		return -1, "", err
	}
	fd, err := unix.Open(start, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, "", err
	}
	for _, component := range components {
		next, openErr := unix.Openat(fd, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		closeErr := unix.Close(fd)
		if openErr != nil || closeErr != nil {
			if openErr == nil {
				_ = unix.Close(next)
			}
			return -1, "", errors.New("unsafe directory")
		}
		fd = next
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Mode&0o7777 != 0o700 || stat.Uid != uint32(os.Geteuid()) || stat.Gid != uint32(os.Getegid()) {
		_ = unix.Close(fd)
		return -1, "", errors.New("unsafe directory")
	}
	return fd, filepath.Clean(path), nil
}

func forceRegularMode0600(fd int) error {
	if err := unix.Fchmod(fd, 0o600); err != nil {
		return err
	}
	return verifyRegularMode0600(fd)
}
func verifyRegularMode0600(fd int) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0o7777 != 0o600 || stat.Uid != uint32(os.Geteuid()) || stat.Gid != uint32(os.Getegid()) {
		return errors.New("unsafe file")
	}
	return nil
}
func sameDirectoryEntry(dirFD int, name string, opened *unix.Stat_t) bool {
	var current unix.Stat_t
	if opened == nil || unix.Fstatat(dirFD, name, &current, unix.AT_SYMLINK_NOFOLLOW) != nil {
		return false
	}
	return current.Dev == opened.Dev && current.Ino == opened.Ino && current.Mode&unix.S_IFMT == opened.Mode&unix.S_IFMT
}

func readDirectoryNames(dirFD int) ([]string, error) {
	fd, err := unix.Openat(dirFD, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "collector-directory")
	entries, readErr := file.ReadDir(-1)
	closeErr := file.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	names := make([]string, len(entries))
	for i := range entries {
		names[i] = entries[i].Name()
	}
	return names, nil
}
