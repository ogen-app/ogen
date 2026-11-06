package packs

import (
	"context"
	"errors"
	"fmt"

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
		assistantkit.Wrap(func(ctx context.Context, in FindDuplicateIdeasInput) (*FindDuplicateIdeasOutput, error) {
			return findDuplicateIdeas(ctx, d, in)
		}))
	merge := genkit.DefineTool(g, "mergeIdeas",
		"Merges ideas into one kept idea: it gains their assets and notes, and they are deleted. Always offered for the user to confirm first.",
		assistantkit.Gated("mergeIdeas", assistantkit.Gate[MergeIdeasInput]{
			Precheck: func(ctx context.Context, in MergeIdeasInput) error {
				_, err := d.Ideas.CheckMerge(ctx, in.KeepID, in.MergeIDs)
				return mergeErr(err)
			},
			Describe: func(ctx context.Context, in MergeIdeasInput) string {
				n := len(dedupe(in.MergeIDs)) + 1
				if keep, err := d.Ideas.Get(ctx, in.KeepID); err == nil {
					return fmt.Sprintf("Merge %d ideas into “%s”", n, keep.Title)
				}
				return fmt.Sprintf("Merge %d ideas into one", n)
			},
		}, assistantkit.Wrap(func(ctx context.Context, in MergeIdeasInput) (*MergeIdeasOutput, error) {
			return mergeIdeas(ctx, d, in)
		})))
	return []assistantkit.Spec{
		spec(ideasPackName, find, assistantkit.TierRead,
			`Call this when the user asks about duplicate, repeated or overlapping ideas — "do I have the same idea twice?", "clean up my ideas". It compares open ideas (not turned down, not made into a post) of this thread's campaign, or the workspace-wide ideas outside a campaign. Each group's keepId is the suggested idea to keep; offer mergeIdeas for the groups the user wants merged.`),
		spec(ideasPackName, merge, assistantkit.TierProposal,
			`Call this to merge duplicate ideas — "merge these", "keep the first one". keepId is the idea to keep (default the group's keepId from findDuplicateIdeas); mergeIds are the others (1 to 50). They must share a campaign and none may have been made into a post. Always offered first; the merged ideas are deleted and it can't be undone.`),
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

// MergeIdeasInput merges ideas into one.
type MergeIdeasInput struct {
	KeepID   string   `json:"keepId"   jsonschema:"description=The idea to keep"`
	MergeIDs []string `json:"mergeIds" jsonschema:"description=The ideas to merge into it and delete; 1 to 50"`
}

// MergeIdeasOutput is the kept idea and the ideas merged away.
type MergeIdeasOutput struct {
	Idea    IdeaRef  `json:"idea"`
	Deleted []string `json:"deleted"`
}

func mergeIdeas(ctx context.Context, d Deps, in MergeIdeasInput) (*MergeIdeasOutput, error) {
	res, err := d.Ideas.Merge(ctx, in.KeepID, in.MergeIDs)
	if err != nil {
		return nil, mergeErr(err)
	}
	assistantkit.Touch(ctx, "idea", "")
	return &MergeIdeasOutput{Idea: ideaRef(*res.Idea), Deleted: res.Deleted}, nil
}

// mergeErr turns a merge's input errors into refusals.
func mergeErr(err error) error {
	if errors.Is(err, ideas.ErrNotFound) {
		return assistantkit.Refused("one of those ideas doesn't exist; get the ids from findDuplicateIdeas or findIdeas")
	}
	return triageErr(err)
}
