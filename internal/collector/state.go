package collector

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

const ledgerRecordMaxBytes = 16 * 1024

var ledgerRecordPattern = regexp.MustCompile(`^([0-9a-f]{64})\.json$`)

type ObjectRecord struct {
	ObjectID         string     `json:"object_id"`
	ArchiveName      string     `json:"archive_name"`
	CompressedSize   int64      `json:"compressed_size"`
	EncryptedSize    int64      `json:"encrypted_size"`
	ReceivedAt       time.Time  `json:"received_at"`
	StoredAt         *time.Time `json:"stored_at,omitempty"`
	LastAttemptAt    *time.Time `json:"last_attempt_at,omitempty"`
	LatestRetryClass string     `json:"latest_retry_class,omitempty"`
	RetryCount       int        `json:"retry_count"`
}
type ledgerOps struct {
	fsync   func(int) error
	rename  func(int, string, int, string) error
	random  io.Reader
	openat  func(int, string, int, uint32) (int, error)
	fstatat func(int, string, *unix.Stat_t, int) error
	close   func(int) error
}

func defaultLedgerOps() ledgerOps {
	return ledgerOps{unix.Fsync, unix.Renameat, rand.Reader, unix.Openat, unix.Fstatat, unix.Close}
}

type Ledger struct {
	dirFD      int
	path       string
	ops        ledgerOps
	life       sync.RWMutex
	transition sync.Mutex
	closed     bool
}

func OpenLedger(dir string) (*Ledger, error) { return openLedger(dir, defaultLedgerOps()) }
func openLedger(dir string, ops ledgerOps) (*Ledger, error) {
	if ops.fsync == nil || ops.rename == nil || ops.random == nil || ops.openat == nil || ops.fstatat == nil || ops.close == nil {
		return nil, errors.New("collector ledger configuration invalid")
	}
	abs, e := filepath.Abs(dir)
	if e != nil {
		return nil, errors.New("collector ledger directory unsafe")
	}
	abs = filepath.Clean(abs)
	fd, _, e := openPrivateDirectory(abs)
	if e != nil {
		return nil, errors.New("collector ledger directory unsafe")
	}
	return &Ledger{dirFD: fd, path: abs, ops: ops}, nil
}
func (l *Ledger) begin() bool {
	l.life.RLock()
	if l.closed || l.dirFD < 0 {
		l.life.RUnlock()
		return false
	}
	return true
}
func (l *Ledger) end() { l.life.RUnlock() }
func (l *Ledger) Close() error {
	if l == nil {
		return nil
	}
	l.life.Lock()
	defer l.life.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	if l.ops.close(l.dirFD) != nil {
		l.dirFD = -1
		return errors.New("collector ledger close failed")
	}
	l.dirFD = -1
	return nil
}
func (l *Ledger) Get(id string) (ObjectRecord, bool, error) {
	var z ObjectRecord
	if !l.begin() {
		return z, false, errors.New("collector ledger unavailable")
	}
	defer l.end()
	l.transition.Lock()
	defer l.transition.Unlock()
	d, ok := canonicalDigest(id)
	if !ok {
		return z, false, errors.New("collector object ID invalid")
	}
	return l.readRecord(d+".json", false)
}
func (l *Ledger) Put(r ObjectRecord) error {
	if !l.begin() {
		return errors.New("collector ledger unavailable")
	}
	defer l.end()
	l.transition.Lock()
	defer l.transition.Unlock()
	d, ok := validateObjectRecord(r)
	if !ok {
		return errors.New("collector ledger record invalid")
	}
	data, e := json.Marshal(r)
	if e != nil || len(data) == 0 || len(data) > ledgerRecordMaxBytes {
		return errors.New("collector ledger record invalid")
	}
	n, fd, e := l.createTemp()
	if e != nil {
		return errors.New("collector ledger write failed")
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = unix.Unlinkat(l.dirFD, n, 0)
		}
	}()
	if e = writeFD(fd, data); e != nil {
		_ = l.ops.close(fd)
		return errors.New("collector ledger write failed")
	}
	if e = l.ops.fsync(fd); e != nil {
		_ = l.ops.close(fd)
		return errors.New("collector ledger synchronization failed")
	}
	var opened unix.Stat_t
	if unix.Fstat(fd, &opened) != nil {
		_ = l.ops.close(fd)
		return errors.New("collector ledger replacement unsafe")
	}
	if e = l.ops.close(fd); e != nil {
		return errors.New("collector ledger close failed")
	}
	target := d + ".json"
	if e = l.ops.rename(l.dirFD, n, l.dirFD, target); e != nil {
		return errors.New("collector ledger replacement failed")
	}
	cleanup = false
	if !l.sameEntry(target, &opened) {
		return errors.New("collector ledger replacement unsafe")
	}
	if l.ops.fsync(l.dirFD) != nil {
		return errors.New("collector ledger synchronization failed")
	}
	return nil
}
func (l *Ledger) List() ([]ObjectRecord, error) {
	if !l.begin() {
		return nil, errors.New("collector ledger unavailable")
	}
	defer l.end()
	l.transition.Lock()
	defer l.transition.Unlock()
	names, e := readDirectoryNames(l.dirFD)
	if e != nil {
		return nil, errors.New("collector ledger list failed")
	}
	var out []ObjectRecord
	for _, n := range names {
		m := ledgerRecordPattern.FindStringSubmatch(n)
		if m == nil {
			continue
		}
		r, found, e := l.readRecord(n, true)
		if e != nil {
			return nil, e
		}
		if !found {
			continue
		}
		d, ok := canonicalDigest(r.ObjectID)
		if !ok || d != m[1] || r.ArchiveName != "sherpa-"+m[1] {
			return nil, errors.New("collector ledger record invalid")
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ReceivedAt.Equal(out[j].ReceivedAt) {
			return out[i].ObjectID < out[j].ObjectID
		}
		return out[i].ReceivedAt.Before(out[j].ReceivedAt)
	})
	return out, nil
}
func (l *Ledger) readRecord(n string, ignoreStatic bool) (ObjectRecord, bool, error) {
	var z ObjectRecord
	var meta unix.Stat_t
	e := l.ops.fstatat(l.dirFD, n, &meta, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(e, unix.ENOENT) {
		return z, false, nil
	}
	if e != nil {
		return z, false, errors.New("collector ledger record unavailable")
	}
	if meta.Mode&unix.S_IFMT != unix.S_IFREG {
		if ignoreStatic {
			return z, false, nil
		}
		return z, false, errors.New("collector ledger record unsafe")
	}
	if meta.Mode&0o7777 != 0o600 || meta.Uid != uint32(os.Geteuid()) || meta.Gid != uint32(os.Getegid()) {
		return z, false, errors.New("collector ledger record unsafe")
	}
	fd, e := l.ops.openat(l.dirFD, n, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if e != nil {
		return z, false, errors.New("collector ledger record unavailable")
	}
	var opened unix.Stat_t
	if unix.Fstat(fd, &opened) != nil || !sameInode(&meta, &opened) {
		_ = l.ops.close(fd)
		return z, false, errors.New("collector ledger record unsafe")
	}
	f := os.NewFile(uintptr(fd), "record")
	info, e := f.Stat()
	if e != nil {
		_ = f.Close()
		return z, false, errors.New("collector ledger record unavailable")
	}
	if info.Size() <= 0 || info.Size() > ledgerRecordMaxBytes {
		_ = f.Close()
		return z, false, errors.New("collector ledger record invalid")
	}
	dec := json.NewDecoder(io.LimitReader(f, ledgerRecordMaxBytes+1))
	dec.DisallowUnknownFields()
	var r ObjectRecord
	de := dec.Decode(&r)
	var trailing any
	te := dec.Decode(&trailing)
	ce := f.Close()
	if de != nil || !errors.Is(te, io.EOF) || ce != nil {
		return z, false, errors.New("collector ledger record invalid")
	}
	d, ok := validateObjectRecord(r)
	if !ok || n != d+".json" {
		return z, false, errors.New("collector ledger record invalid")
	}
	return r, true, nil
}
func (l *Ledger) createTemp() (string, int, error) {
	for i := 0; i < 8; i++ {
		var b [16]byte
		if _, e := io.ReadFull(l.ops.random, b[:]); e != nil {
			return "", -1, e
		}
		n := ".sherpa-ledger-" + hex.EncodeToString(b[:]) + ".tmp"
		fd, e := l.ops.openat(l.dirFD, n, unix.O_CREAT|unix.O_EXCL|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o600)
		if errors.Is(e, unix.EEXIST) {
			continue
		}
		if e != nil {
			return "", -1, e
		}
		if forceRegularMode0600(fd) != nil {
			_ = l.ops.close(fd)
			_ = unix.Unlinkat(l.dirFD, n, 0)
			return "", -1, e
		}
		return n, fd, nil
	}
	return "", -1, errors.New("collector ledger temporary unavailable")
}
func (l *Ledger) sameEntry(n string, st *unix.Stat_t) bool {
	var cur unix.Stat_t
	return l.ops.fstatat(l.dirFD, n, &cur, unix.AT_SYMLINK_NOFOLLOW) == nil && safeRegularMetadata(&cur) && sameInode(st, &cur)
}
func validateObjectRecord(r ObjectRecord) (string, bool) {
	d, ok := canonicalDigest(r.ObjectID)
	if !ok || r.ArchiveName != "sherpa-"+d || r.CompressedSize <= 0 || r.EncryptedSize <= 0 || r.RetryCount < 0 || r.ReceivedAt.IsZero() || !allowedRetryClass(r.LatestRetryClass) {
		return "", false
	}
	if r.StoredAt != nil && (r.StoredAt.IsZero() || r.StoredAt.Before(r.ReceivedAt)) {
		return "", false
	}
	if r.LastAttemptAt != nil && (r.LastAttemptAt.IsZero() || r.LastAttemptAt.Before(r.ReceivedAt)) {
		return "", false
	}
	if r.RetryCount == 0 {
		return d, r.LastAttemptAt == nil && r.LatestRetryClass == ""
	}
	return d, r.LastAttemptAt != nil && r.LatestRetryClass != ""
}
func allowedRetryClass(s string) bool {
	switch s {
	case "", "temporary", "backend_unavailable", "backend_timeout", "backend_failed", "remote_verification_failed", "local_state_failed", "cancelled":
		return true
	}
	return false
}
func writeFD(fd int, p []byte) error {
	for len(p) > 0 {
		n, e := unix.Write(fd, p)
		if n > 0 {
			p = p[n:]
		}
		if e != nil {
			return e
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}
func writeAll(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, e := w.Write(p)
		if n < 0 || n > len(p) {
			return errors.New("invalid write count")
		}
		p = p[n:]
		if e != nil {
			return e
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}
