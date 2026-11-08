package handlers

import (
	"context"
	"database/sql"
	"errors"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/usecase/post_actions/compose"
)

// IdeaPostsHandler serves the link between ideas and the posts made from
// them: compiling ideas into a post, and reading the link both ways.
type IdeaPostsHandler struct {
	compile  func(ctx context.Context, in compose.Input) (*compose.Result, error)
	links    repository.PostIdeaRepository
	ideas    repository.IdeaRepository
	posts    repository.PostRepository
	activity *activity.Recorder
	auth     fiber.Handler
}

// NewIdeaPostsHandler wires the endpoints; activity nil is a no-op.
func NewIdeaPostsHandler(
	compile func(ctx context.Context, in compose.Input) (*compose.Result, error),
	links repository.PostIdeaRepository,
	ideas repository.IdeaRepository,
	posts repository.PostRepository,
	activityRec *activity.Recorder,
	auth fiber.Handler,
) *IdeaPostsHandler {
	return &IdeaPostsHandler{compile: compile, links: links, ideas: ideas, posts: posts, activity: activityRec, auth: auth}
}

// Register must run before the ideas and posts handlers, so /compile wins
// over /:id.
func (h *IdeaPostsHandler) Register(app *fiber.App) {
	app.Post("/api/ideas/compile", h.auth, h.Compile)
	app.Get("/api/ideas/:id/posts", h.auth, h.PostsOfIdea)
	app.Get("/api/posts/:id/ideas", h.auth, h.IdeasOfPost)
}

type compileIdeasRequest struct {
	IdeaIDs []string `json:"idea_ids" validate:"required,min=1"`
	// CampaignID defaults to the campaign the ideas share; an idea in no
	// campaign goes along with it.
	CampaignID string `json:"campaign_id"`
	// Title defaults to the first idea's title.
	Title string `json:"title"`
	// PlatformID, PlatformPostType, PhaseID and TargetDate default to the
	// first idea plan that sets each.
	PlatformID       string               `json:"platform_id"`
	PlatformPostType string               `json:"platform_post_type"`
	PhaseID          string               `json:"phase_id"`
	TargetDate       *models.CalendarDate `json:"target_date" swaggertype:"string" example:"2026-12-01" extensions:"x-nullable"`
}

type compileIdeasResponse struct {
	Post models.Post `json:"post"`
	// Ideas are the ideas the post was made from, in order.
	Ideas []models.PostSourceIdea `json:"ideas"`
}

// Compile godoc
// @Summary      Compile ideas into a planned post
// @Description  Makes one `planned` post from 1–10 ideas: a draft_thesis note with a thesis per idea (its
// @Description  title, then its angle or note), the ideas' source assets as its sources, and a link to each
// @Description  idea (GET /api/posts/{id}/ideas). The campaign defaults to the one the ideas share (an idea in no
// @Description  campaign goes along with it); the title
// @Description  to the first idea's; platform, post type, phase and target date to the first idea plan that sets
// @Description  each. A target date is placed at the campaign's publishing time. A platform or post type you name
// @Description  must be one of the campaign's; a stale suggested one is dropped. The ideas themselves are left as
// @Description  they are, so one idea can feed several posts.
// @Tags         ideas
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        body  body      compileIdeasRequest  true  "Ideas and overrides"
// @Success      201   {object}  compileIdeasResponse
// @Failure      400   {object}  map[string]string
// @Failure      401   {object}  map[string]string
// @Router       /api/ideas/compile [post]
func (h *IdeaPostsHandler) Compile(c *fiber.Ctx) error {
	var req compileIdeasRequest
	if err := bindAndValidate(c, &req); err != nil {
		return err
	}
	session, err := sessionFrom(c)
	if err != nil {
		return err
	}
	res, err := h.compile(reqCtx(c), compose.Input{
		IdeaIDs:    req.IdeaIDs,
		CampaignID: req.CampaignID,
		Title:      req.Title,
		PlatformID: req.PlatformID,
		PostType:   req.PlatformPostType,
		PhaseID:    req.PhaseID,
		TargetDate: req.TargetDate,
		Actor:      session.UserID,
		NoteOrigin: models.PostNoteOriginManual,
	})
	if err != nil {
		if compose.IsValidation(err) {
			return fiber.NewError(fiber.StatusBadRequest, err.Error())
		}
		return err
	}
	ideas := make([]models.PostSourceIdea, len(res.Ideas))
	ids := make([]string, len(res.Ideas))
	for i, idea := range res.Ideas {
		ideas[i] = models.PostSourceIdea{ID: idea.ID, Title: idea.Title, Origin: idea.Origin}
		ids[i] = idea.ID
	}
	h.activity.Record(reqCtx(c), activity.CategoryIdea, "ideas_compiled",
		activity.WithSource(activity.SourceAPI),
		activity.WithEntity("post", res.Post.ID),
		activity.WithPayload(map[string]any{"idea_ids": ids}),
	)
	return c.Status(fiber.StatusCreated).JSON(compileIdeasResponse{Post: *res.Post, Ideas: ideas})
}

// PostsOfIdea godoc
// @Summary  Posts made from an idea, oldest first
// @Tags     ideas
// @Produce  json
// @Security CookieAuth
// @Param    id  path  string  true  "Idea id"
// @Success  200 {array}  models.IdeaPostRef
// @Failure  404 {object} map[string]string
// @Router   /api/ideas/{id}/posts [get]
func (h *IdeaPostsHandler) PostsOfIdea(c *fiber.Ctx) error {
	ctx := reqCtx(c)
	if _, err := h.ideas.GetByID(ctx, c.Params("id")); err != nil {
		return notFoundOr(err, "idea not found")
	}
	refs, err := h.links.PostsOfIdea(ctx, c.Params("id"))
	if err != nil {
		return err
	}
	return c.JSON(refs)
}

// IdeasOfPost godoc
// @Summary  Ideas a post was made from, in order
// @Tags     posts
// @Produce  json
// @Security CookieAuth
// @Param    id  path  string  true  "Post Sqid"
// @Success  200 {array}  models.PostSourceIdea
// @Failure  404 {object} map[string]string
// @Router   /api/posts/{id}/ideas [get]
func (h *IdeaPostsHandler) IdeasOfPost(c *fiber.Ctx) error {
	ctx := reqCtx(c)
	if _, err := h.posts.GetByID(ctx, c.Params("id")); err != nil {
		return notFoundOr(err, "post not found")
	}
	ideas, err := h.links.IdeasOfPost(ctx, c.Params("id"))
	if err != nil {
		return err
	}
	return c.JSON(ideas)
}

// notFoundOr maps a missing row to a 404 with msg.
func notFoundOr(err error, msg string) error {
	if errors.Is(err, sql.ErrNoRows) {
		return fiber.NewError(fiber.StatusNotFound, msg)
	}
	return err
}
