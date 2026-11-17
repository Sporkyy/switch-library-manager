package process

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// placeholderTestOperations behaves like a filesystem, such as macOS smbfs,
// that rejects exclusive rename but supports a plain, replacing rename. Any
// attempt to copy the source fails the test.
func placeholderTestOperations(t *testing.T) organizationFileOperations {
	t.Helper()
	operations := defaultOrganizationFileOperations()
	operations.renameNoReplace = func(string, string) error { return errors.ErrUnsupported }
	operations.openSource = func(path string) (organizationSourceFile, error) {
		t.Fatalf("source %q was copied instead of renamed", path)
		return nil, errors.New("unreachable")
	}
	return operations
}

// A move within one share must not copy file data: the source is renamed
// over an empty placeholder the move created exclusively at the destination.
func TestUnsupportedExclusiveRenameRenamesOverPlaceholder(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.nsp")
	destination := filepath.Join(root, "Game", "renamed.nsp")
	mustMkdir(t, filepath.Dir(destination))
	writeFixture(t, source, "synthetic renamed content")
	stamp := time.Date(2020, 2, 3, 4, 5, 6, 0, time.UTC)
	if err := os.Chtimes(source, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	move := organizationMoveForTest(t, source, destination)
	operations := placeholderTestOperations(t)
	renamed := 0
	operations.rename = func(from, to string) error {
		renamed++
		info, err := os.Lstat(to)
		if err != nil || info.Size() != 0 {
			t.Fatalf("rename target before rename = %v, error = %v; want the empty placeholder", info, err)
		}
		return os.Rename(from, to)
	}

	if err := moveFileWithOperations(move, operations); err != nil {
		t.Fatalf("move = %v", err)
	}
	if renamed != 1 {
		t.Fatalf("plain renames = %d, want 1", renamed)
	}
	assertNotExists(t, source)
	assertUnsupportedTestContents(t, destination, "synthetic renamed content")
	info, err := os.Lstat(destination)
	if err != nil || !info.ModTime().Equal(stamp) || !os.SameFile(info, move.SourceStamp) {
		t.Fatalf("destination = %v, error = %v; want the original file with its time", info, err)
	}
	entries, err := os.ReadDir(filepath.Dir(destination))
	if err != nil || len(entries) != 1 {
		t.Fatalf("destination folder = %v, error = %v; want only the moved file", entries, err)
	}
}

// An existing destination, including one created after planning, is never
// replaced: the placeholder cannot be created, so nothing is renamed.
func TestPlaceholderRenameNeverReplacesAnExistingDestination(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.nsp")
	destination := filepath.Join(root, "renamed.nsp")
	writeFixture(t, source, "synthetic source")
	move := organizationMoveForTest(t, source, destination)
	writeFixture(t, destination, "late destination")
	operations := placeholderTestOperations(t)
	operations.rename = func(string, string) error {
		t.Fatal("renamed although the destination already existed")
		return nil
	}

	err := moveFileWithOperations(move, operations)
	if !errors.Is(err, os.ErrExist) || !strings.Contains(err.Error(), "O_CREATE|O_EXCL") || !strings.Contains(err.Error(), destination) {
		t.Fatalf("move error = %v, want an exclusive-creation collision naming the destination", err)
	}
	assertUnsupportedTestContents(t, source, "synthetic source")
	assertUnsupportedTestContents(t, destination, "late destination")
}

// If another client replaces the placeholder before the rename, its file is
// kept, the source is kept, and the error names the destination.
func TestPlaceholderRenameKeepsAReplacedPlaceholder(t *testing.T) {
	for _, replacement := range []string{"other file", "written into placeholder"} {
		t.Run(replacement, func(t *testing.T) {
			root := t.TempDir()
			source := filepath.Join(root, "source.nsp")
			destination := filepath.Join(root, "renamed.nsp")
			writeFixture(t, source, "synthetic source")
			move := organizationMoveForTest(t, source, destination)
			operations := placeholderTestOperations(t)
			create := operations.createPlaceholder
			operations.createPlaceholder = func(path string) (organizationTemporaryFile, error) {
				file, err := create(path)
				if err != nil {
					return nil, err
				}
				if replacement == "other file" {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				}
				writeFixture(t, path, "another client's file")
				return file, nil
			}
			operations.rename = func(string, string) error {
				t.Fatal("renamed over a placeholder that was replaced")
				return nil
			}

			err := moveFileWithOperations(move, operations)
			if err == nil || !strings.Contains(err.Error(), destination) || !strings.Contains(err.Error(), "was not moved") {
				t.Fatalf("move error = %v, want a replaced-placeholder error naming the destination", err)
			}
			assertUnsupportedTestContents(t, source, "synthetic source")
			assertUnsupportedTestContents(t, destination, "another client's file")
		})
	}
}

// A source changed after planning is neither renamed nor given a placeholder.
func TestPlaceholderRenameRejectsSourceChangedAfterPlanning(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.nsp")
	destination := filepath.Join(root, "renamed.nsp")
	writeFixture(t, source, "synthetic source")
	move := organizationMoveForTest(t, source, destination)
	writeFixture(t, source, "synthetic source, changed")
	operations := placeholderTestOperations(t)

	err := moveFileWithOperations(move, operations)
	if err == nil || !strings.Contains(err.Error(), "changed after organization preflight") {
		t.Fatalf("move error = %v, want a changed-source error", err)
	}
	assertUnsupportedTestContents(t, source, "synthetic source, changed")
	assertNotExists(t, destination)
}

func TestPlaceholderRenameRechecksDestinationAfterSource(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.nsp")
	destination := filepath.Join(root, "renamed.nsp")
	writeFixture(t, source, "synthetic source")
	move := organizationMoveForTest(t, source, destination)
	operations := placeholderTestOperations(t)
	sourceChecks := 0
	operations.lstat = func(path string) (os.FileInfo, error) {
		if path == source {
			sourceChecks++
			if sourceChecks == 2 {
				writeFixture(t, destination, "another client's file")
			}
		}
		return os.Lstat(path)
	}
	if err := moveFileWithOperations(move, operations); err == nil {
		t.Fatal("move replaced a destination written during the source check")
	}
	assertUnsupportedTestContents(t, source, "synthetic source")
	assertUnsupportedTestContents(t, destination, "another client's file")
}

// A plain rename that fails for an ordinary reason removes the placeholder,
// keeps the source, and does not fall back to copying the file.
func TestPlaceholderRenameFailureRemovesPlaceholder(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.nsp")
	destination := filepath.Join(root, "renamed.nsp")
	writeFixture(t, source, "synthetic source")
	move := organizationMoveForTest(t, source, destination)
	operations := placeholderTestOperations(t)
	injected := errors.New("injected rename failure")
	operations.rename = func(string, string) error { return injected }

	err := moveFileWithOperations(move, operations)
	if !errors.Is(err, injected) || !strings.Contains(err.Error(), destination) {
		t.Fatalf("move error = %v, want the rename failure naming the destination", err)
	}
	assertUnsupportedTestContents(t, source, "synthetic source")
	assertNotExists(t, destination)
}

// When a plain rename is unsupported too, or the source is on another
// filesystem, the placeholder is removed and the move copies the file.
func TestPlaceholderRenameFallsBackToCopy(t *testing.T) {
	for _, kind := range []string{"unsupported", "cross device"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			source := filepath.Join(root, "source.nsp")
			destination := filepath.Join(root, "library", "renamed.nsp")
			mustMkdir(t, filepath.Dir(destination))
			writeFixture(t, source, "synthetic copied content")
			move := organizationMoveForTest(t, source, destination)
			operations := defaultOrganizationFileOperations()
			operations.renameNoReplace = func(string, string) error { return errors.ErrUnsupported }
			renameErr := error(errors.ErrUnsupported)
			if kind == "cross device" {
				renameErr = crossDeviceTestError
				operations.isCrossDevice = func(err error) bool { return errors.Is(err, crossDeviceTestError) }
			}
			operations.rename = func(string, string) error { return renameErr }
			copied := 0
			open := operations.openSource
			operations.openSource = func(path string) (organizationSourceFile, error) {
				copied++
				return open(path)
			}

			if err := moveFileWithOperations(move, operations); err != nil {
				t.Fatalf("move = %v", err)
			}
			if copied != 1 {
				t.Fatalf("copies = %d, want 1", copied)
			}
			assertNotExists(t, source)
			assertUnsupportedTestContents(t, destination, "synthetic copied content")
			entries, err := os.ReadDir(filepath.Dir(destination))
			if err != nil || len(entries) != 1 {
				t.Fatalf("destination folder = %v, error = %v; want only the copied file", entries, err)
			}
		})
	}
}

// A placeholder whose handle cannot be inspected cannot be proven to be the
// move's own file, so it is left in place and named in the error.
func TestPlaceholderRenameRetainsUnidentifiedPlaceholder(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.nsp")
	destination := filepath.Join(root, "renamed.nsp")
	writeFixture(t, source, "synthetic source")
	move := organizationMoveForTest(t, source, destination)
	operations := placeholderTestOperations(t)
	injected := errors.New("injected placeholder stat failure")
	create := operations.createPlaceholder
	operations.createPlaceholder = func(path string) (organizationTemporaryFile, error) {
		file, err := create(path)
		if err != nil {
			return nil, err
		}
		return testTemporaryFile{file: file.(*os.File), statErr: injected}, nil
	}

	err := moveFileWithOperations(move, operations)
	if !errors.Is(err, injected) || !strings.Contains(err.Error(), "was retained") || !strings.Contains(err.Error(), destination) {
		t.Fatalf("move error = %v, want the stat failure and the retained placeholder path", err)
	}
	assertUnsupportedTestContents(t, source, "synthetic source")
	assertExists(t, destination)
}

// After the rename, a destination that does not match the source is reported
// so the run stops instead of continuing on an unexpected result.
func TestPlaceholderRenameVerifiesTheMovedFile(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.nsp")
	destination := filepath.Join(root, "renamed.nsp")
	writeFixture(t, source, "synthetic source")
	move := organizationMoveForTest(t, source, destination)
	operations := placeholderTestOperations(t)
	operations.rename = func(from, to string) error {
		if err := os.Rename(from, to); err != nil {
			return err
		}
		writeFixture(t, to, "different size after rename")
		return nil
	}

	err := moveFileWithOperations(move, operations)
	if err == nil || !strings.Contains(err.Error(), "does not match") || !strings.Contains(err.Error(), destination) {
		t.Fatalf("move error = %v, want a verification failure naming the destination", err)
	}
}

// On a share whose file IDs are unstable, the placeholder cannot be proven to
// be the move's own file, so it is neither replaced nor removed.
func TestPlaceholderRenameStopsWhenPlaceholderIdentityIsUnstable(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.nsp")
	destination := filepath.Join(root, "renamed.nsp")
	writeFixture(t, source, "synthetic source")
	move := organizationMoveForTest(t, source, destination)
	operations := identityChangingOperations(destination, placeholderTestOperations(t))
	operations.rename = func(string, string) error {
		t.Fatal("renamed over an unidentified placeholder")
		return nil
	}

	err := moveFileWithOperations(move, operations)
	if err == nil || !strings.Contains(err.Error(), "unstable file IDs") || !strings.Contains(err.Error(), "was retained") {
		t.Fatalf("move error = %v, want an unidentified placeholder retained", err)
	}
	assertUnsupportedTestContents(t, source, "synthetic source")
	assertUnsupportedTestContents(t, destination, "")
}

// Shares without stable file IDs can report a new ID after a rename. The move
// is accepted when the size and time match, and rejected when the time differs.
func TestPlaceholderRenameAcceptsNewFileIDOnlyWithMatchingTime(t *testing.T) {
	for _, sameTime := range []bool{true, false} {
		name := "same time"
		if !sameTime {
			name = "different time"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			source := filepath.Join(root, "source.nsp")
			destination := filepath.Join(root, "renamed.nsp")
			writeFixture(t, source, "synthetic source")
			move := organizationMoveForTest(t, source, destination)
			operations := placeholderTestOperations(t)
			renamed := false
			operations.rename = func(from, to string) error {
				if err := os.Rename(from, to); err != nil {
					return err
				}
				renamed = true
				if !sameTime {
					later := move.SourceStamp.ModTime().Add(time.Hour)
					return os.Chtimes(to, later, later)
				}
				return nil
			}
			lstat := operations.lstat
			operations.lstat = func(path string) (os.FileInfo, error) {
				info, err := lstat(path)
				if err == nil && renamed && filepath.Clean(path) == filepath.Clean(destination) {
					return unstableIdentityInfo{info}, nil
				}
				return info, err
			}

			err := moveFileWithOperations(move, operations)
			if sameTime && err != nil {
				t.Fatalf("move = %v, want success despite the new file ID", err)
			}
			if !sameTime && (err == nil || !strings.Contains(err.Error(), "does not match")) {
				t.Fatalf("move error = %v, want a mismatch for a new ID and time", err)
			}
			assertNotExists(t, source)
			assertUnsupportedTestContents(t, destination, "synthetic source")
		})
	}
}

// A source name that still exists after the rename stops the run.
func TestPlaceholderRenameReportsSourceThatStillExists(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.nsp")
	destination := filepath.Join(root, "renamed.nsp")
	writeFixture(t, source, "synthetic source")
	move := organizationMoveForTest(t, source, destination)
	operations := placeholderTestOperations(t)
	operations.rename = func(from, to string) error {
		if err := os.Rename(from, to); err != nil {
			return err
		}
		writeFixture(t, from, "new file at the source name")
		return nil
	}

	err := moveFileWithOperations(move, operations)
	if err == nil || !strings.Contains(err.Error(), "still exists") {
		t.Fatalf("move error = %v, want the remaining source reported", err)
	}
	assertUnsupportedTestContents(t, source, "new file at the source name")
	assertUnsupportedTestContents(t, destination, "synthetic source")
}

// After a failed rename, a placeholder that another client replaced is kept,
// and one that has disappeared is reported; the source is kept either way.
func TestPlaceholderRenameFailureKeepsWhatItCannotIdentify(t *testing.T) {
	for _, state := range []string{"replaced", "gone"} {
		t.Run(state, func(t *testing.T) {
			root := t.TempDir()
			source := filepath.Join(root, "source.nsp")
			destination := filepath.Join(root, "renamed.nsp")
			writeFixture(t, source, "synthetic source")
			move := organizationMoveForTest(t, source, destination)
			operations := placeholderTestOperations(t)
			injected := errors.New("injected rename failure")
			operations.rename = func(_, to string) error {
				if err := os.Remove(to); err != nil {
					t.Fatal(err)
				}
				if state == "replaced" {
					writeFixture(t, to, "another client's file")
				}
				return injected
			}

			err := moveFileWithOperations(move, operations)
			if !errors.Is(err, injected) || !strings.Contains(err.Error(), destination) || !strings.Contains(err.Error(), "was not moved") {
				t.Fatalf("move error = %v, want the rename failure naming the destination", err)
			}
			assertUnsupportedTestContents(t, source, "synthetic source")
			if state == "replaced" {
				assertUnsupportedTestContents(t, destination, "another client's file")
			} else {
				assertNotExists(t, destination)
			}
		})
	}
}
