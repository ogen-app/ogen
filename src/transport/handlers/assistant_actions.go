package handlers

import (
	"context"
	"errors"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/usecase/assistantundo"
)

// AssistantActionsHandler undoes reversible changes the assistant made, from
// the UNDO on their result card.
type AssistantActionsHandler struct {
	undo     func(ctx context.Context, id, by string) (*models.AssistantAction, error)
	activity *activity.Recorder
	auth     fiber.Handler
}

// NewAssistantActionsHandler wires the undo endpoint. undo nil leaves it at
// 503; activity nil is a no-op.
func NewAssistantActionsHandler(undo func(ctx context.Context, id, by string) (*models.AssistantAction, error), activityRec *activity.Recorder, auth fiber.Handler) *AssistantActionsHandler {
	return &AssistantActionsHandler{undo: undo, activity: activityRec, auth: auth}
}

func (h *AssistantActionsHandler) Register(app *fiber.App) {
	app.Post("/api/assistant/actions/:id/undo", h.auth, h.Undo)
}

// undoResponse names what was restored, so the client refreshes it.
type undoResponse struct {
	Undone   bool   `json:"undone"`
	Kind     string `json:"kind"`
	EntityID string `json:"entityId"`
}

// Undo godoc
// @Summary      Undo an assistant change
// @Description  Restores what a reversible assistant change replaced. kind and entityId name what to refresh:
// @Description  campaign_dates, campaign_brief, campaign_platforms, campaign_assets, campaign_created (the campaign is
// @Description  deleted), idea_verdict, idea_edit, idea_verdicts (no entityId: several ideas), post_created (the post
// @Description  made from ideas is deleted), brand_fact, brand_voice, brand_audience, brand_guardrails, series,
// @Description  campaign_series, post_schedules and post_types (entityId: the campaign whose posts changed), and
// @Description  auto_publish.
// @Description  409 when it was already undone, or when it was edited again since: undo never overwrites a later edit.
// @Tags         assistant
// @Produce      json
// @Security     CookieAuth
// @Param        id   path      string  true  "Undo id from the change's result"
// @Success      200  {object}  undoResponse
// @Failure      401  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      409  {object}  map[string]string
// @Failure      503  {object}  map[string]string
// @Router       /api/assistant/actions/{id}/undo [post]
func (h *AssistantActionsHandler) Undo(c *fiber.Ctx) error {
	if h.undo == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "undo is not available")
	}
	session, err := sessionFrom(c)
	if err != nil {
		return err
	}
	a, err := h.undo(reqCtx(c), c.Params("id"), session.UserID)
	switch {
	case errors.Is(err, assistantundo.ErrNotFound):
		return fiber.NewError(fiber.StatusNotFound, "This change no longer exists.")
	case errors.Is(err, assistantundo.ErrAlreadyUndone), errors.Is(err, assistantundo.ErrChangedSince):
		return fiber.NewError(fiber.StatusConflict, err.Error())
	case err != nil:
		return err
	}
	h.activity.Record(reqCtx(c), activity.CategoryAIFlow, "assistant_change_undone",
		activity.WithEntity(string(a.ScopeKind), a.EntityID),
		activity.WithSource(activity.SourceAssistant),
	)
	return c.JSON(undoResponse{Undone: true, Kind: string(a.Kind), EntityID: a.EntityID})
}
