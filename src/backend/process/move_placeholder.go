package process

import (
	"errors"
	"fmt"
	"os"

	"go.uber.org/zap"
)

// errPlaceholderRenameUnavailable reports that the filesystem refused the
// plain rename itself, for example across filesystems. The placeholder was
// removed, so the caller may copy the file instead.
var errPlaceholderRenameUnavailable = errors.New("rename over placeholder is unavailable")

// renameOverPlaceholder moves a file without copying its data on filesystems,
// such as macOS smbfs, that support neither exclusive rename nor hard links.
//
// It first claims the destination name by creating an empty file there with
// O_CREATE|O_EXCL, which fails if anything already has that name. It then
// renames the source over that placeholder with the platform's ordinary,
// replacing rename. On a network share the server performs the rename, so no
// file data crosses the network and nothing is partially written.
//
// The only file the rename can replace is the empty placeholder, checked
// immediately before the rename to still be the file this move created. If
// another client replaced or wrote to it, the move stops and keeps both files.
func renameOverPlaceholder(move OrganizationMove, operations organizationFileOperations) error {
	if err := verifyMoveSourcePath(move, operations.lstat); err != nil {
		return err
	}
	placeholder, err := operations.createPlaceholder(move.Destination)
	if err != nil {
		return fmt.Errorf("create destination %q with O_CREATE|O_EXCL for source %q: %w", move.Destination, move.Source, err)
	}
	created, statErr := placeholder.Stat()
	closeErr := placeholder.Close()
	if statErr != nil {
		// Without the handle's identity the path cannot be proven to be this
		// placeholder, so it is not removed.
		return fmt.Errorf("empty placeholder %q was retained and source %q was not moved: inspect placeholder: %w",
			move.Destination, move.Source, errors.Join(statErr, closeErr))
	}
	if closeErr != nil {
		return discardPlaceholder(move, created, operations, fmt.Errorf("close placeholder: %w", closeErr))
	}
	current, err := operations.lstat(move.Destination)
	if err != nil {
		return discardPlaceholder(move, created, operations, fmt.Errorf("inspect placeholder: %w", err))
	}
	if !isOwnPlaceholder(created, current) {
		return fmt.Errorf("destination %q could not be confirmed as this move's empty placeholder and was retained; source %q was not moved (another client replaced or wrote to it, or the share has unstable file IDs)", move.Destination, move.Source)
	}
	if err := verifyMoveSourcePath(move, operations.lstat); err != nil {
		return discardPlaceholder(move, created, operations, err)
	}
	if err := operations.rename(move.Source, move.Destination); err != nil {
		renameErr := fmt.Errorf("rename %q over placeholder %q: %w", move.Source, move.Destination, err)
		if isUnsupportedRenameError(err) || operations.isCrossDevice(err) {
			if discardErr := discardPlaceholder(move, created, operations, renameErr); !isPlaceholderRemoved(discardErr) {
				return discardErr
			}
			return fmt.Errorf("%w: %w", errPlaceholderRenameUnavailable, renameErr)
		}
		return discardPlaceholder(move, created, operations, renameErr)
	}
	return verifyRenamedOverPlaceholder(move, operations)
}

// verifyRenamedOverPlaceholder confirms the destination now holds the source
// file and the source name is gone. The rename has already happened, so a
// mismatch is reported to stop the run rather than repaired.
func verifyRenamedOverPlaceholder(move OrganizationMove, operations organizationFileOperations) error {
	moved, err := operations.lstat(move.Destination)
	if err != nil {
		return fmt.Errorf("renamed %q to %q but could not inspect the moved file: %w", move.Source, move.Destination, err)
	}
	if !moved.Mode().IsRegular() || moved.Size() != move.SourceSize {
		return fmt.Errorf("moved file at %q does not match source %q: %d bytes, expected %d", move.Destination, move.Source, moved.Size(), move.SourceSize)
	}
	if move.SourceStamp != nil && !os.SameFile(move.SourceStamp, moved) {
		// Shares without stable file IDs can report a new ID after a rename.
		// The size must match, and so must the time, which rename keeps.
		if !modTimesWithin(moved.ModTime(), move.SourceStamp.ModTime(), exclusiveDestinationTimeTolerance) {
			return fmt.Errorf("moved file at %q does not match source %q: modified %v, expected %v", move.Destination, move.Source, moved.ModTime(), move.SourceStamp.ModTime())
		}
		zap.S().Warnw("Moved file has a new file ID after rename", "source", move.Source, "destination", move.Destination)
	}
	if _, err := operations.lstat(move.Source); err == nil {
		return fmt.Errorf("source %q still exists after it was renamed to %q", move.Source, move.Destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("renamed %q to %q but could not confirm the source name is gone: %w", move.Source, move.Destination, err)
	}
	return nil
}

func isOwnPlaceholder(created, current os.FileInfo) bool {
	return current.Mode().IsRegular() && current.Size() == 0 && os.SameFile(created, current)
}

// errPlaceholderRemoved marks the outcome where cleanup removed the placeholder.
var errPlaceholderRemoved = errors.New("placeholder was removed")

func isPlaceholderRemoved(err error) bool {
	return errors.Is(err, errPlaceholderRemoved)
}

// discardPlaceholder removes the move's empty placeholder after a failure,
// but only while the path still identifies it and it is still empty.
func discardPlaceholder(move OrganizationMove, created os.FileInfo, operations organizationFileOperations, cause error) error {
	current, err := operations.lstat(move.Destination)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("placeholder %q is gone; source %q was not moved: %w", move.Destination, move.Source, cause)
	case err != nil:
		cause = errors.Join(cause, fmt.Errorf("inspect placeholder %q: %w", move.Destination, err))
	case !isOwnPlaceholder(created, current):
		return fmt.Errorf("destination %q changed after its placeholder was created and was retained; source %q was not moved: %w", move.Destination, move.Source, cause)
	default:
		removeErr := operations.remove(move.Destination)
		if removeErr == nil {
			return fmt.Errorf("%w: %q; source %q was not moved: %w", errPlaceholderRemoved, move.Destination, move.Source, cause)
		}
		cause = errors.Join(cause, fmt.Errorf("remove placeholder %q: %w", move.Destination, removeErr))
	}
	return fmt.Errorf("empty placeholder %q was retained and source %q was not moved: %w", move.Destination, move.Source, cause)
}
