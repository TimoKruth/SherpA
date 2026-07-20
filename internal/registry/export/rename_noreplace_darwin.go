//go:build darwin

package export

import "golang.org/x/sys/unix"

func renameNoReplace(oldDirFD int, oldName string, newDirFD int, newName string) error {
	return unix.RenameatxNp(oldDirFD, oldName, newDirFD, newName, unix.RENAME_EXCL)
}
