package process

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/trembon/switch-library-manager/backend/db"
	"github.com/trembon/switch-library-manager/backend/settings"
	"go.uber.org/zap"
)

// OrganizationMove describes one physical file move after template expansion.
type OrganizationMove struct {
	Source      string
	Destination string
	SourceSize  int64
	SourceStamp os.FileInfo
}

// OrganizationConflict describes a source whose planned destination cannot be
// used without replacing or colliding with another file.
type OrganizationConflict struct {
	Source      string
	Destination string
	OtherSource string
	Reason      string
}

// OrganizationPlan contains every move and preflight issue found for a scan.
type OrganizationPlan struct {
	Moves     []OrganizationMove
	Conflicts []OrganizationConflict
	// Cleanup lists the redundant files ExecuteOrganizationPlan deletes.
	// It is empty unless DeleteOldUpdateFiles is enabled.
	Cleanup []db.CleanupCandidate

	// pendingCleanup maps the path key of each Cleanup file to its index.
	// Those files are never planned as moves.
	pendingCleanup map[string]int
	// cleanupAfterImport maps a Cleanup index to the move that must succeed first.
	cleanupAfterImport map[int]int
}

// OrganizationConflictsError marks a plan rejected before any filesystem
// changes. Callers can publish its conflicts in their existing Issues view.
type OrganizationConflictsError struct {
	Conflicts []OrganizationConflict
}

func (e *OrganizationConflictsError) Error() string {
	return fmt.Sprintf("organization canceled: %d destination conflict(s); see Issues", len(e.Conflicts))
}

// BuildOrganizationPlan computes and validates all destinations without
// creating directories or changing library files. Files outside baseFolder,
// such as those in scan folders, are left untouched unless
// MoveScanFilesToLibrary imports them into baseFolder.
//
// With DeleteOldUpdateFiles enabled the plan deletes in-scope redundant
// files. If the replacement is in intake, deletion waits until it is imported.
// When a destination occupant is an exact duplicate of the file planned to
// replace it, the plan can keep the occupant and delete the other copy.
func BuildOrganizationPlan(
	baseFolder string,
	localDB *db.LocalSwitchFilesDB,
	titlesDB *db.SwitchTitlesDB,
	options settings.OrganizeOptions,
) (OrganizationPlan, error) {
	if localDB == nil {
		return OrganizationPlan{}, fmt.Errorf("local library has not been scanned")
	}
	if !IsOptionsValid(options) {
		return OrganizationPlan{}, fmt.Errorf("organization options are invalid")
	}

	var candidates []db.CleanupCandidate
	if options.DeleteOldUpdateFiles {
		var err error
		candidates, err = cleanupCandidatesInScope(baseFolder, localDB, options.MoveScanFilesToLibrary)
		if err != nil {
			return OrganizationPlan{}, err
		}
	}
	// Each pass can only drop candidates, so this ends after at most one pass
	// per candidate.
	for {
		plan, err := buildOrganizationPlan(baseFolder, localDB, titlesDB, options, candidates)
		if err != nil {
			return OrganizationPlan{}, err
		}
		kept, err := plan.cleanupKeptInLibrary(baseFolder)
		if err != nil {
			return OrganizationPlan{}, err
		}
		if len(kept) == len(candidates) {
			validateOrganizationPlan(&plan)
			zap.L().Info("Organization plan built",
				zap.String("library_root", baseFolder),
				zap.Int("planned_moves", len(plan.Moves)),
				zap.Int("planned_deletions", len(plan.Cleanup)),
				zap.Int("conflicts", len(plan.Conflicts)),
				zap.Bool("rename_files", options.RenameFiles),
				zap.Bool("create_folder_per_game", options.CreateFolderPerGame),
				zap.Bool("move_scan_files_to_library", options.MoveScanFilesToLibrary),
			)
			return plan, nil
		}
		// The planning pass may have treated a dropped file as deleted, so
		// plan again from the scan's candidates without it.
		remaining := make([]db.CleanupCandidate, 0, len(kept))
		for _, index := range kept {
			remaining = append(remaining, candidates[index])
		}
		candidates = remaining
	}
}

// cleanupKeptInLibrary returns the indexes of the plan's cleanup candidates
// whose kept copy is in the library or is moved there by the plan. Deleting
// any other candidate would leave the library relying on a scan-folder copy,
// for example when the plan skips a game that has no base file.
func (plan *OrganizationPlan) cleanupKeptInLibrary(baseFolder string) ([]int, error) {
	if len(plan.Cleanup) == 0 {
		return nil, nil
	}
	moved := make(map[string]int, len(plan.Moves))
	for index, move := range plan.Moves {
		key, err := organizationPathKey(move.Source)
		if err != nil {
			return nil, fmt.Errorf("resolve planned move %q: %w", move.Source, err)
		}
		moved[key] = index
	}
	kept := make([]int, 0, len(plan.Cleanup))
	for index, candidate := range plan.Cleanup {
		inside, err := organizationPathWithin(baseFolder, candidate.Replacement.BaseFolder)
		if err != nil {
			return nil, fmt.Errorf("check cleanup replacement scope: %w", err)
		}
		if inside {
			kept = append(kept, index)
			continue
		}
		replacement := filepath.Join(candidate.Replacement.BaseFolder, candidate.Replacement.FileName)
		key, err := organizationPathKey(replacement)
		if err != nil {
			return nil, fmt.Errorf("resolve cleanup replacement %q: %w", replacement, err)
		}
		moveIndex, ok := moved[key]
		if !ok {
			zap.L().Info("Kept redundant file because its replacement is not imported",
				zap.String("file", filepath.Join(candidate.File.BaseFolder, candidate.File.FileName)),
				zap.String("replacement", replacement),
			)
			continue
		}
		if plan.cleanupAfterImport == nil {
			plan.cleanupAfterImport = make(map[int]int)
		}
		plan.cleanupAfterImport[index] = moveIndex
		kept = append(kept, index)
	}
	return kept, nil
}

// buildOrganizationPlan plans every move for one set of cleanup candidates.
func buildOrganizationPlan(
	baseFolder string,
	localDB *db.LocalSwitchFilesDB,
	titlesDB *db.SwitchTitlesDB,
	options settings.OrganizeOptions,
	candidates []db.CleanupCandidate,
) (OrganizationPlan, error) {
	plan := OrganizationPlan{Moves: []OrganizationMove{}, Conflicts: []OrganizationConflict{}}
	if options.DeleteOldUpdateFiles {
		// keepDuplicateInPlace rewrites entries, so plan on a copy.
		plan.Cleanup = append([]db.CleanupCandidate{}, candidates...)
		plan.pendingCleanup = make(map[string]int, len(candidates))
		for index, candidate := range candidates {
			key, err := organizationPathKey(filepath.Join(candidate.File.BaseFolder, candidate.File.FileName))
			if err != nil {
				return OrganizationPlan{}, fmt.Errorf("resolve cleanup candidate %q: %w", candidate.File.FileName, err)
			}
			plan.pendingCleanup[key] = index
		}
	}
	keys := make([]string, 0, len(localDB.TitlesMap))
	for key := range localDB.TitlesMap {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	seenSources := map[string]struct{}{}

	for _, key := range keys {
		game := localDB.TitlesMap[key]
		if game == nil || (!game.BaseExist && !options.ProcessWhenMissingBaseGame) {
			continue
		}
		title := (*db.SwitchTitle)(nil)
		if titlesDB != nil {
			title = titlesDB.TitlesMap[key]
		}
		titleName := db.ResolveGameTitleName(title, game, options.AllowUnicodeFileNames)
		templateData := map[string]string{
			settings.TEMPLATE_TITLE_NAME:  titleName,
			settings.TEMPLATE_VERSION_TXT: "",
		}
		if title != nil {
			templateData[settings.TEMPLATE_TITLE_ID] = title.Attributes.Id
			templateData[settings.TEMPLATE_REGION] = title.Attributes.Region
		}
		if templateData[settings.TEMPLATE_TITLE_ID] == "" && game.File.Metadata != nil {
			templateData[settings.TEMPLATE_TITLE_ID] = game.File.Metadata.TitleId
		}
		setBaseFileVersionTemplateData(templateData, game)

		gameFolder := ""
		if options.CreateFolderPerGame {
			gameFolder = getFolderName(options, templateData)
		}

		if game.IsSplit && game.BaseExist {
			fileDestination, included, err := organizationDestinationFolder(baseFolder, gameFolder, game.File.ExtendedInfo.BaseFolder, options)
			if err != nil {
				return OrganizationPlan{}, fmt.Errorf("resolve split destination for %q: %w", key, err)
			}
			if !included {
				continue
			}
			if err := addSplitMoves(&plan, seenSources, game.File.ExtendedInfo.FileName, game.File.ExtendedInfo.BaseFolder, fileDestination); err != nil {
				return OrganizationPlan{}, err
			}
			continue
		}

		if game.BaseExist {
			templateData[settings.TEMPLATE_TYPE] = "BASE"
			templateData[settings.TEMPLATE_PACKAGE_CONTENTS] = packageContents(game)
			setFileSizeTemplateData(templateData, game.File.ExtendedInfo.Size)
			target, included, err := organizationTargetPath(baseFolder, gameFolder, game.File.ExtendedInfo.BaseFolder, game.File.ExtendedInfo.FileName, options, "base", templateData, 0)
			if err != nil {
				return OrganizationPlan{}, fmt.Errorf("resolve base destination for %q: %w", key, err)
			}
			if included {
				if err := addOrganizationMove(&plan, seenSources, filepath.Join(game.File.ExtendedInfo.BaseFolder, game.File.ExtendedInfo.FileName), target); err != nil {
					return OrganizationPlan{}, fmt.Errorf("plan base file for %q: %w", key, err)
				}
			}
		}
		templateData[settings.TEMPLATE_PACKAGE_CONTENTS] = ""

		updateVersions := make([]int, 0, len(game.Updates))
		for version := range game.Updates {
			updateVersions = append(updateVersions, version)
		}
		sort.Sort(sort.Reverse(sort.IntSlice(updateVersions)))
		for _, version := range updateVersions {
			update := game.Updates[version]
			if game.BaseExist && samePhysicalFilePath(game.File.ExtendedInfo, update.ExtendedInfo) {
				continue
			}
			if update.Metadata != nil {
				templateData[settings.TEMPLATE_TITLE_ID] = update.Metadata.TitleId
				if update.Metadata.Ncap != nil {
					templateData[settings.TEMPLATE_VERSION_TXT] = update.Metadata.Ncap.DisplayVersion
				}
			}
			templateData[settings.TEMPLATE_VERSION] = strconv.Itoa(version)
			templateData[settings.TEMPLATE_TYPE] = "UPD"
			setFileSizeTemplateData(templateData, update.ExtendedInfo.Size)
			target, included, err := organizationTargetPath(baseFolder, gameFolder, update.ExtendedInfo.BaseFolder, update.ExtendedInfo.FileName, options, "update", templateData, 0)
			if err != nil {
				return OrganizationPlan{}, fmt.Errorf("resolve update destination for %q: %w", key, err)
			}
			if !included {
				continue
			}
			if err := addOrganizationMove(&plan, seenSources, filepath.Join(update.ExtendedInfo.BaseFolder, update.ExtendedInfo.FileName), target); err != nil {
				return OrganizationPlan{}, fmt.Errorf("plan update file for %q: %w", key, err)
			}
		}

		for _, entry := range organizationDlcFiles(game) {
			id, dlc := entry.id, entry.file
			if game.BaseExist && samePhysicalFilePath(game.File.ExtendedInfo, dlc.ExtendedInfo) {
				continue
			}
			setDlcVersionTemplateData(templateData, dlc)
			templateData[settings.TEMPLATE_TYPE] = "DLC"
			templateData[settings.TEMPLATE_TITLE_ID] = id
			templateData[settings.TEMPLATE_DLC_NAME] = getDlcName(title, dlc)
			setFileSizeTemplateData(templateData, dlc.ExtendedInfo.Size)
			target, included, err := organizationTargetPath(baseFolder, gameFolder, dlc.ExtendedInfo.BaseFolder, dlc.ExtendedInfo.FileName, options, "dlc", templateData, 0)
			if err != nil {
				return OrganizationPlan{}, fmt.Errorf("resolve DLC destination for %q: %w", key, err)
			}
			if !included {
				continue
			}
			if err := addOrganizationMove(&plan, seenSources, filepath.Join(dlc.ExtendedInfo.BaseFolder, dlc.ExtendedInfo.FileName), target); err != nil {
				return OrganizationPlan{}, fmt.Errorf("plan DLC file for %q: %w", key, err)
			}
		}
	}

	return plan, nil
}

type organizationDlcFile struct {
	id   string
	file db.SwitchFileInfo
}

// organizationDlcFiles lists the game's current DLC by title ID, followed by
// superseded DLC versions by path. Superseded versions are organized like old
// updates unless cleanup is enabled.
func organizationDlcFiles(game *db.SwitchGameFiles) []organizationDlcFile {
	files := make([]organizationDlcFile, 0, len(game.Dlc)+len(game.OlderDlc))
	for id, dlc := range game.Dlc {
		files = append(files, organizationDlcFile{id: id, file: dlc})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].id < files[j].id })
	older := make([]organizationDlcFile, 0, len(game.OlderDlc))
	for _, dlc := range game.OlderDlc {
		if dlc.Metadata == nil {
			continue
		}
		older = append(older, organizationDlcFile{id: strings.ToLower(dlc.Metadata.TitleId), file: dlc})
	}
	sort.Slice(older, func(i, j int) bool {
		return filepath.Join(older[i].file.ExtendedInfo.BaseFolder, older[i].file.ExtendedInfo.FileName) <
			filepath.Join(older[j].file.ExtendedInfo.BaseFolder, older[j].file.ExtendedInfo.FileName)
	})
	return append(files, older...)
}

func addSplitMoves(plan *OrganizationPlan, seenSources map[string]struct{}, firstPartName, sourceFolder, destinationFolder string) error {
	entries, err := os.ReadDir(sourceFolder)
	if err != nil {
		return fmt.Errorf("read split folder %q: %w", sourceFolder, err)
	}
	partPrefix := strings.TrimSuffix(firstPartName, "00")
	if partPrefix == firstPartName {
		return fmt.Errorf("split file %q does not end with part 00", firstPartName)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || len(name) < 2 || !strings.HasPrefix(name, partPrefix) {
			continue
		}
		if _, err := strconv.Atoi(name[len(name)-2:]); err != nil {
			continue
		}
		source := filepath.Join(sourceFolder, name)
		if err := addOrganizationMove(plan, seenSources, source, filepath.Join(destinationFolder, name)); err != nil {
			return err
		}
	}
	return nil
}

func addOrganizationMove(plan *OrganizationPlan, seenSources map[string]struct{}, source, destination string) error {
	sourceKey, err := organizationPathKey(source)
	if err != nil {
		return fmt.Errorf("resolve source %q: %w", source, err)
	}
	if _, exists := seenSources[sourceKey]; exists {
		return nil
	}
	seenSources[sourceKey] = struct{}{}
	if _, pending := plan.pendingCleanup[sourceKey]; pending {
		return nil
	}

	info, err := os.Lstat(source)
	if err != nil {
		plan.Conflicts = append(plan.Conflicts, OrganizationConflict{Source: source, Destination: destination, Reason: "source file is unavailable: " + err.Error()})
		return nil
	}
	if !info.Mode().IsRegular() {
		plan.Conflicts = append(plan.Conflicts, OrganizationConflict{Source: source, Destination: destination, Reason: "source is not a regular file"})
		return nil
	}
	if sameOrganizationPath(source, destination) {
		return nil
	}
	if blockedParent, parentErr := organizationDestinationParentConflict(destination); parentErr != nil {
		plan.Conflicts = append(plan.Conflicts, OrganizationConflict{Source: source, Destination: destination, OtherSource: blockedParent, Reason: "cannot use destination parent: " + parentErr.Error()})
		return nil
	} else if blockedParent != "" {
		plan.Conflicts = append(plan.Conflicts, OrganizationConflict{Source: source, Destination: destination, OtherSource: blockedParent, Reason: "destination parent is not a directory"})
		return nil
	}
	if destinationInfo, statErr := os.Lstat(destination); statErr == nil {
		if os.SameFile(info, destinationInfo) {
			return nil
		}
		destinationKey, keyErr := organizationPathKey(destination)
		index, pending := plan.pendingCleanup[destinationKey]
		if keyErr != nil || !pending {
			plan.Conflicts = append(plan.Conflicts, OrganizationConflict{Source: source, Destination: destination, OtherSource: destination, Reason: "destination already exists"})
			return nil
		}
		if plan.keepDuplicateInPlace(index, sourceKey, destinationKey) {
			return nil
		}
		// Plan validation checks that cleanup can free this destination first.
	} else if !os.IsNotExist(statErr) {
		plan.Conflicts = append(plan.Conflicts, OrganizationConflict{Source: source, Destination: destination, Reason: "cannot inspect destination: " + statErr.Error()})
		return nil
	}
	plan.Moves = append(plan.Moves, OrganizationMove{Source: source, Destination: destination, SourceSize: info.Size(), SourceStamp: info})
	return nil
}

// keepDuplicateInPlace reverses a cleanup decision when the file cleanup would
// delete already has the name organization gives the file it keeps, and the
// two are exact duplicates of the same size. Keeping the occupant avoids
// deleting it and then moving, or on some shares copying, the other file into
// its place. It reports whether the move of the kept file is no longer needed.
func (plan *OrganizationPlan) keepDuplicateInPlace(index int, sourceKey, destinationKey string) bool {
	candidate := plan.Cleanup[index]
	if !candidate.Interchangeable || candidate.ReasonCode != db.REASON_DUPLICATE || candidate.File.Size != candidate.Replacement.Size {
		return false
	}
	replacementKey, err := organizationPathKey(filepath.Join(candidate.Replacement.BaseFolder, candidate.Replacement.FileName))
	if err != nil || replacementKey != sourceKey {
		return false
	}
	plan.Cleanup[index] = db.CleanupCandidate{File: candidate.Replacement, Replacement: candidate.File, ReasonCode: db.REASON_DUPLICATE, Interchangeable: true}
	delete(plan.pendingCleanup, destinationKey)
	plan.pendingCleanup[sourceKey] = index
	// Other files deleted in favor of the source now rely on the occupant.
	for other := range plan.Cleanup {
		if other == index {
			continue
		}
		otherKey, err := organizationPathKey(filepath.Join(plan.Cleanup[other].Replacement.BaseFolder, plan.Cleanup[other].Replacement.FileName))
		if err == nil && otherKey == sourceKey {
			plan.Cleanup[other].Replacement = candidate.File
		}
	}
	return true
}

func organizationDestinationParentConflict(destination string) (string, error) {
	parent := filepath.Dir(destination)
	for {
		info, err := os.Stat(parent)
		if err == nil {
			if !info.IsDir() {
				return parent, nil
			}
			return "", nil
		}
		if !os.IsNotExist(err) {
			return parent, err
		}
		next := filepath.Dir(parent)
		if next == parent {
			return "", nil
		}
		parent = next
	}
}

func validateOrganizationPlan(plan *OrganizationPlan) {
	owners := make(map[string]OrganizationMove, len(plan.Moves))
	sources := make(map[string]OrganizationMove, len(plan.Moves))
	for _, move := range plan.Moves {
		key, err := organizationPathKey(move.Source)
		if err == nil {
			sources[key] = move
		}
	}
	for _, move := range plan.Moves {
		destinationKey, err := organizationPathKey(move.Destination)
		if err != nil {
			plan.Conflicts = append(plan.Conflicts, OrganizationConflict{Source: move.Source, Destination: move.Destination, Reason: "cannot resolve destination: " + err.Error()})
			continue
		}
		if prior, exists := owners[destinationKey]; exists && !sameOrganizationPath(prior.Source, move.Source) {
			plan.Conflicts = append(plan.Conflicts,
				OrganizationConflict{Source: prior.Source, Destination: move.Destination, OtherSource: move.Source, Reason: "multiple files have the same destination"},
				OrganizationConflict{Source: move.Source, Destination: move.Destination, OtherSource: prior.Source, Reason: "multiple files have the same destination"},
			)
		} else {
			owners[destinationKey] = move
		}
		if occupant, exists := sources[destinationKey]; exists && !sameOrganizationPath(occupant.Source, move.Source) {
			plan.Conflicts = append(plan.Conflicts, OrganizationConflict{Source: move.Source, Destination: move.Destination, OtherSource: occupant.Source, Reason: "destination belongs to another planned source"})
		}
		if cleanupIndex, exists := plan.pendingCleanup[destinationKey]; exists {
			if _, waitsForImport := plan.cleanupAfterImport[cleanupIndex]; waitsForImport {
				plan.Conflicts = append(plan.Conflicts, OrganizationConflict{Source: move.Source, Destination: move.Destination, OtherSource: move.Destination, Reason: "destination holds a file kept until its replacement is imported"})
			}
		}
	}
	plan.Conflicts = uniqueOrganizationConflicts(plan.Conflicts)
}

func uniqueOrganizationConflicts(conflicts []OrganizationConflict) []OrganizationConflict {
	seen := map[string]struct{}{}
	result := make([]OrganizationConflict, 0, len(conflicts))
	for _, conflict := range conflicts {
		key := strings.Join([]string{conflict.Source, conflict.Destination, conflict.OtherSource, conflict.Reason}, "\x00")
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, conflict)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Destination != result[j].Destination {
			return result[i].Destination < result[j].Destination
		}
		return result[i].Source < result[j].Source
	})
	return result
}

func organizationPathKey(path string) (string, error) {
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		abs = strings.ToLower(abs)
	}
	return abs, nil
}

func sameOrganizationPath(left, right string) bool {
	leftKey, leftErr := organizationPathKey(left)
	rightKey, rightErr := organizationPathKey(right)
	if leftErr == nil && rightErr == nil && leftKey == rightKey {
		return true
	}
	leftInfo, leftStatErr := os.Stat(left)
	rightInfo, rightStatErr := os.Stat(right)
	return leftStatErr == nil && rightStatErr == nil && os.SameFile(leftInfo, rightInfo)
}

func organizationPathWithin(root, candidate string) (bool, error) {
	rootKey, err := organizationPathKey(root)
	if err != nil {
		return false, fmt.Errorf("resolve library folder %q: %w", root, err)
	}
	candidateKey, err := organizationPathKey(candidate)
	if err != nil {
		return false, fmt.Errorf("resolve path %q: %w", candidate, err)
	}
	if organizationPathIsWithin(rootKey, candidateKey) {
		return true, nil
	}
	rootHasSymlink, err := organizationPathHasSymlink(rootKey)
	if err != nil {
		return false, fmt.Errorf("inspect library folder path: %w", err)
	}
	candidateHasSymlink, err := organizationPathHasSymlink(candidateKey)
	if err != nil {
		return false, fmt.Errorf("inspect source folder path: %w", err)
	}
	if !rootHasSymlink && !candidateHasSymlink {
		return false, nil
	}
	resolvedRoot, err := resolveOrganizationPath(rootKey)
	if err != nil {
		return false, fmt.Errorf("resolve library folder %q: %w", root, err)
	}
	resolvedCandidate, err := resolveOrganizationPath(candidateKey)
	if err != nil {
		return false, fmt.Errorf("resolve path %q: %w", candidate, err)
	}
	resolvedRoot, err = organizationPathKey(resolvedRoot)
	if err != nil {
		return false, err
	}
	resolvedCandidate, err = organizationPathKey(resolvedCandidate)
	if err != nil {
		return false, err
	}
	return organizationPathIsWithin(resolvedRoot, resolvedCandidate), nil
}

func organizationPathIsWithin(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	if err != nil {
		// A different drive or volume is necessarily outside the library root.
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

func organizationPathHasSymlink(path string) (bool, error) {
	volume := filepath.VolumeName(path)
	current := volume + string(filepath.Separator)
	remainder := strings.TrimPrefix(path, current)
	for _, part := range strings.Split(remainder, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return true, nil
		}
	}
	return false, nil
}

func resolveOrganizationPath(path string) (string, error) {
	absolute, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("make path absolute: %w", err)
	}
	absolute = filepath.Clean(absolute)
	hasSymlink, err := organizationPathHasSymlink(absolute)
	if err != nil {
		return "", fmt.Errorf("inspect path for symbolic links: %w", err)
	}
	if !hasSymlink {
		return absolute, nil
	}
	missing := []string{}
	for current := absolute; ; current = filepath.Dir(current) {
		resolved, resolveErr := filepath.EvalSymlinks(current)
		if resolveErr == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(resolveErr, os.ErrNotExist) {
			return "", resolveErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", resolveErr
		}
		missing = append(missing, filepath.Base(current))
	}
}

// ExecuteOrganizationPlan applies a previously validated plan and refuses to
// replace destinations that appear after preflight. Cleanup runs before moves
// when the replacement is already in the library, or after the replacement
// import succeeds. Empty-folder cleanup runs
// inside baseFolder only; scanFolders nested inside it are kept.
func ExecuteOrganizationPlan(plan OrganizationPlan, localDB *db.LocalSwitchFilesDB, baseFolder string, options settings.OrganizeOptions, progress db.ProgressUpdater, scanFolders ...string) (retErr error) {
	completed, deleted := 0, 0
	defer func() {
		if retErr != nil {
			zap.L().Error("Organization failed",
				zap.String("library_root", baseFolder),
				zap.Int("completed_moves", completed),
				zap.Int("planned_moves", len(plan.Moves)),
				zap.Int("deleted_files", deleted),
				zap.Int("planned_deletions", len(plan.Cleanup)),
				zap.Error(retErr),
			)
		}
	}()
	if len(plan.Conflicts) > 0 {
		return &OrganizationConflictsError{Conflicts: plan.Conflicts}
	}
	beforeMoves := plan.Cleanup
	var afterImports map[int][]db.CleanupCandidate
	if len(plan.cleanupAfterImport) > 0 {
		beforeMoves = make([]db.CleanupCandidate, 0, len(plan.Cleanup)-len(plan.cleanupAfterImport))
		afterImports = make(map[int][]db.CleanupCandidate)
		for index, candidate := range plan.Cleanup {
			if moveIndex, waitsForImport := plan.cleanupAfterImport[index]; waitsForImport {
				afterImports[moveIndex] = append(afterImports[moveIndex], candidate)
			} else {
				beforeMoves = append(beforeMoves, candidate)
			}
		}
	}
	var err error
	deleted, err = deleteCleanupCandidates(beforeMoves, localDB, progress)
	if err != nil {
		return err
	}
	for i, move := range plan.Moves {
		current, err := os.Lstat(move.Source)
		if err != nil {
			return fmt.Errorf("inspect source %q before move: %w", move.Source, err)
		}
		if move.SourceStamp != nil && (!os.SameFile(current, move.SourceStamp) || current.Size() != move.SourceSize || !current.ModTime().Equal(move.SourceStamp.ModTime())) {
			return fmt.Errorf("source %q changed after organization preflight", move.Source)
		}
		cleanup := afterImports[i]
		for _, candidate := range cleanup {
			if !matchesScannedCleanupFile(current, candidate.Replacement) {
				return fmt.Errorf("cleanup replacement %q changed after scanning", move.Source)
			}
		}
		if err := os.MkdirAll(filepath.Dir(move.Destination), os.ModePerm); err != nil {
			return fmt.Errorf("create destination folder for %q: %w", move.Destination, err)
		}
		if progress != nil {
			progress.UpdateProgress(i+1, len(plan.Moves)+1, move.Source)
		}
		if err := moveOrganizationFile(move); err != nil {
			return fmt.Errorf("move %q to %q: %w", move.Source, move.Destination, err)
		}
		completed++
		if err := updateLocalFilePaths(localDB, move.Source, move.Destination); err != nil {
			return err
		}
		zap.S().Infof("Moved file: %s -> %s", move.Source, move.Destination)
		if len(cleanup) > 0 {
			n, err := deleteAfterOrganizationImport(cleanup, move.Destination, localDB, progress)
			deleted += n
			if err != nil {
				return err
			}
		}
	}
	if len(plan.Cleanup) > 0 && localDB != nil {
		localDB.CleanupCandidates = nil
	}
	if completed == 0 && deleted == 0 {
		return nil
	}
	if options.DeleteEmptyFolders {
		if err := deleteEmptyOrganizationFolders(baseFolder, scanFolders); err != nil {
			return fmt.Errorf("delete empty folders: %w", err)
		}
	}
	if progress != nil {
		progress.UpdateProgress(len(plan.Moves)+1, len(plan.Moves)+1, "done")
	}
	return nil
}

func deleteAfterOrganizationImport(candidates []db.CleanupCandidate, destination string, localDB *db.LocalSwitchFilesDB, progress db.ProgressUpdater) (int, error) {
	info, err := os.Lstat(destination)
	if err != nil {
		return 0, fmt.Errorf("inspect imported cleanup replacement %q: %w", destination, err)
	}
	for index := range candidates {
		replacement := &candidates[index].Replacement
		if !info.Mode().IsRegular() || info.Size() != replacement.Size || !modTimesWithin(info.ModTime(), replacement.ModTime, exclusiveDestinationTimeTolerance) {
			return 0, fmt.Errorf("imported cleanup replacement %q does not match the scanned file", destination)
		}
		replacement.BaseFolder = filepath.Dir(destination)
		replacement.FileName = filepath.Base(destination)
		replacement.ModTime = info.ModTime()
	}
	return deleteCleanupCandidates(candidates, localDB, progress)
}

func updateLocalFilePaths(localDB *db.LocalSwitchFilesDB, source, destination string) error {
	if localDB == nil {
		return nil
	}
	replacement, err := os.Stat(destination)
	if err != nil {
		return fmt.Errorf("inspect moved file at %q: %w", destination, err)
	}
	newInfo := db.ExtendedFileInfo{BaseFolder: filepath.Dir(destination), FileName: filepath.Base(destination), Size: replacement.Size(), ModTime: replacement.ModTime()}
	update := func(info *db.ExtendedFileInfo) {
		if info != nil && sameOrganizationPath(filepath.Join(info.BaseFolder, info.FileName), source) {
			*info = newInfo
		}
	}
	for _, game := range localDB.TitlesMap {
		if game == nil {
			continue
		}
		update(&game.File.ExtendedInfo)
		for version, item := range game.Updates {
			update(&item.ExtendedInfo)
			game.Updates[version] = item
		}
		for id, item := range game.Dlc {
			update(&item.ExtendedInfo)
			game.Dlc[id] = item
		}
		for i := range game.OlderDlc {
			update(&game.OlderDlc[i].ExtendedInfo)
		}
	}
	return nil
}

// OrganizeByFolders plans every move before applying any of them.
func OrganizeByFolders(baseFolder string, localDB *db.LocalSwitchFilesDB, titlesDB *db.SwitchTitlesDB, progress db.ProgressUpdater) error {
	settingsObj, err := settings.ReadSettings(baseFolder)
	if err != nil {
		return fmt.Errorf("read settings before organizing files: %w", err)
	}
	libraryRoot := settingsObj.Paths.LibraryFolder
	if libraryRoot == "" {
		libraryRoot = baseFolder
	}
	plan, err := BuildOrganizationPlan(libraryRoot, localDB, titlesDB, settingsObj.Organization)
	if err != nil {
		return err
	}
	return ExecuteOrganizationPlan(plan, localDB, libraryRoot, settingsObj.Organization, progress, settingsObj.Paths.ScanFolders...)
}
