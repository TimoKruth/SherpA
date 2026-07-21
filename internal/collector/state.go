package collector

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"
	"sort"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const ledgerRecordMaxBytes = 16 * 1024

var (
	ledgerRecordPattern = regexp.MustCompile(`^([0-9a-f]{64})\.json$`)
	retryClassPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
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
	fsync  func(int) error
	rename func(int, string, int, string) error
	random io.Reader
}

func defaultLedgerOps() ledgerOps {
	return ledgerOps{fsync: unix.Fsync, rename: unix.Renameat, random: rand.Reader}
}

type Ledger struct {
	dirFD     int
	path      string
	ops       ledgerOps
	closeOnce sync.Once
	closeErr  error
}

func OpenLedger(dir string) (*Ledger, error) { return openLedger(dir, defaultLedgerOps()) }

func openLedger(dir string, ops ledgerOps) (*Ledger, error) {
	if ops.fsync == nil || ops.rename == nil || ops.random == nil {
		return nil, errors.New("collector ledger configuration invalid")
	}
	fd, cleanPath, err := openPrivateDirectory(dir)
	if err != nil {
		return nil, errors.New("collector ledger directory unsafe")
	}
	return &Ledger{dirFD: fd, path: cleanPath, ops: ops}, nil
}

func (l *Ledger) Close() error {
	if l == nil {
		return nil
	}
	l.closeOnce.Do(func() {
		if l.dirFD >= 0 && unix.Close(l.dirFD) != nil {
			l.closeErr = errors.New("collector ledger close failed")
		}
		l.dirFD = -1
	})
	return l.closeErr
}

func (l *Ledger) Get(objectID string) (ObjectRecord, bool, error) {
	var zero ObjectRecord
	if !l.usable() {
		return zero, false, errors.New("collector ledger unavailable")
	}
	digest, ok := canonicalDigest(objectID)
	if !ok {
		return zero, false, errors.New("collector object ID invalid")
	}
	record, found, err := l.readRecord(digest+".json", false)
	if err != nil {
		return zero, false, err
	}
	if !found {
		return zero, false, nil
	}
	return record, true, nil
}

func (l *Ledger) Put(record ObjectRecord) error {
	if !l.usable() {
		return errors.New("collector ledger unavailable")
	}
	digest, ok := validateObjectRecord(record)
	if !ok {
		return errors.New("collector ledger record invalid")
	}
	data, err := json.Marshal(record)
	if err != nil || len(data) == 0 || len(data) > ledgerRecordMaxBytes {
		return errors.New("collector ledger record invalid")
	}
	name, fd, err := l.createTemp()
	if err != nil {
		return errors.New("collector ledger write failed")
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = unix.Unlinkat(l.dirFD, name, 0)
		}
	}()
	file := os.NewFile(uintptr(fd), "collector-ledger-temp")
	if file == nil {
		_ = unix.Close(fd)
		return errors.New("collector ledger write failed")
	}
	closed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
	}()
	if err := writeAll(file, data); err != nil {
		return errors.New("collector ledger write failed")
	}
	if err := l.ops.fsync(fd); err != nil {
		return errors.New("collector ledger synchronization failed")
	}
	closed = true
	if err := file.Close(); err != nil {
		return errors.New("collector ledger close failed")
	}
	target := digest + ".json"
	if err := l.ops.rename(l.dirFD, name, l.dirFD, target); err != nil {
		return errors.New("collector ledger replacement failed")
	}
	cleanup = false
	if err := l.verifyTarget(target); err != nil {
		return errors.New("collector ledger replacement unsafe")
	}
	if err := l.ops.fsync(l.dirFD); err != nil {
		return errors.New("collector ledger synchronization failed")
	}
	return nil
}

func (l *Ledger) List() ([]ObjectRecord, error) {
	if !l.usable() {
		return nil, errors.New("collector ledger unavailable")
	}
	entries, err := readDirectoryNames(l.dirFD)
	if err != nil {
		return nil, errors.New("collector ledger list failed")
	}
	records := make([]ObjectRecord, 0)
	for _, name := range entries {
		match := ledgerRecordPattern.FindStringSubmatch(name)
		if match == nil {
			continue
		}
		record, found, readErr := l.readRecord(name, true)
		if readErr != nil {
			return nil, readErr
		}
		if !found {
			continue
		}
		digest, ok := canonicalDigest(record.ObjectID)
		if !ok || digest != match[1] || record.ArchiveName != "sherpa-"+match[1] {
			return nil, errors.New("collector ledger record invalid")
		}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].ReceivedAt.Equal(records[j].ReceivedAt) {
			return records[i].ObjectID < records[j].ObjectID
		}
		return records[i].ReceivedAt.Before(records[j].ReceivedAt)
	})
	return records, nil
}

func (l *Ledger) readRecord(name string, ignoreStatic bool) (ObjectRecord, bool, error) {
	var zero ObjectRecord
	fd, err := unix.Openat(l.dirFD, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return zero, false, nil
		}
		if ignoreStatic && (errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENXIO)) {
			return zero, false, nil
		}
		return zero, false, errors.New("collector ledger record unavailable")
	}
	if err := verifyRegularMode0600(fd); err != nil {
		var stat unix.Stat_t
		statErr := unix.Fstat(fd, &stat)
		_ = unix.Close(fd)
		if ignoreStatic && statErr == nil && stat.Mode&unix.S_IFMT != unix.S_IFREG {
			return zero, false, nil
		}
		return zero, false, errors.New("collector ledger record unsafe")
	}
	file := os.NewFile(uintptr(fd), "collector-ledger-record")
	info, statErr := file.Stat()
	if statErr != nil {
		_ = file.Close()
		return zero, false, errors.New("collector ledger record unavailable")
	}
	if info.Size() <= 0 || info.Size() > ledgerRecordMaxBytes {
		_ = file.Close()
		return zero, false, errors.New("collector ledger record invalid")
	}
	decoder := json.NewDecoder(io.LimitReader(file, ledgerRecordMaxBytes+1))
	decoder.DisallowUnknownFields()
	var record ObjectRecord
	decodeErr := decoder.Decode(&record)
	var trailing any
	trailingErr := decoder.Decode(&trailing)
	closeErr := file.Close()
	if decodeErr != nil || !errors.Is(trailingErr, io.EOF) || closeErr != nil {
		return zero, false, errors.New("collector ledger record invalid")
	}
	digest, ok := validateObjectRecord(record)
	if !ok || name != digest+".json" {
		return zero, false, errors.New("collector ledger record invalid")
	}
	return record, true, nil
}

func (l *Ledger) createTemp() (string, int, error) {
	for attempt := 0; attempt < 8; attempt++ {
		var randomBytes [16]byte
		if _, err := io.ReadFull(l.ops.random, randomBytes[:]); err != nil {
			return "", -1, err
		}
		name := ".sherpa-ledger-" + hex.EncodeToString(randomBytes[:]) + ".tmp"
		fd, err := unix.Openat(l.dirFD, name, unix.O_CREAT|unix.O_EXCL|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o600)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return "", -1, err
		}
		if err := forceRegularMode0600(fd); err != nil {
			_ = unix.Close(fd)
			_ = unix.Unlinkat(l.dirFD, name, 0)
			return "", -1, err
		}
		return name, fd, nil
	}
	return "", -1, errors.New("collector ledger temporary unavailable")
}

func (l *Ledger) verifyTarget(name string) error {
	fd, err := unix.Openat(l.dirFD, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	verifyErr := verifyRegularMode0600(fd)
	closeErr := unix.Close(fd)
	if verifyErr != nil {
		return verifyErr
	}
	return closeErr
}
func (l *Ledger) usable() bool { return l != nil && l.dirFD >= 0 }

func validateObjectRecord(record ObjectRecord) (string, bool) {
	digest, ok := canonicalDigest(record.ObjectID)
	if !ok || record.ArchiveName != "sherpa-"+digest || record.CompressedSize < 0 || record.EncryptedSize < 0 || record.RetryCount < 0 || record.ReceivedAt.IsZero() || (record.LatestRetryClass != "" && !retryClassPattern.MatchString(record.LatestRetryClass)) {
		return "", false
	}
	if record.StoredAt != nil && record.StoredAt.IsZero() {
		return "", false
	}
	if record.LastAttemptAt != nil && record.LastAttemptAt.IsZero() {
		return "", false
	}
	return digest, true
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := writer.Write(data)
		if n < 0 || n > len(data) {
			return errors.New("invalid write count")
		}
		data = data[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}
