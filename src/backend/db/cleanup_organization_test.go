package db_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/trembon/switch-library-manager/backend/db"
	"github.com/trembon/switch-library-manager/backend/process"
	"github.com/trembon/switch-library-manager/backend/settings"
	"github.com/trembon/switch-library-manager/backend/switchfs"
)

type cleanupFixture struct {
	name     string
	contents string
	records  map[string]*switchfs.ContentMetaAttributes
}

// Seed synthetic deep-scan metadata so these tests exercise the scanner's
// cleanup decisions and the organizer together without encrypted game files.
func scanCleanupFixtures(t *testing.T, root string, fixtures []cleanupFixture) *db.LocalSwitchFilesDB {
	t.Helper()
	state := t.TempDir()
	keyPath := filepath.Join(state, "prod.keys")
	if err := os.WriteFile(keyPath, []byte("header_key = synthetic\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := settings.SaveSettingsWithError(&settings.AppSettings{Paths: settings.PathSettings{ProdKeys: keyPath}}, state); err != nil {
		t.Fatal(err)
	}
	if _, err := settings.InitSwitchKeys(state); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Remove(keyPath); err != nil {
			t.Fatal(err)
		}
		// Key state is global; clear this fixture's keys before another test.
		if _, err := settings.InitSwitchKeys(state); err == nil {
			t.Fatal("synthetic keys remained loaded after removing the fixture")
		}
	})
	persistent, err := db.NewPersistentDB(state)
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range fixtures {
		path := filepath.Join(root, fixture.name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(fixture.contents), 0600); err != nil {
			t.Fatal(err)
		}
		key := path + "|" + filepath.Base(path) + "|" + strconv.Itoa(len(fixture.contents))
		if err := persistent.AddEntry(db.DB_TABLE_FILE_SCAN_METADATA, key, fixture.records); err != nil {
			t.Fatal(err)
		}
	}
	persistent.Close()
	manager, err := db.NewLocalSwitchDBManager(state)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	local, err := manager.CreateLocalSwitchFilesDB([]string{root}, nil, true, true)
	if err != nil {
		t.Fatal(err)
	}
	return local
}

func TestOrganizationKeepsLatestInsteadOfOrganizedOlderDuplicate(t *testing.T) {
	for _, id := range []string{"0100000000010800", "0100000000011001"} {
		t.Run(id, func(t *testing.T) {
			root := t.TempDir()
			records := func(version int) map[string]*switchfs.ContentMetaAttributes {
				return map[string]*switchfs.ContentMetaAttributes{id: {TitleId: id, Version: version}}
			}
			// Scan an old file, its duplicate at the organized name, then a
			// newer version. All three have the same physical size.
			local := scanCleanupFixtures(t, root, []cleanupFixture{
				{"A.nsp", "old111", records(1)},
				{"Content/" + id + ".nsp", "old222", records(1)},
				{"z.nsp", "new333", records(2)},
			})
			options := settings.OrganizeOptions{
				RenameFiles: true, FileNameTemplate: "{TITLE_ID}", UpdatesFolder: "Content", DlcFolder: "Content",
				DeleteOldUpdateFiles: true, ProcessWhenMissingBaseGame: true,
			}
			plan, err := process.BuildOrganizationPlan(root, local, nil, options)
			if err != nil || len(plan.Conflicts) != 0 {
				t.Fatalf("plan=%+v, error=%v", plan, err)
			}
			if err := process.ExecuteOrganizationPlan(plan, local, root, options, nil); err != nil {
				t.Fatal(err)
			}
			assertCleanupContents(t, filepath.Join(root, "Content", id+".nsp"), "new333")
			for _, name := range []string{"A.nsp", "z.nsp"} {
				if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
					t.Fatalf("source %q remains after organization: %v", name, err)
				}
			}
		})
	}
}

func TestOrganizationKeepsBundleInsteadOfOrganizedStandaloneDuplicate(t *testing.T) {
	root := t.TempDir()
	const baseID = "0100000000010000"
	const updateID = "0100000000010800"
	local := scanCleanupFixtures(t, root, []cleanupFixture{
		{"A.xci", "bundle", map[string]*switchfs.ContentMetaAttributes{
			baseID: {TitleId: baseID, Version: 0}, updateID: {TitleId: updateID, Version: 1},
		}},
		{"Game.xci", "single", map[string]*switchfs.ContentMetaAttributes{baseID: {TitleId: baseID, Version: 0}}},
	})
	remote := &db.SwitchTitlesDB{TitlesMap: map[string]*db.SwitchTitle{
		"0100000000010": {Attributes: db.TitleAttributes{Id: baseID, Name: "Game"}},
	}}
	options := settings.OrganizeOptions{RenameFiles: true, FileNameTemplate: "{TITLE_NAME}", DeleteOldUpdateFiles: true}
	plan, err := process.BuildOrganizationPlan(root, local, remote, options)
	if err != nil || len(plan.Conflicts) != 0 {
		t.Fatalf("plan=%+v, error=%v", plan, err)
	}
	if err := process.ExecuteOrganizationPlan(plan, local, root, options, nil); err != nil {
		t.Fatal(err)
	}
	assertCleanupContents(t, filepath.Join(root, "Game.xci"), "bundle")
	game := local.TitlesMap["0100000000010"]
	if game.File.ExtendedInfo.FileName != "Game.xci" || game.Updates[1].ExtendedInfo.FileName != "Game.xci" {
		t.Fatalf("bundle records did not follow the surviving file: %+v", game)
	}
}

func TestOrganizationKeepsAlreadyOrganizedStandaloneDuplicate(t *testing.T) {
	root := t.TempDir()
	const id = "0100000000010000"
	records := map[string]*switchfs.ContentMetaAttributes{id: {TitleId: id, Version: 0}}
	local := scanCleanupFixtures(t, root, []cleanupFixture{
		{"A.nsp", "copy11", records},
		{"Game.nsp", "copy22", records},
		{"Z.nsp", "copy33", records},
	})
	remote := &db.SwitchTitlesDB{TitlesMap: map[string]*db.SwitchTitle{
		"0100000000010": {Attributes: db.TitleAttributes{Id: id, Name: "Game"}},
	}}
	options := settings.OrganizeOptions{RenameFiles: true, FileNameTemplate: "{TITLE_NAME}", DeleteOldUpdateFiles: true}
	plan, err := process.BuildOrganizationPlan(root, local, remote, options)
	if err != nil || len(plan.Conflicts) != 0 || len(plan.Moves) != 0 {
		t.Fatalf("plan=%+v, error=%v; want cleanup without moves", plan, err)
	}
	if err := process.ExecuteOrganizationPlan(plan, local, root, options, nil); err != nil {
		t.Fatal(err)
	}
	assertCleanupContents(t, filepath.Join(root, "Game.nsp"), "copy22")
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 {
		t.Fatalf("library=%v, error=%v; want only the organized copy", entries, err)
	}
}

func TestCleanupRetainsDifferentSizedCopiesRegardlessOfScanOrder(t *testing.T) {
	for _, id := range []string{"0100000000010000", "0100000000010800", "0100000000011001"} {
		for _, partialFirst := range []bool{false, true} {
			t.Run(id+"/partial-first="+strconv.FormatBool(partialFirst), func(t *testing.T) {
				root := t.TempDir()
				first, second := "complete source content", "partial"
				if partialFirst {
					first, second = second, first
				}
				records := map[string]*switchfs.ContentMetaAttributes{id: {TitleId: id, Version: 1}}
				local := scanCleanupFixtures(t, root, []cleanupFixture{
					{"A.nsp", first, records}, {"Z.nsp", second, records},
				})
				if len(local.CleanupCandidates) != 0 {
					t.Fatalf("different-sized copies became cleanup candidates: %+v", local.CleanupCandidates)
				}
				foundMismatch := false
				for _, skipped := range local.Skipped {
					foundMismatch = foundMismatch || strings.Contains(skipped.ReasonText, "different sizes")
				}
				if !foundMismatch {
					t.Fatal("missing size-mismatch diagnostic")
				}
				assertCleanupContents(t, filepath.Join(root, "A.nsp"), first)
				assertCleanupContents(t, filepath.Join(root, "Z.nsp"), second)
			})
		}
	}
}

func assertCleanupContents(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != want {
		t.Fatalf("file %q = %q, error=%v; want %q", path, got, err, want)
	}
}
