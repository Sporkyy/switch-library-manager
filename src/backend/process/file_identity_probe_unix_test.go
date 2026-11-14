//go:build darwin || linux

package process

import (
	"fmt"
	"os"
	"syscall"
)

func fileIdentityForLog(info os.FileInfo) string {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return fmt.Sprintf("dev=%d ino=%d", stat.Dev, stat.Ino)
	}
	return "unavailable"
}
