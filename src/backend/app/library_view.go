package app

import (
	"cmp"
	"path/filepath"
	"slices"

	"github.com/trembon/switch-library-manager/backend/db"
)

func buildLocalLibraryData(localDB *db.LocalSwitchFilesDB, switchDB *db.SwitchTitlesDB, allowUnicode bool) LocalLibraryData {
	response := LocalLibraryData{
		LibraryData: make([]LibraryTemplateData, 0),
		Issues:      make([]Pair, 0),
	}
	issues := make([]libraryIssue, 0)
	for key, value := range localDB.TitlesMap {
		if value.BaseExist {
			version := ""
			titleID := ""
			if value.File.Metadata != nil {
				titleID = value.File.Metadata.TitleId
			}
			var title *db.SwitchTitle
			if switchDB != nil {
				title = switchDB.TitlesMap[key]
			}
			name := db.ResolveGameTitleName(title, value, allowUnicode)
			if value.File.Metadata != nil && value.File.Metadata.Ncap != nil {
				version = value.File.Metadata.Ncap.DisplayVersion
			}

			if latest, ok := value.Updates[value.LatestUpdate]; ok && latest.Metadata != nil && latest.Metadata.Ncap != nil && latest.Metadata.Ncap.DisplayVersion != "" {
				version = latest.Metadata.Ncap.DisplayVersion
			}

			if title != nil {
				response.LibraryData = append(response.LibraryData, LibraryTemplateData{
					Icon:    title.Attributes.IconUrl,
					Name:    name,
					TitleId: title.Attributes.Id,
					Update:  value.LatestUpdate,
					Version: version,
					Region:  title.Attributes.Region,
					Type:    getType(value),
					Path:    filepath.Join(value.File.ExtendedInfo.BaseFolder, value.File.ExtendedInfo.FileName),
				})
			} else {
				response.LibraryData = append(response.LibraryData, LibraryTemplateData{
					Name:    name,
					Update:  value.LatestUpdate,
					Version: version,
					Type:    getType(value),
					TitleId: titleID,
					Path:    filepath.Join(value.File.ExtendedInfo.BaseFolder, value.File.ExtendedInfo.FileName),
				})
			}
		} else {
			for _, update := range value.Updates {
				issues = append(issues, libraryIssue{
					rank:  issueRankMissingBase,
					path:  filepath.Join(update.ExtendedInfo.BaseFolder, update.ExtendedInfo.FileName),
					issue: "base file is missing",
				})
			}
			for _, dlc := range value.Dlc {
				issues = append(issues, libraryIssue{
					rank:  issueRankMissingBase,
					path:  filepath.Join(dlc.ExtendedInfo.BaseFolder, dlc.ExtendedInfo.FileName),
					issue: "base file is missing",
				})
			}
		}
	}
	for key, skipped := range localDB.Skipped {
		issues = append(issues, libraryIssue{
			rank:  skippedIssueRank(skipped.ReasonCode),
			path:  filepath.Join(key.BaseFolder, key.FileName),
			issue: skipped.ReasonText,
		})
	}
	// Map iteration order is random, so give the Issues tab a stable order:
	// grouped by issue type, then by file path.
	slices.SortFunc(issues, func(a, b libraryIssue) int {
		return cmp.Or(
			cmp.Compare(a.rank, b.rank),
			cmp.Compare(a.path, b.path),
			cmp.Compare(a.issue, b.issue),
		)
	})
	for _, issue := range issues {
		response.Issues = append(response.Issues, Pair{Key: issue.path, Value: issue.issue})
	}
	response.NumFiles = localDB.NumFiles
	return response
}

// Issue types in the order they appear on the Issues tab.
const (
	issueRankMissingBase = iota
	issueRankDuplicate
	issueRankOldVersion
	issueRankMalformed
	issueRankUnrecognised
	issueRankUnsupported
	issueRankOther
)

type libraryIssue struct {
	rank  int
	path  string
	issue string
}

func skippedIssueRank(reasonCode int) int {
	switch reasonCode {
	case db.REASON_DUPLICATE:
		return issueRankDuplicate
	case db.REASON_OLD_UPDATE:
		return issueRankOldVersion
	case db.REASON_MALFORMED_FILE:
		return issueRankMalformed
	case db.REASON_UNRECOGNISED:
		return issueRankUnrecognised
	case db.REASON_UNSUPPORTED_TYPE:
		return issueRankUnsupported
	default:
		return issueRankOther
	}
}

func getType(gameFile *db.SwitchGameFiles) string {
	if gameFile.IsSplit {
		return "split"
	}
	if gameFile.MultiContent {
		return "multi-content"
	}
	ext := filepath.Ext(gameFile.File.ExtendedInfo.FileName)
	if len(ext) > 1 {
		return ext[1:]
	}
	return ""
}
