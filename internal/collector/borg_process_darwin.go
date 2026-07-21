//go:build darwin

package collector

import (
	"bytes"
	"errors"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	borgDarwinWaitPID     = 1
	borgDarwinZombieState = 5
)

func waitBorgProcessNoReap(pid int) error {
	var info [32]uint64
	for {
		_, _, errno := unix.Syscall6(
			unix.SYS_WAITID,
			borgDarwinWaitPID,
			uintptr(pid),
			uintptr(unsafe.Pointer(&info[0])),
			unix.WEXITED|unix.WNOWAIT,
			0,
			0,
		)
		runtime.KeepAlive(&info)
		if errno == unix.EINTR {
			continue
		}
		if errno != 0 {
			return errno
		}
		return nil
	}
}

func killBorgProcessGroup(pid int) error {
	err := syscall.Kill(-pid, syscall.SIGKILL)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	if errors.Is(err, syscall.EPERM) {
		processes, inspectErr := unix.SysctlKinfoProcSlice("kern.proc.pgrp", pid)
		if inspectErr == nil && len(processes) == 1 && processes[0].Proc.P_pid == int32(pid) && processes[0].Proc.P_stat == borgDarwinZombieState {
			return nil
		}
	}
	return err
}

type borgPreparedPath struct {
	fd       int
	path     string
	expected unix.Stat_t
}

type borgPreparedCommand struct {
	paths []borgPreparedPath
}

func (p *borgPreparedCommand) valid() bool {
	if p == nil || len(p.paths) < borgStateFileCount {
		return false
	}
	for _, binding := range p.paths {
		path, err := darwinBorgDescriptorPath(binding.fd, &binding.expected)
		if err != nil || path != binding.path {
			return false
		}
	}
	return true
}

func prepareBorgCommand(cmd *exec.Cmd, binding *borgCommandBinding, stage *borgStage) (*borgPreparedCommand, error) {
	if cmd == nil || binding == nil || len(binding.files) != len(binding.stats) || len(binding.files) < borgStateFileCount {
		return nil, errors.New("invalid Borg command binding")
	}
	prepared := &borgPreparedCommand{paths: make([]borgPreparedPath, len(binding.files))}
	for i := range prepared.paths {
		path, err := darwinBorgDescriptorPath(int(binding.files[i].Fd()), &binding.stats[i])
		if err != nil {
			return nil, err
		}
		prepared.paths[i] = borgPreparedPath{
			fd:       int(binding.files[i].Fd()),
			path:     path,
			expected: binding.stats[i],
		}
	}
	for i, key := range []string{"BORG_CACHE_DIR", "BORG_CONFIG_DIR", "BORG_SECURITY_DIR"} {
		if !replaceBorgCommandEnvironment(cmd.Env, key, prepared.paths[i].path) {
			return nil, errors.New("invalid Borg command environment")
		}
	}
	if stage != nil {
		if len(binding.files) != borgStateFileCount+1 {
			return nil, errors.New("invalid Borg stage binding")
		}
		cmd.Dir = prepared.paths[borgStateFileCount].path
	}
	return prepared, nil
}

func darwinBorgDescriptorPath(fd int, expected *unix.Stat_t) (string, error) {
	var buffer [4096]byte
	_, _, errno := unix.Syscall(unix.SYS_FCNTL, uintptr(fd), uintptr(unix.F_GETPATH), uintptr(unsafe.Pointer(&buffer[0])))
	runtime.KeepAlive(&buffer)
	if errno != 0 {
		return "", errno
	}
	end := bytes.IndexByte(buffer[:], 0)
	if end <= 0 {
		return "", errors.New("invalid Borg descriptor path")
	}
	path := string(buffer[:end])
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", errors.New("invalid Borg descriptor path")
	}
	var current unix.Stat_t
	if unix.Lstat(path, &current) != nil || !safeBorgDirectoryMetadata(&current) || !sameInode(expected, &current) {
		return "", errors.New("unsafe Borg descriptor path")
	}
	return path, nil
}

func replaceBorgCommandEnvironment(environment []string, key, value string) bool {
	prefix := key + "="
	found := false
	for i, item := range environment {
		if strings.HasPrefix(item, prefix) {
			if found {
				return false
			}
			environment[i] = prefix + value
			found = true
		}
	}
	return found
}

func borgFDPath(fd int) string {
	return "/dev/fd/" + strconv.Itoa(fd)
}
