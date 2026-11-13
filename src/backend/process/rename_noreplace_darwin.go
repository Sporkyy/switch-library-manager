//go:build darwin

package process

import (
	"os"

	"golang.org/x/sys/unix"
)

func renameNoReplace(from, to string) error {
	if err := unix.RenamexNp(from, to, unix.RENAME_EXCL); err != nil {
		return &os.LinkError{Op: "renamex_np(RENAME_EXCL)", Old: from, New: to, Err: err}
	}
	return nil
}
