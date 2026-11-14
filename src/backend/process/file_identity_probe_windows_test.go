//go:build windows

package process

import "os"

// Windows FileInfo exposes the volume serial and file index only through
// os.SameFile, so the probe reports the comparison results instead.
func fileIdentityForLog(os.FileInfo) string {
	return "n/a"
}
