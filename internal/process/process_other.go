//go:build !darwin && !linux && !windows

package process

import (
	"os/exec"
)

func configureProcess(cmd *exec.Cmd) {}
