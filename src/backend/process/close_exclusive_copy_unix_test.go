//go:build darwin || linux

package process

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// serverStampingFile models an SMB handle whose server-side close stamps the
// close time over any modification time set while it was open.
type serverStampingFile struct {
	*os.File
	closed   *bool
	closeErr error
}

func (f serverStampingFile) Close() error {
	if err := f.File.Close(); err != nil {
		return err
	}
	*f.closed = true
	if f.closeErr != nil {
		return f.closeErr
	}
	now := time.Now()
	return os.Chtimes(f.Name(), now, now)
}

func newExclusiveCopyForTest(t *testing.T) (string, *os.File, os.FileInfo, time.Time) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "copy.nsp")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	if _, err := file.Write([]byte("synthetic copied content")); err != nil {
		t.Fatal(err)
	}
	// Whole microseconds: macOS sets handle times with futimes.
	stamp := time.Date(2020, 2, 3, 4, 5, 6, 7000, time.UTC)
	if err := setOrganizationOpenFileTimes(file, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	created, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	return path, file, created, stamp
}

func smbForTest(organizationTemporaryFile) (bool, error) { return true, nil }

func assertModTimeForTest(t *testing.T, path string, want time.Time) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(want) {
		t.Fatalf("modification time = %v, want %v", info.ModTime().UTC(), want)
	}
}

func TestCloseExclusiveCopyRestoresTimeStampedByServerClose(t *testing.T) {
	path, file, created, stamp := newExclusiveCopyForTest(t)
	closed := false
	ranBeforeClose := false
	err := closeExclusiveCopyRestoringTimes(serverStampingFile{File: file, closed: &closed}, created, stamp, stamp,
		smbForTest,
		func(organizationTemporaryFile) { ranBeforeClose = !closed },
		openOrganizationCopyReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	if !ranBeforeClose {
		t.Fatal("beforeClose did not run while the SMB destination was still open")
	}
	assertModTimeForTest(t, path, stamp)
}

func TestCloseExclusiveCopyRestoresWithoutBeforeClose(t *testing.T) {
	path, file, created, stamp := newExclusiveCopyForTest(t)
	closed := false
	if err := closeExclusiveCopyRestoringTimes(serverStampingFile{File: file, closed: &closed}, created, stamp, stamp,
		smbForTest, nil, openOrganizationCopyReadOnly); err != nil {
		t.Fatal(err)
	}
	assertModTimeForTest(t, path, stamp)
}

func TestCloseExclusiveCopyOnlyClosesOtherFileSystems(t *testing.T) {
	_, file, created, stamp := newExclusiveCopyForTest(t)
	err := closeExclusiveCopyRestoringTimes(file, created, stamp, stamp,
		func(organizationTemporaryFile) (bool, error) { return false, nil },
		func(organizationTemporaryFile) { t.Fatal("non-SMB destination must not run beforeClose") },
		func(string) (organizationTemporaryFile, error) {
			t.Fatal("non-SMB destination must not be reopened")
			return nil, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("late")); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("write after close error = %v, want closed file", err)
	}
}

func TestCloseExclusiveCopyRefusesReplacedDestination(t *testing.T) {
	path, file, created, stamp := newExclusiveCopyForTest(t)
	other := filepath.Join(filepath.Dir(path), "other.nsp")
	writeFixture(t, other, "another client's file")
	otherTime := time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(other, otherTime, otherTime); err != nil {
		t.Fatal(err)
	}
	err := closeExclusiveCopyRestoringTimes(file, created, stamp, stamp, smbForTest, nil,
		func(string) (organizationTemporaryFile, error) { return os.Open(other) })
	if err == nil || !strings.Contains(err.Error(), "no longer identifies") || !strings.Contains(err.Error(), path) {
		t.Fatalf("error = %v, want replaced destination reported with its path", err)
	}
	assertModTimeForTest(t, other, otherTime)
}

func TestCloseExclusiveCopyWarnsWhenFileSystemIsUnknown(t *testing.T) {
	path, file, created, stamp := newExclusiveCopyForTest(t)
	core, logs := observer.New(zap.WarnLevel)
	defer zap.ReplaceGlobals(zap.New(core))()
	err := closeExclusiveCopyRestoringTimes(file, created, stamp, stamp,
		func(organizationTemporaryFile) (bool, error) { return false, errors.New("injected statfs failure") },
		func(organizationTemporaryFile) { t.Fatal("unknown filesystem must not run beforeClose") },
		func(string) (organizationTemporaryFile, error) {
			t.Fatal("unknown filesystem must not be reopened")
			return nil, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if entries := logs.FilterField(zap.String("destination", path)).All(); len(entries) != 1 {
		t.Fatalf("warnings = %v, want one filesystem warning naming the destination", logs.All())
	}
	if _, err := file.Write([]byte("late")); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("write after close error = %v, want closed file", err)
	}
}

func TestCloseExclusiveCopyReportsFailures(t *testing.T) {
	injected := errors.New("injected failure")
	for _, failure := range []string{"close", "reopen", "stat", "reopened close"} {
		t.Run(failure, func(t *testing.T) {
			path, file, created, stamp := newExclusiveCopyForTest(t)
			closed := false
			destination := serverStampingFile{File: file, closed: &closed}
			if failure == "close" {
				destination.closeErr = injected
			}
			reopen := func(path string) (organizationTemporaryFile, error) {
				reopened, err := os.Open(path)
				if err != nil {
					return nil, err
				}
				switch failure {
				case "reopen":
					_ = reopened.Close()
					return nil, injected
				case "stat":
					return testTemporaryFile{file: reopened, statErr: injected}, nil
				case "reopened close":
					return testTemporaryFile{file: reopened, closeErr: injected}, nil
				}
				return reopened, nil
			}
			err := closeExclusiveCopyRestoringTimes(destination, created, stamp, stamp, smbForTest, nil, reopen)
			// copyOrganizationFile adds the path to plain close errors; failures
			// after the close must name the destination themselves.
			if !errors.Is(err, injected) || (failure != "close" && !strings.Contains(err.Error(), path)) {
				t.Fatalf("error = %v, want injected %s failure with destination path", err, failure)
			}
			if !closed {
				t.Fatal("destination handle was left open")
			}
		})
	}
}
