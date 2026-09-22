package handlers

import (
	"database/sql"
	"errors"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/usecase/notes"
)

// PostNotesHandler exposes the CRUD API for per-post Notes (CON-188), nested
// under a post so a UI can manage draft theses, image prompts, and free-form
// notes by hand. Writes go through the shared notes.Service the assistant also
// uses.
type PostNotesHandler struct {
	svc      *notes.Service
	postRepo repository.PostRepository
	auth     fiber.Handler
	activity *activity.Recorder
}

func NewPostNotesHandler(svc *notes.Service, postRepo repository.PostRepository, auth fiber.Handler) *PostNotesHandler {
	return &PostNotesHandler{svc: svc, postRepo: postRepo, auth: auth}
}

// SetActivityRecorder wires the CON-125 activity recorder. nil is a no-op.
func (h *PostNotesHandler) SetActivityRecorder(r *activity.Recorder) {
	h.activity = r
}

func (h *PostNotesHandler) recordActivity(c *fiber.Ctx, typ string, opts ...activity.Option) {
	h.activity.Record(reqCtx(c), activity.CategoryPost, typ,
		append([]activity.Option{activity.WithSource(activity.SourceAPI)}, opts...)...)
}

func (h *PostNotesHandler) Register(app *fiber.App) {
	g := app.Group("/api/posts/:post_id/notes", h.auth)
	g.Get("/", h.List)
	g.Post("/", h.Create)
	g.Get("/:id", h.Get)
	g.Patch("/:id", h.Update)
	g.Delete("/:id", h.Delete)
}

// loadPostOrErr fetches the parent post, returning 404 when it is missing or
// belongs to another tenant.
func (h *PostNotesHandler) loadPostOrErr(c *fiber.Ctx) (*models.Post, error) {
	post, err := h.postRepo.GetByID(reqCtx(c), c.Params("post_id"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fiber.NewError(fiber.StatusNotFound, "post not found")
		}
		return nil, err
	}
	return post, nil
}

// loadNoteOrErr fetches a note and verifies it belongs to the post in the path,
// returning 404 otherwise (so a note id from another post can't be reached).
func (h *PostNotesHandler) loadNoteOrErr(c *fiber.Ctx, postID string) (*models.PostNote, error) {
	note, err := h.svc.Get(reqCtx(c), c.Params("id"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fiber.NewError(fiber.StatusNotFound, "note not found")
		}
		return nil, err
	}
	if note.PostID != postID {
		return nil, fiber.NewError(fiber.StatusNotFound, "note not found")
	}
	return note, nil
}

// createNoteRequest is the POST body. Origin is always stamped `manual`
// server-side for REST-created notes.
type createNoteRequest struct {
	Type  string `json:"type"  validate:"required"`
	Title string `json:"title"`
	Body  string `json:"body"  validate:"required"`
}

// updateNoteRequest is the PATCH body. Every field is optional; at least one
// must be present.
type updateNoteRequest struct {
	Type  *string `json:"type"`
	Title *string `json:"title"`
	Body  *string `json:"body"`
}

func (h *PostNotesHandler) List(c *fiber.Ctx) error {
	post, err := h.loadPostOrErr(c)
	if err != nil {
		return err
	}
	list, err := h.svc.List(reqCtx(c), post.ID)
	if err != nil {
		return err
	}
	// Never return a null body for an empty list.
	if list == nil {
		list = []models.PostNote{}
	}
	return c.JSON(list)
}

func (h *PostNotesHandler) Get(c *fiber.Ctx) error {
	post, err := h.loadPostOrErr(c)
	if err != nil {
		return err
	}
	note, err := h.loadNoteOrErr(c, post.ID)
	if err != nil {
		return err
	}
	return c.JSON(note)
}

func (h *PostNotesHandler) Create(c *fiber.Ctx) error {
	post, err := h.loadPostOrErr(c)
	if err != nil {
		return err
	}

	var req createNoteRequest
	if err := bindAndValidate(c, &req); err != nil {
		return err
	}

	session := c.Locals("session").(*models.Session)
	note, err := h.svc.Create(reqCtx(c), notes.CreateInput{
		PostID:    post.ID,
		Type:      models.PostNoteType(req.Type),
		Title:     req.Title,
		Body:      req.Body,
		Origin:    models.PostNoteOriginManual,
		CreatedBy: session.UserID,
	})
	if err != nil {
		if notes.IsValidation(err) {
			return fiber.NewError(fiber.StatusBadRequest, err.Error())
		}
		return err
	}

	h.recordActivity(c, "note_created",
		activity.WithEntity("note", note.ID),
		activity.WithPayload(map[string]any{"post_id": note.PostID, "type": string(note.Type), "origin": string(note.Origin)}),
	)
	return c.Status(fiber.StatusCreated).JSON(note)
}

func (h *PostNotesHandler) Update(c *fiber.Ctx) error {
	post, err := h.loadPostOrErr(c)
	if err != nil {
		return err
	}
	note, err := h.loadNoteOrErr(c, post.ID)
	if err != nil {
		return err
	}

	var req updateNoteRequest
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	if req.Type == nil && req.Title == nil && req.Body == nil {
		return fiber.NewError(fiber.StatusBadRequest, "at least one of type, title or body is required")
	}

	var typePatch *models.PostNoteType
	if req.Type != nil {
		t := models.PostNoteType(*req.Type)
		typePatch = &t
	}
	updated, err := h.svc.Update(reqCtx(c), note, notes.UpdateInput{
		Title: req.Title,
		Body:  req.Body,
		Type:  typePatch,
	})
	if err != nil {
		switch {
		case notes.IsValidation(err):
			return fiber.NewError(fiber.StatusBadRequest, err.Error())
		case errors.Is(err, notes.ErrNotFound):
			return fiber.NewError(fiber.StatusNotFound, "note not found")
		default:
			return err
		}
	}

	h.recordActivity(c, "note_updated",
		activity.WithEntity("note", updated.ID),
		activity.WithPayload(map[string]any{"post_id": updated.PostID, "type": string(updated.Type)}),
	)
	return c.JSON(updated)
}

func (h *PostNotesHandler) Delete(c *fiber.Ctx) error {
	post, err := h.loadPostOrErr(c)
	if err != nil {
		return err
	}
	// Verify the note exists and belongs to this post before deleting, so a
	// wrong-post id returns 404 rather than silently succeeding.
	note, err := h.loadNoteOrErr(c, post.ID)
	if err != nil {
		return err
	}
	deleted, err := h.svc.Delete(reqCtx(c), note.ID)
	if err != nil {
		return err
	}
	if !deleted {
		return fiber.NewError(fiber.StatusNotFound, "note not found")
	}
	h.recordActivity(c, "note_deleted", activity.WithEntity("note", note.ID))
	return c.SendStatus(fiber.StatusNoContent)
}
