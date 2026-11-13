//go:build windows

package process

import "errors"

// isUnsupportedRenameError matches ERROR_NOT_SUPPORTED and
// ERROR_CALL_NOT_IMPLEMENTED, which syscall.Errno maps to errors.ErrUnsupported.
func isUnsupportedRenameError(err error) bool {
	return errors.Is(err, errors.ErrUnsupported)
}
