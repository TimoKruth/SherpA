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

var (
	ledgerRecordPattern = regexp.MustCompile(`^([0-9a-f]{64})\.json$`)
	ledgerTempPattern   = regexp.MustCompile(`^\.sherpa-ledger-[0-9a-f]{32}\.tmp$`)
)

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
	fsync     func(int) error
	rename    func(int, string, int, string) error
	random    io.Reader
	openat    func(int, string, int, uint32) (int, error)
	fstatat   func(int, string, *unix.Stat_t, int) error
	close     func(int) error
	unlinkat  func(int, string, int) error
	forceMode func(int) error
}

func defaultLedgerOps() ledgerOps {
	return ledgerOps{unix.Fsync, unix.Renameat, rand.Reader, unix.Openat, unix.Fstatat, unix.Close, unix.Unlinkat, forceRegularMode0600}
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
	if ops.fsync == nil || ops.rename == nil || ops.random == nil || ops.openat == nil || ops.fstatat == nil || ops.close == nil || ops.unlinkat == nil || ops.forceMode == nil {
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
	ledger := &Ledger{dirFD: fd, path: abs, ops: ops}
	if ledger.recoverTemps() != nil {
		_ = ops.close(fd)
		return nil, errors.New("collector ledger recovery failed")
	}
	return ledger, nil
}

func (l *Ledger) recoverTemps() error {
	names, err := readDirectoryNames(l.dirFD)
	if err != nil {
		return err
	}
	for _, name := range names {
		if !ledgerTempPattern.MatchString(name) {
			continue
		}
		if err := l.removeVerifiedTemp(name); err != nil {
			return err
		}
	}
	return nil
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
		var unsafe ledgerUnsafeTempError
		if errors.As(e, &unsafe) {
			if unsafe.cleanupFailed {
				return errors.New("collector ledger cleanup failed")
			}
			return errors.New("collector ledger temporary unsafe")
		}
		return errors.New("collector ledger write failed")
	}
	fail := func(primary error) error {
		if fd >= 0 {
			_ = l.ops.close(fd)
			fd = -1
		}
		if l.removeVerifiedTemp(n) != nil {
			return errors.New("collector ledger cleanup failed")
		}
		return primary
	}
	if e = writeFD(fd, data); e != nil {
		return fail(errors.New("collector ledger write failed"))
	}
	if e = l.ops.fsync(fd); e != nil {
		return fail(errors.New("collector ledger synchronization failed"))
	}
	var opened unix.Stat_t
	if unix.Fstat(fd, &opened) != nil || !safeRegularMetadata(&opened) {
		return fail(errors.New("collector ledger replacement unsafe"))
	}
	if e = l.ops.close(fd); e != nil {
		fd = -1
		return fail(errors.New("collector ledger close failed"))
	}
	fd = -1
	target := d + ".json"
	if e = l.ops.rename(l.dirFD, n, l.dirFD, target); e != nil {
		return fail(errors.New("collector ledger replacement failed"))
	}
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
	r, de := decodeObjectRecord(io.LimitReader(f, ledgerRecordMaxBytes+1))
	ce := f.Close()
	if de != nil || ce != nil {
		return z, false, errors.New("collector ledger record invalid")
	}
	d, ok := validateObjectRecord(r)
	if !ok || n != d+".json" {
		return z, false, errors.New("collector ledger record invalid")
	}
	return r, true, nil
}
func decodeObjectRecord(reader io.Reader) (ObjectRecord, error) {
	var record ObjectRecord
	dec := json.NewDecoder(reader)
	start, err := dec.Token()
	if err != nil || start != json.Delim('{') {
		return record, errors.New("invalid ledger object")
	}
	seen := make(map[string]struct{}, 9)
	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			return ObjectRecord{}, errors.New("invalid ledger key")
		}
		key, ok := token.(string)
		if !ok {
			return ObjectRecord{}, errors.New("invalid ledger key")
		}
		if _, duplicate := seen[key]; duplicate {
			return ObjectRecord{}, errors.New("duplicate ledger key")
		}
		seen[key] = struct{}{}
		switch key {
		case "object_id":
			err = dec.Decode(&record.ObjectID)
		case "archive_name":
			err = dec.Decode(&record.ArchiveName)
		case "compressed_size":
			err = dec.Decode(&record.CompressedSize)
		case "encrypted_size":
			err = dec.Decode(&record.EncryptedSize)
		case "received_at":
			err = dec.Decode(&record.ReceivedAt)
		case "stored_at":
			err = dec.Decode(&record.StoredAt)
		case "last_attempt_at":
			err = dec.Decode(&record.LastAttemptAt)
		case "latest_retry_class":
			err = dec.Decode(&record.LatestRetryClass)
		case "retry_count":
			err = dec.Decode(&record.RetryCount)
		default:
			return ObjectRecord{}, errors.New("unknown ledger key")
		}
		if err != nil {
			return ObjectRecord{}, errors.New("invalid ledger value")
		}
	}
	end, err := dec.Token()
	if err != nil || end != json.Delim('}') {
		return ObjectRecord{}, errors.New("invalid ledger object")
	}
	if _, err = dec.Token(); !errors.Is(err, io.EOF) {
		return ObjectRecord{}, errors.New("trailing ledger data")
	}
	return record, nil
}

type ledgerUnsafeTempError struct {
	cause         error
	cleanupFailed bool
}

func (e ledgerUnsafeTempError) Error() string { return e.cause.Error() }
func (e ledgerUnsafeTempError) Unwrap() error { return e.cause }

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
		var created unix.Stat_t
		if statErr := unix.Fstat(fd, &created); statErr != nil {
			_ = l.ops.close(fd)
			return "", -1, ledgerUnsafeTempError{cause: statErr, cleanupFailed: true}
		}
		if modeErr := l.ops.forceMode(fd); modeErr != nil {
			closeErr := l.ops.close(fd)
			cleanupErr := l.removeCreatedTemp(n, &created)
			return "", -1, ledgerUnsafeTempError{
				cause:         modeErr,
				cleanupFailed: closeErr != nil || cleanupErr != nil,
			}
		}
		return n, fd, nil
	}
	return "", -1, errors.New("collector ledger temporary unavailable")
}

func (l *Ledger) removeCreatedTemp(name string, expected *unix.Stat_t) error {
	if !ledgerTempPattern.MatchString(name) || !ownedRegularMetadata(expected) {
		return errors.New("unsafe ledger temporary")
	}
	var current unix.Stat_t
	if l.ops.fstatat(l.dirFD, name, &current, unix.AT_SYMLINK_NOFOLLOW) != nil || !ownedRegularMetadata(&current) || !sameInode(expected, &current) {
		return errors.New("unsafe ledger temporary")
	}
	if err := l.ops.unlinkat(l.dirFD, name, 0); err != nil {
		return err
	}
	return l.ops.fsync(l.dirFD)
}

func ownedRegularMetadata(st *unix.Stat_t) bool {
	return st != nil && st.Mode&unix.S_IFMT == unix.S_IFREG && st.Uid == uint32(os.Geteuid()) && st.Gid == uint32(os.Getegid())
}

func (l *Ledger) removeVerifiedTemp(name string) error {
	if !ledgerTempPattern.MatchString(name) {
		return errors.New("invalid ledger temporary")
	}
	var expected unix.Stat_t
	if l.ops.fstatat(l.dirFD, name, &expected, unix.AT_SYMLINK_NOFOLLOW) != nil || !safeRegularMetadata(&expected) {
		return errors.New("unsafe ledger temporary")
	}
	fd, err := l.ops.openat(l.dirFD, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	var opened unix.Stat_t
	valid := unix.Fstat(fd, &opened) == nil && safeRegularMetadata(&opened) && sameInode(&expected, &opened)
	closeErr := l.ops.close(fd)
	if !valid || closeErr != nil || !l.sameEntry(name, &opened) {
		return errors.New("unsafe ledger temporary")
	}
	if err := l.ops.unlinkat(l.dirFD, name, 0); err != nil {
		return err
	}
	return l.ops.fsync(l.dirFD)
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
