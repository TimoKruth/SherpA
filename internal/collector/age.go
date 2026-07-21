package collector

import (
	"context"
	"errors"
	"io"
	"math"
	"os"
	"strings"

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
		chmod:    func(fd int, mode uint32) error { return unix.Fchmod(fd, mode) },
		ageClose: func(writer io.Closer) error { return writer.Close() },
		sync:     func(file *os.File) error { return file.Sync() },
		close:    func(file *os.File) error { return file.Close() },
	})
}

type encryptionFileFinalizer struct {
	chmod    func(int, uint32) error
	ageClose func(io.Closer) error
	sync     func(*os.File) error
	close    func(*os.File) error
}

func (e *Encryptor) encryptFile(ctx context.Context, sourcePath, partialPath string, finalizer encryptionFileFinalizer) (encryptedBytes int64, returnErr error) {
	if ctx == nil || ctx.Err() != nil {
		return 0, errors.New("collector encryption canceled")
	}
	if e == nil || e.recipient == nil {
		return 0, errors.New("collector encryption setup failed")
	}
	finalizer = completeEncryptionFileFinalizer(finalizer)

	sourceFD, err := openPathNoSymlinks(sourcePath, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return 0, errors.New("collector encryption source unavailable")
	}
	var sourceStat unix.Stat_t
	if err := unix.Fstat(sourceFD, &sourceStat); err != nil || sourceStat.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = unix.Close(sourceFD)
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
	if ctx.Err() != nil {
		return 0, errors.New("collector encryption canceled")
	}

	destinationFD, err := openPathNoSymlinks(
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

	if err := finalizer.chmod(destinationFD, 0o600); err != nil {
		return 0, errors.New("collector encryption destination unsafe")
	}
	var destinationStat unix.Stat_t
	if err := unix.Fstat(destinationFD, &destinationStat); err != nil ||
		destinationStat.Mode&unix.S_IFMT != unix.S_IFREG ||
		destinationStat.Mode&0o7777 != 0o600 {
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
		if err := finalizer.ageClose(ageWriter); err != nil && returnErr == nil {
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
	if err := finalizer.ageClose(ageWriter); err != nil {
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

func completeEncryptionFileFinalizer(finalizer encryptionFileFinalizer) encryptionFileFinalizer {
	if finalizer.chmod == nil {
		finalizer.chmod = func(fd int, mode uint32) error { return unix.Fchmod(fd, mode) }
	}
	if finalizer.ageClose == nil {
		finalizer.ageClose = func(writer io.Closer) error { return writer.Close() }
	}
	if finalizer.sync == nil {
		finalizer.sync = func(file *os.File) error { return file.Sync() }
	}
	if finalizer.close == nil {
		finalizer.close = func(file *os.File) error { return file.Close() }
	}
	return finalizer
}

func openPathNoSymlinks(path string, flags int, mode uint32) (int, error) {
	startPath, components, err := safePathComponents(path)
	if err != nil {
		return -1, err
	}

	parentFD, err := unix.Open(startPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, errors.New("unsafe path")
	}
	for _, component := range components[:len(components)-1] {
		nextFD, openErr := unix.Openat(parentFD, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		closeErr := unix.Close(parentFD)
		if openErr != nil || closeErr != nil {
			if openErr == nil {
				_ = unix.Close(nextFD)
			}
			return -1, errors.New("unsafe path")
		}
		parentFD = nextFD
	}

	fileFD, openErr := unix.Openat(parentFD, components[len(components)-1], flags, mode)
	closeErr := unix.Close(parentFD)
	if openErr != nil || closeErr != nil {
		if openErr == nil {
			_ = unix.Close(fileFD)
		}
		return -1, errors.New("unsafe path")
	}
	return fileFD, nil
}

func safePathComponents(path string) (string, []string, error) {
	if path == "" {
		return "", nil, errors.New("unsafe path")
	}
	separator := string(os.PathSeparator)
	components := strings.Split(path, separator)
	startPath := "."
	if strings.HasPrefix(path, separator) {
		startPath = separator
		components = components[1:]
	}
	if len(components) == 0 {
		return "", nil, errors.New("unsafe path")
	}
	for _, component := range components {
		if component == "" || component == "." || component == ".." {
			return "", nil, errors.New("unsafe path")
		}
	}
	return startPath, components, nil
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
