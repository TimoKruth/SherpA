package state

import (
	"fmt"
	"os"
	"path/filepath"
)

// Lock serializes mutations across CLI and local web processes. Fail rather
// than overwrite another command's state; interrupted owners leave a visible
// lock which can be removed after verifying no SherpA mutation is running.
func Lock(home string) (func(), error) {
	if err := os.MkdirAll(home, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(home, ".mutation-lock")
	if err := os.Mkdir(path, 0700); err != nil {
		return nil, fmt.Errorf("another SherpA operation holds %s; retry, or remove a stale lock after confirming its owner has stopped", path)
	}
	if err := os.WriteFile(filepath.Join(path, "owner"), []byte(fmt.Sprintf("pid=%d\n", os.Getpid())), 0600); err != nil {
		os.RemoveAll(path)
		return nil, err
	}
	return func() { os.RemoveAll(path) }, nil
}
