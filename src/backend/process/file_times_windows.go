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

// closeExclusiveOrganizationCopy only closes here: the deferred SMB close that
// replaces preserved times is specific to the Linux CIFS client.
func closeExclusiveOrganizationCopy(file organizationTemporaryFile, _ os.FileInfo, _, _ time.Time) error {
	return file.Close()
}
