package process

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestFileIdentityStabilityOnMount checks the property the exclusive-copy
// fallback relies on before it removes a source: after the exclusively created
// destination is closed, its path must still identify the file that was open.
// Shares whose file IDs are unstable fail this, and organizing onto them keeps
// both the copy and the source.
//
// Opt in with SLM_FILE_ID_TEST_ROOTS, a list of existing directories separated
// by the OS path-list separator (":" on macOS/Linux), for example:
//
//	SLM_FILE_ID_TEST_ROOTS=/Volumes/share1:/Volumes/share2 go test ./backend/process -run TestFileIdentityStabilityOnMount -v
//
// SLM_ORGANIZE_TEST_ROOT is used when the list is unset. Each root gets its own
// .slm-fileid-test-* scratch directory; nothing else is read, moved or removed.
func TestFileIdentityStabilityOnMount(t *testing.T) {
	roots := filepath.SplitList(os.Getenv("SLM_FILE_ID_TEST_ROOTS"))
	if len(roots) == 0 {
		if root := os.Getenv("SLM_ORGANIZE_TEST_ROOT"); root != "" {
			roots = []string{root}
		}
	}
	if len(roots) == 0 {
		t.Skip("set SLM_FILE_ID_TEST_ROOTS or SLM_ORGANIZE_TEST_ROOT to probe a mount")
	}
	for _, root := range roots {
		t.Run(root, func(t *testing.T) {
			probeFileIdentity(t, root)
		})
	}
}

func probeFileIdentity(t *testing.T, root string) {
	const rounds = 10
	dir, err := os.MkdirTemp(root, ".slm-fileid-test-")
	if err != nil {
		t.Fatalf("create scratch directory in %q: %v", root, err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("remove scratch directory %q: %v", dir, err)
		}
	})
	operations := defaultOrganizationFileOperations()
	payload := bytes.Repeat([]byte("synthetic file identity probe\n"), 1<<17) // ~3.8 MiB
	stamp := time.Date(2020, 2, 3, 4, 5, 6, 0, time.UTC)
	var closedMismatches, reopenMismatches, laterMismatches, renameMismatches int
	paths := make([]string, 0, rounds)
	created := make([]os.FileInfo, 0, rounds)

	for round := 0; round < rounds; round++ {
		path := filepath.Join(dir, "probe-"+string(rune('a'+round))+".bin")
		// Same sequence as copyOrganizationFile: exclusive create, write,
		// flush, set times on the handle, stat the handle, close, lstat.
		file, err := operations.createExclusive(path)
		if err != nil {
			t.Fatalf("create %q exclusively: %v", path, err)
		}
		handleInfo, err := file.Stat()
		if err != nil {
			t.Fatalf("stat handle %q: %v", path, err)
		}
		if _, err := file.Write(payload); err != nil {
			t.Fatalf("write %q: %v", path, err)
		}
		if err := file.Sync(); err != nil {
			t.Fatalf("flush %q: %v", path, err)
		}
		if err := operations.chtimesOpen(file, stamp, stamp); err != nil {
			t.Fatalf("set times on handle %q: %v", path, err)
		}
		if err := file.Close(); err != nil {
			t.Fatalf("close %q: %v", path, err)
		}
		closedInfo, err := os.Lstat(path)
		if err != nil {
			t.Fatalf("lstat %q after close: %v", path, err)
		}
		reopened, err := os.Open(path)
		if err != nil {
			t.Fatalf("reopen %q: %v", path, err)
		}
		reopenedInfo, statErr := reopened.Stat()
		closeErr := reopened.Close()
		if statErr != nil || closeErr != nil {
			t.Fatalf("stat reopened %q: %v; close: %v", path, statErr, closeErr)
		}

		closedSame := os.SameFile(handleInfo, closedInfo)
		reopenSame := os.SameFile(closedInfo, reopenedInfo)
		if !closedSame {
			closedMismatches++
		}
		if !reopenSame {
			reopenMismatches++
		}
		t.Logf("round %2d: handle id %s, path id after close %s, reopened id %s; handle==path %t, path==reopened %t",
			round+1, fileIdentityForLog(handleInfo), fileIdentityForLog(closedInfo), fileIdentityForLog(reopenedInfo), closedSame, reopenSame)
		paths = append(paths, path)
		created = append(created, handleInfo)
	}

	// Identity that drifts after a delay or a rename points at client-side
	// IDs that are regenerated, for example Linux CIFS with noserverino.
	time.Sleep(3 * time.Second)
	for i, path := range paths {
		later, err := os.Lstat(path)
		if err != nil {
			t.Fatalf("lstat %q after delay: %v", path, err)
		}
		if !os.SameFile(created[i], later) {
			laterMismatches++
		}
		renamed := path + ".renamed"
		if err := os.Rename(path, renamed); err != nil {
			t.Fatalf("rename %q within scratch directory: %v", path, err)
		}
		afterRename, err := os.Lstat(renamed)
		if err != nil {
			t.Fatalf("lstat %q after rename: %v", renamed, err)
		}
		if !os.SameFile(created[i], afterRename) {
			renameMismatches++
		}
	}

	t.Logf("summary for %s: handle vs path after close %d/%d mismatched (required by Organize); "+
		"path vs reopen %d/%d; after 3s %d/%d; after rename %d/%d",
		root, closedMismatches, rounds, reopenMismatches, rounds, laterMismatches, rounds, renameMismatches, rounds)
	if closedMismatches > 0 {
		t.Errorf("file IDs on %s are not stable across close: Organize will keep both the copy and the source for exclusive-copy moves here", root)
	}
	if laterMismatches > 0 || renameMismatches > 0 {
		t.Logf("note: IDs on %s change over time or on rename; Organize does not depend on this, but it suggests client-generated IDs", root)
	}
}
