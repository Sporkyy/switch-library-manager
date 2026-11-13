//go:build windows

package process

import (
	"os"

	"golang.org/x/sys/windows"
)

func renameNoReplace(from, to string) error {
	fromPath, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return &os.LinkError{Op: "MoveFileEx", Old: from, New: to, Err: err}
	}
	toPath, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return &os.LinkError{Op: "MoveFileEx", Old: from, New: to, Err: err}
	}
	// Keep flags at zero: MOVEFILE_REPLACE_EXISTING would overwrite a file that
	// appears at the destination after organization preflight.
	if err := windows.MoveFileEx(fromPath, toPath, 0); err != nil {
		return &os.LinkError{Op: "MoveFileEx", Old: from, New: to, Err: err}
	}
	return nil
}
