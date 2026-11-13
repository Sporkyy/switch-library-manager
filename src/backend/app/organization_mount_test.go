package app

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/trembon/switch-library-manager/backend/db"
	"github.com/trembon/switch-library-manager/backend/process"
	"github.com/trembon/switch-library-manager/backend/settings"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// To exercise a real mount, set SLM_ORGANIZE_TEST_ROOT to its existing root.
// Only uniquely created scratch directories are scanned, moved, and removed.
// SLM_ORGANIZE_TEST_INTAKE_ROOT can select a second volume for the intake case.
// The fixture deliberately uses filename metadata, not encrypted game contents.
func TestOrganizeLibraryScratchMount(t *testing.T) {
	for _, scenario := range []string{"library_rename", "intake_same_mount", "intake_other_mount", "late_collision"} {
		t.Run(scenario, func(t *testing.T) {
			parent := os.Getenv("SLM_ORGANIZE_TEST_ROOT")
			if parent == "" {
				parent = t.TempDir()
			}
			root := organizationScratchDirectory(t, parent)
			library := filepath.Join(root, "library")
			incoming := filepath.Join(root, "incoming")
			if scenario == "intake_other_mount" {
				intakeParent := os.Getenv("SLM_ORGANIZE_TEST_INTAKE_ROOT")
				if intakeParent == "" {
					intakeParent = t.TempDir()
				}
				incoming = organizationScratchDirectory(t, intakeParent)
			}
			for _, directory := range []string{library, incoming} {
				if err := os.MkdirAll(directory, 0700); err != nil {
					t.Fatal(err)
				}
			}
			importing := scenario == "intake_same_mount" || scenario == "intake_other_mount"
			sourceFolder := incoming
			if !importing {
				sourceFolder = library
			}
			const titleID = "0100000000010000"
			source := filepath.Join(sourceFolder, "Needs Rename ["+titleID+"][v0].nsp")
			destination := filepath.Join(library, "Scratch Game ["+titleID+"][v0].nsp")
			contents := bytes.Repeat([]byte("synthetic filename-metadata fixture\n"), 4096)
			if err := os.WriteFile(source, contents, 0600); err != nil {
				t.Fatal(err)
			}
			// Flush fixture writes before backdating it: SMB can otherwise flush
			// delayed writes on the next open and change its preflight timestamp.
			fixture, err := os.OpenFile(source, os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			flushErr := fixture.Sync()
			closeErr := fixture.Close()
			if flushErr != nil || closeErr != nil {
				t.Fatalf("flush fixture: %v; close fixture: %v", flushErr, closeErr)
			}
			stamp := time.Date(2020, 2, 3, 4, 5, 6, 0, time.UTC)
			if err := os.Chtimes(source, stamp, stamp); err != nil {
				t.Fatal(err)
			}
			base := t.TempDir()
			options := settings.OrganizeOptions{
				RenameFiles:            true,
				FileNameTemplate:       "{TITLE_NAME} ({DLC_NAME})[{TITLE_ID}][v{VERSION}]",
				MoveScanFilesToLibrary: importing,
			}
			if err := settings.SaveSettingsWithError(&settings.AppSettings{
				Paths:        settings.PathSettings{LibraryFolder: library, ScanFolders: []string{incoming}},
				Scan:         settings.ScanSettings{Recursive: true, IgnoreFileTypes: []string{}},
				Organization: options,
			}, base); err != nil {
				t.Fatal(err)
			}
			manager, err := db.NewLocalSwitchDBManager(base)
			if err != nil {
				t.Fatal(err)
			}
			defer manager.Close()
			core, logs := observer.New(zap.InfoLevel)
			logger := zap.New(core)
			restore := zap.ReplaceGlobals(logger)
			defer restore()
			application := &App{
				baseFolder: base, localDbManager: manager, sugarLogger: logger.Sugar(),
				state: State{localDB: &db.LocalSwitchFilesDB{}, switchDB: &db.SwitchTitlesDB{TitlesMap: map[string]*db.SwitchTitle{
					"0100000000010": {Attributes: db.TitleAttributes{Id: titleID, Name: "Scratch Game"}},
				}}},
			}
			local, err := application.buildLocalDB(true)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := process.BuildOrganizationPlan(library, local, application.state.switchDB, options)
			if err != nil || len(plan.Moves) != 1 || len(plan.Conflicts) != 0 || plan.Moves[0].Source != source || plan.Moves[0].Destination != destination {
				t.Fatalf("expected one distinct-path move: plan=%+v error=%v", plan, err)
			}
			t.Logf("planned_moves=1 source=%q destination=%q", source, destination)
			if scenario == "late_collision" {
				if err := os.WriteFile(destination, []byte("late destination"), 0600); err != nil {
					t.Fatal(err)
				}
				err := process.ExecuteOrganizationPlan(plan, local, library, options, nil)
				for _, entry := range logs.All() {
					t.Log(entry.Message, entry.ContextMap())
				}
				if !errors.Is(err, os.ErrExist) {
					t.Fatalf("late collision error=%v, want existence error", err)
				}
				got, readErr := os.ReadFile(source)
				if readErr != nil || !bytes.Equal(got, contents) {
					t.Fatalf("source changed after collision: error=%v", readErr)
				}
				got, readErr = os.ReadFile(destination)
				if readErr != nil || string(got) != "late destination" {
					t.Fatalf("destination changed after collision: contents=%q error=%v", got, readErr)
				}
				return
			}
			result, err := application.OrganizeLibrary()
			for _, entry := range logs.All() {
				t.Log(entry.Message, entry.ContextMap())
			}
			if err != nil || result.Status != "completed" {
				t.Fatalf("OrganizeLibrary() status=%q error=%v", result.Status, err)
			}
			if _, err := os.Lstat(source); !os.IsNotExist(err) {
				t.Fatalf("source remained after successful move: %v", err)
			}
			got, err := os.ReadFile(destination)
			if err != nil || !bytes.Equal(got, contents) {
				t.Fatalf("destination contents differ: bytes=%d error=%v", len(got), err)
			}
			info, err := os.Stat(destination)
			if err != nil || !info.ModTime().Equal(stamp) {
				t.Fatalf("destination modification time was not preserved: info=%v error=%v", info, err)
			}
			again, err := process.BuildOrganizationPlan(library, application.state.localDB, application.state.switchDB, options)
			if err != nil || len(again.Moves) != 0 || len(again.Conflicts) != 0 {
				t.Fatalf("rerun should plan zero moves: plan=%+v error=%v", again, err)
			}
		})
	}
}

func organizationScratchDirectory(t *testing.T, parent string) string {
	t.Helper()
	root, err := os.MkdirTemp(parent, ".slm-organize-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove scratch directory %q: %v", root, err)
		}
	})
	return root
}
