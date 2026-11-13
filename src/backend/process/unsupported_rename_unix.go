//go:build darwin || linux

package process

import "errors"

// isUnsupportedRenameError matches EOPNOTSUPP, ENOTSUP and ENOSYS, which
// syscall.Errno maps to errors.ErrUnsupported. Linux EINVAL is classified in
// renameNoReplaceOnLinux, where it can be told apart from invalid paths.
func isUnsupportedRenameError(err error) bool {
	return errors.Is(err, errors.ErrUnsupported)
}
