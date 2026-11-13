package process

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"go.uber.org/zap"
)

type organizationSourceFile interface {
	io.Reader
	Stat() (os.FileInfo, error)
	Close() error
}

type organizationTemporaryFile interface {
	io.Writer
	Name() string
	Fd() uintptr
	Stat() (os.FileInfo, error)
	Chmod(os.FileMode) error
	Sync() error
	Close() error
}

type organizationFileOperations struct {
	renameNoReplace func(string, string) error
	isCrossDevice   func(error) bool
	openSource      func(string) (organizationSourceFile, error)
	createTemp      func(string, string) (organizationTemporaryFile, error)
	createExclusive func(string) (organizationTemporaryFile, error)
	lstat           func(string) (os.FileInfo, error)
	remove          func(string) error
	chtimes         func(string, time.Time, time.Time) error
	chtimesOpen     func(organizationTemporaryFile, time.Time, time.Time) error
}

func defaultOrganizationFileOperations() organizationFileOperations {
	return organizationFileOperations{
		renameNoReplace: renameNoReplace,
		isCrossDevice:   isCrossDeviceMoveError,
		openSource: func(path string) (organizationSourceFile, error) {
			return os.Open(path)
		},
		createTemp: func(directory, pattern string) (organizationTemporaryFile, error) {
			return os.CreateTemp(directory, pattern)
		},
		createExclusive: func(path string) (organizationTemporaryFile, error) {
			return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		},
		lstat:       os.Lstat,
		remove:      os.Remove,
		chtimes:     os.Chtimes,
		chtimesOpen: setOrganizationOpenFileTimes,
	}
}

func moveFile(from, to string) error {
	if sameOrganizationPath(from, to) {
		return nil
	}
	info, err := os.Lstat(from)
	if err != nil {
		return err
	}
	return moveOrganizationFile(OrganizationMove{Source: from, Destination: to, SourceSize: info.Size(), SourceStamp: info})
}

func moveOrganizationFile(move OrganizationMove) error {
	if sameOrganizationPath(move.Source, move.Destination) {
		return nil
	}
	return moveFileWithOperations(move, defaultOrganizationFileOperations())
}

// errExclusivePublicationUnsupported reports that a temporary copy could not
// be renamed into place without replacing; the temporary copy was removed.
var errExclusivePublicationUnsupported = errors.New("exclusive publication of temporary copy is unsupported")

// exclusiveDestinationTimeTolerance covers filesystems that round timestamps,
// such as FAT/exFAT-backed shares (2s) and microsecond Unix handle updates.
const exclusiveDestinationTimeTolerance = 2 * time.Second

func moveFileWithOperations(move OrganizationMove, operations organizationFileOperations) error {
	err := operations.renameNoReplace(move.Source, move.Destination)
	switch {
	case err == nil:
		return nil
	case isUnsupportedRenameError(err):
		zap.S().Infow("Exclusive rename unavailable; copying to an exclusively created destination", "source", move.Source, "destination", move.Destination, "error", err)
		return copyOrganizationFile(move, operations, true)
	case operations.isCrossDevice(err):
		return moveAcrossFileSystems(move, operations)
	default:
		return fmt.Errorf("exclusive rename %q to %q: %w", move.Source, move.Destination, err)
	}
}

// moveAcrossFileSystems copies through a temporary file and publishes it with
// an exclusive rename. When the destination filesystem cannot rename
// exclusively, the original source is copied straight to an exclusively
// created destination instead, so the data crosses the network only once.
func moveAcrossFileSystems(move OrganizationMove, operations organizationFileOperations) error {
	directory := filepath.Dir(move.Destination)
	supported, probeErr := probeExclusiveRename(directory, operations)
	if probeErr != nil {
		zap.S().Warnw("Exclusive rename probe failed", "directory", directory, "assume_supported", supported, "error", probeErr)
	}
	if !supported {
		zap.S().Infow("Exclusive rename unavailable at destination; copying to an exclusively created destination", "source", move.Source, "destination", move.Destination)
		return copyOrganizationFile(move, operations, true)
	}
	err := copyOrganizationFile(move, operations, false)
	if errors.Is(err, errExclusivePublicationUnsupported) {
		zap.S().Infow("Exclusive publication unavailable; copying source to an exclusively created destination", "source", move.Source, "destination", move.Destination, "error", err)
		return copyOrganizationFile(move, operations, true)
	}
	return err
}

// probeExclusiveRename renames an empty scratch file inside directory to learn
// whether the filesystem supports exclusive rename. If the probe cannot reach
// a conclusion it reports supported, so the temporary-copy path and its own
// unsupported-publication fallback are used.
func probeExclusiveRename(directory string, operations organizationFileOperations) (bool, error) {
	renameErr, err := runRenameProbe(directory, operations.createTemp, operations.renameNoReplace, operations.remove)
	if renameErr == nil {
		return true, err
	}
	if isUnsupportedRenameError(renameErr) {
		return false, err
	}
	return true, errors.Join(fmt.Errorf("probe exclusive rename in %q: %w", directory, renameErr), err)
}

// runRenameProbe returns the error from renaming a fresh empty file inside
// directory (nil when the rename worked) and, separately, any setup or cleanup
// error. Both names are unique to this probe, so removing them is safe.
func runRenameProbe(directory string, createTemp func(string, string) (organizationTemporaryFile, error), rename func(string, string) error, remove func(string) error) (renameErr, err error) {
	probe, err := createTemp(directory, ".slm-probe-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("create rename probe in %q: %w", directory, err)
	}
	name := probe.Name()
	target := name + ".renamed"
	if closeErr := probe.Close(); closeErr != nil {
		err = fmt.Errorf("close rename probe %q: %w", name, closeErr)
	}
	renameErr = rename(name, target)
	leftover := name
	if renameErr == nil {
		leftover = target
	}
	if removeErr := remove(leftover); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		err = errors.Join(err, fmt.Errorf("remove rename probe %q: %w", leftover, removeErr))
	}
	return renameErr, err
}

// An exclusive destination is visible while copying. On failure it is removed
// only while it is still provably the file this copy created; otherwise it is
// retained, because removing it by name could remove a file placed there by
// another client. The original is removed only after a complete, flushed,
// closed copy.
func copyOrganizationFile(move OrganizationMove, operations organizationFileOperations, exclusiveDestination bool) (retErr error) {
	source, err := operations.openSource(move.Source)
	if err != nil {
		return fmt.Errorf("open source for copy: %w", err)
	}
	sourceOpen := true
	defer func() {
		if sourceOpen {
			if err := source.Close(); err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("close source after copy: %w", err))
			}
		}
	}()

	initial, err := source.Stat()
	if err != nil {
		return fmt.Errorf("inspect source before copy: %w", err)
	}
	if err := validateMoveSource(move, initial); err != nil {
		return err
	}
	pathInfo, err := operations.lstat(move.Source)
	if err != nil {
		return fmt.Errorf("inspect source path before copy: %w", err)
	}
	if err := validateMoveSource(move, pathInfo); err != nil || !os.SameFile(initial, pathInfo) {
		if err != nil {
			return err
		}
		return fmt.Errorf("source %q changed before copy", move.Source)
	}

	var destination organizationTemporaryFile
	if exclusiveDestination {
		destination, err = operations.createExclusive(move.Destination)
		if err != nil {
			return fmt.Errorf("create destination %q with O_CREATE|O_EXCL for source %q: %w", move.Destination, move.Source, err)
		}
	} else {
		destination, err = operations.createTemp(filepath.Dir(move.Destination), ".slm-move-*.tmp")
		if err != nil {
			return fmt.Errorf("create temporary destination for copy: %w", err)
		}
	}
	destinationOpen := true
	published := false
	temporaryMoved := false
	// createdStamp identifies the exclusively created file, so cleanup and
	// verification never act on a replacement placed at the same name.
	var createdStamp os.FileInfo
	defer func() {
		if destinationOpen {
			if err := destination.Close(); err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("close destination copy %q: %w", destination.Name(), err))
			}
		}
		if !exclusiveDestination {
			if !temporaryMoved {
				if err := operations.remove(destination.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
					retErr = errors.Join(retErr, fmt.Errorf("remove temporary copy %q: %w", destination.Name(), err))
				}
			}
			return
		}
		if retErr != nil && !published {
			retErr = discardIncompleteExclusiveDestination(move.Destination, createdStamp, operations, retErr)
		}
	}()

	if exclusiveDestination {
		createdStamp, err = destination.Stat()
		if err != nil {
			return fmt.Errorf("inspect exclusive destination handle %q: %w", move.Destination, err)
		}
	}
	copied, err := io.Copy(destination, source)
	if err != nil {
		return fmt.Errorf("copy source to destination copy %q: %w", destination.Name(), err)
	}
	if copied != initial.Size() {
		return fmt.Errorf("copy of %q wrote %d bytes, expected %d", move.Source, copied, initial.Size())
	}
	if err := destination.Chmod(initial.Mode().Perm()); err != nil {
		return fmt.Errorf("preserve source permissions on destination copy: %w", err)
	}
	if err := destination.Sync(); err != nil {
		return fmt.Errorf("flush destination copy: %w", err)
	}
	// SMB servers can update the modification time when flushing pending writes.
	// Set it after the data flush so the preserved time survives that flush.
	var timeErr error
	if exclusiveDestination {
		timeErr = operations.chtimesOpen(destination, initial.ModTime(), initial.ModTime())
	} else {
		timeErr = operations.chtimes(destination.Name(), initial.ModTime(), initial.ModTime())
	}
	if timeErr != nil {
		return fmt.Errorf("preserve source modification time on destination copy: %w", timeErr)
	}
	if exclusiveDestination {
		handleInfo, err := destination.Stat()
		if err != nil {
			return fmt.Errorf("inspect exclusive destination handle %q: %w", move.Destination, err)
		}
		if handleInfo.Size() != initial.Size() {
			return fmt.Errorf("exclusive destination %q has %d bytes, expected %d", move.Destination, handleInfo.Size(), initial.Size())
		}
	}
	destinationOpen = false
	if err := destination.Close(); err != nil {
		return fmt.Errorf("close destination copy %q: %w", destination.Name(), err)
	}

	finalInfo, err := source.Stat()
	if err != nil {
		return fmt.Errorf("verify source after copy: %w", err)
	}
	if err := validateMoveSource(move, finalInfo); err != nil || !os.SameFile(initial, finalInfo) {
		if err != nil {
			return err
		}
		return fmt.Errorf("source %q changed during copy", move.Source)
	}
	sourceOpen = false
	if err := source.Close(); err != nil {
		return fmt.Errorf("close source before publishing copy: %w", err)
	}
	if err := verifyMoveSourcePath(move, operations.lstat); err != nil {
		return err
	}
	if exclusiveDestination {
		if err := verifyExclusiveDestination(move, createdStamp, initial, operations.lstat); err != nil {
			return err
		}
	} else {
		if err := operations.renameNoReplace(destination.Name(), move.Destination); err != nil {
			if isUnsupportedRenameError(err) {
				return fmt.Errorf("%w: %w", errExclusivePublicationUnsupported, err)
			}
			return fmt.Errorf("publish copy %q at %q: %w", destination.Name(), move.Destination, err)
		}
		temporaryMoved = true
	}
	published = true
	if err := verifyMoveSourcePath(move, operations.lstat); err != nil {
		return fmt.Errorf("copy was published at %q but source was retained: %w", move.Destination, err)
	}
	if err := operations.remove(move.Source); err != nil {
		return fmt.Errorf("copy was published at %q but source %q could not be removed: %w", move.Destination, move.Source, err)
	}
	return nil
}

// verifyExclusiveDestination confirms that the closed destination path is
// still the copy this move wrote. SMB clients do not always report stable file
// IDs, so a matching size and preserved modification time also identify it.
func verifyExclusiveDestination(move OrganizationMove, created, source os.FileInfo, lstat func(string) (os.FileInfo, error)) error {
	current, err := lstat(move.Destination)
	if err != nil {
		return fmt.Errorf("verify exclusive destination %q before source removal: %w", move.Destination, err)
	}
	if !current.Mode().IsRegular() || current.Size() != source.Size() {
		return fmt.Errorf("exclusive destination %q changed during copy; source was retained", move.Destination)
	}
	timeMatches := modTimesWithin(current.ModTime(), source.ModTime(), exclusiveDestinationTimeTolerance)
	if os.SameFile(created, current) {
		if !timeMatches {
			zap.S().Warnw("Destination modification time changed after close", "destination", move.Destination, "want", source.ModTime(), "got", current.ModTime())
		}
		return nil
	}
	if !timeMatches {
		return fmt.Errorf("exclusive destination %q changed during copy; source was retained", move.Destination)
	}
	zap.S().Infow("Destination file identity differs after close; accepted matching size and modification time", "destination", move.Destination)
	return nil
}

// discardIncompleteExclusiveDestination removes a failed exclusive copy while
// the path still refers to the file this move created, and otherwise keeps it.
func discardIncompleteExclusiveDestination(path string, created os.FileInfo, operations organizationFileOperations, cause error) error {
	if created != nil {
		current, err := operations.lstat(path)
		switch {
		case errors.Is(err, os.ErrNotExist):
			return fmt.Errorf("exclusive destination %q no longer exists; source was not removed: %w", path, cause)
		case err != nil:
			cause = errors.Join(cause, fmt.Errorf("inspect incomplete destination %q: %w", path, err))
		case current.Mode().IsRegular() && os.SameFile(created, current):
			if err := operations.remove(path); err == nil {
				return fmt.Errorf("incomplete exclusive destination %q was removed; source was not removed: %w", path, cause)
			} else {
				cause = errors.Join(cause, fmt.Errorf("remove incomplete destination %q: %w", path, err))
			}
		}
	}
	return fmt.Errorf("exclusive destination %q may be incomplete and was retained; source was not removed: %w", path, cause)
}

func modTimesWithin(a, b time.Time, tolerance time.Duration) bool {
	difference := a.Sub(b)
	return difference <= tolerance && difference >= -tolerance
}

func validateMoveSource(move OrganizationMove, current os.FileInfo) error {
	if current == nil || !current.Mode().IsRegular() {
		return fmt.Errorf("source %q is no longer a regular file", move.Source)
	}
	if move.SourceStamp != nil &&
		(!os.SameFile(move.SourceStamp, current) || current.Size() != move.SourceSize || !current.ModTime().Equal(move.SourceStamp.ModTime())) {
		return fmt.Errorf("source %q changed after organization preflight", move.Source)
	}
	return nil
}

func verifyMoveSourcePath(move OrganizationMove, lstat func(string) (os.FileInfo, error)) error {
	current, err := lstat(move.Source)
	if err != nil {
		return fmt.Errorf("inspect source %q before removal: %w", move.Source, err)
	}
	if err := validateMoveSource(move, current); err != nil {
		return err
	}
	return nil
}
