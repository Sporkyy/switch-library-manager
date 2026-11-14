//go:build linux

package app

import (
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

// serverModTime asks the filesystem for fresh attributes. Linux CIFS treats
// AT_STATX_FORCE_SYNC as a revalidation request, bypassing actimeo and leases.
func serverModTime(path string) (time.Time, error) {
	var info unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, path, unix.AT_SYMLINK_NOFOLLOW|unix.AT_STATX_FORCE_SYNC, unix.STATX_MTIME, &info); err != nil {
		return time.Time{}, fmt.Errorf("statx %q with AT_STATX_FORCE_SYNC: %w", path, err)
	}
	return time.Unix(info.Mtime.Sec, int64(info.Mtime.Nsec)), nil
}
