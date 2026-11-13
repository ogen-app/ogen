package campaign_assistant

import (
	"context"
	"log/slog"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/content_plan"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/usecase/assistantundo"
)

// logChange records a reversible change to the campaign so its result card
// can offer undo. It returns the undo id, or "" when the change could not be
// logged: the change still stands, it just can't be undone.
func (st *requestState) logChange(ctx context.Context, kind models.AssistantActionKind, before, after models.JSONMap) string {
	if st.repos.Actions == nil {
		return ""
	}
	by, _ := logging.UserIDFrom(ctx)
	id, err := assistantundo.Record(ctx, st.repos.Actions, &models.AssistantAction{
		ScopeKind: models.AssistantScopeCampaign, ScopeID: st.campaignID,
		Kind: kind, EntityID: st.campaignID, Before: before, After: after, CreatedBy: by,
	})
	if err != nil {
		slog.WarnContext(ctx, "log reversible change failed; it can't be undone", logging.AttrComponent, logComponent,
			"campaign_id", st.campaignID, "kind", kind, logging.AttrError, err)
		return ""
	}
	return id
}

// logIdeasCreated records the ideas a generation saved, as they read back,
// so undo can delete them while they are as made. It returns "" when there
// are none, or they could not be read or logged.
func (st *requestState) logIdeasCreated(ctx context.Context, planned []content_plan.DraftPost) string {
	if st.repos.Ideas == nil {
		return ""
	}
	var made []*models.Idea
	for _, p := range planned {
		if p.IdeaID == "" {
			continue
		}
		idea, err := st.repos.Ideas.GetByID(ctx, p.IdeaID)
		if err != nil {
			slog.WarnContext(ctx, "read saved idea failed; the ideas can't be undone", logging.AttrComponent, logComponent,
				"idea_id", p.IdeaID, logging.AttrError, err)
			return ""
		}
		made = append(made, idea)
	}
	if len(made) == 0 {
		return ""
	}
	return st.logChange(ctx, models.AssistantActionIdeasCreated, models.JSONMap{}, assistantundo.IdeasCreated(made))
}
