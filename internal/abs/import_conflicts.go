package abs

import (
	"context"
	"strings"

	"github.com/vavallee/bindery/internal/models"
	"github.com/vavallee/bindery/internal/textutil"
)

func (i *Importer) applyConflictField(
	ctx context.Context,
	cfg ImportConfig,
	item NormalizedLibraryItem,
	entityType string,
	localID int64,
	fieldName, absValue, upstreamValue string,
	apply func(string) error,
	currentValue func() string,
) (metadataMergeResult, bool, error) {
	result, changed, conflict, err := i.planConflictField(ctx, cfg, item, entityType, localID, fieldName, absValue, upstreamValue, apply, currentValue)
	if err != nil {
		return metadataMergeResult{}, false, err
	}
	if conflict != nil {
		if err := i.conflicts.Upsert(ctx, conflict); err != nil {
			return metadataMergeResult{}, false, err
		}
	}
	return result, changed, nil
}

// planConflictField is applyConflictField without the write to the conflicts
// table: it applies the chosen value through apply and returns the conflict
// row to record, if any, so a caller whose entity write can lose a guard
// records it only once that write lands (#2926).
func (i *Importer) planConflictField(
	ctx context.Context,
	cfg ImportConfig,
	item NormalizedLibraryItem,
	entityType string,
	localID int64,
	fieldName, absValue, upstreamValue string,
	apply func(string) error,
	currentValue func() string,
) (metadataMergeResult, bool, *models.ABSMetadataConflict, error) {
	result := metadataMergeResult{}
	if apply == nil || currentValue == nil {
		return result, false, nil, nil
	}
	normABS := normalizeConflictValue(fieldName, absValue)
	normUpstream := normalizeConflictValue(fieldName, upstreamValue)
	existing := (*models.ABSMetadataConflict)(nil)
	if i.conflicts != nil {
		conflict, err := i.conflicts.GetByEntityField(ctx, entityType, localID, fieldName)
		if err != nil {
			return result, false, nil, err
		}
		existing = conflict
	}

	chosenSource := ""
	chosenValue := ""
	preferredSource := ""
	resolutionStatus := conflictStatusResolved
	shouldPersist := existing != nil
	if existing != nil && existing.PreferredSource != "" {
		preferredSource = existing.PreferredSource
	}

	switch {
	case normABS == "" && normUpstream == "":
		return result, false, nil, nil
	case normABS == "":
		chosenSource = MetadataSourceUpstream
		chosenValue = upstreamValue
		result.AutoResolved++
	case normUpstream == "":
		chosenSource = MetadataSourceABS
		chosenValue = absValue
		result.AutoResolved++
	case normABS == normUpstream:
		chosenSource = MetadataSourceUpstream
		chosenValue = upstreamValue
		if strings.TrimSpace(chosenValue) == "" {
			chosenSource = MetadataSourceABS
			chosenValue = absValue
		}
		result.AutoResolved++
	default:
		shouldPersist = true
		chosenSource = MetadataSourceUpstream
		chosenValue = upstreamValue
		resolutionStatus = conflictStatusUnresolved
		if existing != nil && existing.PreferredSource != "" {
			preferredSource = existing.PreferredSource
			chosenSource = preferredSource
			if preferredSource == MetadataSourceABS {
				chosenValue = absValue
			} else {
				chosenValue = upstreamValue
			}
			resolutionStatus = conflictStatusResolved
			result.AutoResolved++
		} else {
			result.Conflicts++
		}
	}

	changed := normalizeConflictValue(fieldName, currentValue()) != normalizeConflictValue(fieldName, chosenValue)
	if changed {
		if err := apply(chosenValue); err != nil {
			return metadataMergeResult{}, false, nil, err
		}
	}
	if !shouldPersist || i.conflicts == nil {
		return result, changed, nil, nil
	}
	conflict := &models.ABSMetadataConflict{
		SourceID:         cfg.SourceID,
		LibraryID:        item.LibraryID,
		ItemID:           item.ItemID,
		EntityType:       entityType,
		LocalID:          localID,
		FieldName:        fieldName,
		ABSValue:         absValue,
		UpstreamValue:    upstreamValue,
		AppliedSource:    chosenSource,
		PreferredSource:  preferredSource,
		ResolutionStatus: resolutionStatus,
	}
	return result, changed, conflict, nil
}

func bookABSCandidateValue(book *models.Book, item NormalizedLibraryItem, field string) string {
	switch field {
	case "description":
		if desc := textutil.CleanDescription(item.Description); desc != "" {
			return desc
		}
	case "release_date":
		if date := formatConflictDate(parseABSDate(item.PublishedDate, item.PublishedYear)); date != "" {
			return date
		}
	case "language":
		if lang := normalizeLanguage(item.Language); lang != "" {
			return lang
		}
	}
	return SerializeBookConflictValue(book, field)
}
