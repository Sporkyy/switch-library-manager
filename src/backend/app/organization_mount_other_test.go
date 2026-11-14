//go:build !linux

package app

import (
	"os"
	"time"
)

// serverModTime falls back to lstat where no forced revalidation is
// available. Callers wait out the attribute cache before relying on it.
func serverModTime(path string) (time.Time, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return time.Time{}, err
	}
	return info.ModTime(), nil
}
