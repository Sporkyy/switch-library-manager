//go:build linux

package process

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"time"
	"unsafe"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// setOrganizationOpenFileTimes calls utimensat(fd, NULL, ...), the kernel
// form of futimens. It keeps nanosecond precision and, unlike unix.Futimes,
// does not depend on /proc being mounted.
func setOrganizationOpenFileTimes(file organizationTemporaryFile, atime, mtime time.Time) error {
	times := [2]unix.Timespec{
		unix.NsecToTimespec(atime.UnixNano()),
		unix.NsecToTimespec(mtime.UnixNano()),
	}
	_, _, errno := unix.Syscall6(unix.SYS_UTIMENSAT, file.Fd(), 0, uintptr(unsafe.Pointer(&times[0])), 0, 0, 0)
	runtime.KeepAlive(file)
	if errno != 0 {
		return &os.PathError{Op: "utimensat", Path: file.Name(), Err: errno}
	}
	return nil
}

func closeExclusiveOrganizationCopy(file organizationTemporaryFile, created os.FileInfo, atime, mtime time.Time) error {
	return closeExclusiveCopyOnLinux(file, created, atime, mtime, linuxOpenFileSystemType, lockOrganizationCopyForSynchronousClose, func(path string) (organizationTemporaryFile, error) {
		return os.Open(path)
	})
}

// closeExclusiveCopyOnLinux closes a written copy so its preserved times
// survive on SMB/CIFS. The Linux client defers the server close of a leased
// handle by closetimeo; when that close arrives, the server stamps the close
// time over the time set through the handle. A byte-range lock makes the
// client close synchronously (CIFS_INO_CLOSE_ON_LOCK), and the times are then
// set again through a fresh read-only handle that performs no writes.
// https://github.com/torvalds/linux/blob/master/fs/smb/client/file.c
// Mounts with nobrl keep locks local, so the close stays deferred there.
func closeExclusiveCopyOnLinux(file organizationTemporaryFile, created os.FileInfo, atime, mtime time.Time,
	fileSystemType func(organizationTemporaryFile) (uint32, error),
	lock func(organizationTemporaryFile) error,
	reopen func(string) (organizationTemporaryFile, error)) error {
	name := file.Name()
	kind, err := fileSystemType(file)
	if err != nil {
		zap.S().Warnw("Could not identify destination filesystem before close; an SMB server may replace its modification time", "destination", name, "error", err)
		return file.Close()
	}
	if !isLinuxSMBFileSystem(kind) {
		return file.Close()
	}
	if err := lock(file); err != nil {
		zap.S().Warnw("Could not lock SMB destination to force a synchronous close; the server may replace its modification time", "destination", name, "error", err)
	}
	if err := file.Close(); err != nil {
		return err
	}
	reopened, err := reopen(name)
	if err != nil {
		return fmt.Errorf("reopen %q to restore its modification time after close: %w", name, err)
	}
	current, statErr := reopened.Stat()
	var restoreErr error
	switch {
	case statErr != nil:
		restoreErr = fmt.Errorf("inspect reopened destination %q: %w", name, statErr)
	case !os.SameFile(created, current):
		restoreErr = fmt.Errorf("reopened destination %q no longer identifies the created copy; modification time was not restored", name)
	default:
		if err := setOrganizationOpenFileTimes(reopened, atime, mtime); err != nil {
			restoreErr = fmt.Errorf("restore modification time on %q after close: %w", name, err)
		}
	}
	if err := reopened.Close(); err != nil {
		restoreErr = errors.Join(restoreErr, fmt.Errorf("close reopened destination %q: %w", name, err))
	}
	return restoreErr
}

func linuxOpenFileSystemType(file organizationTemporaryFile) (uint32, error) {
	var info unix.Statfs_t
	err := unix.Fstatfs(int(file.Fd()), &info)
	runtime.KeepAlive(file)
	if err != nil {
		return 0, &os.PathError{Op: "fstatfs", Path: file.Name(), Err: err}
	}
	return uint32(info.Type), nil
}

// lockOrganizationCopyForSynchronousClose takes a write lock over the whole
// copy. The lock is released by the close it is meant to make synchronous.
func lockOrganizationCopyForSynchronousClose(file organizationTemporaryFile) error {
	lock := unix.Flock_t{Type: unix.F_WRLCK, Whence: io.SeekStart}
	err := unix.FcntlFlock(file.Fd(), unix.F_SETLK, &lock)
	runtime.KeepAlive(file)
	if err != nil {
		return &os.PathError{Op: "fcntl(F_SETLK)", Path: file.Name(), Err: err}
	}
	return nil
}
