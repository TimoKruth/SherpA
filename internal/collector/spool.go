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
	spoolSafetyReserve   int64 = 1 << 30
	spoolLockName              = ".sherpa-spool.lock"
	spoolCopyBufferSize        = 32 * 1024
	maxZeroProgressReads       = 8
)

var (
	objectIDPattern      = regexp.MustCompile(`^sha256:([0-9a-f]{64})$`)
	completeAgePattern   = regexp.MustCompile(`^([0-9a-f]{64})\.tar\.gz\.age$`)
	partialAgePattern    = regexp.MustCompile(`^[0-9a-f]{64}\.tar\.gz\.age\.partial$`)
	uploadPartialPattern = regexp.MustCompile(`^\.sherpa-upload-[0-9a-f]{32}\.upload\.partial$`)
)

type PendingObject struct {
	ObjectID, DigestHex, ArchiveName, EncryptedPath string
	EncryptedSize                                   int64
	ReceivedAt                                      time.Time
}

type spoolOps struct {
	statfs          func(int, *unix.Statfs_t) error
	fsync           func(int) error
	renameNoReplace func(int, string, int, string) error
	openat          func(int, string, int, uint32) (int, error)
	fstatat         func(int, string, *unix.Stat_t, int) error
	close           func(int) error
	write           func(int, []byte) (int, error)
	unlinkat        func(int, string, int) error
	flock           func(int, int) error
	random          io.Reader
	now             func() time.Time
}

func defaultSpoolOps() spoolOps {
	return spoolOps{unix.Fstatfs, unix.Fsync, renameAtNoReplace, unix.Openat, unix.Fstatat, unix.Close, unix.Write, unix.Unlinkat, unix.Flock, rand.Reader, time.Now}
}

type Spool struct {
	dirFD         int
	path          string
	partialMaxAge time.Duration
	ops           spoolOps
	life          sync.RWMutex
	transition    sync.Mutex
	active        map[string]struct{}
	closed        bool
}

func OpenSpool(dir string, age time.Duration) (*Spool, error) {
	return openSpool(dir, age, defaultSpoolOps())
}
func openSpool(dir string, age time.Duration, ops spoolOps) (*Spool, error) {
	if age <= 0 || ops.statfs == nil || ops.fsync == nil || ops.renameNoReplace == nil || ops.openat == nil || ops.fstatat == nil || ops.close == nil || ops.write == nil || ops.unlinkat == nil || ops.flock == nil || ops.random == nil || ops.now == nil {
		return nil, errors.New("collector spool configuration invalid")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, errors.New("collector spool directory unsafe")
	}
	abs = filepath.Clean(abs)
	fd, _, err := openPrivateDirectory(abs)
	if err != nil {
		return nil, errors.New("collector spool directory unsafe")
	}
	return &Spool{dirFD: fd, path: abs, partialMaxAge: age, ops: ops, active: make(map[string]struct{})}, nil
}
func (s *Spool) Close() error {
	if s == nil {
		return nil
	}
	s.life.Lock()
	defer s.life.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.dirFD >= 0 && s.ops.close(s.dirFD) != nil {
		s.dirFD = -1
		return errors.New("collector spool close failed")
	}
	s.dirFD = -1
	return nil
}
func (s *Spool) begin() bool {
	s.life.RLock()
	if s.closed || s.dirFD < 0 {
		s.life.RUnlock()
		return false
	}
	return true
}
func (s *Spool) end() { s.life.RUnlock() }

func (s *Spool) AcquireLock() (func() error, error) {
	if !s.begin() {
		return nil, errors.New("collector spool unavailable")
	}
	defer s.end()
	s.transition.Lock()
	defer s.transition.Unlock()
	var expected unix.Stat_t
	metaErr := s.ops.fstatat(s.dirFD, spoolLockName, &expected, unix.AT_SYMLINK_NOFOLLOW)
	flags := unix.O_RDWR | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	created := false
	if errors.Is(metaErr, unix.ENOENT) {
		flags |= unix.O_CREAT | unix.O_EXCL
		created = true
	} else if metaErr != nil || !safeRegularMetadata(&expected) {
		return nil, errors.New("collector spool lock unsafe")
	}
	fd, err := s.ops.openat(s.dirFD, spoolLockName, flags, 0o600)
	if err != nil {
		return nil, classifyStorage(err, "collector spool lock unavailable")
	}
	if created && forceRegularMode0600(fd) != nil {
		_ = s.ops.close(fd)
		return nil, errors.New("collector spool lock unsafe")
	}
	var opened unix.Stat_t
	if unix.Fstat(fd, &opened) != nil || !safeRegularMetadata(&opened) || (!created && !sameInode(&expected, &opened)) {
		_ = s.ops.close(fd)
		return nil, errors.New("collector spool lock unsafe")
	}
	if err = s.ops.flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = s.ops.close(fd)
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, errors.New("collector spool already in use")
		}
		return nil, errors.New("collector spool lock unavailable")
	}
	if !s.sameEntry(spoolLockName, &opened) {
		_ = s.ops.close(fd)
		return nil, errors.New("collector spool lock unsafe")
	}
	var once sync.Once
	var releaseErr error
	return func() error {
		once.Do(func() {
			if s.ops.close(fd) != nil {
				releaseErr = errors.New("collector spool lock release failed")
			}
		})
		return releaseErr
	}, nil
}
func (s *Spool) Admit(n int64) error {
	if !s.begin() {
		return errors.New("collector insufficient storage")
	}
	defer s.end()
	return s.admit(n)
}
func (s *Spool) admit(n int64) error {
	if n <= 0 || n > (math.MaxInt64-spoolSafetyReserve)/2 {
		return errors.New("collector insufficient storage")
	}
	var st unix.Statfs_t
	if s.ops.statfs(s.dirFD, &st) != nil {
		return errors.New("collector insufficient storage")
	}
	bs := uint64(st.Bsize)
	if bs == 0 || st.Bavail > math.MaxUint64/bs || st.Bavail*bs < uint64(spoolSafetyReserve+2*n) {
		return errors.New("collector insufficient storage")
	}
	return nil
}

func (s *Spool) Receive(ctx context.Context, source io.Reader, length int64) (path, id string, written int64, retErr error) {
	if ctx == nil || source == nil {
		return "", "", 0, errors.New("collector upload invalid")
	}
	readCloser, ok := source.(io.ReadCloser)
	if !ok {
		return "", "", 0, errors.New("collector upload source not closable")
	}
	var closeOnce sync.Once
	var sourceCloseErr error
	closeSource := func() { closeOnce.Do(func() { sourceCloseErr = readCloser.Close() }) }
	stopWatcher := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-ctx.Done():
			closeSource()
		case <-stopWatcher:
		}
	}()
	var finishOnce sync.Once
	finishSource := func() error {
		finishOnce.Do(func() {
			close(stopWatcher)
			<-watcherDone
			closeSource()
		})
		return sourceCloseErr
	}
	defer func() {
		if finishSource() != nil && retErr == nil {
			path, id, written, retErr = "", "", 0, errors.New("collector upload source close failed")
		}
	}()
	if ctx.Err() != nil {
		return "", "", 0, errors.New("collector upload canceled")
	}
	if !s.begin() {
		return "", "", 0, errors.New("collector spool unavailable")
	}
	defer s.end()
	if err := s.admit(length); err != nil {
		return "", "", 0, err
	}
	s.transition.Lock()
	name, fd, err := s.createRandomFile(".sherpa-upload-", ".upload.partial")
	if err == nil {
		s.active[name] = struct{}{}
	}
	s.transition.Unlock()
	if err != nil {
		return "", "", 0, classifyStorage(err, "collector upload unavailable")
	}
	success := false
	defer func() {
		if !success {
			s.transition.Lock()
			delete(s.active, name)
			s.transition.Unlock()
		}
		if fd >= 0 {
			_ = s.ops.close(fd)
		}
	}()
	h := sha256.New()
	writer := io.MultiWriter(fdWriter{fd, s.ops.write}, h)
	buf := make([]byte, spoolCopyBufferSize)
	zeroReads := 0
	sawEOF := false
	for written < length {
		want := int64(len(buf))
		if remaining := length - written; remaining < want {
			want = remaining
		}
		n, readErr := readCloser.Read(buf[:want])
		if n < 0 || n > int(want) {
			return "", "", 0, errors.New("collector upload read failed")
		}
		if n > 0 {
			zeroReads = 0
			wn, writeErr := writer.Write(buf[:n])
			written += int64(wn)
			if writeErr != nil || wn != n {
				return "", "", 0, classifyStorage(writeErr, "collector upload write failed")
			}
		} else {
			zeroReads++
			if zeroReads > maxZeroProgressReads {
				return "", "", 0, errors.New("collector upload read failed")
			}
		}
		if readErr != nil {
			if ctx.Err() != nil {
				return "", "", 0, errors.New("collector upload canceled")
			}
			if errors.Is(readErr, io.EOF) {
				if written != length {
					return "", "", 0, errors.New("collector upload length mismatch")
				}
				sawEOF = true
				break
			}
			return "", "", 0, errors.New("collector upload read failed")
		}
	}
	if !sawEOF {
		var extra [1]byte
		zeroReads = 0
		for {
			n, readErr := readCloser.Read(extra[:])
			if n < 0 || n > 1 {
				return "", "", 0, errors.New("collector upload read failed")
			}
			if n > 0 {
				return "", "", 0, errors.New("collector upload length mismatch")
			}
			zeroReads++
			if zeroReads > maxZeroProgressReads {
				return "", "", 0, errors.New("collector upload length mismatch")
			}
			if readErr != nil {
				if ctx.Err() != nil {
					return "", "", 0, errors.New("collector upload canceled")
				}
				if errors.Is(readErr, io.EOF) {
					break
				}
				return "", "", 0, errors.New("collector upload read failed")
			}
		}
	}
	if err := s.ops.fsync(fd); err != nil {
		return "", "", 0, classifyStorage(err, "collector upload synchronization failed")
	}
	if err := s.ops.close(fd); err != nil {
		fd = -1
		return "", "", 0, classifyStorage(err, "collector upload close failed")
	}
	fd = -1
	if finishSource() != nil {
		return "", "", 0, errors.New("collector upload source close failed")
	}
	success = true
	return filepath.Join(s.path, name), "sha256:" + hex.EncodeToString(h.Sum(nil)), written, nil
}

type fdWriter struct {
	fd    int
	write func(int, []byte) (int, error)
}

func (w fdWriter) Write(p []byte) (int, error) { return w.write(w.fd, p) }

func (s *Spool) EncryptedPaths(id string) (string, string, error) {
	if !s.begin() {
		return "", "", errors.New("collector spool unavailable")
	}
	defer s.end()
	d, ok := canonicalDigest(id)
	if !ok {
		return "", "", errors.New("collector object ID invalid")
	}
	n := d + ".tar.gz.age.partial"
	s.transition.Lock()
	defer s.transition.Unlock()
	if _, exists := s.active[n]; exists {
		return "", "", errors.New("collector encrypted path active")
	}
	if err := s.discardPartialLocked(n); err != nil {
		return "", "", errors.New("collector encrypted partial cleanup failed")
	}
	s.active[n] = struct{}{}
	return filepath.Join(s.path, n), filepath.Join(s.path, d+".tar.gz.age"), nil
}

func (s *Spool) DiscardEncryptedPartial(path string) error {
	if !s.begin() {
		return errors.New("collector spool unavailable")
	}
	defer s.end()
	s.transition.Lock()
	defer s.transition.Unlock()
	name, ok := s.exactChild(path)
	if !ok || !partialAgePattern.MatchString(name) {
		return errors.New("collector encrypted path invalid")
	}
	delete(s.active, name)
	if err := s.discardPartialLocked(name); err != nil {
		return errors.New("collector encrypted partial cleanup failed")
	}
	return nil
}

func (s *Spool) discardPartialLocked(name string) error {
	var metadata unix.Stat_t
	err := s.ops.fstatat(s.dirFD, name, &metadata, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil || !safeRegularMetadata(&metadata) {
		return errors.New("unsafe partial")
	}
	fd, err := s.ops.openat(s.dirFD, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	var opened unix.Stat_t
	valid := unix.Fstat(fd, &opened) == nil && safeRegularMetadata(&opened) && sameInode(&metadata, &opened)
	closeErr := s.ops.close(fd)
	if !valid || closeErr != nil || !s.sameEntry(name, &opened) {
		return errors.New("unsafe partial")
	}
	if err := s.ops.unlinkat(s.dirFD, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return s.ops.fsync(s.dirFD)
}
func (s *Spool) CommitEncrypted(partial, final string) error {
	if !s.begin() {
		return errors.New("collector spool unavailable")
	}
	defer s.end()
	s.transition.Lock()
	defer s.transition.Unlock()
	pn, fn, ok := s.validEncryptedPair(partial, final)
	if !ok {
		return errors.New("collector encrypted path invalid")
	}
	defer delete(s.active, pn)
	st, regular, err := s.entryMetadata(pn)
	if err != nil || !regular {
		return errors.New("collector encrypted partial unsafe")
	}
	fd, err := s.ops.openat(s.dirFD, pn, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return errors.New("collector encrypted partial unavailable")
	}
	var opened unix.Stat_t
	if unix.Fstat(fd, &opened) != nil || !sameInode(&st, &opened) {
		_ = s.ops.close(fd)
		return errors.New("collector encrypted partial unsafe")
	}
	if err = s.ops.fsync(fd); err != nil {
		_ = s.ops.close(fd)
		return classifyStorage(err, "collector encrypted synchronization failed")
	}
	if err = s.ops.close(fd); err != nil {
		return classifyStorage(err, "collector encrypted close failed")
	}
	if !s.sameEntry(pn, &opened) {
		return errors.New("collector encrypted partial unsafe")
	}
	if err = s.ops.renameNoReplace(s.dirFD, pn, s.dirFD, fn); err != nil {
		return classifyStorage(err, "collector encrypted commit failed")
	}
	if !s.sameEntry(fn, &opened) {
		return errors.New("collector encrypted commit unsafe")
	}
	if err = s.ops.fsync(s.dirFD); err != nil {
		return classifyStorage(err, "collector spool synchronization failed")
	}
	return nil
}
func (s *Spool) RemovePlaintext(path string) error { return s.remove(path, true) }
func (s *Spool) RemoveEncrypted(path string) error { return s.remove(path, false) }
func (s *Spool) remove(path string, plain bool) error {
	if !s.begin() {
		return errors.New("collector spool unavailable")
	}
	defer s.end()
	s.transition.Lock()
	defer s.transition.Unlock()
	name, ok := s.exactChild(path)
	valid := ok && ((plain && validUploadPartialName(name)) || (!plain && (completeAgePattern.MatchString(name) || partialAgePattern.MatchString(name))))
	if !valid {
		return errors.New("collector removal path invalid")
	}
	delete(s.active, name)
	if err := s.verifiedUnlink(name); err != nil {
		return errors.New("collector spool removal failed")
	}
	if s.ops.fsync(s.dirFD) != nil {
		return errors.New("collector spool synchronization failed")
	}
	return nil
}

func (s *Spool) verifiedUnlink(name string) error {
	st, regular, err := s.entryMetadata(name)
	if err != nil || !regular {
		return errors.New("unsafe removal")
	}
	fd, err := s.ops.openat(s.dirFD, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	var opened unix.Stat_t
	valid := unix.Fstat(fd, &opened) == nil && safeRegularMetadata(&opened) && sameInode(&st, &opened)
	closeErr := s.ops.close(fd)
	if !valid || closeErr != nil || !s.sameEntry(name, &opened) {
		return errors.New("unsafe removal")
	}
	return s.ops.unlinkat(s.dirFD, name, 0)
}

func (s *Spool) Discover() ([]PendingObject, error) {
	if !s.begin() {
		return nil, errors.New("collector spool unavailable")
	}
	defer s.end()
	s.transition.Lock()
	defer s.transition.Unlock()
	names, err := readDirectoryNames(s.dirFD)
	if err != nil {
		return nil, errors.New("collector spool discovery failed")
	}
	var out []PendingObject
	for _, n := range names {
		m := completeAgePattern.FindStringSubmatch(n)
		if m == nil {
			continue
		}
		st, regular, e := s.entryMetadata(n)
		if e != nil {
			return nil, errors.New("collector spool object unsafe")
		}
		if !regular {
			continue
		}
		fd, e := s.ops.openat(s.dirFD, n, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		if e != nil {
			return nil, errors.New("collector spool object unavailable")
		}
		var opened unix.Stat_t
		if unix.Fstat(fd, &opened) != nil || !sameInode(&st, &opened) {
			_ = s.ops.close(fd)
			return nil, errors.New("collector spool object unsafe")
		}
		f := os.NewFile(uintptr(fd), "object")
		info, e := f.Stat()
		ce := f.Close()
		if e != nil || ce != nil || !s.sameEntry(n, &opened) {
			return nil, errors.New("collector spool object unavailable")
		}
		d := m[1]
		out = append(out, PendingObject{"sha256:" + d, d, "sherpa-" + d, filepath.Join(s.path, n), info.Size(), info.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ReceivedAt.Equal(out[j].ReceivedAt) {
			return out[i].DigestHex < out[j].DigestHex
		}
		return out[i].ReceivedAt.Before(out[j].ReceivedAt)
	})
	return out, nil
}
func (s *Spool) CleanupAbandonedPartials() (int, error) {
	if !s.begin() {
		return 0, errors.New("collector spool unavailable")
	}
	defer s.end()
	s.transition.Lock()
	defer s.transition.Unlock()
	names, err := readDirectoryNames(s.dirFD)
	if err != nil {
		return 0, errors.New("collector spool cleanup failed")
	}
	removed := 0
	for _, name := range names {
		if !validUploadPartialName(name) && !partialAgePattern.MatchString(name) {
			continue
		}
		if _, active := s.active[name]; active {
			continue
		}
		if err := s.discardPartialLocked(name); err != nil {
			return removed, errors.New("collector spool cleanup failed")
		}
		removed++
	}
	return removed, nil
}

func (s *Spool) CleanupStalePartials() (int, error) { return s.cleanupStalePartialsAt(s.ops.now()) }
func (s *Spool) cleanupStalePartialsAt(now time.Time) (int, error) {
	if !s.begin() {
		return 0, errors.New("collector spool unavailable")
	}
	defer s.end()
	s.transition.Lock()
	defer s.transition.Unlock()
	names, err := readDirectoryNames(s.dirFD)
	if err != nil {
		return 0, errors.New("collector spool cleanup failed")
	}
	cut := now.Add(-s.partialMaxAge)
	removed := 0
	for _, n := range names {
		if !validUploadPartialName(n) && !partialAgePattern.MatchString(n) {
			continue
		}
		if _, active := s.active[n]; active {
			continue
		}
		st, regular, e := s.entryMetadata(n)
		if e != nil {
			return removed, errors.New("collector spool cleanup failed")
		}
		if !regular {
			continue
		}
		fd, e := s.ops.openat(s.dirFD, n, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		if errors.Is(e, unix.ENOENT) {
			continue
		}
		if e != nil {
			return removed, errors.New("collector spool cleanup failed")
		}
		var opened unix.Stat_t
		if unix.Fstat(fd, &opened) != nil || !sameInode(&st, &opened) {
			_ = s.ops.close(fd)
			return removed, errors.New("collector spool cleanup failed")
		}
		f := os.NewFile(uintptr(fd), "partial")
		info, e := f.Stat()
		ce := f.Close()
		if e != nil || ce != nil {
			return removed, errors.New("collector spool cleanup failed")
		}
		if !info.ModTime().Before(cut) {
			continue
		}
		if !s.sameEntry(n, &opened) {
			return removed, errors.New("collector spool cleanup failed")
		}
		if s.ops.unlinkat(s.dirFD, n, 0) != nil {
			return removed, errors.New("collector spool cleanup failed")
		}
		if s.ops.fsync(s.dirFD) != nil {
			return removed, errors.New("collector spool cleanup failed")
		}
		removed++
	}
	return removed, nil
}
func (s *Spool) CheckWritable() error {
	if !s.begin() {
		return errors.New("collector spool unavailable")
	}
	defer s.end()
	s.transition.Lock()
	defer s.transition.Unlock()
	n, fd, e := s.createRandomFile(".sherpa-upload-", ".upload.partial")
	if e != nil {
		return classifyStorage(e, "collector spool not writable")
	}
	cleanup := func(primary error) error {
		if fd >= 0 {
			_ = s.ops.close(fd)
			fd = -1
		}
		if s.ops.unlinkat(s.dirFD, n, 0) != nil || s.ops.fsync(s.dirFD) != nil {
			return errors.New("collector spool not writable")
		}
		return primary
	}
	if e = s.ops.fsync(fd); e != nil {
		return cleanup(classifyStorage(e, "collector spool not writable"))
	}
	if e = s.ops.close(fd); e != nil {
		fd = -1
		return cleanup(classifyStorage(e, "collector spool not writable"))
	}
	fd = -1
	return cleanup(nil)
}

func (s *Spool) createRandomFile(pre, suf string) (string, int, error) {
	for i := 0; i < 8; i++ {
		n, e := s.randomName(pre, suf)
		if e != nil {
			return "", -1, e
		}
		fd, e := s.ops.openat(s.dirFD, n, unix.O_CREAT|unix.O_EXCL|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o600)
		if errors.Is(e, unix.EEXIST) {
			continue
		}
		if e != nil {
			return "", -1, e
		}
		if forceRegularMode0600(fd) != nil {
			_ = s.ops.close(fd)
			_ = s.ops.unlinkat(s.dirFD, n, 0)
			return "", -1, errors.New("unsafe file")
		}
		return n, fd, nil
	}
	return "", -1, errors.New("random collision")
}
func (s *Spool) randomName(pre, suf string) (string, error) {
	var b [16]byte
	if _, e := io.ReadFull(s.ops.random, b[:]); e != nil {
		return "", e
	}
	return pre + hex.EncodeToString(b[:]) + suf, nil
}
func (s *Spool) entryMetadata(n string) (unix.Stat_t, bool, error) {
	var st unix.Stat_t
	if e := s.ops.fstatat(s.dirFD, n, &st, unix.AT_SYMLINK_NOFOLLOW); e != nil {
		return st, false, e
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return st, false, nil
	}
	if st.Mode&0o7777 != 0o600 || st.Uid != uint32(os.Geteuid()) || st.Gid != uint32(os.Getegid()) {
		return st, false, errors.New("unsafe")
	}
	return st, true, nil
}
func (s *Spool) sameEntry(n string, st *unix.Stat_t) bool {
	var cur unix.Stat_t
	return s.ops.fstatat(s.dirFD, n, &cur, unix.AT_SYMLINK_NOFOLLOW) == nil && safeRegularMetadata(&cur) && sameInode(st, &cur)
}
func safeRegularMetadata(st *unix.Stat_t) bool {
	return st != nil && st.Mode&unix.S_IFMT == unix.S_IFREG && st.Mode&0o7777 == 0o600 && st.Uid == uint32(os.Geteuid()) && st.Gid == uint32(os.Getegid())
}
func sameInode(a, b *unix.Stat_t) bool {
	return a != nil && b != nil && a.Dev == b.Dev && a.Ino == b.Ino && a.Mode&unix.S_IFMT == b.Mode&unix.S_IFMT
}
func classifyStorage(err error, fallback string) error {
	if errors.Is(err, unix.ENOSPC) || errors.Is(err, unix.EDQUOT) {
		return errors.New("collector insufficient storage")
	}
	return errors.New(fallback)
}
func (s *Spool) validEncryptedPair(p, f string) (string, string, bool) {
	pn, po := s.exactChild(p)
	fn, fo := s.exactChild(f)
	return pn, fn, po && fo && partialAgePattern.MatchString(pn) && completeAgePattern.MatchString(fn) && pn == fn+".partial"
}
func (s *Spool) exactChild(p string) (string, bool) {
	if p == "" || filepath.Clean(p) != p || filepath.Dir(p) != s.path {
		return "", false
	}
	n := filepath.Base(p)
	return n, n != "." && !strings.Contains(n, string(os.PathSeparator))
}
func canonicalDigest(id string) (string, bool) {
	m := objectIDPattern.FindStringSubmatch(id)
	if m == nil {
		return "", false
	}
	return m[1], true
}
func validUploadPartialName(n string) bool { return uploadPartialPattern.MatchString(n) }

func openPrivateDirectory(path string) (int, string, error) {
	start, parts, e := safePathComponents(path)
	if e != nil {
		return -1, "", e
	}
	fd, e := unix.Open(start, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if e != nil {
		return -1, "", e
	}
	for _, p := range parts {
		next, oe := unix.Openat(fd, p, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		ce := unix.Close(fd)
		if oe != nil || ce != nil {
			if oe == nil {
				_ = unix.Close(next)
			}
			return -1, "", errors.New("unsafe directory")
		}
		fd = next
	}
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Mode&0o7777 != 0o700 || st.Uid != uint32(os.Geteuid()) || st.Gid != uint32(os.Getegid()) {
		_ = unix.Close(fd)
		return -1, "", errors.New("unsafe directory")
	}
	return fd, filepath.Clean(path), nil
}
func forceRegularMode0600(fd int) error {
	if e := unix.Fchmod(fd, 0o600); e != nil {
		return e
	}
	return verifyRegularMode0600(fd)
}
func verifyRegularMode0600(fd int) error {
	var st unix.Stat_t
	if e := unix.Fstat(fd, &st); e != nil {
		return e
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&0o7777 != 0o600 || st.Uid != uint32(os.Geteuid()) || st.Gid != uint32(os.Getegid()) {
		return errors.New("unsafe file")
	}
	return nil
}
func sameDirectoryEntry(fd int, n string, st *unix.Stat_t) bool {
	var cur unix.Stat_t
	return st != nil && unix.Fstatat(fd, n, &cur, unix.AT_SYMLINK_NOFOLLOW) == nil && sameInode(st, &cur)
}
func readDirectoryNames(fd int) ([]string, error) {
	d, e := unix.Openat(fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if e != nil {
		return nil, e
	}
	f := os.NewFile(uintptr(d), "dir")
	entries, re := f.ReadDir(-1)
	ce := f.Close()
	if re != nil {
		return nil, re
	}
	if ce != nil {
		return nil, ce
	}
	names := make([]string, len(entries))
	for i := range entries {
		names[i] = entries[i].Name()
	}
	return names, nil
}
