//go:build darwin

package process

import (
	"os"
	"runtime"
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

// closeExclusiveOrganizationCopy restores preserved times after close on
// smbfs. The macOS client keeps the server handle of a written copy open
// after close and closes it on the next open of that file; some servers, such
// as Samba on a UGREEN NAS, then stamp the close time over the preserved time.
// Reopening sends that close first, and the times are set again through the
// reopened handle. macOS refuses byte-range locks on smbfs, so unlike Linux
// there is no lock before the close.
func closeExclusiveOrganizationCopy(file organizationTemporaryFile, created os.FileInfo, atime, mtime time.Time) error {
	return closeExclusiveCopyRestoringTimes(file, created, atime, mtime, darwinOpenFileIsSMB, nil, openOrganizationCopyReadOnly)
}

func darwinOpenFileIsSMB(file organizationTemporaryFile) (bool, error) {
	var info unix.Statfs_t
	err := unix.Fstatfs(int(file.Fd()), &info)
	runtime.KeepAlive(file)
	if err != nil {
		return false, &os.PathError{Op: "fstatfs", Path: file.Name(), Err: err}
	}
	return unix.ByteSliceToString(info.Fstypename[:]) == "smbfs", nil
}
