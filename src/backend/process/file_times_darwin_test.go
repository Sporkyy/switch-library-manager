//go:build darwin

package process

import (
	"errors"
	"os"
	"testing"
)

func TestDarwinOpenFileIsSMBOnLocalFile(t *testing.T) {
	_, file, _, _ := newExclusiveCopyForTest(t)
	smb, err := darwinOpenFileIsSMB(file)
	if err != nil || smb {
		t.Fatalf("local temp file: smb = %t, error = %v; want a non-SMB filesystem", smb, err)
	}
}

func TestDarwinOpenFileIsSMBReportsClosedFile(t *testing.T) {
	_, file, _, _ := newExclusiveCopyForTest(t)
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := darwinOpenFileIsSMB(file); err == nil {
		t.Fatal("fstatfs on a closed file succeeded")
	}
}

func TestDarwinCloseExclusiveCopyOnLocalFileClosesAndKeepsTime(t *testing.T) {
	path, file, created, stamp := newExclusiveCopyForTest(t)
	if err := closeExclusiveOrganizationCopy(file, created, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("late")); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("write after close error = %v, want closed file", err)
	}
	assertModTimeForTest(t, path, stamp)
}
