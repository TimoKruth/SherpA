//go:build linux

package collector

import (
	"errors"
	"os/exec"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

func waitBorgProcessNoReap(pid int) error {
	for {
		var info unix.Siginfo
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		return err
	}
}

func killBorgProcessGroup(pid int) error {
	err := syscall.Kill(-pid, syscall.SIGKILL)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func prepareBorgCommand(cmd *exec.Cmd, binding *borgCommandBinding, stage *borgStage) error {
	if stage == nil {
		return nil
	}
	if cmd == nil || binding == nil || len(binding.files) != borgStateFileCount+1 {
		return errors.New("invalid Borg stage binding")
	}
	cmd.Dir = borgFDPath(int(binding.files[borgStateFileCount].Fd()))
	return nil
}

func borgFDPath(fd int) string {
	return "/proc/self/fd/" + strconv.Itoa(fd)
}
