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
	crashAfterPendingLedger    serviceCrashPoint = "pending-ledger-write"
	crashAfterAgeRename        serviceCrashPoint = "age-rename"
	crashAfterPlaintextRemoval serviceCrashPoint = "plaintext-removal"
	crashAfterRemoteCreate     serviceCrashPoint = "remote-create"
	crashAfterRemotePresence   serviceCrashPoint = "remote-presence-query"
	crashAfterLedgerUpdate     serviceCrashPoint = "stored-ledger-write"
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
	publishMu sync.Mutex
	commitMu  sync.Mutex
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
	receivedAt := s.ops.now()
	if receivedAt.IsZero() {
		if s.cleanupOwned(plaintextPath, "", "") != nil {
			return Result{}, s.localFailure()
		}
		return Result{}, s.localFailure()
	}
	if err := s.crash(crashAfterUploadPartial); err != nil {
		return Result{}, err
	}
	if _, err := s.ops.validateFile(ctx, plaintextPath, s.limits); err != nil {
		if s.cleanupOwned(plaintextPath, "", "") != nil {
			return Result{}, s.localFailure()
		}
		if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return Result{}, errors.New("collector ingestion canceled")
		}
		return Result{}, errors.New("collector archive invalid")
	}

	pending, reused, err := s.findDurableObject(objectID)
	if err != nil {
		if s.cleanupOwned(plaintextPath, "", "") != nil {
			return Result{}, s.localFailure()
		}
		return Result{}, s.localFailure()
	}
	var record ObjectRecord
	if reused {
		var found bool
		record, found, err = s.ledger.Get(objectID)
		if err != nil || !found || record.CompressedSize != compressedSize {
			if s.cleanupOwned(plaintextPath, "", "") != nil {
				return Result{}, s.localFailure()
			}
			return Result{}, s.localFailure()
		}
		pending.ReceivedAt = record.ReceivedAt
		if !recordMatchesPending(record, pending) {
			if s.cleanupOwned(plaintextPath, "", "") != nil {
				return Result{}, s.localFailure()
			}
			return Result{}, s.localFailure()
		}
	} else {
		partialPath, finalPath, pathErr := s.spool.EncryptedPaths(objectID)
		if pathErr != nil {
			if s.cleanupOwned(plaintextPath, "", "") != nil {
				return Result{}, s.localFailure()
			}
			return Result{}, s.localFailure()
		}
		encryptedSize, encryptErr := s.ops.encryptFile(ctx, plaintextPath, partialPath)
		if encryptErr != nil {
			if s.cleanupOwned(plaintextPath, partialPath, "") != nil {
				return Result{}, s.localFailure()
			}
			if ctx.Err() != nil || errors.Is(encryptErr, context.Canceled) || errors.Is(encryptErr, context.DeadlineExceeded) {
				return Result{}, errors.New("collector ingestion canceled")
			}
			return Result{}, errors.New("collector encryption failed")
		}
		if err := s.crash(crashAfterAgePartial); err != nil {
			return Result{}, err
		}
		digest, ok := canonicalDigest(objectID)
		if !ok || encryptedSize <= 0 {
			if s.cleanupOwned(plaintextPath, partialPath, "") != nil {
				return Result{}, s.localFailure()
			}
			return Result{}, s.localFailure()
		}
		pending = PendingObject{
			ObjectID:      objectID,
			DigestHex:     digest,
			ArchiveName:   "sherpa-" + digest,
			EncryptedPath: finalPath,
			EncryptedSize: encryptedSize,
			ReceivedAt:    receivedAt,
		}
		record, err = s.publishEncrypted(plaintextPath, partialPath, finalPath, pending, compressedSize)
		if err != nil {
			return Result{}, err
		}
		pending.ReceivedAt = record.ReceivedAt
	}

	if reused {
		if err := s.spool.RemovePlaintext(plaintextPath); err != nil {
			return Result{}, s.localFailure()
		}
		if err := s.crash(crashAfterPlaintextRemoval); err != nil {
			return Result{}, err
		}
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

func (s *Service) publishEncrypted(plaintextPath, partialPath, finalPath string, object PendingObject, compressedSize int64) (ObjectRecord, error) {
	s.publishMu.Lock()
	defer s.publishMu.Unlock()

	record, cleanupPending, err := s.ensurePendingRecord(object, compressedSize)
	if err != nil {
		pendingID := ""
		if cleanupPending {
			pendingID = object.ObjectID
		}
		if s.cleanupOwned(plaintextPath, partialPath, pendingID) != nil {
			return ObjectRecord{}, s.localFailure()
		}
		return ObjectRecord{}, err
	}
	object.ReceivedAt = record.ReceivedAt
	if err := s.crash(crashAfterPendingLedger); err != nil {
		return ObjectRecord{}, err
	}
	if err := s.spool.CommitEncrypted(partialPath, finalPath); err != nil {
		published, foundPublished, discoverErr := s.findDurableObject(object.ObjectID)
		if discoverErr != nil || foundPublished || published.ObjectID != "" {
			return ObjectRecord{}, s.localFailure()
		}
		if s.cleanupOwned(plaintextPath, partialPath, object.ObjectID) != nil {
			return ObjectRecord{}, s.localFailure()
		}
		return ObjectRecord{}, s.localFailure()
	}
	if err := s.crash(crashAfterAgeRename); err != nil {
		return ObjectRecord{}, err
	}
	if err := s.spool.RemovePlaintext(plaintextPath); err != nil {
		return ObjectRecord{}, s.localFailure()
	}
	if err := s.crash(crashAfterPlaintextRemoval); err != nil {
		return ObjectRecord{}, err
	}
	return record, nil
}

func (s *Service) CommitPending(ctx context.Context, object PendingObject) (bool, error) {
	if s == nil || s.ledger == nil || s.spool == nil || s.backend == nil || s.status == nil || s.ops.now == nil || ctx == nil {
		return false, errors.New("collector service unavailable")
	}
	if ctx.Err() != nil {
		return false, errors.New("collector backend cancelled")
	}
	s.commitMu.Lock()
	defer s.commitMu.Unlock()
	if ctx.Err() != nil {
		return false, errors.New("collector backend cancelled")
	}

	record, found, err := s.ledger.Get(object.ObjectID)
	if err != nil || !found || !recordMatchesPending(record, object) {
		return false, s.localFailure()
	}
	local, hasLocal, err := s.findDurableObject(object.ObjectID)
	if err != nil {
		return false, s.localFailure()
	}
	if hasLocal {
		local.ReceivedAt = record.ReceivedAt
		if !recordMatchesPending(record, local) {
			return false, s.localFailure()
		}
		object = local
	} else {
		if record.StoredAt == nil {
			return false, s.localFailure()
		}
		digest, ok := canonicalDigest(record.ObjectID)
		if !ok {
			return false, s.localFailure()
		}
		object = PendingObject{
			ObjectID: record.ObjectID, DigestHex: digest, ArchiveName: record.ArchiveName,
			EncryptedSize: record.EncryptedSize, ReceivedAt: record.ReceivedAt,
		}
	}

	present, existsErr := s.backend.Exists(ctx, record.ObjectID)
	if existsErr != nil {
		return false, s.backendFailureLocked(record, existsErr)
	}
	if err := s.crash(crashAfterRemotePresence); err != nil {
		return false, err
	}
	existing := present
	if !present {
		if !hasLocal && record.StoredAt != nil {
			return false, s.localFailure()
		}
		createErr := s.backend.Create(ctx, object)
		if err := s.crash(crashAfterRemoteCreate); err != nil {
			return false, err
		}
		present, existsErr = s.backend.Exists(ctx, record.ObjectID)
		if existsErr != nil {
			return false, s.backendFailureLocked(record, existsErr)
		}
		if err := s.crash(crashAfterRemotePresence); err != nil {
			return false, err
		}
		if !present {
			if createErr != nil {
				return false, s.backendFailureLocked(record, createErr)
			}
			return false, s.backendFailureClassLocked(record, "remote_verification_failed")
		}
	}

	if record.StoredAt == nil {
		storedAt := s.ops.now()
		if storedAt.IsZero() || storedAt.Before(record.ReceivedAt) {
			return false, s.localFailure()
		}
		record.StoredAt = timePointer(storedAt)
		if err := s.ledger.Put(record); err != nil {
			return false, s.localFailure()
		}
	}
	if err := s.crash(crashAfterLedgerUpdate); err != nil {
		return false, err
	}
	if hasLocal {
		if err := s.spool.RemoveEncrypted(object.EncryptedPath); err != nil {
			return false, s.localFailure()
		}
	}
	if err := s.crash(crashAfterAgeRemoval); err != nil {
		return false, err
	}
	s.status.RemovePending(record.ObjectID)
	s.status.RecordSuccess(*record.StoredAt)
	return existing, nil
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

func (s *Service) ensurePendingRecord(object PendingObject, compressedSize int64) (ObjectRecord, bool, error) {
	record, found, err := s.ledger.Get(object.ObjectID)
	if err != nil {
		return ObjectRecord{}, false, s.localFailure()
	}
	if found {
		if record.CompressedSize != compressedSize || !recordMatchesPending(record, object) {
			return ObjectRecord{}, false, s.localFailure()
		}
		return record, false, nil
	}
	if compressedSize <= 0 || object.ReceivedAt.IsZero() {
		return ObjectRecord{}, false, s.localFailure()
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
		return ObjectRecord{}, true, s.localFailure()
	}
	return record, true, nil
}

func (s *Service) cleanupOwned(plaintextPath, partialPath, pendingID string) error {
	failed := false
	if partialPath != "" && s.spool.DiscardEncryptedPartial(partialPath) != nil {
		failed = true
	}
	if pendingID != "" && s.ledger.DeletePending(pendingID) != nil {
		failed = true
	}
	if plaintextPath != "" && s.spool.RemovePlaintext(plaintextPath) != nil {
		failed = true
	}
	if failed {
		return errors.New("collector cleanup failed")
	}
	return nil
}

func recordMatchesPending(record ObjectRecord, object PendingObject) bool {
	digest, ok := canonicalDigest(object.ObjectID)
	return ok && object.DigestHex == digest && object.ArchiveName == "sherpa-"+digest &&
		record.ObjectID == object.ObjectID && record.ArchiveName == object.ArchiveName &&
		record.EncryptedSize == object.EncryptedSize && !record.ReceivedAt.IsZero()
}

func (s *Service) backendFailureLocked(record ObjectRecord, backendErr error) error {
	return s.backendFailureClassLocked(record, retryClass(backendErr))
}

func (s *Service) backendFailureClassLocked(record ObjectRecord, class string) error {
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
