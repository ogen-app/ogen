package packs

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
)

// stillThere keeps the ids of kind that still exist, in order and once
// each: in the tenant, or among campaignID's posts or ideas when it is set.
// None left is a refusal. Without a way to read them every id is kept.
func stillThere(ctx context.Context, d Deps, kind viewcontext.EntityKind, campaignID string, ids []string) ([]string, error) {
	ids = dedupe(ids)
	if d.Titles == nil || len(ids) == 0 {
		return ids, nil
	}
	read := func() (map[string]string, error) { return d.Titles.Titles(ctx, kind, ids) }
	if campaignID != "" {
		read = func() (map[string]string, error) { return d.Titles.CampaignTitles(ctx, kind, campaignID, ids) }
	}
	got, err := read()
	if err != nil {
		return nil, fmt.Errorf("recheck %ss: %w", kind, err)
	}
	keep := slices.DeleteFunc(ids, func(id string) bool { _, ok := got[id]; return !ok })
	if len(keep) == 0 {
		return nil, assistantkit.Refused(fmt.Sprintf("none of those %ss exist any more", kind))
	}
	return keep, nil
}

// recheckIDs is a Gate.Recheck for a call on a list of ids of kind: of reads
// the call's ids and their campaign, if they must be its, and with narrows
// the call to the ones still there, saying how many went. When none is,
// the refusal comes with the same change, every one of them gone.
func recheckIDs[In any](d Deps, kind viewcontext.EntityKind, of func(context.Context, In) (campaignID string, ids []string), with func(In, []string) In) func(context.Context, In) (In, []assistantkit.Change, error) {
	return func(ctx context.Context, in In) (In, []assistantkit.Change, error) {
		campaignID, ids := of(ctx, in)
		all := len(dedupe(ids))
		keep, err := stillThere(ctx, d, kind, campaignID, ids)
		if _, ok := errors.AsType[*assistantkit.Refusal](err); ok {
			return in, []assistantkit.Change{idsGone(kind, campaignID, all, all)}, err
		}
		if err != nil {
			return in, nil, err
		}
		if len(keep) == all {
			return in, nil, nil
		}
		return with(in, keep), []assistantkit.Change{idsGone(kind, campaignID, all-len(keep), all)}, nil
	}
}

// idsGone is n of a call's all ids of kind gone: deleted, or out of the
// campaign when they must be campaignID's.
func idsGone(kind viewcontext.EntityKind, campaignID string, n, all int) assistantkit.Change {
	gone := assistantkit.Change{Reason: fmt.Sprintf("%d of the %d %ss no longer exist", n, all, kind),
		Code: assistantkit.ChangeDeleted, Item: string(kind), Count: n, Of: all}
	if campaignID != "" {
		gone.Reason = fmt.Sprintf("%d of the %d %ss are no longer in the campaign", n, all, kind)
		gone.Code = assistantkit.ChangeLeftCampaign
	}
	return gone
}

// ideasIn reads a call's idea ids, which may be any campaign's.
func ideasIn(ids []string) (string, []string) { return "", ids }

// campaignPostsIn reads a call's post ids, which must be its campaign's:
// the one named, else the thread's.
func campaignPostsIn(ctx context.Context, in PostsRef) (string, []string) {
	if in.CampaignID != "" {
		return in.CampaignID, in.PostIDs
	}
	return assistant.CampaignID(ctx), in.PostIDs
}
