package process

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func unsupportedRenameTestOperations() organizationFileOperations {
	operations := defaultOrganizationFileOperations()
	operations.renameNoReplace = func(string, string) error { return errors.ErrUnsupported }
	return operations
}

func TestUnsupportedRenamePreservesDestinationCollisions(t *testing.T) {
	for _, kind := range []string{"file", "symlink", "directory", "late file"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			source := filepath.Join(root, "source.nsp")
			destination := filepath.Join(root, "renamed.nsp")
			writeFixture(t, source, "synthetic source")
			move := organizationMoveForTest(t, source, destination)
			operations := unsupportedRenameTestOperations()
			target := filepath.Join(root, "unrelated.nsp")
			switch kind {
			case "file":
				writeFixture(t, destination, "occupied destination")
			case "symlink":
				writeFixture(t, target, "occupied destination")
				if err := os.Symlink(target, destination); err != nil {
					if runtime.GOOS == "windows" {
						t.Skipf("creating symlinks requires Windows privileges: %v", err)
					}
					t.Fatal(err)
				}
			case "directory":
				mustMkdir(t, destination)
				writeFixture(t, filepath.Join(destination, "child.nsp"), "occupied destination")
			case "late file":
				create := operations.createExclusive
				operations.createExclusive = func(path string) (organizationTemporaryFile, error) {
					writeFixture(t, path, "occupied destination")
					return create(path)
				}
			}
			err := moveFileWithOperations(move, operations)
			// Windows can report access denied when CREATE_NEW encounters a
			// directory. It must still fail and leave every file untouched.
			collision := errors.Is(err, os.ErrExist) || (runtime.GOOS == "windows" && kind == "directory" && errors.Is(err, os.ErrPermission))
			if !collision || !strings.Contains(err.Error(), "O_CREATE|O_EXCL") || !strings.Contains(err.Error(), destination) {
				t.Fatalf("move error = %v, want exclusive creation collision with destination path", err)
			}
			assertUnsupportedTestContents(t, source, "synthetic source")
			if kind == "directory" {
				assertUnsupportedTestContents(t, filepath.Join(destination, "child.nsp"), "occupied destination")
			} else {
				assertUnsupportedTestContents(t, destination, "occupied destination")
			}
			if kind == "symlink" {
				got, err := os.Readlink(destination)
				if err != nil || got != target {
					t.Fatalf("destination symlink = %q, error = %v, want %q", got, err, target)
				}
				assertUnsupportedTestContents(t, target, "occupied destination")
			}
		})
	}
}

func TestUnsupportedRenameFailuresRetainSourceAndReportDestination(t *testing.T) {
	for _, failure := range []string{"source read", "destination write", "destination chmod", "destination sync", "destination close", "destination stat", "destination chtimes"} {
		t.Run(failure, func(t *testing.T) {
			root := t.TempDir()
			source := filepath.Join(root, "source.nsp")
			destination := filepath.Join(root, "renamed.nsp")
			writeFixture(t, source, "synthetic retained source")
			move := organizationMoveForTest(t, source, destination)
			operations := unsupportedRenameTestOperations()
			injected := errors.New("injected " + failure + " failure")
			if failure == "source read" {
				open := operations.openSource
				operations.openSource = func(path string) (organizationSourceFile, error) {
					file, err := open(path)
					if err != nil {
						return nil, err
					}
					return testSourceFile{File: file.(*os.File), readErr: injected}, nil
				}
			} else if failure == "destination chtimes" {
				operations.chtimesOpen = func(organizationTemporaryFile, time.Time, time.Time) error { return injected }
			} else {
				create := operations.createExclusive
				operations.createExclusive = func(path string) (organizationTemporaryFile, error) {
					file, err := create(path)
					if err != nil {
						return nil, err
					}
					wrapped := testTemporaryFile{file: file.(*os.File)}
					switch failure {
					case "destination write":
						wrapped.writeErr = injected
					case "destination chmod":
						wrapped.chmodErr = injected
					case "destination sync":
						wrapped.syncErr = injected
					case "destination close":
						wrapped.closeErr = injected
					case "destination stat":
						wrapped.statErr = injected
					}
					return wrapped, nil
				}
			}
			err := moveFileWithOperations(move, operations)
			if !errors.Is(err, injected) {
				t.Fatalf("move error = %v, want injected failure %v", err, injected)
			}
			// The handle stat identifies the created file; without it the
			// destination cannot be proven to be ours and must be retained.
			retained := failure == "destination stat"
			details := []string{destination, "was removed", "source was not removed"}
			if retained {
				details = []string{destination, "may be incomplete", "was retained", "source was not removed"}
			}
			for _, detail := range details {
				if !strings.Contains(err.Error(), detail) {
					t.Errorf("move error = %v, want detail %q", err, detail)
				}
			}
			assertUnsupportedTestContents(t, source, "synthetic retained source")
			if retained {
				assertExists(t, destination)
			} else {
				assertNotExists(t, destination)
			}
		})
	}
}

type unsupportedCopyHookFile struct {
	organizationTemporaryFile
	beforeWrite func() error
	afterSync   func() error
}

func (f unsupportedCopyHookFile) Write(data []byte) (int, error) {
	if f.beforeWrite != nil {
		if err := f.beforeWrite(); err != nil {
			return 0, err
		}
	}
	return f.organizationTemporaryFile.Write(data)
}

func (f unsupportedCopyHookFile) Sync() error {
	if err := f.organizationTemporaryFile.Sync(); err != nil {
		return err
	}
	if f.afterSync != nil {
		return f.afterSync()
	}
	return nil
}

func TestUnsupportedRenameRetainsSourceChangedDuringCopy(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.nsp")
	destination := filepath.Join(root, "renamed.nsp")
	writeFixture(t, source, "synthetic source changed during copy")
	move := organizationMoveForTest(t, source, destination)
	operations := unsupportedRenameTestOperations()
	create := operations.createExclusive
	operations.createExclusive = func(path string) (organizationTemporaryFile, error) {
		file, err := create(path)
		if err != nil {
			return nil, err
		}
		return unsupportedCopyHookFile{organizationTemporaryFile: file, beforeWrite: func() error {
			changed := move.SourceStamp.ModTime().Add(time.Hour)
			return os.Chtimes(source, changed, changed)
		}}, nil
	}
	err := moveFileWithOperations(move, operations)
	if err == nil || !strings.Contains(err.Error(), "changed after organization preflight") {
		t.Fatalf("move error = %v, want source-change error", err)
	}
	assertUnsupportedTestContents(t, source, "synthetic source changed during copy")
	assertNotExists(t, destination)
}

func TestUnsupportedRenameDoesNotClobberReplacementDestination(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("replacing an open destination requires Unix file-handle semantics")
	}
	root := t.TempDir()
	source := filepath.Join(root, "source.nsp")
	destination := filepath.Join(root, "renamed.nsp")
	displaced := filepath.Join(root, "displaced.nsp")
	writeFixture(t, source, "synthetic source")
	move := organizationMoveForTest(t, source, destination)
	operations := unsupportedRenameTestOperations()
	create := operations.createExclusive
	wantTime := time.Date(2019, 4, 5, 6, 7, 8, 0, time.UTC)
	operations.createExclusive = func(path string) (organizationTemporaryFile, error) {
		file, err := create(path)
		if err != nil {
			return nil, err
		}
		if err := os.Rename(path, displaced); err != nil {
			return nil, errors.Join(err, file.Close())
		}
		writeFixture(t, path, "unrelated replacement")
		if err := os.Chmod(path, 0640); err != nil {
			return nil, errors.Join(err, file.Close())
		}
		if err := os.Chtimes(path, wantTime, wantTime); err != nil {
			return nil, errors.Join(err, file.Close())
		}
		return file, nil
	}
	err := moveFileWithOperations(move, operations)
	if err == nil || !strings.Contains(err.Error(), "changed during copy") {
		t.Fatalf("move error = %v, want destination-change error", err)
	}
	assertUnsupportedTestContents(t, source, "synthetic source")
	assertUnsupportedTestContents(t, destination, "unrelated replacement")
	info, err := os.Stat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0640 || !info.ModTime().Equal(wantTime) {
		t.Fatalf("replacement metadata changed: mode = %v, time = %v", info.Mode().Perm(), info.ModTime())
	}
	assertUnsupportedTestContents(t, displaced, "synthetic source")
}

func TestUnsupportedPublishCollisionPreservesOriginalAndRemovesTemporaryCopy(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.nsp")
	destination := filepath.Join(root, "renamed.nsp")
	writeFixture(t, source, "synthetic cross-device source")
	operations := crossDeviceTestOperations(source)
	rename := operations.renameNoReplace
	operations.renameNoReplace = func(from, to string) error {
		if from == source {
			return rename(from, to)
		}
		return errors.ErrUnsupported
	}
	create := operations.createExclusive
	operations.createExclusive = func(path string) (organizationTemporaryFile, error) {
		writeFixture(t, path, "late destination")
		return create(path)
	}
	err := moveFileWithOperations(organizationMoveForTest(t, source, destination), operations)
	if !errors.Is(err, os.ErrExist) || !strings.Contains(err.Error(), source) || !strings.Contains(err.Error(), destination) {
		t.Fatalf("move error = %v, want collision with original source and destination", err)
	}
	assertUnsupportedTestContents(t, source, "synthetic cross-device source")
	assertUnsupportedTestContents(t, destination, "late destination")
	temporary, err := filepath.Glob(filepath.Join(root, ".slm-move-*"))
	if err != nil || len(temporary) != 0 {
		t.Fatalf("temporary copies = %v, error = %v; want none", temporary, err)
	}
}

func TestUnsupportedRenamePreservesTimestampAfterFlush(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.nsp")
	destination := filepath.Join(root, "renamed.nsp")
	writeFixture(t, source, "synthetic timestamp content")
	wantTime := time.Date(2020, 2, 3, 4, 5, 6, 0, time.UTC)
	if err := os.Chtimes(source, wantTime, wantTime); err != nil {
		t.Fatal(err)
	}
	operations := unsupportedRenameTestOperations()
	create := operations.createExclusive
	operations.createExclusive = func(path string) (organizationTemporaryFile, error) {
		file, err := create(path)
		if err != nil {
			return nil, err
		}
		return unsupportedCopyHookFile{organizationTemporaryFile: file, afterSync: func() error {
			// Model an SMB server updating the timestamp as it flushes writes.
			serverTime := wantTime.Add(24 * time.Hour)
			return os.Chtimes(path, serverTime, serverTime)
		}}, nil
	}
	if err := moveFileWithOperations(organizationMoveForTest(t, source, destination), operations); err != nil {
		t.Fatal(err)
	}
	assertNotExists(t, source)
	info, err := os.Stat(destination)
	if err != nil || !info.ModTime().Equal(wantTime) {
		t.Fatalf("destination timestamp = %v, error = %v, want %v", info, err, wantTime)
	}
}

func TestRenameGeneralErrorsDoNotStartCopyFallback(t *testing.T) {
	for _, injected := range []error{os.ErrPermission, errors.New("injected general rename failure")} {
		t.Run(injected.Error(), func(t *testing.T) {
			root := t.TempDir()
			source := filepath.Join(root, "source.nsp")
			destination := filepath.Join(root, "renamed.nsp")
			writeFixture(t, source, "synthetic untouched source")
			operations := defaultOrganizationFileOperations()
			operations.renameNoReplace = func(string, string) error { return injected }
			operations.openSource = func(string) (organizationSourceFile, error) {
				t.Fatal("copy fallback opened source after a general rename error")
				return nil, injected
			}
			err := moveFileWithOperations(organizationMoveForTest(t, source, destination), operations)
			if !errors.Is(err, injected) {
				t.Fatalf("move error = %v, want %v", err, injected)
			}
			assertUnsupportedTestContents(t, source, "synthetic untouched source")
			assertNotExists(t, destination)
		})
	}
}

func assertUnsupportedTestContents(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != want {
		t.Fatalf("contents of %q = %q, error = %v, want %q", path, got, err, want)
	}
}

func TestUnsupportedRenameRetainsIncompleteDestinationReplacedByAnotherClient(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("replacing an open destination requires Unix file-handle semantics")
	}
	root := t.TempDir()
	source := filepath.Join(root, "source.nsp")
	destination := filepath.Join(root, "renamed.nsp")
	writeFixture(t, source, "synthetic source")
	operations := unsupportedRenameTestOperations()
	injected := errors.New("injected write failure after replacement")
	create := operations.createExclusive
	operations.createExclusive = func(path string) (organizationTemporaryFile, error) {
		file, err := create(path)
		if err != nil {
			return nil, err
		}
		return unsupportedCopyHookFile{organizationTemporaryFile: file, beforeWrite: func() error {
			if err := os.Remove(path); err != nil {
				return err
			}
			writeFixture(t, path, "another client's file")
			return injected
		}}, nil
	}
	err := moveFileWithOperations(organizationMoveForTest(t, source, destination), operations)
	if !errors.Is(err, injected) || !strings.Contains(err.Error(), "was retained") {
		t.Fatalf("move error = %v, want retained destination", err)
	}
	assertUnsupportedTestContents(t, source, "synthetic source")
	assertUnsupportedTestContents(t, destination, "another client's file")
}

// identityChangingOperations models an SMB client whose path lookup reports a
// different file ID from the open handle, as happens without stable server IDs.
func identityChangingOperations(destination string, operations organizationFileOperations) organizationFileOperations {
	lstat := operations.lstat
	operations.lstat = func(path string) (os.FileInfo, error) {
		info, err := lstat(path)
		if err != nil || filepath.Clean(path) != filepath.Clean(destination) {
			return info, err
		}
		return unstableIdentityInfo{info}, nil
	}
	return operations
}

type unstableIdentityInfo struct{ os.FileInfo }

func (unstableIdentityInfo) Sys() any { return nil }

func TestUnsupportedRenameRetainsSourceWhenReplacementMetadataMatches(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("replacing an open destination requires Unix file-handle semantics")
	}
	root := t.TempDir()
	source := filepath.Join(root, "source.nsp")
	destination := filepath.Join(root, "renamed.nsp")
	writeFixture(t, source, "original content")
	stamp := time.Date(2020, 2, 3, 4, 5, 6, 0, time.UTC)
	if err := os.Chtimes(source, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	move := organizationMoveForTest(t, source, destination)
	operations := unsupportedRenameTestOperations()
	create := operations.createExclusive
	operations.createExclusive = func(path string) (organizationTemporaryFile, error) {
		file, err := create(path)
		if err != nil {
			return nil, err
		}
		return unsupportedCopyHookFile{organizationTemporaryFile: file, afterSync: func() error {
			// The copy's handle remains open, but its bytes disappear on close
			// after another client unlinks it and supplies a different file.
			if err := os.Remove(path); err != nil {
				return err
			}
			if err := os.WriteFile(path, []byte("replaced content"), 0600); err != nil {
				return err
			}
			return os.Chtimes(path, stamp, stamp)
		}}, nil
	}
	err := moveFileWithOperations(move, operations)
	if err == nil {
		t.Error("move accepted a replacement with matching size and modification time")
	} else {
		for _, detail := range []string{source, destination, "identity", "was retained"} {
			if !strings.Contains(err.Error(), detail) {
				t.Errorf("move error = %v, want detail %q", err, detail)
			}
		}
	}
	assertUnsupportedTestContents(t, source, "original content")
	assertUnsupportedTestContents(t, destination, "replaced content")
	info, err := os.Stat(destination)
	if err != nil || !info.ModTime().Equal(stamp) {
		t.Fatalf("replacement modification time changed: info=%v error=%v", info, err)
	}
}

func TestUnsupportedRenameRetainsSourceWithUnstableIdentity(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.nsp")
	destination := filepath.Join(root, "renamed.nsp")
	writeFixture(t, source, "synthetic unstable identity content")
	operations := identityChangingOperations(destination, unsupportedRenameTestOperations())
	err := moveFileWithOperations(organizationMoveForTest(t, source, destination), operations)
	if err == nil || !strings.Contains(err.Error(), "was retained") {
		t.Errorf("move error = %v, want unverifiable destination retained", err)
	}
	assertUnsupportedTestContents(t, source, "synthetic unstable identity content")
	assertUnsupportedTestContents(t, destination, "synthetic unstable identity content")
}

func TestUnsupportedRenameAcceptsServerTimestampChangeOnClose(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.nsp")
	destination := filepath.Join(root, "renamed.nsp")
	writeFixture(t, source, "synthetic close timestamp content")
	operations := unsupportedRenameTestOperations()
	lstat := operations.lstat
	operations.lstat = func(path string) (os.FileInfo, error) {
		if filepath.Clean(path) == filepath.Clean(destination) {
			// Model a server that bumps the modification time on close.
			bumped := time.Now().Add(time.Hour)
			if err := os.Chtimes(path, bumped, bumped); err != nil {
				return nil, err
			}
		}
		return lstat(path)
	}
	if err := moveFileWithOperations(organizationMoveForTest(t, source, destination), operations); err != nil {
		t.Fatal(err)
	}
	assertNotExists(t, source)
	assertUnsupportedTestContents(t, destination, "synthetic close timestamp content")
}

func TestUnsupportedRenameRejectsUnstableIdentityWithDifferentTime(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.nsp")
	destination := filepath.Join(root, "renamed.nsp")
	writeFixture(t, source, "synthetic source")
	operations := identityChangingOperations(destination, unsupportedRenameTestOperations())
	chtimes := operations.chtimesOpen
	operations.chtimesOpen = func(file organizationTemporaryFile, atime, mtime time.Time) error {
		shifted := mtime.Add(time.Hour)
		return chtimes(file, shifted, shifted)
	}
	err := moveFileWithOperations(organizationMoveForTest(t, source, destination), operations)
	if err == nil || !strings.Contains(err.Error(), "changed during copy") || !strings.Contains(err.Error(), "was retained") {
		t.Fatalf("move error = %v, want unverifiable destination retained", err)
	}
	assertUnsupportedTestContents(t, source, "synthetic source")
	assertExists(t, destination)
}

func TestCrossDeviceMoveCopiesDirectlyWhenDestinationLacksExclusiveRename(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.nsp")
	library := filepath.Join(root, "library")
	mustMkdir(t, library)
	destination := filepath.Join(library, "renamed.nsp")
	writeFixture(t, source, "synthetic direct copy content")
	operations := crossDeviceTestOperations(source)
	rename := operations.renameNoReplace
	operations.renameNoReplace = func(from, to string) error {
		if from == source {
			return rename(from, to)
		}
		return errors.ErrUnsupported
	}
	operations.createTemp = func(directory, pattern string) (organizationTemporaryFile, error) {
		if strings.HasPrefix(pattern, ".slm-move-") {
			t.Fatal("temporary copy created although exclusive rename is unsupported")
		}
		return os.CreateTemp(directory, pattern)
	}
	if err := moveFileWithOperations(organizationMoveForTest(t, source, destination), operations); err != nil {
		t.Fatal(err)
	}
	assertNotExists(t, source)
	assertUnsupportedTestContents(t, destination, "synthetic direct copy content")
	entries, err := os.ReadDir(library)
	if err != nil || len(entries) != 1 {
		t.Fatalf("library after move = %v, error = %v; want only the destination", entries, err)
	}
}

func TestCrossDeviceMoveRecopiesSourceWhenPublicationIsUnsupported(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.nsp")
	library := filepath.Join(root, "library")
	mustMkdir(t, library)
	destination := filepath.Join(library, "renamed.nsp")
	writeFixture(t, source, "synthetic recopy content")
	operations := crossDeviceTestOperations(source)
	rename := operations.renameNoReplace
	operations.renameNoReplace = func(from, to string) error {
		if strings.HasPrefix(filepath.Base(from), ".slm-move-") {
			return errors.ErrUnsupported
		}
		return rename(from, to)
	}
	opened := []string{}
	open := operations.openSource
	operations.openSource = func(path string) (organizationSourceFile, error) {
		opened = append(opened, path)
		return open(path)
	}
	if err := moveFileWithOperations(organizationMoveForTest(t, source, destination), operations); err != nil {
		t.Fatal(err)
	}
	if len(opened) != 2 || opened[0] != source || opened[1] != source {
		t.Fatalf("copy sources = %q, want the original source twice", opened)
	}
	assertNotExists(t, source)
	assertUnsupportedTestContents(t, destination, "synthetic recopy content")
	entries, err := os.ReadDir(library)
	if err != nil || len(entries) != 1 {
		t.Fatalf("library after move = %v, error = %v; want only the destination", entries, err)
	}
}

func TestExclusiveRenameProbeFailureFallsBackToTemporaryCopy(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.nsp")
	library := filepath.Join(root, "library")
	mustMkdir(t, library)
	destination := filepath.Join(library, "renamed.nsp")
	writeFixture(t, source, "synthetic probe failure content")
	operations := crossDeviceTestOperations(source)
	createTemp := operations.createTemp
	operations.createTemp = func(directory, pattern string) (organizationTemporaryFile, error) {
		if strings.HasPrefix(pattern, ".slm-probe-") {
			return nil, os.ErrPermission
		}
		return createTemp(directory, pattern)
	}
	core, logs := observer.New(zap.WarnLevel)
	defer zap.ReplaceGlobals(zap.New(core))()
	if err := moveFileWithOperations(organizationMoveForTest(t, source, destination), operations); err != nil {
		t.Fatal(err)
	}
	assertNotExists(t, source)
	assertUnsupportedTestContents(t, destination, "synthetic probe failure content")
	if logs.FilterMessage("Exclusive rename probe failed").Len() != 1 {
		t.Fatalf("probe failure was not logged: %v", logs.All())
	}
}

func TestExclusiveRenameProbeReportsCleanupFailure(t *testing.T) {
	directory := t.TempDir()
	removeErr := errors.New("injected probe removal failure")
	operations := defaultOrganizationFileOperations()
	operations.remove = func(string) error { return removeErr }
	supported, err := probeExclusiveRename(directory, operations)
	if !supported || !errors.Is(err, removeErr) {
		t.Fatalf("probe = %t, %v; want supported with cleanup error", supported, err)
	}
	operations.renameNoReplace = func(string, string) error { return os.ErrPermission }
	supported, err = probeExclusiveRename(directory, operations)
	if !supported || !errors.Is(err, os.ErrPermission) || !errors.Is(err, removeErr) {
		t.Fatalf("probe = %t, %v; want supported with rename and cleanup errors", supported, err)
	}
}
