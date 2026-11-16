package app

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/trembon/switch-library-manager/backend/db"
	"github.com/trembon/switch-library-manager/backend/settings"
	"go.uber.org/zap"
)

// These cases run a whole organize pass (scan, plan, cleanup, moves) against
// synthetic filename-metadata fixtures in which several files carry the same
// content: older and newer updates, older and newer DLC, and exact duplicates
// split between the intake folder and the library.
func TestOrganizeLibraryResolvesSupersededAndDuplicateFiles(t *testing.T) {
	const (
		base    = "0100000000010000"
		update  = "0100000000010800"
		dlc     = "0100000000011001"
		game    = "Scratch Game [" + base + "]"
		baseOut = game + "/Scratch Game [" + base + "][v0].nsp"
	)
	updateOut := func(version string) string {
		return game + "/Updates/Scratch Game [" + update + "][v" + version + "].nsp"
	}
	dlcOut := func(version string) string {
		return game + "/DLC/Scratch Game [" + dlc + "][v" + version + "].nsp"
	}
	cases := []struct {
		name        string
		intake      []string
		library     []string
		keepOld     bool
		wantLibrary []string
	}{
		{
			name:        "two updates in intake",
			intake:      []string{"sxs-game_v65536 [" + update + "][v65536].nsp", "sxs-game_v131072 [" + update + "][v131072].nsp"},
			library:     []string{baseOut},
			wantLibrary: []string{baseOut, updateOut("131072")},
		},
		{
			name:        "three updates in intake and an older one in the library",
			intake:      []string{"a [" + update + "][v131072].nsp", "b [" + update + "][v262144].nsp", "c [" + update + "][v196608].nsp"},
			library:     []string{baseOut, updateOut("65536")},
			wantLibrary: []string{baseOut, updateOut("262144")},
		},
		{
			name:        "intake update is older than the library update",
			intake:      []string{"a [" + update + "][v65536].nsp"},
			library:     []string{baseOut, updateOut("131072")},
			wantLibrary: []string{baseOut, updateOut("131072")},
		},
		{
			name:        "intake update duplicates the library update",
			intake:      []string{"a [" + update + "][v65536].nsp"},
			library:     []string{baseOut, updateOut("65536")},
			wantLibrary: []string{baseOut, updateOut("65536")},
		},
		{
			name:        "intake base duplicates the library base",
			intake:      []string{"a [" + base + "][v0].nsp"},
			library:     []string{baseOut},
			wantLibrary: []string{baseOut},
		},
		{
			name:        "two copies of one update in intake",
			intake:      []string{"a [" + update + "][v65536].nsp", "b [" + update + "][v65536].nsp"},
			library:     []string{baseOut},
			wantLibrary: []string{baseOut, updateOut("65536")},
		},
		{
			name:        "older DLC sorts first in intake",
			intake:      []string{"a [" + dlc + "][v0].nsp", "b [" + dlc + "][v65536].nsp"},
			library:     []string{baseOut},
			wantLibrary: []string{baseOut, dlcOut("65536")},
		},
		{
			name:        "newer DLC sorts first in intake",
			intake:      []string{"a [" + dlc + "][v65536].nsp", "b [" + dlc + "][v0].nsp"},
			library:     []string{baseOut},
			wantLibrary: []string{baseOut, dlcOut("65536")},
		},
		{
			name:        "intake DLC is newer than the library DLC",
			intake:      []string{"a [" + dlc + "][v65536].nsp"},
			library:     []string{baseOut, dlcOut("0")},
			wantLibrary: []string{baseOut, dlcOut("65536")},
		},
		{
			name:        "intake DLC is older than the library DLC",
			intake:      []string{"a [" + dlc + "][v0].nsp"},
			library:     []string{baseOut, dlcOut("65536")},
			wantLibrary: []string{baseOut, dlcOut("65536")},
		},
		{
			name:        "intake DLC duplicates the library DLC",
			intake:      []string{"a [" + dlc + "][v0].nsp"},
			library:     []string{baseOut, dlcOut("0")},
			wantLibrary: []string{baseOut, dlcOut("0")},
		},
		{
			name:        "old DLC versions are kept when cleanup is disabled",
			intake:      []string{"a [" + dlc + "][v0].nsp", "b [" + dlc + "][v65536].nsp"},
			library:     []string{baseOut},
			keepOld:     true,
			wantLibrary: []string{baseOut, dlcOut("0"), dlcOut("65536")},
		},
		{
			name:        "older intake DLC joins the library DLC when cleanup is disabled",
			intake:      []string{"a [" + dlc + "][v0].nsp"},
			library:     []string{baseOut, dlcOut("65536")},
			keepOld:     true,
			wantLibrary: []string{baseOut, dlcOut("0"), dlcOut("65536")},
		},
		{
			name:        "old updates are kept when cleanup is disabled",
			intake:      []string{"a [" + update + "][v65536].nsp", "b [" + update + "][v131072].nsp"},
			library:     []string{baseOut},
			keepOld:     true,
			wantLibrary: []string{baseOut, updateOut("131072"), updateOut("65536")},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			application, library, intake := newScratchOrganizeApp(t, !test.keepOld)
			for _, name := range test.intake {
				writeScratchFile(t, intake, name, intake+"/"+name)
			}
			for _, name := range test.library {
				writeScratchFile(t, library, name, library+"/"+name)
			}

			result, err := application.OrganizeLibrary()
			if err != nil || result.Status != "completed" {
				t.Fatalf("OrganizeLibrary() status=%q conflicts=%d error=%v\nlibrary: %v\nintake: %v",
					result.Status, result.ConflictCount, err, listTree(t, library), listTree(t, intake))
			}
			want := append([]string{}, test.wantLibrary...)
			sort.Strings(want)
			if got := listTree(t, library); strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("library after organize:\n got %v\nwant %v", got, want)
			}
			if got := listTree(t, intake); len(got) != 0 {
				t.Errorf("intake after organize: got %v, want it emptied", got)
			}
			// A file already filed in the library must be the copy that is kept
			// when the intake folder holds nothing newer.
			for _, name := range test.library {
				contents, readErr := os.ReadFile(filepath.Join(library, filepath.FromSlash(name)))
				if readErr == nil && string(contents) != library+"/"+name {
					t.Errorf("library file %q was replaced by %q", name, contents)
				}
			}

			again, err := application.OrganizeLibrary()
			if err != nil || again.Status != "completed" {
				t.Fatalf("second OrganizeLibrary() status=%q error=%v", again.Status, err)
			}
			if got := listTree(t, library); strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("library after second organize:\n got %v\nwant %v", got, want)
			}
		})
	}
}

// When two library files hold the same title and version and one already has
// the organized name, organizing keeps that file instead of deleting it and
// moving the other into its place, which would copy the whole file on shares
// that cannot rename exclusively.
func TestOrganizeLibraryResolvesDuplicateAtItsOrganizedName(t *testing.T) {
	const (
		base    = "0100000000010000"
		baseOut = "Scratch Game [" + base + "]/Scratch Game [" + base + "][v0].nsp"
		// The scan visits this name first, so it is the copy the scan keeps.
		misnamed = "A copy [" + base + "][v0].nsp"
	)
	cases := []struct {
		name          string
		misnamed      string
		organized     string
		cleanup       bool
		wantStatus    string
		wantLibrary   []string
		wantOrganized string
	}{
		{
			name: "identical size keeps the organized file", misnamed: "copy one", organized: "copy two",
			cleanup: true, wantStatus: "completed", wantLibrary: []string{baseOut}, wantOrganized: "copy two",
		},
		{
			name: "different size replaces the organized name with the kept copy", misnamed: "the longer copy", organized: "short",
			cleanup: true, wantStatus: "completed", wantLibrary: []string{baseOut}, wantOrganized: "the longer copy",
		},
		{
			name: "without cleanup the occupied name is a conflict", misnamed: "copy one", organized: "copy two",
			cleanup: false, wantStatus: "blocked", wantLibrary: []string{misnamed, baseOut}, wantOrganized: "copy two",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			application, library, _ := newScratchOrganizeApp(t, test.cleanup)
			writeScratchFile(t, library, misnamed, test.misnamed)
			writeScratchFile(t, library, baseOut, test.organized)

			result, err := application.OrganizeLibrary()
			if err != nil || result.Status != test.wantStatus {
				t.Fatalf("OrganizeLibrary() status=%q conflicts=%d error=%v, want %q\nlibrary: %v",
					result.Status, result.ConflictCount, err, test.wantStatus, listTree(t, library))
			}
			want := append([]string{}, test.wantLibrary...)
			sort.Strings(want)
			if got := listTree(t, library); strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("library after organize:\n got %v\nwant %v", got, want)
			}
			if contents, err := os.ReadFile(filepath.Join(library, filepath.FromSlash(baseOut))); err != nil || string(contents) != test.wantOrganized {
				t.Errorf("organized file contents = %q, error = %v; want %q", contents, err, test.wantOrganized)
			}
		})
	}
}

// newScratchOrganizeApp returns an app whose library and intake folders are
// fresh temporary directories, with organization importing intake files into
// per-game folders.
func newScratchOrganizeApp(t *testing.T, cleanup bool) (*App, string, string) {
	t.Helper()
	root := t.TempDir()
	library := filepath.Join(root, "library")
	intake := filepath.Join(root, "intake")
	for _, folder := range []string{library, intake} {
		if err := os.MkdirAll(folder, 0700); err != nil {
			t.Fatal(err)
		}
	}
	baseFolder := t.TempDir()
	if err := settings.SaveSettingsWithError(&settings.AppSettings{
		Paths: settings.PathSettings{LibraryFolder: library, ScanFolders: []string{intake}},
		Scan:  settings.ScanSettings{Recursive: true, IgnoreFileTypes: []string{}},
		Organization: settings.OrganizeOptions{
			CreateFolderPerGame:    true,
			RenameFiles:            true,
			MoveScanFilesToLibrary: true,
			DeleteEmptyFolders:     true,
			DeleteOldUpdateFiles:   cleanup,
			UpdatesFolder:          "Updates",
			DlcFolder:              "DLC",
			FolderNameTemplate:     "{TITLE_NAME} [{TITLE_ID}]",
			FileNameTemplate:       "{TITLE_NAME} [{TITLE_ID}][v{VERSION}]",
		},
	}, baseFolder); err != nil {
		t.Fatal(err)
	}
	manager, err := db.NewLocalSwitchDBManager(baseFolder)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { manager.Close() })
	application := &App{
		baseFolder: baseFolder, localDbManager: manager, sugarLogger: zap.NewNop().Sugar(),
		state: State{localDB: &db.LocalSwitchFilesDB{}, switchDB: &db.SwitchTitlesDB{TitlesMap: map[string]*db.SwitchTitle{
			"0100000000010": {Attributes: db.TitleAttributes{Id: "0100000000010000", Name: "Scratch Game"}},
		}}},
	}
	return application, library, intake
}

func writeScratchFile(t *testing.T, folder, name, contents string) {
	t.Helper()
	path := filepath.Join(folder, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}

func listTree(t *testing.T, root string) []string {
	t.Helper()
	files := []string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			relative, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return relErr
			}
			files = append(files, filepath.ToSlash(relative))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	return files
}
