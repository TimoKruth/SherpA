package collector

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"regexp"

	"golang.org/x/sys/unix"
)

const (
	borgStageCreateAttempts = 8
	borgStateFileCount      = 3
)

var (
	borgStageNamePattern = regexp.MustCompile(`^\.sherpa-borg-[0-9a-f]{32}\.tmp$`)
	borgStageFilePattern = regexp.MustCompile(`^[0-9a-f]{64}\.tar\.gz\.age$`)
	borgStateNames       = [...]string{"cache", "config", "security"}
)

type borgSystem struct {
	linkat           func(int, string, int, string, int) error
	unlinkat         func(int, string, int) error
	beforeStart      func(*exec.Cmd) error
	waitNoReap       func(int) error
	killProcessGroup func(int) error
	waitProcess      func(*exec.Cmd) error
}

func defaultBorgSystem() borgSystem {
	return borgSystem{
		linkat:           unix.Linkat,
		unlinkat:         unix.Unlinkat,
		beforeStart:      func(*exec.Cmd) error { return nil },
		waitNoReap:       waitBorgProcessNoReap,
		killProcessGroup: killBorgProcessGroup,
		waitProcess:      func(cmd *exec.Cmd) error { return cmd.Wait() },
	}
}

func (s borgSystem) valid() bool {
	return s.linkat != nil && s.unlinkat != nil && s.beforeStart != nil && s.waitNoReap != nil && s.killProcessGroup != nil && s.waitProcess != nil
}

type borgStateIdentity struct {
	root     unix.Stat_t
	children [borgStateFileCount]unix.Stat_t
}

func initializeBorgState(workFD int, system borgSystem) (borgStateIdentity, error) {
	if err := recoverBorgStages(workFD, system); err != nil {
		return borgStateIdentity{}, err
	}
	var identity borgStateIdentity
	if unix.Fstat(workFD, &identity.root) != nil || !safeBorgDirectoryMetadata(&identity.root) {
		return borgStateIdentity{}, errors.New("unsafe Borg work root")
	}
	for i, name := range borgStateNames {
		if err := ensureBorgStateDirectory(workFD, name); err != nil {
			return borgStateIdentity{}, err
		}
		fd, stat, err := openVerifiedBorgDirectory(workFD, name, nil)
		if err != nil {
			return borgStateIdentity{}, err
		}
		identity.children[i] = stat
		if err := unix.Close(fd); err != nil {
			return borgStateIdentity{}, err
		}
	}
	return identity, nil
}

type borgCommandBinding struct {
	files []*os.File
	stats []unix.Stat_t
}

func (b *BorgBackend) bindCommand(stage *borgStage) (*borgCommandBinding, error) {
	workFD, clean, err := openPrivateDirectory(b.config.WorkDir)
	if err != nil || clean != b.config.WorkDir {
		return nil, errors.New("unsafe Borg work root")
	}
	var workStat unix.Stat_t
	if unix.Fstat(workFD, &workStat) != nil || !safeBorgDirectoryMetadata(&workStat) || !sameInode(&b.state.root, &workStat) {
		_ = unix.Close(workFD)
		return nil, errors.New("unsafe Borg work root")
	}
	binding := &borgCommandBinding{
		files: make([]*os.File, 0, borgStateFileCount+1),
		stats: make([]unix.Stat_t, 0, borgStateFileCount+1),
	}
	failed := true
	defer func() {
		_ = unix.Close(workFD)
		if failed {
			_ = binding.close()
		}
	}()
	for i, name := range borgStateNames {
		fd, stat, openErr := openVerifiedBorgDirectory(workFD, name, &b.state.children[i])
		if openErr != nil {
			return nil, openErr
		}
		file := os.NewFile(uintptr(fd), name)
		if file == nil {
			_ = unix.Close(fd)
			return nil, errors.New("unsafe Borg state descriptor")
		}
		binding.files = append(binding.files, file)
		binding.stats = append(binding.stats, stat)
	}
	if stage != nil {
		if !stage.valid() {
			return nil, errors.New("unsafe Borg stage")
		}
		fd, err := unix.Dup(stage.dirFD)
		if err != nil {
			return nil, err
		}
		file := os.NewFile(uintptr(fd), stage.name)
		if file == nil {
			_ = unix.Close(fd)
			return nil, errors.New("unsafe Borg stage descriptor")
		}
		binding.files = append(binding.files, file)
		binding.stats = append(binding.stats, stage.dirStat)
	}
	failed = false
	return binding, nil
}

func (b *borgCommandBinding) valid() bool {
	if b == nil || len(b.files) != len(b.stats) {
		return false
	}
	for i, file := range b.files {
		if file == nil {
			return false
		}
		var stat unix.Stat_t
		if unix.Fstat(int(file.Fd()), &stat) != nil || !safeBorgDirectoryMetadata(&stat) || !sameInode(&b.stats[i], &stat) {
			return false
		}
	}
	return true
}

func (b *borgCommandBinding) close() error {
	if b == nil {
		return nil
	}
	var closeErr error
	for i := len(b.files) - 1; i >= 0; i-- {
		if b.files[i] != nil {
			if err := b.files[i].Close(); err != nil && closeErr == nil {
				closeErr = err
			}
			b.files[i] = nil
		}
	}
	return closeErr
}

func openVerifiedBorgDirectory(parentFD int, name string, expected *unix.Stat_t) (int, unix.Stat_t, error) {
	fd, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, unix.Stat_t{}, err
	}
	var opened unix.Stat_t
	if unix.Fstat(fd, &opened) != nil || !safeBorgDirectoryMetadata(&opened) || (expected != nil && !sameInode(expected, &opened)) {
		_ = unix.Close(fd)
		return -1, unix.Stat_t{}, errors.New("unsafe Borg state directory")
	}
	var current unix.Stat_t
	if unix.Fstatat(parentFD, name, &current, unix.AT_SYMLINK_NOFOLLOW) != nil || !safeBorgDirectoryMetadata(&current) || !sameInode(&opened, &current) {
		_ = unix.Close(fd)
		return -1, unix.Stat_t{}, errors.New("unsafe Borg state directory")
	}
	return fd, opened, nil
}

type borgStage struct {
	system   borgSystem
	workFD   int
	dirFD    int
	fileFD   int
	name     string
	fileName string
	workStat unix.Stat_t
	dirStat  unix.Stat_t
	fileStat unix.Stat_t
}

func (b *BorgBackend) stageInput(source *heldBorgInput) (*borgStage, error) {
	if source == nil || !source.valid() {
		return nil, errors.New("unsafe Borg input")
	}
	workFD, clean, err := openPrivateDirectory(b.config.WorkDir)
	if err != nil || clean != b.config.WorkDir {
		return nil, errors.New("unsafe Borg work root")
	}
	var workStat unix.Stat_t
	if unix.Fstat(workFD, &workStat) != nil || !safeBorgDirectoryMetadata(&workStat) || !sameInode(&b.state.root, &workStat) {
		_ = unix.Close(workFD)
		return nil, errors.New("unsafe Borg work root")
	}
	stage := &borgStage{system: b.system, workFD: workFD, dirFD: -1, fileFD: -1, fileName: source.name, workStat: workStat}
	created := false
	defer func() {
		if !created {
			_ = stage.cleanup()
		}
	}()
	for attempt := 0; attempt < borgStageCreateAttempts; attempt++ {
		name, nameErr := randomBorgStageName()
		if nameErr != nil {
			return nil, nameErr
		}
		if err := unix.Mkdirat(workFD, name, 0o700); errors.Is(err, unix.EEXIST) {
			continue
		} else if err != nil {
			return nil, err
		}
		stage.name = name
		created = true
		break
	}
	if !created {
		return nil, errors.New("Borg stage unavailable")
	}
	created = false
	var entry unix.Stat_t
	if unix.Fstatat(workFD, stage.name, &entry, unix.AT_SYMLINK_NOFOLLOW) != nil || !ownedBorgDirectoryMetadata(&entry) {
		return nil, errors.New("unsafe Borg stage")
	}
	dirFD, err := unix.Openat(workFD, stage.name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	stage.dirFD = dirFD
	if unix.Fstat(dirFD, &stage.dirStat) != nil || !ownedBorgDirectoryMetadata(&stage.dirStat) || !sameInode(&entry, &stage.dirStat) {
		return nil, errors.New("unsafe Borg stage")
	}
	if err := unix.Fchmod(dirFD, 0o700); err != nil {
		return nil, err
	}
	if unix.Fstat(dirFD, &stage.dirStat) != nil || !safeBorgDirectoryMetadata(&stage.dirStat) || !sameDirectoryEntry(workFD, stage.name, &stage.dirStat) {
		return nil, errors.New("unsafe Borg stage")
	}
	if err := unix.Fsync(workFD); err != nil {
		return nil, err
	}
	if err := stage.materialize(source); err != nil {
		return nil, err
	}
	if !stage.valid() {
		return nil, errors.New("unsafe Borg stage")
	}
	created = true
	return stage, nil
}

func randomBorgStageName() (string, error) {
	var value [16]byte
	if _, err := io.ReadFull(rand.Reader, value[:]); err != nil {
		return "", err
	}
	return ".sherpa-borg-" + hex.EncodeToString(value[:]) + ".tmp", nil
}

func (s *borgStage) materialize(source *heldBorgInput) error {
	err := s.system.linkat(source.dirFD, source.name, s.dirFD, source.name, 0)
	if err == nil {
		fd, stat, openErr := openVerifiedBorgStageFile(s.dirFD, source.name, source.size, &source.stat)
		if openErr != nil {
			return openErr
		}
		s.fileFD = fd
		s.fileStat = stat
		return unix.Fsync(s.dirFD)
	}
	if !errors.Is(err, unix.EXDEV) {
		return err
	}
	fd, err := unix.Openat(s.dirFD, source.name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o600)
	if err != nil {
		return err
	}
	copyOK := false
	defer func() {
		if !copyOK {
			_ = unix.Close(fd)
		}
	}()
	if err := unix.Fchmod(fd, 0o600); err != nil {
		return err
	}
	if err := copyBorgInput(fd, source.fileFD, source.size); err != nil {
		return err
	}
	if err := unix.Fsync(fd); err != nil {
		return err
	}
	var copied unix.Stat_t
	if unix.Fstat(fd, &copied) != nil || !safeRegularMetadata(&copied) || copied.Size != source.size {
		return errors.New("unsafe Borg stage copy")
	}
	if err := unix.Close(fd); err != nil {
		return err
	}
	copyOK = true
	readFD, stat, err := openVerifiedBorgStageFile(s.dirFD, source.name, source.size, nil)
	if err != nil {
		return err
	}
	s.fileFD = readFD
	s.fileStat = stat
	return unix.Fsync(s.dirFD)
}

func copyBorgInput(destinationFD, sourceFD int, size int64) error {
	buffer := make([]byte, 128*1024)
	var offset int64
	for offset < size {
		want := int64(len(buffer))
		if remaining := size - offset; remaining < want {
			want = remaining
		}
		n, err := unix.Pread(sourceFD, buffer[:want], offset)
		if err != nil {
			return err
		}
		if n <= 0 {
			return errors.New("short Borg stage input")
		}
		written := 0
		for written < n {
			count, writeErr := unix.Write(destinationFD, buffer[written:n])
			if writeErr != nil {
				return writeErr
			}
			if count <= 0 {
				return errors.New("short Borg stage write")
			}
			written += count
		}
		offset += int64(n)
	}
	var extra [1]byte
	n, err := unix.Pread(sourceFD, extra[:], size)
	if err != nil {
		return err
	}
	if n != 0 {
		return errors.New("long Borg stage input")
	}
	return nil
}

func openVerifiedBorgStageFile(dirFD int, name string, size int64, expected *unix.Stat_t) (int, unix.Stat_t, error) {
	fd, err := unix.Openat(dirFD, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return -1, unix.Stat_t{}, err
	}
	var stat unix.Stat_t
	if unix.Fstat(fd, &stat) != nil || !safeRegularMetadata(&stat) || stat.Size != size || (expected != nil && !sameInode(expected, &stat)) || !sameDirectoryEntry(dirFD, name, &stat) {
		_ = unix.Close(fd)
		return -1, unix.Stat_t{}, errors.New("unsafe Borg stage file")
	}
	return fd, stat, nil
}

func (s *borgStage) valid() bool {
	if s == nil || s.workFD < 0 || s.dirFD < 0 || s.fileFD < 0 || !borgStageNamePattern.MatchString(s.name) || !borgStageFilePattern.MatchString(s.fileName) {
		return false
	}
	var work unix.Stat_t
	if unix.Fstat(s.workFD, &work) != nil || !safeBorgDirectoryMetadata(&work) || !sameInode(&s.workStat, &work) {
		return false
	}
	var dir unix.Stat_t
	if unix.Fstat(s.dirFD, &dir) != nil || !safeBorgDirectoryMetadata(&dir) || !sameInode(&s.dirStat, &dir) || !sameDirectoryEntry(s.workFD, s.name, &dir) {
		return false
	}
	var file unix.Stat_t
	return unix.Fstat(s.fileFD, &file) == nil && safeRegularMetadata(&file) && file.Size == s.fileStat.Size && sameInode(&s.fileStat, &file) && sameDirectoryEntry(s.dirFD, s.fileName, &file)
}

func (s *borgStage) cleanup() error {
	if s == nil {
		return nil
	}
	var cleanupErr error
	if s.fileFD >= 0 {
		var current unix.Stat_t
		if s.dirFD < 0 || unix.Fstatat(s.dirFD, s.fileName, &current, unix.AT_SYMLINK_NOFOLLOW) != nil || !safeRegularMetadata(&current) || !sameInode(&s.fileStat, &current) {
			cleanupErr = errors.New("unsafe Borg stage cleanup")
		} else if err := s.system.unlinkat(s.dirFD, s.fileName, 0); err != nil {
			cleanupErr = err
		} else if err := unix.Fsync(s.dirFD); err != nil {
			cleanupErr = err
		}
		if err := unix.Close(s.fileFD); err != nil && cleanupErr == nil {
			cleanupErr = err
		}
		s.fileFD = -1
	}
	if s.dirFD >= 0 {
		if err := unix.Close(s.dirFD); err != nil && cleanupErr == nil {
			cleanupErr = err
		}
		s.dirFD = -1
	}
	if s.workFD >= 0 {
		var current unix.Stat_t
		if cleanupErr == nil && (unix.Fstatat(s.workFD, s.name, &current, unix.AT_SYMLINK_NOFOLLOW) != nil || !safeBorgDirectoryMetadata(&current) || !sameInode(&s.dirStat, &current)) {
			cleanupErr = errors.New("unsafe Borg stage cleanup")
		}
		if cleanupErr == nil {
			if err := s.system.unlinkat(s.workFD, s.name, unix.AT_REMOVEDIR); err != nil {
				cleanupErr = err
			} else if err := unix.Fsync(s.workFD); err != nil {
				cleanupErr = err
			}
		}
		if err := unix.Close(s.workFD); err != nil && cleanupErr == nil {
			cleanupErr = err
		}
		s.workFD = -1
	}
	return cleanupErr
}

type recoverableBorgStage struct {
	name     string
	dirFD    int
	dirStat  unix.Stat_t
	fileName string
	fileFD   int
	fileStat unix.Stat_t
}

func recoverBorgStages(workFD int, system borgSystem) error {
	names, err := readDirectoryNames(workFD)
	if err != nil {
		return err
	}
	stages := make([]recoverableBorgStage, 0)
	defer func() {
		for i := range stages {
			if stages[i].fileFD >= 0 {
				_ = unix.Close(stages[i].fileFD)
			}
			if stages[i].dirFD >= 0 {
				_ = unix.Close(stages[i].dirFD)
			}
		}
	}()
	for _, name := range names {
		if !borgStageNamePattern.MatchString(name) {
			continue
		}
		dirFD, dirStat, err := openVerifiedBorgDirectory(workFD, name, nil)
		if err != nil {
			return err
		}
		stage := recoverableBorgStage{name: name, dirFD: dirFD, dirStat: dirStat, fileFD: -1}
		contents, err := readDirectoryNames(dirFD)
		if err != nil || len(contents) > 1 {
			_ = unix.Close(dirFD)
			return errors.New("unsafe Borg stage residue")
		}
		if len(contents) == 1 {
			if !borgStageFilePattern.MatchString(contents[0]) {
				_ = unix.Close(dirFD)
				return errors.New("unsafe Borg stage residue")
			}
			fileFD, err := unix.Openat(dirFD, contents[0], unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
			if err != nil {
				_ = unix.Close(dirFD)
				return err
			}
			stage.fileName = contents[0]
			stage.fileFD = fileFD
			if unix.Fstat(fileFD, &stage.fileStat) != nil || !safeRegularMetadata(&stage.fileStat) || !sameDirectoryEntry(dirFD, stage.fileName, &stage.fileStat) {
				_ = unix.Close(fileFD)
				_ = unix.Close(dirFD)
				return errors.New("unsafe Borg stage residue")
			}
		}
		stages = append(stages, stage)
	}
	for i := range stages {
		if err := cleanupRecoveredBorgStage(workFD, &stages[i], system); err != nil {
			return err
		}
	}
	return nil
}

func cleanupRecoveredBorgStage(workFD int, stage *recoverableBorgStage, system borgSystem) error {
	if stage.fileFD >= 0 {
		var current unix.Stat_t
		if unix.Fstatat(stage.dirFD, stage.fileName, &current, unix.AT_SYMLINK_NOFOLLOW) != nil || !safeRegularMetadata(&current) || !sameInode(&stage.fileStat, &current) {
			return errors.New("unsafe Borg stage residue")
		}
		if err := system.unlinkat(stage.dirFD, stage.fileName, 0); err != nil {
			return err
		}
		if err := unix.Fsync(stage.dirFD); err != nil {
			return err
		}
		if err := unix.Close(stage.fileFD); err != nil {
			return err
		}
		stage.fileFD = -1
	}
	if err := unix.Close(stage.dirFD); err != nil {
		return err
	}
	stage.dirFD = -1
	var current unix.Stat_t
	if unix.Fstatat(workFD, stage.name, &current, unix.AT_SYMLINK_NOFOLLOW) != nil || !safeBorgDirectoryMetadata(&current) || !sameInode(&stage.dirStat, &current) {
		return errors.New("unsafe Borg stage residue")
	}
	if err := system.unlinkat(workFD, stage.name, unix.AT_REMOVEDIR); err != nil {
		return err
	}
	return unix.Fsync(workFD)
}

func ownedBorgDirectoryMetadata(stat *unix.Stat_t) bool {
	return stat != nil && stat.Mode&unix.S_IFMT == unix.S_IFDIR && stat.Uid == uint32(os.Geteuid()) && stat.Gid == uint32(os.Getegid())
}
