package campaign_assistant

import (
	"context"
	"log/slog"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/flowkit"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/state"
)

// snapshot reads the campaign's state for this turn. Each part degrades on
// its own: a failed read leaves that part out rather than failing the turn,
// since the tools still check what they need.
func (t *turn) snapshot(ctx context.Context, c *models.Campaign) state.Snapshot {
	var posts []models.Post
	if t.repos.Posts != nil {
		rows, err := t.repos.Posts.ListRowsByCampaign(ctx, c.ID, "id", "status")
		if err != nil {
			slog.WarnContext(ctx, "count campaign posts for state failed", logging.AttrComponent, logComponent,
				"campaign_id", c.ID, logging.AttrError, err)
		}
		posts = rows
	}
	ready := -1
	if t.repos.Assets != nil {
		ids, _, err := flowkit.ReadyCampaignAssetIDs(ctx, t.repos.Assets, c)
		if err != nil {
			slog.WarnContext(ctx, "count campaign assets for state failed", logging.AttrComponent, logComponent,
				"campaign_id", c.ID, logging.AttrError, err)
		} else {
			ready = len(ids)
		}
	}
	return state.Build(c, posts, ready)
}
