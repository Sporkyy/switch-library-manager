package process

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trembon/switch-library-manager/backend/db"
	"github.com/trembon/switch-library-manager/backend/settings"
	"github.com/trembon/switch-library-manager/backend/switchfs"
)

// Cleanup runs before the planned moves, so a file it deletes must not also
// be planned as a move.
func TestBuildOrganizationPlanLeavesOutFilesCleanupDeletes(t *testing.T) {
	root := t.TempDir()
	library := filepath.Join(root, "library")
	intake := filepath.Join(root, "intake")
	for _, folder := range []string{library, intake} {
		if err := os.Mkdir(folder, 0700); err != nil {
			t.Fatal(err)
		}
	}
	const updateID = "0100000000010800"
	oldPath := filepath.Join(intake, "old.nsp")
	newPath := filepath.Join(intake, "new.nsp")
	for _, path := range []string{oldPath, newPath} {
		if err := os.WriteFile(path, []byte(path), 0600); err != nil {
			t.Fatal(err)
		}
	}
	oldInfo, newInfo := extendedInfoForPath(t, oldPath), extendedInfoForPath(t, newPath)
	local := &db.LocalSwitchFilesDB{
		TitlesMap: map[string]*db.SwitchGameFiles{"0100000000010": {
			LatestUpdate: 2,
			Updates: map[int]db.SwitchFileInfo{
				1: {ExtendedInfo: oldInfo, Metadata: &switchfs.ContentMetaAttributes{TitleId: updateID, Version: 1}},
				2: {ExtendedInfo: newInfo, Metadata: &switchfs.ContentMetaAttributes{TitleId: updateID, Version: 2}},
			},
			Dlc: map[string]db.SwitchFileInfo{},
		}},
		CleanupCandidates: []db.CleanupCandidate{{File: oldInfo, Replacement: newInfo, ReasonCode: db.REASON_OLD_UPDATE}},
	}
	options := settings.OrganizeOptions{
		RenameFiles:                true,
		FileNameTemplate:           "{TITLE_ID} [v{VERSION}]",
		MoveScanFilesToLibrary:     true,
		ProcessWhenMissingBaseGame: true,
	}
	sources := func(plan OrganizationPlan) []string {
		result := []string{}
		for _, move := range plan.Moves {
			result = append(result, filepath.Base(move.Source))
		}
		return result
	}

	plan, err := BuildOrganizationPlan(library, local, nil, options)
	if err != nil || len(plan.Conflicts) != 0 || len(plan.Moves) != 2 {
		t.Fatalf("without cleanup both updates are moved: sources=%v conflicts=%v error=%v", sources(plan), plan.Conflicts, err)
	}

	options.DeleteOldUpdateFiles = true
	plan, err = BuildOrganizationPlan(library, local, nil, options)
	if err != nil || len(plan.Conflicts) != 0 || len(plan.Moves) != 1 || plan.Moves[0].Source != newPath || len(plan.Cleanup) != 1 {
		t.Fatalf("with cleanup only the newest update is moved: sources=%v conflicts=%v error=%v", sources(plan), plan.Conflicts, err)
	}

	// Without importing, cleanup leaves scan-folder files alone, so the plan
	// does too: nothing outside the library is deleted or moved.
	options.MoveScanFilesToLibrary = false
	plan, err = BuildOrganizationPlan(library, local, nil, options)
	if err != nil || len(plan.Conflicts) != 0 || len(plan.Moves) != 0 {
		t.Fatalf("scan-folder files stay untouched without importing: sources=%v conflicts=%v error=%v", sources(plan), plan.Conflicts, err)
	}
}

// Three copies of one base file: the scan keeps "a.nsp", but "Game.nsp"
// already has the organized name. The plan keeps "Game.nsp" in place and
// deletes the other two, each relying on the copy that survives.
func TestOrganizationPlanKeepsDuplicateThatHasTheOrganizedName(t *testing.T) {
	root := t.TempDir()
	keeper := filepath.Join(root, "a.nsp")
	organized := filepath.Join(root, "Game.nsp")
	other := filepath.Join(root, "z.nsp")
	for _, path := range []string{keeper, organized, other} {
		writeFixture(t, path, "same size")
	}
	keeperInfo := extendedInfoForPath(t, keeper)
	organizedInfo := extendedInfoForPath(t, organized)
	otherInfo := extendedInfoForPath(t, other)
	local := &db.LocalSwitchFilesDB{
		TitlesMap: map[string]*db.SwitchGameFiles{"0100000000010": {
			BaseExist: true,
			File:      db.SwitchFileInfo{ExtendedInfo: keeperInfo, Metadata: metadataForID("0100000000010000", 0)},
		}},
		CleanupCandidates: []db.CleanupCandidate{
			{File: organizedInfo, Replacement: keeperInfo, ReasonCode: db.REASON_DUPLICATE, Interchangeable: true},
			{File: otherInfo, Replacement: keeperInfo, ReasonCode: db.REASON_DUPLICATE, Interchangeable: true},
		},
	}
	remote := &db.SwitchTitlesDB{TitlesMap: map[string]*db.SwitchTitle{
		"0100000000010": {Attributes: db.TitleAttributes{Id: "0100000000010000", Name: "Game"}},
	}}
	options := settings.OrganizeOptions{RenameFiles: true, FileNameTemplate: "{TITLE_NAME}", DeleteOldUpdateFiles: true}

	plan, err := BuildOrganizationPlan(root, local, remote, options)
	if err != nil || len(plan.Conflicts) != 0 || len(plan.Moves) != 0 {
		t.Fatalf("plan moves=%v conflicts=%v error=%v; want no moves", plan.Moves, plan.Conflicts, err)
	}
	if len(plan.Cleanup) != 2 {
		t.Fatalf("cleanup = %#v, want two deletions", plan.Cleanup)
	}
	for _, candidate := range plan.Cleanup {
		if candidate.Replacement != organizedInfo || (candidate.File != keeperInfo && candidate.File != otherInfo) {
			t.Fatalf("cleanup = %#v, want a.nsp and z.nsp deleted in favor of Game.nsp", plan.Cleanup)
		}
	}
	if len(local.CleanupCandidates) != 2 || local.CleanupCandidates[0].File != organizedInfo {
		t.Fatal("planning changed the scan's cleanup candidates")
	}

	if err := ExecuteOrganizationPlan(plan, local, root, options, nil); err != nil {
		t.Fatalf("ExecuteOrganizationPlan() = %v", err)
	}
	assertExists(t, organized)
	assertNotExists(t, keeper)
	assertNotExists(t, other)
	if local.CleanupCandidates != nil {
		t.Fatal("executed cleanup candidates were left for a second deletion pass")
	}
}

// Older versions can differ in size. Cleanup deletes the old version before
// moving the new one into its organized name when the template omits VERSION.
func TestOrganizationPlanReplacesOlderVersionAtOrganizedName(t *testing.T) {
	root := t.TempDir()
	const updateID = "0100000000010800"
	keeper := filepath.Join(root, "a.nsp")
	organized := filepath.Join(root, updateID+".nsp")
	writeFixture(t, keeper, "the kept copy")
	writeFixture(t, organized, "short")
	keeperInfo := extendedInfoForPath(t, keeper)
	local := &db.LocalSwitchFilesDB{
		TitlesMap: map[string]*db.SwitchGameFiles{"0100000000010": {
			LatestUpdate: 2,
			Updates: map[int]db.SwitchFileInfo{
				1: {ExtendedInfo: extendedInfoForPath(t, organized), Metadata: metadataForID(updateID, 1)},
				2: {ExtendedInfo: keeperInfo, Metadata: metadataForID(updateID, 2)},
			},
		}},
		CleanupCandidates: []db.CleanupCandidate{{File: extendedInfoForPath(t, organized), Replacement: keeperInfo, ReasonCode: db.REASON_OLD_UPDATE}},
	}
	options := settings.OrganizeOptions{RenameFiles: true, FileNameTemplate: "{TITLE_ID}", DeleteOldUpdateFiles: true, ProcessWhenMissingBaseGame: true}

	plan, err := BuildOrganizationPlan(root, local, nil, options)
	if err != nil || len(plan.Conflicts) != 0 || len(plan.Moves) != 1 || len(plan.Cleanup) != 1 {
		t.Fatalf("plan = %#v, error = %v; want one deletion then one move", plan, err)
	}
	if err := ExecuteOrganizationPlan(plan, local, root, options, nil); err != nil {
		t.Fatalf("ExecuteOrganizationPlan() = %v", err)
	}
	assertNotExists(t, keeper)
	if got, err := os.ReadFile(organized); err != nil || string(got) != "the kept copy" {
		t.Fatalf("organized file = %q, error = %v; want the kept copy", got, err)
	}
}

// A cleanup file that changed after the scan stops the run before any move.
func TestExecuteOrganizationPlanStopsWhenCleanupFails(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.nsp")
	stale := filepath.Join(root, "stale.nsp")
	writeFixture(t, source, "source")
	writeFixture(t, stale, "stale")
	staleInfo := extendedInfoForPath(t, stale)
	staleInfo.Size++
	plan := OrganizationPlan{
		Moves:   []OrganizationMove{{Source: source, Destination: filepath.Join(root, "moved.nsp")}},
		Cleanup: []db.CleanupCandidate{{File: staleInfo, Replacement: extendedInfoForPath(t, source), ReasonCode: db.REASON_DUPLICATE}},
	}
	if err := ExecuteOrganizationPlan(plan, nil, root, settings.OrganizeOptions{}, nil); err == nil {
		t.Fatal("ExecuteOrganizationPlan() ignored a cleanup file changed after the scan")
	}
	assertExists(t, source)
	assertExists(t, stale)
	assertNotExists(t, filepath.Join(root, "moved.nsp"))
}

// With cleanup off, a superseded DLC version is filed beside the current one,
// as old updates are, and the scan record follows the move.
func TestOrganizationPlanFilesSupersededDLCWhenCleanupIsOff(t *testing.T) {
	root := t.TempDir()
	const dlcID = "0100000000011001"
	current := filepath.Join(root, "current.nsp")
	older := filepath.Join(root, "older.nsp")
	writeFixture(t, current, "current")
	writeFixture(t, older, "older")
	game := &db.SwitchGameFiles{
		Dlc:      map[string]db.SwitchFileInfo{strings.ToLower(dlcID): {ExtendedInfo: extendedInfoForPath(t, current), Metadata: metadataForID(dlcID, 2)}},
		OlderDlc: []db.SwitchFileInfo{{ExtendedInfo: extendedInfoForPath(t, older), Metadata: metadataForID(dlcID, 1)}, {ExtendedInfo: db.ExtendedFileInfo{BaseFolder: root, FileName: "no-metadata.nsp"}}},
	}
	local := &db.LocalSwitchFilesDB{TitlesMap: map[string]*db.SwitchGameFiles{"0100000000010": game}}
	options := settings.OrganizeOptions{RenameFiles: true, FileNameTemplate: "{TITLE_ID} [v{VERSION}]", ProcessWhenMissingBaseGame: true}

	plan, err := BuildOrganizationPlan(root, local, nil, options)
	if err != nil || len(plan.Conflicts) != 0 || len(plan.Moves) != 2 {
		t.Fatalf("plan moves=%v conflicts=%v error=%v; want both DLC versions", plan.Moves, plan.Conflicts, err)
	}
	if err := ExecuteOrganizationPlan(plan, local, root, options, nil); err != nil {
		t.Fatalf("ExecuteOrganizationPlan() = %v", err)
	}
	for _, name := range []string{"0100000000011001 [v2].nsp", "0100000000011001 [v1].nsp"} {
		assertExists(t, filepath.Join(root, name))
	}
	if game.OlderDlc[0].ExtendedInfo.FileName != "0100000000011001 [v1].nsp" {
		t.Fatalf("superseded DLC record = %#v, want it to follow the move", game.OlderDlc[0].ExtendedInfo)
	}
}
