//go:build linux

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
	"golang.org/x/sys/unix"
)

// serverStampingFile models a Linux CIFS handle whose server-side close
// stamps the close time over any modification time set while it was open.
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
	stamp := time.Date(2020, 2, 3, 4, 5, 6, 7, time.UTC)
	if err := setOrganizationOpenFileTimes(file, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	created, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	return path, file, created, stamp
}

func smbFileSystemForTest(organizationTemporaryFile) (uint32, error) {
	return unix.SMB2_SUPER_MAGIC, nil
}

func reopenForTest(path string) (organizationTemporaryFile, error) {
	return os.Open(path)
}

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

func TestCloseExclusiveCopyOnLinuxRestoresTimeStampedByServerClose(t *testing.T) {
	path, file, created, stamp := newExclusiveCopyForTest(t)
	closed := false
	lockedWhileOpen := false
	err := closeExclusiveCopyOnLinux(serverStampingFile{File: file, closed: &closed}, created, stamp, stamp,
		smbFileSystemForTest,
		func(organizationTemporaryFile) error {
			lockedWhileOpen = !closed
			return nil
		},
		reopenForTest)
	if err != nil {
		t.Fatal(err)
	}
	if !lockedWhileOpen {
		t.Fatal("SMB destination was not locked before close; the close would be deferred")
	}
	assertModTimeForTest(t, path, stamp)
}

func TestCloseExclusiveCopyOnLinuxOnlyClosesOtherFileSystems(t *testing.T) {
	_, file, created, stamp := newExclusiveCopyForTest(t)
	err := closeExclusiveCopyOnLinux(file, created, stamp, stamp,
		func(organizationTemporaryFile) (uint32, error) { return unix.EXT4_SUPER_MAGIC, nil },
		func(organizationTemporaryFile) error {
			t.Fatal("non-SMB destination must not be locked")
			return nil
		},
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

func TestCloseExclusiveCopyOnLinuxRefusesReplacedDestination(t *testing.T) {
	path, file, created, stamp := newExclusiveCopyForTest(t)
	other := filepath.Join(filepath.Dir(path), "other.nsp")
	writeFixture(t, other, "another client's file")
	otherTime := time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(other, otherTime, otherTime); err != nil {
		t.Fatal(err)
	}
	err := closeExclusiveCopyOnLinux(file, created, stamp, stamp, smbFileSystemForTest,
		func(organizationTemporaryFile) error { return nil },
		func(string) (organizationTemporaryFile, error) { return os.Open(other) })
	if err == nil || !strings.Contains(err.Error(), "no longer identifies") || !strings.Contains(err.Error(), path) {
		t.Fatalf("error = %v, want replaced destination reported with its path", err)
	}
	assertModTimeForTest(t, other, otherTime)
}

func TestCloseExclusiveCopyOnLinuxWarnsWhenLockFails(t *testing.T) {
	path, file, created, stamp := newExclusiveCopyForTest(t)
	core, logs := observer.New(zap.WarnLevel)
	defer zap.ReplaceGlobals(zap.New(core))()
	err := closeExclusiveCopyOnLinux(file, created, stamp, stamp, smbFileSystemForTest,
		func(organizationTemporaryFile) error { return unix.ENOLCK },
		reopenForTest)
	if err != nil {
		t.Fatal(err)
	}
	if entries := logs.FilterField(zap.String("destination", path)).All(); len(entries) != 1 || !strings.Contains(entries[0].Message, "lock") {
		t.Fatalf("warnings = %v, want one lock warning naming the destination", logs.All())
	}
	assertModTimeForTest(t, path, stamp)
}

func TestCloseExclusiveCopyOnLinuxWarnsWhenFileSystemIsUnknown(t *testing.T) {
	path, file, created, stamp := newExclusiveCopyForTest(t)
	core, logs := observer.New(zap.WarnLevel)
	defer zap.ReplaceGlobals(zap.New(core))()
	err := closeExclusiveCopyOnLinux(file, created, stamp, stamp,
		func(organizationTemporaryFile) (uint32, error) { return 0, unix.EIO },
		func(organizationTemporaryFile) error {
			t.Fatal("unknown filesystem must not be locked")
			return nil
		},
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
}

func TestCloseExclusiveCopyOnLinuxReportsFailures(t *testing.T) {
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
			err := closeExclusiveCopyOnLinux(destination, created, stamp, stamp, smbFileSystemForTest,
				func(organizationTemporaryFile) error { return nil }, reopen)
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

func TestLinuxSynchronousCloseLockOnLocalFile(t *testing.T) {
	_, file, _, _ := newExclusiveCopyForTest(t)
	if err := lockOrganizationCopyForSynchronousClose(file); err != nil {
		t.Fatalf("lock exclusively created copy: %v", err)
	}
	kind, err := linuxOpenFileSystemType(file)
	if err != nil || kind == 0 {
		t.Fatalf("filesystem type = %#x, error = %v", kind, err)
	}
}
