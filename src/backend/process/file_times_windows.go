//go:build windows

package process

import (
	"os"
	"time"

	"golang.org/x/sys/windows"
)

func setOrganizationOpenFileTimes(file organizationTemporaryFile, atime, mtime time.Time) error {
	access := windows.NsecToFiletime(atime.UnixNano())
	modified := windows.NsecToFiletime(mtime.UnixNano())
	return windows.SetFileTime(windows.Handle(file.Fd()), nil, &access, &modified)
}

// closeExclusiveOrganizationCopy only closes here. The deferred SMB close that
// replaces preserved times was observed with the Linux and macOS clients;
// Windows SMB destinations have not been tested.
func closeExclusiveOrganizationCopy(file organizationTemporaryFile, _ os.FileInfo, _, _ time.Time) error {
	return file.Close()
}
