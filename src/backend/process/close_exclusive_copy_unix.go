//go:build darwin || linux

package process

import (
	"errors"
	"fmt"
	"os"
	"time"

	"go.uber.org/zap"
)

// closeExclusiveCopyRestoringTimes closes a written copy so that its preserved
// times survive on an SMB destination. The Linux and macOS SMB clients both
// hold back the server close of a written handle, and the server stamps the
// close time over any time set through that handle when the close arrives.
// On SMB, the times are therefore set again through a fresh read-only handle
// that performs no writes, after confirming the path still names the created
// copy. beforeClose, when set, runs on SMB destinations only, just before the
// close. Other filesystems are only closed. The handle is always released.
func closeExclusiveCopyRestoringTimes(file organizationTemporaryFile, created os.FileInfo, atime, mtime time.Time,
	isSMB func(organizationTemporaryFile) (bool, error),
	beforeClose func(organizationTemporaryFile),
	reopen func(string) (organizationTemporaryFile, error)) error {
	name := file.Name()
	smb, err := isSMB(file)
	if err != nil {
		zap.S().Warnw("Could not identify destination filesystem before close; an SMB server may replace its modification time", "destination", name, "error", err)
		return file.Close()
	}
	if !smb {
		return file.Close()
	}
	if beforeClose != nil {
		beforeClose(file)
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

func openOrganizationCopyReadOnly(path string) (organizationTemporaryFile, error) {
	return os.Open(path)
}
