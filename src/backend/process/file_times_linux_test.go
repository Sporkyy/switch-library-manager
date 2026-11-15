//go:build linux

package process

import (
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sys/unix"
)

// The shared close-and-restore behavior is covered in
// close_exclusive_copy_unix_test.go; these tests cover the Linux wrapper's
// filesystem classification and synchronous-close lock.

func smbFileSystemForTest(organizationTemporaryFile) (uint32, error) {
	return unix.SMB2_SUPER_MAGIC, nil
}

func TestCloseExclusiveCopyOnLinuxLocksSMBBeforeCloseAndRestores(t *testing.T) {
	path, file, created, stamp := newExclusiveCopyForTest(t)
	closed := false
	lockedWhileOpen := false
	err := closeExclusiveCopyOnLinux(serverStampingFile{File: file, closed: &closed}, created, stamp, stamp,
		smbFileSystemForTest,
		func(organizationTemporaryFile) error {
			lockedWhileOpen = !closed
			return nil
		},
		openOrganizationCopyReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	if !lockedWhileOpen {
		t.Fatal("SMB destination was not locked before close; the close would be deferred")
	}
	assertModTimeForTest(t, path, stamp)
}

func TestCloseExclusiveCopyOnLinuxRecognizesEverySMBMagic(t *testing.T) {
	for _, kind := range []uint32{unix.CIFS_SUPER_MAGIC, unix.SMB2_SUPER_MAGIC, unix.SMB_SUPER_MAGIC} {
		_, file, created, stamp := newExclusiveCopyForTest(t)
		locked := false
		err := closeExclusiveCopyOnLinux(file, created, stamp, stamp,
			func(organizationTemporaryFile) (uint32, error) { return kind, nil },
			func(organizationTemporaryFile) error { locked = true; return nil },
			openOrganizationCopyReadOnly)
		if err != nil || !locked {
			t.Fatalf("filesystem %#x: locked = %t, error = %v; want SMB handling", kind, locked, err)
		}
	}
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
}

func TestCloseExclusiveCopyOnLinuxWarnsWhenLockFails(t *testing.T) {
	path, file, created, stamp := newExclusiveCopyForTest(t)
	core, logs := observer.New(zap.WarnLevel)
	defer zap.ReplaceGlobals(zap.New(core))()
	err := closeExclusiveCopyOnLinux(file, created, stamp, stamp, smbFileSystemForTest,
		func(organizationTemporaryFile) error { return unix.ENOLCK },
		openOrganizationCopyReadOnly)
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

func TestLinuxFileSystemTypeReportsClosedFile(t *testing.T) {
	_, file, _, _ := newExclusiveCopyForTest(t)
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := linuxOpenFileSystemType(file); err == nil {
		t.Fatal("fstatfs on a closed file succeeded")
	}
}
