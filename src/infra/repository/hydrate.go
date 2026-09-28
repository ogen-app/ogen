package repository

import (
	"context"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// HydrateTags populates the Tags field on each item in items.
// getIDs extracts the tag ID list from an item; getTags returns a pointer to
// the Tags slice on the item so it can be initialised and appended to.
func HydrateTags[T any](
	ctx context.Context,
	tagRepo TagRepository,
	items []T,
	getIDs func(T) []string,
	getTags func(*T) *[]models.Tag,
) error {
	for i := range items {
		*getTags(&items[i]) = []models.Tag{}
	}
	ids := collectIDsFlat(items, getIDs)
	tagByID, err := tagRepo.GetByIDs(ctx, ids)
	if err != nil {
		return err
	}
	for i := range items {
		for _, id := range getIDs(items[i]) {
			if t, ok := tagByID[id]; ok {
				*getTags(&items[i]) = append(*getTags(&items[i]), t)
			}
		}
	}
	return nil
}

// collectIDs returns a deduplicated slice of IDs extracted one-per-item.
func collectIDs[T any](items []T, getID func(T) string) []string {
	seen := make(map[string]struct{})
	var ids []string
	for _, item := range items {
		id := getID(item)
		if _, ok := seen[id]; !ok {
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}
	return ids
}

// collectIDsPtr returns a deduplicated slice of non-nil IDs extracted from nullable pointer fields.
func collectIDsPtr[T any](items []T, getID func(T) *string) []string {
	seen := make(map[string]struct{})
	var ids []string
	for _, item := range items {
		if ptr := getID(item); ptr != nil {
			if _, ok := seen[*ptr]; !ok {
				seen[*ptr] = struct{}{}
				ids = append(ids, *ptr)
			}
		}
	}
	return ids
}

// collectIDsFlat returns a deduplicated slice of IDs extracted from a per-item slice.
func collectIDsFlat[T any](items []T, getIDs func(T) []string) []string {
	seen := make(map[string]struct{})
	var ids []string
	for _, item := range items {
		for _, id := range getIDs(item) {
			if _, ok := seen[id]; !ok {
				seen[id] = struct{}{}
				ids = append(ids, id)
			}
		}
	}
	return ids
}

// fetchByIDs fetches records of type T whose id column matches any value in ids
// and returns an ID→*T index map. Returns an empty map (no error) when ids is empty.
func fetchByIDs[T any](
	ctx context.Context,
	db *bun.DB,
	ids []string,
	getID func(*T) string,
) (map[string]*T, error) {
	result := make(map[string]*T)
	if len(ids) == 0 {
		return result, nil
	}
	var items []T
	if err := db.NewSelect().Model(&items).Where("id IN (?)", bun.List(ids)).Scan(ctx); err != nil {
		return nil, err
	}
	for i := range items {
		result[getID(&items[i])] = &items[i]
	}
	return result, nil
}
