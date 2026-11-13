//go:build linux

package process

import (
	"os"
	"runtime"
	"time"
	"unsafe"

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
