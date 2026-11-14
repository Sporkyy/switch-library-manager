//go:build linux

package process

import (
	"errors"
	"os"
	"path/filepath"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

func renameNoReplace(from, to string) error {
	return renameNoReplaceOnLinux(from, to, linuxOrganizationFileSystemType, func(from, to string) error {
		return unix.Renameat2(unix.AT_FDCWD, from, unix.AT_FDCWD, to, unix.RENAME_NOREPLACE)
	}, func(directory, pattern string) (organizationTemporaryFile, error) {
		return os.CreateTemp(directory, pattern)
	})
}

func linuxOrganizationFileSystemType(path string) (uint32, error) {
	var info unix.Statfs_t
	if err := unix.Statfs(path, &info); err != nil {
		return 0, err
	}
	return uint32(info.Type), nil
}

func renameNoReplaceOnLinux(from, to string, fileSystemType func(string) (uint32, error), rename func(string, string) error, createTemp func(string, string) (organizationTemporaryFile, error)) error {
	directory := filepath.Dir(to)
	kind, err := fileSystemType(directory)
	if err != nil {
		// The SMB check is a safeguard, not a precondition: statfs can fail on
		// FUSE and network mounts where renameat2 itself still works.
		zap.S().Warnw("Could not identify destination filesystem before exclusive rename", "directory", directory, "error", err)
	} else if isLinuxSMBFileSystem(kind) {
		// Linux CIFS cifs_rename2 does not pass RENAME_NOREPLACE to
		// cifs_do_rename; SMB2_OP_RENAME sets ReplaceIfExists = 1.
		// Another client's destination can therefore be overwritten.
		// https://github.com/torvalds/linux/blob/master/fs/smb/client/inode.c
		// https://github.com/torvalds/linux/blob/master/fs/smb/client/smb2inode.c
		// Use the exclusive-create copy path instead, including publication.
		return &os.LinkError{Op: "renameat2(RENAME_NOREPLACE) unavailable on Linux SMB/CIFS", Old: from, New: to, Err: unix.EOPNOTSUPP}
	}
	renameErr := rename(from, to)
	if renameErr == nil {
		return nil
	}
	if errors.Is(renameErr, unix.EINVAL) && linuxNoReplaceUnsupported(directory, rename, createTemp) {
		// Filesystems without RENAME_NOREPLACE support reject the flag with
		// EINVAL. A scratch rename in the same directory failing the same way
		// distinguishes that from EINVAL caused by these particular paths.
		return &os.LinkError{Op: "renameat2(RENAME_NOREPLACE) unsupported by destination filesystem", Old: from, New: to, Err: errors.Join(unix.EOPNOTSUPP, renameErr)}
	}
	return &os.LinkError{Op: "renameat2(RENAME_NOREPLACE)", Old: from, New: to, Err: renameErr}
}

func isLinuxSMBFileSystem(kind uint32) bool {
	switch kind {
	case unix.CIFS_SUPER_MAGIC, unix.SMB2_SUPER_MAGIC, unix.SMB_SUPER_MAGIC:
		return true
	}
	return false
}

func linuxNoReplaceUnsupported(directory string, rename func(string, string) error, createTemp func(string, string) (organizationTemporaryFile, error)) bool {
	probeErr, err := runRenameProbe(directory, createTemp, rename, os.Remove)
	if err != nil {
		zap.S().Warnw("Exclusive rename probe failed", "directory", directory, "error", err)
	}
	return probeErr != nil && errors.Is(probeErr, unix.EINVAL)
}
