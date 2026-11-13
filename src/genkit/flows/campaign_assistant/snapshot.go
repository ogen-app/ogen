package campaign_assistant

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/flowkit"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/calendarcheck"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/state"
)

// snapshotTop caps the calendar problems the state names one by one.
const snapshotTop = 3

// snapshotColumns are the post columns the state and its calendar check
// read: the sources for "same source close together", the account for the
// crowded slots.
var snapshotColumns = []string{"id", "title", "status", "platform_id", "platform_post_type", "social_account_id", "scheduled_at", "used_asset_ids"}

// snapshot reads the campaign's state for this turn. Each part degrades on
// its own: a failed read leaves that part out rather than failing the turn,
// since the tools still check what they need. Unread posts mark the goal's
// room unknown, never the whole target.
func snapshot(ctx context.Context, repos CampaignAssistantRepos, c *models.Campaign, now time.Time) state.Snapshot {
	var posts []models.Post
	postsRead := true
	if repos.Posts != nil {
		rows, err := repos.Posts.ListRowsByCampaign(ctx, c.ID, snapshotColumns...)
		if err != nil {
			warnState(ctx, c, "count campaign posts", err)
			postsRead = false
		}
		posts = rows
	}
	s := state.Build(c, posts, sources(ctx, repos, c))
	if !postsRead {
		return s.WithoutPosts()
	}
	if p := calendarcheck.CurrentPeriod(c, posts, now); p != nil {
		s = s.WithPeriod(p.Label, p.Target, p.Planned)
	}
	s.Calendar = calendar(ctx, repos, c, posts, now)
	return s
}

// sources counts what generation has to draw on: the campaign's attached
// assets, or the whole bank when it attached none, how many are ready, and
// how many have a finished analysis — through the campaign's lens for its
// own assets, the workspace lens for the bank. nil when they could not be
// read; Analysed is -1 when only the analyses could not. A campaign's own
// assets are listed one by one too.
func sources(ctx context.Context, repos CampaignAssistantRepos, c *models.Campaign) *state.Sources {
	if repos.Assets == nil {
		return nil
	}
	ready, skipped, metas, err := flowkit.CampaignAssets(ctx, repos.Assets, c)
	if err != nil {
		warnState(ctx, c, "count campaign assets", err)
		return nil
	}
	src := &state.Sources{WholeBank: len(c.AssetIDs) == 0, Attached: len(ready) + len(skipped), Ready: len(ready), Analysed: -1}
	var digests map[string]repository.LensDigest
	if repos.Analyses != nil && len(ready) > 0 {
		var lens *string
		if !src.WholeBank {
			lens = &c.ID
		}
		digests, err = repos.Analyses.LensDigests(ctx, ready, lens)
		if err != nil {
			warnState(ctx, c, "count analysed sources", err)
			digests = nil
		} else {
			src.Analysed = 0
			for _, d := range digests {
				if d.Status == models.AssetAnalysisStatusReady {
					src.Analysed++
				}
			}
		}
	}
	if len(ready) == 0 {
		src.Analysed = 0
	}
	src.Items = sourceItems(c, metas, digests, digests != nil)
	return src
}

// sourceItems lists the campaign's attached assets in its order, up to
// state.MaxSourceItems: unreadable when the asset failed or is gone, ready
// once it is processed and, when the analyses were read, analysed through
// the campaign's lens; analysing until then.
func sourceItems(c *models.Campaign, metas []models.Asset, digests map[string]repository.LensDigest, analysesRead bool) []state.SourceItem {
	var items []state.SourceItem
	for _, id := range c.AssetIDs[:min(len(c.AssetIDs), state.MaxSourceItems)] {
		item := state.SourceItem{ID: id, Status: state.SourceUnreadable}
		i := slices.IndexFunc(metas, func(a models.Asset) bool { return a.ID == id })
		if i >= 0 {
			a := metas[i]
			item.Title = a.Title
			if a.Type != nil {
				item.Kind = *a.Type
			}
			switch {
			case a.Status == models.AssetStatusFailed || a.Status == models.AssetStatusPartial:
			case a.Status == models.AssetStatusReady && (!analysesRead || digests[id].Status == models.AssetAnalysisStatusReady):
				item.Status = state.SourceReady
			default:
				item.Status = state.SourceAnalysing
			}
		}
		items = append(items, item)
	}
	return items
}

// calendar runs the calendar check over the posts and summarises it: the
// number of problems, by kind, and the first few, warnings first. The ideas
// links are read for "same idea close together"; without them that check is
// skipped rather than the whole summary.
func calendar(ctx context.Context, repos CampaignAssistantRepos, c *models.Campaign, posts []models.Post, now time.Time) *state.Calendar {
	var ideas map[string][]string
	if repos.Links != nil {
		got, err := repos.Links.IdeasOfCampaignPosts(ctx, c.ID)
		if err != nil {
			warnState(ctx, c, "read post ideas", err)
		}
		ideas = got
	}
	findings := calendarcheck.Check(c, posts, ideas, now)
	cal := &state.Calendar{Total: len(findings)}
	for _, f := range findings {
		kind := strings.ReplaceAll(f.Code, "_", " ")
		if i := slices.IndexFunc(cal.Kinds, func(k state.KindCount) bool { return k.Kind == kind }); i >= 0 {
			cal.Kinds[i].Count++
		} else {
			cal.Kinds = append(cal.Kinds, state.KindCount{Kind: kind, Count: 1})
		}
		if len(cal.Top) < snapshotTop {
			cal.Top = append(cal.Top, f.Message)
		}
	}
	return cal
}

func warnState(ctx context.Context, c *models.Campaign, what string, err error) {
	slog.WarnContext(ctx, what+" for state failed", logging.AttrComponent, logComponent, "campaign_id", c.ID, logging.AttrError, err)
}
