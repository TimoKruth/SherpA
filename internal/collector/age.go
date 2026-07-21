package collector

import (
	"context"
	"errors"
	"io"
	"math"
	"os"

	"filippo.io/age"
	"golang.org/x/sys/unix"
)

const encryptionCopyBufferSize = 32 * 1024

type Encryptor struct {
	recipient age.Recipient
}

func NewEncryptor(recipient age.Recipient) *Encryptor {
	return &Encryptor{recipient: recipient}
}

func (e *Encryptor) EncryptFile(ctx context.Context, sourcePath, partialPath string) (int64, error) {
	return e.encryptFile(ctx, sourcePath, partialPath, encryptionFileFinalizer{
		sync:  func(file *os.File) error { return file.Sync() },
		close: func(file *os.File) error { return file.Close() },
	})
}

type encryptionFileFinalizer struct {
	sync  func(*os.File) error
	close func(*os.File) error
}

func (e *Encryptor) encryptFile(ctx context.Context, sourcePath, partialPath string, finalizer encryptionFileFinalizer) (encryptedBytes int64, returnErr error) {
	if ctx == nil || ctx.Err() != nil {
		return 0, errors.New("collector encryption canceled")
	}
	if e == nil || e.recipient == nil {
		return 0, errors.New("collector encryption setup failed")
	}

	sourceFD, err := unix.Open(sourcePath, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0, errors.New("collector encryption source unavailable")
	}
	source := os.NewFile(uintptr(sourceFD), "collector-encryption-source")
	if source == nil {
		_ = unix.Close(sourceFD)
		return 0, errors.New("collector encryption source unavailable")
	}
	sourceCloseAttempted := false
	defer func() {
		if sourceCloseAttempted {
			return
		}
		sourceCloseAttempted = true
		if err := source.Close(); err != nil && returnErr == nil {
			encryptedBytes = 0
			returnErr = errors.New("collector encryption source close failed")
		}
	}()

	destinationFD, err := unix.Open(
		partialPath,
		unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0o600,
	)
	if err != nil {
		return 0, errors.New("collector encryption destination unavailable")
	}
	destination := os.NewFile(uintptr(destinationFD), "collector-encryption-destination")
	if destination == nil {
		_ = unix.Close(destinationFD)
		return 0, errors.New("collector encryption destination unavailable")
	}
	destinationCloseAttempted := false
	defer func() {
		if destinationCloseAttempted {
			return
		}
		destinationCloseAttempted = true
		if err := finalizer.close(destination); err != nil && returnErr == nil {
			encryptedBytes = 0
			returnErr = errors.New("collector encryption destination close failed")
		}
	}()

	if err := unix.Fchmod(destinationFD, 0o600); err != nil {
		return 0, errors.New("collector encryption destination unsafe")
	}
	var destinationStat unix.Stat_t
	if err := unix.Fstat(destinationFD, &destinationStat); err != nil || destinationStat.Mode&unix.S_IFMT != unix.S_IFREG {
		return 0, errors.New("collector encryption destination unsafe")
	}
	if ctx.Err() != nil {
		return 0, errors.New("collector encryption canceled")
	}

	counter := &encryptedByteWriter{writer: destination}
	ageWriter, err := age.Encrypt(counter, e.recipient)
	if err != nil {
		return 0, errors.New("collector encryption setup failed")
	}
	ageCloseAttempted := false
	defer func() {
		if ageCloseAttempted {
			return
		}
		ageCloseAttempted = true
		if err := ageWriter.Close(); err != nil && returnErr == nil {
			encryptedBytes = 0
			returnErr = errors.New("collector encryption finalization failed")
		}
	}()

	buffer := make([]byte, encryptionCopyBufferSize)
	_, err = io.CopyBuffer(ageWriter, &contextReader{ctx: ctx, reader: source}, buffer)
	if err != nil {
		if ctx.Err() != nil {
			return 0, errors.New("collector encryption canceled")
		}
		return 0, errors.New("collector encryption copy failed")
	}
	if ctx.Err() != nil {
		return 0, errors.New("collector encryption canceled")
	}

	ageCloseAttempted = true
	if err := ageWriter.Close(); err != nil {
		return 0, errors.New("collector encryption finalization failed")
	}
	if err := finalizer.sync(destination); err != nil {
		return 0, errors.New("collector encryption synchronization failed")
	}
	destinationCloseAttempted = true
	if err := finalizer.close(destination); err != nil {
		return 0, errors.New("collector encryption destination close failed")
	}

	return counter.written, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.reader.Read(buffer)
	if contextErr := r.ctx.Err(); contextErr != nil {
		return 0, contextErr
	}
	return n, err
}

type encryptedByteWriter struct {
	writer  io.Writer
	written int64
}

func (w *encryptedByteWriter) Write(buffer []byte) (int, error) {
	n, err := w.writer.Write(buffer)
	if n < 0 || n > len(buffer) {
		return 0, errors.New("invalid encrypted output write count")
	}
	if int64(n) > math.MaxInt64-w.written {
		return n, errors.New("encrypted output size overflow")
	}
	w.written += int64(n)
	if n != len(buffer) && err == nil {
		err = io.ErrShortWrite
	}
	return n, err
}
