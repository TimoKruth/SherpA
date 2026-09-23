// Package process runs cancellable commands with bounded pipe waits and process-tree cleanup.
package process

import (
	"context"
	"os/exec"
	"time"
)

func Command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = 2 * time.Second
	configureProcess(cmd)
	return cmd
}
