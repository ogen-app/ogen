package ideas

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// MaxMergeIDs caps the ideas one merge folds into the kept one.
const MaxMergeIDs = 50

// Merge refusals. IsValidation counts them, so the handler answers 400.
var (
	ErrMergeSize     = errors.New("merge_ids must name 1 to 50 ideas")
	ErrMergeSelf     = errors.New("the kept idea cannot be merged into itself")
	ErrMergeScope    = errors.New("ideas can only be merged into an idea of the same campaign")
	ErrMergeCompiled = errors.New("an idea a post was made from cannot be merged")
)

// MergeResult is a done merge: the kept idea as it is now, and the ids of
// the ideas merged into it and deleted.
type MergeResult struct {
	Idea    *models.Idea
	Deleted []string
}

// CheckMerge validates a merge without making it and returns the idea it
// would keep, as it is now.
func (s *Service) CheckMerge(ctx context.Context, keepID string, mergeIDs []string) (*models.Idea, error) {
	keepID, mergeIDs, err := mergeArgs(keepID, mergeIDs)
	if err != nil {
		return nil, err
	}
	set, err := s.repo.MergeSet(ctx, append([]string{keepID}, mergeIDs...))
	if err != nil {
		return nil, err
	}
	m, err := planMerge(set, keepID, mergeIDs)
	if err != nil {
		return nil, err
	}
	return m.Keep, nil
}

// Merge folds the ideas of mergeIDs into keepID's in one transaction and
// deletes them. The kept idea keeps its title, verdict, plan and campaign;
// it gains every asset they drew on (its own first, then theirs in order),
// grounded when any of them was, and each of their notes its own note does
// not already hold, appended in order while they fit. An analysis idea's
// note is its analysis's, so it gains no notes. Every idea must be in the
// kept one's campaign scope, and none may have had a post made from it.
func (s *Service) Merge(ctx context.Context, keepID string, mergeIDs []string) (*MergeResult, error) {
	keepID, mergeIDs, err := mergeArgs(keepID, mergeIDs)
	if err != nil {
		return nil, err
	}
	now := s.stamp()
	err = s.repo.MergeIdeas(ctx, append([]string{keepID}, mergeIDs...), func(set repository.IdeaMergeSet) (repository.IdeaMerge, error) {
		m, err := planMerge(set, keepID, mergeIDs)
		if err == nil {
			m.Keep.UpdatedAt = now
		}
		return m, err
	})
	if err != nil {
		return nil, err
	}
	idea, err := s.Get(ctx, keepID)
	if err != nil {
		return nil, err
	}
	return &MergeResult{Idea: idea, Deleted: mergeIDs}, nil
}

// mergeArgs trims and dedupes the ids of a merge and checks its shape.
func mergeArgs(keepID string, mergeIDs []string) (string, []string, error) {
	keepID = strings.TrimSpace(keepID)
	var ids []string
	for _, id := range mergeIDs {
		if id = strings.TrimSpace(id); id != "" && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	switch {
	case keepID == "":
		return "", nil, ErrNotFound
	case len(ids) == 0 || len(ids) > MaxMergeIDs:
		return "", nil, ErrMergeSize
	case slices.Contains(ids, keepID):
		return "", nil, ErrMergeSelf
	}
	return keepID, ids, nil
}

// planMerge checks a merge against the ideas it names and works out the
// kept idea's new columns.
func planMerge(set repository.IdeaMergeSet, keepID string, mergeIDs []string) (repository.IdeaMerge, error) {
	byID := make(map[string]models.Idea, len(set.Ideas))
	for _, idea := range set.Ideas {
		byID[idea.ID] = idea
	}
	keep, ok := byID[keepID]
	if !ok {
		return repository.IdeaMerge{}, ErrNotFound
	}
	merged := make([]models.Idea, 0, len(mergeIDs))
	for _, id := range mergeIDs {
		idea, ok := byID[id]
		if !ok {
			return repository.IdeaMerge{}, ErrNotFound
		}
		if !sameCampaign(idea.CampaignID, keep.CampaignID) {
			return repository.IdeaMerge{}, ErrMergeScope
		}
		merged = append(merged, idea)
	}
	if len(set.Compiled) > 0 {
		return repository.IdeaMerge{}, ErrMergeCompiled
	}
	return repository.IdeaMerge{Keep: &keep, Columns: foldInto(&keep, merged), Delete: mergeIDs}, nil
}

// foldInto applies merged to keep and returns the columns it changed.
func foldInto(keep *models.Idea, merged []models.Idea) []string {
	var columns []string
	assets := slices.Clone(keep.AssetIDs)
	grounded := keep.GroundingMode != nil && *keep.GroundingMode == models.GroundingGrounded
	note := keep.Note
	for _, idea := range merged {
		for _, a := range idea.AssetIDs {
			if !slices.Contains(assets, a) {
				assets = append(assets, a)
			}
		}
		grounded = grounded || (idea.GroundingMode != nil && *idea.GroundingMode == models.GroundingGrounded)
		if keep.Origin != models.IdeaOriginAssetAnalysis {
			note = appendNote(note, idea.Note)
		}
	}
	if len(assets) != len(keep.AssetIDs) {
		keep.AssetIDs = assets
		columns = append(columns, "asset_ids")
	}
	if grounded && (keep.GroundingMode == nil || *keep.GroundingMode != models.GroundingGrounded) {
		keep.GroundingMode = new(models.GroundingGrounded)
		columns = append(columns, "grounding_mode")
	}
	if note != keep.Note {
		keep.Note = note
		columns = append(columns, "note")
	}
	return columns
}

// appendNote adds extra to note as a new paragraph unless note already
// holds it (ignoring case and spacing) or the result would pass MaxNoteLen.
func appendNote(note, extra string) string {
	extra = strings.TrimSpace(extra)
	if extra == "" || strings.Contains(squash(note), squash(extra)) {
		return note
	}
	next := extra
	if note != "" {
		next = note + "\n\n" + extra
	}
	if len([]rune(next)) > MaxNoteLen {
		return note
	}
	return next
}

// squash lowercases s and collapses its whitespace.
func squash(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}
