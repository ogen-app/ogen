package handlers

import (
	"database/sql"
	"errors"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// PostLogsHandler exposes the read API over the Post Log table
// (CON-69 §11). Writes happen wherever the originating event lives —
// the handler does not expose an Append endpoint.
type PostLogsHandler struct {
	repo     repository.PostLogRepository
	postRepo repository.PostRepository
	auth     fiber.Handler
}

func NewPostLogsHandler(repo repository.PostLogRepository, postRepo repository.PostRepository, auth fiber.Handler) *PostLogsHandler {
	return &PostLogsHandler{repo: repo, postRepo: postRepo, auth: auth}
}

func (h *PostLogsHandler) Register(app *fiber.App) {
	app.Get("/api/posts/:post_id/log", h.auth, h.ListByPost)
	app.Get("/api/post-logs", h.auth, h.ListFiltered)
}

// ListByPost godoc
// @Summary      List Post Log entries for a post
// @Description  Returns the audit log for a single post in chronological
// @Description  order (oldest first). Default cap 500 entries; pass
// @Description  `?limit=N` to override.
// @Tags         post-logs
// @Produce      json
// @Security     CookieAuth
// @Param        post_id  path   string  true   "Post Sqid"
// @Param        limit    query  int     false  "Max entries"
// @Success      200  {array}   models.PostLog
// @Failure      401  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Router       /api/posts/{post_id}/log [get]
func (h *PostLogsHandler) ListByPost(c *fiber.Ctx) error {
	postID := c.Params("post_id")
	if _, err := h.postRepo.GetByID(reqCtx(c), postID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fiber.NewError(fiber.StatusNotFound, "post not found")
		}
		return err
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	logs, err := h.repo.ListByPostID(reqCtx(c), postID, limit)
	if err != nil {
		return err
	}
	return c.JSON(logs)
}

// ListFiltered godoc
// @Summary      Search Post Log entries
// @Description  Returns log entries filtered by any combination of
// @Description  `post_id`, `event_type`, `actor`, `since`/`until`
// @Description  (RFC3339), and `limit`. Default order is newest first.
// @Tags         post-logs
// @Produce      json
// @Security     CookieAuth
// @Param        post_id     query  string  false  "Post Sqid"
// @Param        event_type  query  string  false  "Event type"
// @Param        actor       query  string  false  "Actor (user id or 'system')"
// @Param        since       query  string  false  "RFC3339 lower bound"
// @Param        until       query  string  false  "RFC3339 upper bound"
// @Param        limit       query  int     false  "Max entries"
// @Success      200  {array}   models.PostLog
// @Failure      400  {object}  map[string]string
// @Failure      401  {object}  map[string]string
// @Router       /api/post-logs [get]
func (h *PostLogsHandler) ListFiltered(c *fiber.Ctx) error {
	f := repository.PostLogFilter{
		PostID:    c.Query("post_id"),
		EventType: models.PostLogEventType(c.Query("event_type")),
		Actor:     c.Query("actor"),
	}
	if v := c.Query("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return fiber.NewError(fiber.StatusBadRequest, "since must be RFC3339")
		}
		f.Since = t
	}
	if v := c.Query("until"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return fiber.NewError(fiber.StatusBadRequest, "until must be RFC3339")
		}
		f.Until = t
	}
	if v := c.Query("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return fiber.NewError(fiber.StatusBadRequest, "limit must be a non-negative integer")
		}
		f.Limit = n
	}
	logs, err := h.repo.ListFiltered(reqCtx(c), f)
	if err != nil {
		return err
	}
	return c.JSON(logs)
}
