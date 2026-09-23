//go:build !darwin && !linux && !windows

package launch

import (
	"os/exec"
)

func configureProcess(cmd *exec.Cmd) {}
