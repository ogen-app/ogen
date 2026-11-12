package packs

import (
	"context"
	"errors"
	"fmt"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/usecase/ideas"
)

// maxDuplicateGroups caps the groups one findDuplicateIdeas call returns.
const maxDuplicateGroups = 20

// defineMergeTools defines findDuplicateIdeas and mergeIdeas.
func defineMergeTools(g *genkit.Genkit, d Deps) []assistantkit.Spec {
	find := genkit.DefineTool(g, "findDuplicateIdeas",
		"Finds groups of open ideas that read as the same idea, in this thread's campaign or among the workspace-wide ideas, each with a suggested idea to keep.",
		assistantkit.Wrap(recorded(ideasPackName, "findDuplicateIdeas", nil, func(ctx context.Context, in FindDuplicateIdeasInput) (*FindDuplicateIdeasOutput, error) {
			return findDuplicateIdeas(ctx, d, in)
		})), ai.WithStrictSchema(false))
	merge := genkit.DefineTool(g, "mergeIdeas",
		"Merges ideas into one kept idea: it gains their assets and notes, and they are deleted. Always offered for the user to confirm first.",
		assistantkit.Gated("mergeIdeas", assistantkit.Gate[MergeIdeasInput]{
			Precheck: func(ctx context.Context, in MergeIdeasInput) error {
				groups, err := mergeGroups(in)
				if err != nil {
					return err
				}
				for _, g := range groups {
					if _, err := d.Ideas.CheckMerge(ctx, g.KeepID, g.MergeIDs); err != nil {
						return mergeErr(err)
					}
				}
				return nil
			},
			Describe: func(ctx context.Context, in MergeIdeasInput) string {
				groups, _ := mergeGroups(in)
				if len(groups) != 1 {
					n := 0
					for _, g := range groups {
						n += len(g.MergeIDs) + 1
					}
					return fmt.Sprintf("Merge %d groups: %d ideas into %d", len(groups), n, len(groups))
				}
				n := len(groups[0].MergeIDs) + 1
				if keep, err := d.Ideas.Get(ctx, groups[0].KeepID); err == nil {
					return fmt.Sprintf("Merge %d ideas into “%s”", n, keep.Title)
				}
				return fmt.Sprintf("Merge %d ideas into one", n)
			},
		}, assistantkit.Wrap(recorded(ideasPackName, "mergeIdeas", nil, func(ctx context.Context, in MergeIdeasInput) (*MergeIdeasOutput, error) {
			return mergeIdeas(ctx, d, in)
		}))), ai.WithStrictSchema(false))
	return []assistantkit.Spec{
		spec(ideasPackName, find, assistantkit.TierRead,
			`Call this when the user asks about duplicate, repeated or overlapping ideas — "do I have the same idea twice?", "clean up my ideas". It compares open ideas (not turned down, not made into a post) of this thread's campaign, or the workspace-wide ideas outside a campaign. Each group's keepId is the suggested idea to keep; offer mergeIdeas for the groups the user wants merged.`),
		spec(ideasPackName, merge, assistantkit.TierProposal,
			`Call this to merge duplicate ideas — "merge these", "keep the first one", "merge both pairs". keepId is the idea to keep (default the group's keepId from findDuplicateIdeas); mergeIds are the others (1 to 50). To merge several groups at once — "merge them all" after findDuplicateIdeas — pass groups instead, one {keepId, mergeIds} per group, in one call so the user confirms them together. Each group's ideas must share a campaign and none may have been made into a post. Always offered first; the merged ideas are deleted and it can't be undone.`),
	}
}

// FindDuplicateIdeasInput asks for duplicate groups.
type FindDuplicateIdeasInput struct {
	Limit int `json:"limit,omitempty" jsonschema:"description=How many groups to return, largest first; at most 20"`
}

// DuplicateGroupRef is one group of near-duplicate ideas, its keepId first.
type DuplicateGroupRef struct {
	KeepID     string    `json:"keepId"`
	Similarity float64   `json:"similarity"`
	Ideas      []IdeaRef `json:"ideas"`
}

// FindDuplicateIdeasOutput is the groups found.
type FindDuplicateIdeasOutput struct {
	Groups []DuplicateGroupRef `json:"groups"`
	// Total counts every group found, Truncated that only the newest ideas
	// were compared.
	Total     int  `json:"total"`
	Truncated bool `json:"truncated,omitempty"`
}

func findDuplicateIdeas(ctx context.Context, d Deps, in FindDuplicateIdeasInput) (*FindDuplicateIdeasOutput, error) {
	var campaignID *string
	if id := assistant.CampaignID(ctx); id != "" {
		campaignID = &id
	}
	res, err := d.Ideas.Duplicates(ctx, campaignID)
	if errors.Is(err, ideas.ErrScopeNotFound) {
		return nil, assistantkit.Refused("this thread's campaign is gone")
	}
	if err != nil {
		return nil, err
	}
	limit := maxDuplicateGroups
	if in.Limit > 0 {
		limit = min(in.Limit, maxDuplicateGroups)
	}
	out := &FindDuplicateIdeasOutput{Groups: []DuplicateGroupRef{}, Total: len(res.Groups), Truncated: res.Truncated}
	for _, g := range res.Groups[:min(len(res.Groups), limit)] {
		ref := DuplicateGroupRef{KeepID: g.KeepID, Similarity: g.Similarity, Ideas: make([]IdeaRef, len(g.Ideas))}
		for i, m := range g.Ideas {
			ref.Ideas[i] = ideaRef(m.Idea)
		}
		out.Groups = append(out.Groups, ref)
	}
	return out, nil
}

// MergeIdeasInput merges ideas into one, or each group of them into one.
type MergeIdeasInput struct {
	KeepID   string       `json:"keepId,omitempty"   jsonschema:"description=The idea to keep"`
	MergeIDs []string     `json:"mergeIds,omitempty" jsonschema:"description=The ideas to merge into it and delete; 1 to 50"`
	Groups   []MergeGroup `json:"groups,omitempty"   jsonschema:"description=Several merges at once, instead of keepId and mergeIds; at most 20"`
}

// MergeGroup is one merge: the idea kept and the ideas merged into it.
type MergeGroup struct {
	KeepID   string   `json:"keepId"`
	MergeIDs []string `json:"mergeIds"`
}

// mergeGroups is in as its groups: keepId and mergeIds first, then groups.
// An idea may be in one group only, kept or merged.
func mergeGroups(in MergeIdeasInput) ([]MergeGroup, error) {
	var groups []MergeGroup
	if in.KeepID != "" || len(in.MergeIDs) > 0 {
		groups = append(groups, MergeGroup{KeepID: in.KeepID, MergeIDs: in.MergeIDs})
	}
	groups = append(groups, in.Groups...)
	switch {
	case len(groups) == 0:
		return nil, assistantkit.Refused("which ideas? pass keepId and mergeIds, or groups")
	case len(groups) > maxDuplicateGroups:
		return nil, assistantkit.Refused(fmt.Sprintf("at most %d groups at once", maxDuplicateGroups))
	}
	seen := map[string]bool{}
	for _, g := range groups {
		for _, id := range append([]string{g.KeepID}, dedupe(g.MergeIDs)...) {
			if seen[id] {
				return nil, assistantkit.Refused("an idea can be in one merge only; " + id + " is in two")
			}
			seen[id] = true
		}
	}
	return groups, nil
}

// MergeIdeasOutput is each kept idea and the ideas merged into it. Idea and
// Deleted are the first group's, for a call that merged one group.
type MergeIdeasOutput struct {
	Idea    IdeaRef       `json:"idea"`
	Deleted []string      `json:"deleted"`
	Groups  []MergedGroup `json:"groups"`
	// Stopped is why the merges stopped part way; the groups after it were
	// not merged.
	Stopped string `json:"stopped,omitempty"`
}

// MergedGroup is one merge done.
type MergedGroup struct {
	Idea    IdeaRef  `json:"idea"`
	Deleted []string `json:"deleted"`
}

func mergeIdeas(ctx context.Context, d Deps, in MergeIdeasInput) (*MergeIdeasOutput, error) {
	groups, err := mergeGroups(in)
	if err != nil {
		return nil, err
	}
	out := &MergeIdeasOutput{Groups: []MergedGroup{}}
	for _, g := range groups {
		res, err := d.Ideas.Merge(ctx, g.KeepID, g.MergeIDs)
		if err != nil {
			if len(out.Groups) == 0 {
				return nil, mergeErr(err)
			}
			out.Stopped = "stopped early: " + mergeErr(err).Error()
			break
		}
		// Other sessions showing a merged idea learn it is gone.
		ideas.PublishDeleted(ctx, d.Hub, res.Deleted)
		out.Groups = append(out.Groups, MergedGroup{Idea: ideaRef(*res.Idea), Deleted: res.Deleted})
	}
	assistantkit.Touch(ctx, "idea", "")
	out.Idea, out.Deleted = out.Groups[0].Idea, out.Groups[0].Deleted
	return out, nil
}

// mergeErr turns a merge's input errors into refusals.
func mergeErr(err error) error {
	if errors.Is(err, ideas.ErrNotFound) {
		return assistantkit.Refused("one of those ideas doesn't exist; get the ids from findDuplicateIdeas or findIdeas")
	}
	return triageErr(err)
}
