package collector

import (
	"context"
	"errors"
	"io"
	"math"
	"sync"
	"time"

	"sherpa/internal/recoveryarchive"
)

type ResultStatus string

const (
	ResultStored   ResultStatus = "stored"
	ResultExisting ResultStatus = "existing"
)

type Result struct {
	ObjectID string
	Status   ResultStatus
}

type Ingestor interface {
	Ingest(context.Context, io.Reader, int64) (Result, error)
}

type serviceCrashPoint string

const (
	crashAfterUploadPartial    serviceCrashPoint = "upload-partial-write"
	crashAfterAgePartial       serviceCrashPoint = "age-partial-write"
	crashAfterAgeRename        serviceCrashPoint = "age-rename"
	crashAfterPlaintextRemoval serviceCrashPoint = "plaintext-removal"
	crashAfterRemoteCreate     serviceCrashPoint = "remote-create"
	crashAfterRemotePresence   serviceCrashPoint = "remote-presence-query"
	crashAfterLedgerUpdate     serviceCrashPoint = "ledger-update"
	crashAfterAgeRemoval       serviceCrashPoint = "age-removal"
)

type serviceOps struct {
	now          func() time.Time
	validateFile func(context.Context, string, recoveryarchive.Limits) (recoveryarchive.Report, error)
	encryptFile  func(context.Context, string, string) (int64, error)
	crash        func(serviceCrashPoint) error
}

type Service struct {
	spool     *Spool
	ledger    *Ledger
	encryptor *Encryptor
	backend   Backend
	status    *StatusTracker
	limits    recoveryarchive.Limits
	ops       serviceOps
	backendMu sync.Mutex
}

func NewService(spool *Spool, ledger *Ledger, encryptor *Encryptor, backend Backend, status *StatusTracker, limits recoveryarchive.Limits) *Service {
	return newService(spool, ledger, encryptor, backend, status, limits, serviceOps{})
}

func newService(spool *Spool, ledger *Ledger, encryptor *Encryptor, backend Backend, status *StatusTracker, limits recoveryarchive.Limits, ops serviceOps) *Service {
	if ops.now == nil {
		ops.now = time.Now
	}
	if ops.validateFile == nil {
		ops.validateFile = recoveryarchive.ValidateFile
	}
	if ops.encryptFile == nil && encryptor != nil {
		ops.encryptFile = encryptor.EncryptFile
	}
	if ops.crash == nil {
		ops.crash = func(serviceCrashPoint) error { return nil }
	}
	return &Service{spool: spool, ledger: ledger, encryptor: encryptor, backend: backend, status: status, limits: limits, ops: ops}
}

func (s *Service) Ingest(ctx context.Context, source io.Reader, length int64) (Result, error) {
	if s == nil || s.spool == nil || s.ledger == nil || s.backend == nil || s.status == nil || s.ops.now == nil || s.ops.validateFile == nil || s.ops.encryptFile == nil || ctx == nil {
		return Result{}, errors.New("collector service unavailable")
	}
	if ctx.Err() != nil {
		return Result{}, errors.New("collector ingestion canceled")
	}
	plaintextPath, objectID, compressedSize, err := s.spool.Receive(ctx, source, length)
	if err != nil {
		return Result{}, fixedIngestError(err)
	}
	if err := s.crash(crashAfterUploadPartial); err != nil {
		return Result{}, err
	}
	if _, err := s.ops.validateFile(ctx, plaintextPath, s.limits); err != nil {
		if removeErr := s.spool.RemovePlaintext(plaintextPath); removeErr != nil {
			return Result{}, s.localFailure()
		}
		if ctx.Err() != nil {
			return Result{}, errors.New("collector ingestion canceled")
		}
		return Result{}, errors.New("collector archive invalid")
	}

	pending, reused, err := s.findDurableObject(objectID)
	if err != nil {
		return Result{}, s.localFailure()
	}
	if !reused {
		partialPath, finalPath, pathErr := s.spool.EncryptedPaths(objectID)
		if pathErr != nil {
			return Result{}, s.localFailure()
		}
		encryptedSize, encryptErr := s.ops.encryptFile(ctx, plaintextPath, partialPath)
		if encryptErr != nil {
			if ctx.Err() != nil {
				return Result{}, errors.New("collector ingestion canceled")
			}
			return Result{}, errors.New("collector encryption failed")
		}
		if err := s.crash(crashAfterAgePartial); err != nil {
			return Result{}, err
		}
		if err := s.spool.CommitEncrypted(partialPath, finalPath); err != nil {
			return Result{}, s.localFailure()
		}
		digest, ok := canonicalDigest(objectID)
		if !ok || encryptedSize <= 0 {
			return Result{}, s.localFailure()
		}
		pending = PendingObject{
			ObjectID:      objectID,
			DigestHex:     digest,
			ArchiveName:   "sherpa-" + digest,
			EncryptedPath: finalPath,
			EncryptedSize: encryptedSize,
			ReceivedAt:    s.ops.now(),
		}
	}

	record, err := s.ensurePendingRecord(pending, compressedSize)
	if err != nil {
		return Result{}, err
	}
	pending.ReceivedAt = record.ReceivedAt
	if err := s.crash(crashAfterAgeRename); err != nil {
		return Result{}, err
	}
	if err := s.spool.RemovePlaintext(plaintextPath); err != nil {
		return Result{}, s.localFailure()
	}
	if err := s.crash(crashAfterPlaintextRemoval); err != nil {
		return Result{}, err
	}
	if record.StoredAt == nil {
		s.status.RecordPending(pending.ObjectID, pending.ReceivedAt)
	}
	existing, err := s.CommitPending(ctx, pending)
	if err != nil {
		return Result{}, err
	}
	status := ResultStored
	if existing {
		status = ResultExisting
	}
	return Result{ObjectID: objectID, Status: status}, nil
}

func (s *Service) CommitPending(ctx context.Context, object PendingObject) (bool, error) {
	if s == nil || s.ledger == nil || s.spool == nil || s.backend == nil || s.status == nil || ctx == nil {
		return false, errors.New("collector service unavailable")
	}
	record, found, err := s.ledger.Get(object.ObjectID)
	if err != nil || !found || !recordMatchesPending(record, object) {
		return false, s.localFailure()
	}

	existing := false
	s.backendMu.Lock()
	present, existsErr := s.backend.Exists(ctx, object.ObjectID)
	if existsErr != nil {
		s.backendMu.Unlock()
		return false, s.backendFailure(record, existsErr)
	}
	if err := s.crash(crashAfterRemotePresence); err != nil {
		s.backendMu.Unlock()
		return false, err
	}
	if present {
		existing = true
	} else {
		createErr := s.backend.Create(ctx, object)
		if err := s.crash(crashAfterRemoteCreate); err != nil {
			s.backendMu.Unlock()
			return false, err
		}
		present, existsErr = s.backend.Exists(ctx, object.ObjectID)
		if existsErr == nil {
			if err := s.crash(crashAfterRemotePresence); err != nil {
				s.backendMu.Unlock()
				return false, err
			}
		}
		if existsErr != nil {
			s.backendMu.Unlock()
			return false, s.backendFailure(record, existsErr)
		}
		if !present {
			s.backendMu.Unlock()
			if createErr != nil {
				return false, s.backendFailure(record, createErr)
			}
			return false, s.backendFailureClass(record, "remote_verification_failed")
		}
	}
	s.backendMu.Unlock()
	if err := s.finishVerified(object, record); err != nil {
		return false, err
	}
	return existing, nil
}

func (s *Service) queryRemotePresence(ctx context.Context, record ObjectRecord, objectID string) (bool, error) {
	s.backendMu.Lock()
	present, err := s.backend.Exists(ctx, objectID)
	if err == nil {
		if crashErr := s.crash(crashAfterRemotePresence); crashErr != nil {
			s.backendMu.Unlock()
			return false, crashErr
		}
	}
	s.backendMu.Unlock()
	if err != nil {
		return false, s.backendFailure(record, err)
	}
	return present, nil
}

func (s *Service) finishVerified(object PendingObject, record ObjectRecord) error {
	storedAt := s.ops.now()
	if storedAt.IsZero() || storedAt.Before(record.ReceivedAt) {
		return s.localFailure()
	}
	if record.StoredAt == nil {
		record.StoredAt = timePointer(storedAt)
	}
	if err := s.ledger.Put(record); err != nil {
		return s.localFailure()
	}
	if err := s.crash(crashAfterLedgerUpdate); err != nil {
		return err
	}
	if err := s.spool.RemoveEncrypted(object.EncryptedPath); err != nil {
		return s.localFailure()
	}
	if err := s.crash(crashAfterAgeRemoval); err != nil {
		return err
	}
	s.status.RemovePending(object.ObjectID)
	s.status.RecordSuccess(*record.StoredAt)
	return nil
}

func (s *Service) findDurableObject(objectID string) (PendingObject, bool, error) {
	objects, err := s.spool.Discover()
	if err != nil {
		return PendingObject{}, false, err
	}
	for _, object := range objects {
		if object.ObjectID == objectID {
			return object, true, nil
		}
	}
	return PendingObject{}, false, nil
}

func (s *Service) ensurePendingRecord(object PendingObject, compressedSize int64) (ObjectRecord, error) {
	record, found, err := s.ledger.Get(object.ObjectID)
	if err != nil {
		return ObjectRecord{}, s.localFailure()
	}
	if found {
		if !recordMatchesPending(record, object) {
			return ObjectRecord{}, s.localFailure()
		}
		return record, nil
	}
	if compressedSize <= 0 || object.ReceivedAt.IsZero() {
		return ObjectRecord{}, s.localFailure()
	}
	record = ObjectRecord{
		ObjectID:       object.ObjectID,
		ArchiveName:    object.ArchiveName,
		CompressedSize: compressedSize,
		EncryptedSize:  object.EncryptedSize,
		ReceivedAt:     object.ReceivedAt,
		RetryCount:     0,
	}
	if err := s.ledger.Put(record); err != nil {
		return ObjectRecord{}, s.localFailure()
	}
	return record, nil
}

func recordMatchesPending(record ObjectRecord, object PendingObject) bool {
	digest, ok := canonicalDigest(object.ObjectID)
	return ok && object.DigestHex == digest && object.ArchiveName == "sherpa-"+digest &&
		record.ObjectID == object.ObjectID && record.ArchiveName == object.ArchiveName &&
		record.EncryptedSize == object.EncryptedSize && !record.ReceivedAt.IsZero()
}

func (s *Service) backendFailure(record ObjectRecord, backendErr error) error {
	return s.backendFailureClass(record, retryClass(backendErr))
}

func (s *Service) backendFailureClass(record ObjectRecord, class string) error {
	attemptedAt := s.ops.now()
	if attemptedAt.IsZero() || attemptedAt.Before(record.ReceivedAt) {
		return s.localFailure()
	}
	if record.RetryCount < math.MaxInt {
		record.RetryCount++
	}
	record.LastAttemptAt = timePointer(attemptedAt)
	record.LatestRetryClass = class
	if err := s.ledger.Put(record); err != nil {
		return s.localFailure()
	}
	switch class {
	case "backend_timeout":
		return errors.New("collector backend timeout")
	case "cancelled":
		return errors.New("collector backend cancelled")
	case "remote_verification_failed":
		return errors.New("collector remote verification failed")
	default:
		return errors.New("collector backend unavailable")
	}
}

func retryClass(err error) string {
	if err == nil {
		return "backend_failed"
	}
	switch err.Error() {
	case "collector backend timeout":
		return "backend_timeout"
	case "collector backend cancelled", "collector backend canceled":
		return "cancelled"
	case "collector remote verification failed":
		return "remote_verification_failed"
	case "collector backend unavailable":
		return "backend_unavailable"
	case "collector backend temporary":
		return "temporary"
	default:
		return "backend_failed"
	}
}

func (s *Service) crash(point serviceCrashPoint) error {
	if s.ops.crash != nil && s.ops.crash(point) != nil {
		return errors.New("collector operation interrupted")
	}
	return nil
}

func (s *Service) localFailure() error {
	if s != nil && s.status != nil {
		s.status.RecordTerminalLocalError()
	}
	return errors.New("collector local state failed")
}

func fixedIngestError(err error) error {
	if err == nil {
		return nil
	}
	switch err.Error() {
	case "collector upload source not closable", "collector upload length mismatch", "collector upload invalid", "collector insufficient storage", "collector upload canceled", "collector upload source close failed":
		return errors.New(err.Error())
	default:
		return errors.New("collector upload failed")
	}
}

func timePointer(value time.Time) *time.Time {
	copy := value
	return &copy
}
