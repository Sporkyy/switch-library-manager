package process

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/trembon/switch-library-manager/backend/db"
	"github.com/trembon/switch-library-manager/backend/settings"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestBuildOrganizationPlanLogsActualCountsIncludingEmptyPlans(t *testing.T) {
	for _, test := range []struct {
		name      string
		hasSource bool
		collision bool
		moves     int64
		conflicts int64
	}{
		{name: "empty"},
		{name: "one move", hasSource: true, moves: 1},
		{name: "conflict", hasSource: true, collision: true, conflicts: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			core, observed := observer.New(zap.InfoLevel)
			t.Cleanup(zap.ReplaceGlobals(zap.New(core)))
			root := t.TempDir()
			local := &db.LocalSwitchFilesDB{TitlesMap: map[string]*db.SwitchGameFiles{}}
			if test.hasSource {
				writeFixture(t, filepath.Join(root, "incoming.nsp"), "synthetic logging fixture")
				local.TitlesMap["0100e95004039"] = &db.SwitchGameFiles{
					BaseExist: true,
					File: db.SwitchFileInfo{
						ExtendedInfo: db.ExtendedFileInfo{BaseFolder: root, FileName: "incoming.nsp"},
						Metadata:     metadataForID("0100E95004039000", 0),
					},
				}
			}
			options := settings.OrganizeOptions{RenameFiles: true, FileNameTemplate: "{TITLE_ID}"}
			if test.collision {
				writeFixture(t, filepath.Join(root, "0100E95004039000.nsp"), "occupied destination")
			}
			plan, err := BuildOrganizationPlan(root, local, nil, options)
			if err != nil {
				t.Fatal(err)
			}
			if int64(len(plan.Moves)) != test.moves || int64(len(plan.Conflicts)) != test.conflicts {
				t.Fatalf("plan = %#v, want %d moves and %d conflicts", plan, test.moves, test.conflicts)
			}
			entries := observed.FilterMessage("Organization plan built").All()
			if len(entries) != 1 {
				t.Fatalf("plan log count = %d, want 1", len(entries))
			}
			fields := entries[0].ContextMap()
			for key, want := range map[string]any{
				"library_root": root, "planned_moves": test.moves, "conflicts": test.conflicts,
				"rename_files": true, "create_folder_per_game": false, "move_scan_files_to_library": false,
			} {
				if fields[key] != want {
					t.Errorf("plan log field %s = %#v, want %#v", key, fields[key], want)
				}
			}
		})
	}
}

func TestExecuteOrganizationPlanLogsFailureAndStopsRemainingMoves(t *testing.T) {
	core, observed := observer.New(zap.InfoLevel)
	t.Cleanup(zap.ReplaceGlobals(zap.New(core)))
	root := t.TempDir()
	plan := OrganizationPlan{}
	for _, name := range []string{"first", "second", "third"} {
		source := filepath.Join(root, name+".nsp")
		writeFixture(t, source, "synthetic "+name)
		plan.Moves = append(plan.Moves, organizationMoveForTest(t, source, filepath.Join(root, name+"-organized.nsp")))
	}
	// Simulate a destination created by another process after preflight.
	writeFixture(t, plan.Moves[1].Destination, "late destination")
	err := ExecuteOrganizationPlan(plan, nil, root, settings.OrganizeOptions{}, nil)
	if err == nil {
		t.Fatal("ExecuteOrganizationPlan() returned nil for occupied destination")
	}
	assertNotExists(t, plan.Moves[0].Source)
	assertExists(t, plan.Moves[0].Destination)
	for _, move := range plan.Moves[1:] {
		assertExists(t, move.Source)
	}
	assertNotExists(t, plan.Moves[2].Destination)
	contents, readErr := os.ReadFile(plan.Moves[1].Destination)
	if readErr != nil || string(contents) != "late destination" {
		t.Fatalf("late destination = %q, err = %v", contents, readErr)
	}
	entries := observed.FilterMessage("Organization failed").All()
	if len(entries) != 1 {
		t.Fatalf("failure log count = %d, want 1", len(entries))
	}
	fields := entries[0].ContextMap()
	for key, want := range map[string]any{
		"library_root": root, "completed_moves": int64(1), "planned_moves": int64(3), "error": err.Error(),
	} {
		if fields[key] != want {
			t.Errorf("failure log field %s = %#v, want %#v", key, fields[key], want)
		}
	}
}
