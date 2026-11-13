//go:build linux

package process

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestRenameNoReplaceOnLinuxBypassesSMB(t *testing.T) {
	for _, test := range []struct {
		name string
		kind uint32
	}{
		{name: "CIFS", kind: unix.CIFS_SUPER_MAGIC},
		{name: "SMB2", kind: unix.SMB2_SUPER_MAGIC},
		{name: "SMB", kind: unix.SMB_SUPER_MAGIC},
	} {
		t.Run(test.name, func(t *testing.T) {
			from := filepath.Join("intake", "source.nsp")
			to := filepath.Join("library", "renamed.nsp")
			err := renameNoReplaceOnLinux(from, to, func(path string) (uint32, error) {
				if path != filepath.Dir(to) {
					t.Fatalf("inspected filesystem at %q, want destination parent", path)
				}
				return test.kind, nil
			}, func(string, string) error {
				t.Fatal("SMB must not call the unsafe native rename")
				return nil
			}, unusedLinuxProbeCreate(t))
			var linkErr *os.LinkError
			if !errors.Is(err, unix.EOPNOTSUPP) || !errors.As(err, &linkErr) {
				t.Fatalf("error = %v, want unsupported operation with paths", err)
			}
			if linkErr.Old != from || linkErr.New != to || !strings.Contains(linkErr.Op, "SMB/CIFS") {
				t.Fatalf("error lacks mount or move context: %#v", linkErr)
			}
		})
	}
}

func unusedLinuxProbeCreate(t *testing.T) func(string, string) (organizationTemporaryFile, error) {
	return func(string, string) (organizationTemporaryFile, error) {
		t.Fatal("rename probe must not run")
		return nil, errors.New("unexpected probe")
	}
}

func linuxProbeCreate(directory string, pattern string) (organizationTemporaryFile, error) {
	return os.CreateTemp(directory, pattern)
}

func TestRenameNoReplaceOnLinuxErrorContext(t *testing.T) {
	from, to := "source.nsp", filepath.Join("library", "destination.nsp")
	t.Run("filesystem inspection failure still renames", func(t *testing.T) {
		calls := 0
		err := renameNoReplaceOnLinux(from, to, func(string) (uint32, error) {
			return 0, unix.EACCES
		}, func(string, string) error {
			calls++
			return nil
		}, unusedLinuxProbeCreate(t))
		if err != nil || calls != 1 {
			t.Fatalf("error = %v, calls = %d; want native rename despite statfs failure", err, calls)
		}
	})
	t.Run("native rename failure", func(t *testing.T) {
		calls := 0
		err := renameNoReplaceOnLinux(from, to, func(string) (uint32, error) {
			return unix.EXT4_SUPER_MAGIC, nil
		}, func(old, new string) error {
			calls++
			if old != from || new != to {
				t.Fatalf("rename paths = %q, %q", old, new)
			}
			return unix.EEXIST
		}, unusedLinuxProbeCreate(t))
		var linkErr *os.LinkError
		if calls != 1 || !errors.Is(err, unix.EEXIST) || !errors.As(err, &linkErr) || linkErr.Old != from || linkErr.New != to || linkErr.Op != "renameat2(RENAME_NOREPLACE)" {
			t.Fatalf("error = %v, calls = %d; want native operation, both paths and errno", err, calls)
		}
	})
}

func TestRenameNoReplaceOnLinuxClassifiesEINVAL(t *testing.T) {
	for _, test := range []struct {
		name            string
		probeFails      bool
		wantUnsupported bool
	}{
		{name: "flag unsupported by filesystem", probeFails: true, wantUnsupported: true},
		{name: "path-specific EINVAL", probeFails: false, wantUnsupported: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			from, to := filepath.Join(directory, "source.nsp"), filepath.Join(directory, "bad name.nsp")
			err := renameNoReplaceOnLinux(from, to, func(string) (uint32, error) {
				return unix.EXT4_SUPER_MAGIC, nil
			}, func(old, new string) error {
				if old == from || test.probeFails {
					return unix.EINVAL
				}
				return os.Rename(old, new)
			}, linuxProbeCreate)
			if !errors.Is(err, unix.EINVAL) {
				t.Fatalf("error = %v, want original EINVAL retained", err)
			}
			if got := isUnsupportedRenameError(err); got != test.wantUnsupported {
				t.Fatalf("isUnsupportedRenameError(%v) = %t, want %t", err, got, test.wantUnsupported)
			}
			entries, readErr := os.ReadDir(directory)
			if readErr != nil || len(entries) != 0 {
				t.Fatalf("probe files left behind: %v, error = %v", entries, readErr)
			}
		})
	}
}

func TestRenameNoReplaceLinuxDistinctPaths(t *testing.T) {
	directory := t.TempDir()
	from := filepath.Join(directory, "source.nsp")
	to := filepath.Join(directory, "destination.nsp")
	if err := os.WriteFile(from, []byte("synthetic source"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, []byte("existing destination"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := renameNoReplace(from, to); !errors.Is(err, os.ErrExist) {
		t.Fatalf("rename collision = %v, want existing destination error", err)
	}
	for path, want := range map[string]string{from: "synthetic source", to: "existing destination"} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("after collision %q = %q, %v; want %q", path, got, err, want)
		}
	}
	if err := os.Remove(to); err != nil {
		t.Fatal(err)
	}
	if err := renameNoReplace(from, to); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(from); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source after successful rename = %v, want absent", err)
	}
	got, err := os.ReadFile(to)
	if err != nil || string(got) != "synthetic source" {
		t.Fatalf("destination = %q, %v", got, err)
	}
}
