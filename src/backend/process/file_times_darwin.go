//go:build darwin

package process

import (
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// macOS exposes no futimens in x/sys, so handle-based timestamps are limited
// to the microsecond precision of futimes.
func setOrganizationOpenFileTimes(file organizationTemporaryFile, atime, mtime time.Time) error {
	return unix.Futimes(int(file.Fd()), []unix.Timeval{
		unix.NsecToTimeval(atime.UnixNano()),
		unix.NsecToTimeval(mtime.UnixNano()),
	})
}

// closeExclusiveOrganizationCopy only closes here: the deferred SMB close that
// replaces preserved times is specific to the Linux CIFS client.
func closeExclusiveOrganizationCopy(file organizationTemporaryFile, _ os.FileInfo, _, _ time.Time) error {
	return file.Close()
}
