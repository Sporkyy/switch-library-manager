//go:build windows

package process

import (
	"time"

	"golang.org/x/sys/windows"
)

func setOrganizationOpenFileTimes(file organizationTemporaryFile, atime, mtime time.Time) error {
	access := windows.NsecToFiletime(atime.UnixNano())
	modified := windows.NsecToFiletime(mtime.UnixNano())
	return windows.SetFileTime(windows.Handle(file.Fd()), nil, &access, &modified)
}
