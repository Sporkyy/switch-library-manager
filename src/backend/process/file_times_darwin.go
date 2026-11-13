//go:build darwin

package process

import (
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
