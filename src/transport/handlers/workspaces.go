package handlers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// WorkspacesHandler serves the account-level workspace surface (CON-147): list
// the workspaces an account belongs to, create a new one, and choose the default
// the next fresh tab/login seeds into. These routes are account-scoped, not
// workspace-scoped — they act across the account's memberships — so unlike the
// content endpoints they do NOT read tenantctx; the active-workspace header
// resolved by RequireAuth is irrelevant here.
type WorkspacesHandler struct {
	db            *bun.DB
	workspaceRepo repository.WorkspaceRepository
	userRepo      repository.UserRepository
	accountRepo   repository.AccountRepository
	tenantRepo    repository.TenantRepository
	sessionRepo   repository.SessionRepository
	// profileJobs provisions a per-workspace Zernio profile on create (reusing the
	// signup bootstrap, CON-102) and tears it down on delete (CON-203). nil
	// disables both (create/delete still succeed; the profile is left orphaned).
	profileJobs ProfileLifecycleEnqueuer
	auth        fiber.Handler
	activity    *activity.Recorder
}

// NewWorkspacesHandler builds the handler.
func NewWorkspacesHandler(
	db *bun.DB,
	workspaceRepo repository.WorkspaceRepository,
	userRepo repository.UserRepository,
	accountRepo repository.AccountRepository,
	tenantRepo repository.TenantRepository,
	sessionRepo repository.SessionRepository,
	profileJobs ProfileLifecycleEnqueuer,
	auth fiber.Handler,
) *WorkspacesHandler {
	return &WorkspacesHandler{
		db:            db,
		workspaceRepo: workspaceRepo,
		userRepo:      userRepo,
		accountRepo:   accountRepo,
		tenantRepo:    tenantRepo,
		sessionRepo:   sessionRepo,
		profileJobs:   profileJobs,
		auth:          auth,
	}
}

// SetActivityRecorder wires the CON-125 activity recorder (nil-safe no-op).
func (h *WorkspacesHandler) SetActivityRecorder(r *activity.Recorder) { h.activity = r }

func (h *WorkspacesHandler) Register(app *fiber.App) {
	g := app.Group("/api/workspaces", h.auth)
	g.Get("/", h.List)
	g.Post("/", h.Create)
	g.Post("/:id/switch", h.Switch)
	g.Delete("/:id", h.Delete)
}

type createWorkspaceRequest struct {
	Name string `json:"name" validate:"required,min=1,max=100"`
}

// List godoc
// @Summary      List workspaces
// @Description  Returns every workspace the authenticated account is a member of, with the caller's role, the member count, and which one is the session default (CON-147).
// @Tags         workspaces
// @Produce      json
// @Security     CookieAuth
// @Success      200  {array}   repository.WorkspaceListItem
// @Failure      401  {object}  map[string]string
// @Router       /api/workspaces [get]
func (h *WorkspacesHandler) List(c *fiber.Ctx) error {
	session := c.Locals("session").(*models.Session)
	items, err := h.workspaceRepo.ListForAccount(reqCtx(c), session.AccountID)
	if err != nil {
		return err
	}
	// Mark the stored default (RequireAuth stashed it before the active-workspace
	// override, so it survives an X-Workspace-Id on this very request).
	def, _ := c.Locals(DefaultWorkspaceLocal).(string)
	for i := range items {
		items[i].IsDefault = items[i].ID == def
	}
	return c.JSON(items)
}

// Create godoc
// @Summary      Create a workspace
// @Description  Creates a new, independent workspace owned by the authenticated account, provisions its Zernio profile, and returns it. Does not switch — the caller chooses when to move into it (CON-147).
// @Tags         workspaces
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        body  body      createWorkspaceRequest  true  "Workspace name"
// @Success      201   {object}  models.Tenant
// @Failure      400   {object}  map[string]string
// @Failure      401   {object}  map[string]string
// @Router       /api/workspaces [post]
func (h *WorkspacesHandler) Create(c *fiber.Ctx) error {
	session := c.Locals("session").(*models.Session)

	var req createWorkspaceRequest
	if err := bindAndValidate(c, &req); err != nil {
		return err
	}

	// The membership is created for the caller's account, so it inherits the
	// account's display name/email (denormalised onto the users row, as signup
	// does).
	account, err := h.accountRepo.GetByID(reqCtx(c), session.AccountID)
	if err != nil {
		return err
	}

	slug, err := h.uniqueSlug(reqCtx(c), req.Name)
	if err != nil {
		return err
	}
	tenantID, err := models.NewID()
	if err != nil {
		return err
	}
	userID, err := models.NewID()
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	// CON-208: tenant.tier_id is required (NOT NULL); a new workspace starts on
	// the seeded default tier, reassignable later by Harbor.
	tenant := &models.Tenant{ID: tenantID, Name: req.Name, Slug: slug, TierID: models.DefaultTierID, CreatedAt: now, UpdatedAt: now}
	// The creator owns the new workspace (CON-26 role model).
	membership := &models.User{ID: userID, AccountID: account.ID, TenantID: tenantID, Name: account.Name, Email: account.Email, Role: models.RoleOwner, CreatedAt: now, UpdatedAt: now}

	if err := h.db.RunInTx(reqCtx(c), nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewInsert().Model(tenant).Exec(ctx); err != nil {
			return err
		}
		if err := h.userRepo.CreateTx(ctx, tx, membership); err != nil {
			return err
		}
		// Per-workspace Zernio profile, enqueued in-tx so the job exists iff the
		// workspace does — same pattern as signup (CON-102). Creation never blocks
		// on Zernio being reachable.
		if h.profileJobs != nil {
			if err := h.profileJobs.EnqueueBootstrapProfileTx(ctx, tx.Tx, tenantID); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		if isUniqueViolation(err) {
			// Lost a slug race; the caller can retry.
			return fiber.NewError(fiber.StatusConflict, "could not allocate a unique workspace slug")
		}
		return err
	}

	h.activity.Record(
		logging.WithUserID(tenantctx.With(reqCtx(c), tenantID), userID),
		activity.CategoryWorkspace, "workspace_created",
		activity.WithEntity("tenant", tenantID), activity.WithSource(activity.SourceAPI),
	)

	return c.Status(fiber.StatusCreated).JSON(tenant)
}

// Switch godoc
// @Summary      Set the default workspace
// @Description  Repoints the session's default workspace — where a fresh tab or the next login seeds. It does not rescope open tabs (each carries its own X-Workspace-Id), so switching in one tab leaves the others alone (CON-147). 403 if the account isn't a member of the target.
// @Tags         workspaces
// @Security     CookieAuth
// @Param        id  path  string  true  "Workspace id"
// @Success      204
// @Failure      401  {object}  map[string]string
// @Failure      403  {object}  map[string]string
// @Router       /api/workspaces/{id}/switch [post]
func (h *WorkspacesHandler) Switch(c *fiber.Ctx) error {
	session := c.Locals("session").(*models.Session)
	targetID := c.Params("id")

	// Only a workspace the account belongs to can become its default.
	membership, err := h.userRepo.GetMembership(reqCtx(c), session.AccountID, targetID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// 404 (not 403) so the endpoint doesn't confirm a workspace the account
			// can't see even exists (CON-97 §12.3).
			return fiber.NewError(fiber.StatusNotFound, "workspace not found")
		}
		return err
	}

	if err := h.sessionRepo.SetDefaultWorkspace(reqCtx(c), session.ID, membership.ID, targetID); err != nil {
		return err
	}

	h.activity.Record(
		logging.WithUserID(tenantctx.With(reqCtx(c), targetID), membership.ID),
		activity.CategoryWorkspace, "workspace_switched",
		activity.WithEntity("tenant", targetID), activity.WithSource(activity.SourceAPI),
	)

	return c.SendStatus(fiber.StatusNoContent)
}

// errLastWorkspace is the in-transaction sentinel the delete guard returns when
// soft-deleting the target would leave the account with no live workspace; the
// handler maps it to a 409 and the transaction rolls back.
var errLastWorkspace = errors.New("cannot delete the account's only workspace")

// Delete godoc
// @Summary      Delete a workspace
// @Description  Owner-only. Soft-deletes the workspace: it leaves every member's list and can't be entered again, but the row survives (recovery is a manual support request — there is no self-serve restore). Published posts stay live on the networks. Refuses to delete the account's only workspace (409). If it was the session default, the default is re-pointed to a surviving workspace (CON-147 PR4).
// @Tags         workspaces
// @Security     CookieAuth
// @Param        id  path  string  true  "Workspace id"
// @Success      204
// @Failure      401  {object}  map[string]string
// @Failure      403  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      409  {object}  map[string]string
// @Router       /api/workspaces/{id} [delete]
func (h *WorkspacesHandler) Delete(c *fiber.Ctx) error {
	session := c.Locals("session").(*models.Session)
	targetID := c.Params("id")

	// Resolve the target from the path (like Switch), independent of the active
	// workspace — an owner can delete any workspace they own. 404 (not 403) hides
	// the existence of workspaces the account can't see (CON-97 §12.3).
	membership, err := h.userRepo.GetMembership(reqCtx(c), session.AccountID, targetID)
	if err != nil {
		return notFound(err, "workspace not found")
	}
	if membership.Role != models.RoleOwner {
		return fiber.NewError(fiber.StatusForbidden, "only an owner can delete a workspace")
	}

	now := time.Now().UTC()
	if err := h.db.RunInTx(reqCtx(c), nil, func(ctx context.Context, tx bun.Tx) error {
		if err := h.tenantRepo.SoftDeleteTx(ctx, tx, targetID, now); err != nil {
			return err
		}
		// Last-workspace guard, evaluated INSIDE the tx so it counts against the
		// soft-delete just made: an account must keep at least one workspace to work
		// in (and to stay able to authenticate — RequireAuth needs a live
		// membership). Recounting here rather than before the tx closes the TOCTOU
		// window a pre-transaction check leaves open — a concurrent delete that
		// committed since would otherwise slip through. No live workspace left →
		// roll back with a 409.
		remaining, cerr := h.workspaceRepo.ListForAccountTx(ctx, tx, session.AccountID)
		if cerr != nil {
			return cerr
		}
		if len(remaining) == 0 {
			return errLastWorkspace
		}
		// Tear down the workspace's per-tenant Zernio profile (CON-203). Enqueued
		// INSIDE this tx (transactional outbox): the job exists iff the soft-delete
		// commits, so a rolled-back delete — e.g. the last-workspace guard above —
		// queues nothing. The Zernio deletes happen in the worker, so the delete
		// response never blocks on Zernio; the worker is idempotent and retries on
		// Zernio errors. nil enqueuer leaves the profile orphaned (pre-CON-203).
		if h.profileJobs != nil {
			if err := h.profileJobs.EnqueueTeardownProfileTx(ctx, tx.Tx, targetID); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		if errors.Is(err, errLastWorkspace) {
			return fiber.NewError(fiber.StatusConflict, "you can't delete your only workspace — create another first")
		}
		return err
	}

	// If the deleted workspace was this session's stored default, re-point it to a
	// surviving one so a fresh tab / no-header request lands somewhere live. Best
	// effort — RequireAuth's recovery also handles a dead default.
	if def, _ := c.Locals(DefaultWorkspaceLocal).(string); def == targetID {
		if next, nerr := h.userRepo.GetByAccountID(reqCtx(c), session.AccountID); nerr == nil {
			_ = h.sessionRepo.SetDefaultWorkspace(reqCtx(c), session.ID, next.ID, next.TenantID)
		}
	}

	h.activity.Record(
		logging.WithUserID(tenantctx.With(reqCtx(c), targetID), membership.ID),
		activity.CategoryWorkspace, "workspace_deleted",
		activity.WithEntity("tenant", targetID), activity.WithSource(activity.SourceAPI),
	)

	return c.SendStatus(fiber.StatusNoContent)
}

// uniqueSlug returns slugify(name) suffixed with -2, -3, … until free. Mirrors
// TenantsHandler.uniqueSlug so a workspace created while logged in slugs exactly
// like one created at signup.
func (h *WorkspacesHandler) uniqueSlug(ctx context.Context, name string) (string, error) {
	base := slugify(name)
	slug := base
	for i := 2; i <= 1000; i++ {
		_, err := h.tenantRepo.GetBySlug(ctx, slug)
		if errors.Is(err, sql.ErrNoRows) {
			return slug, nil
		}
		if err != nil {
			return "", err
		}
		slug = fmt.Sprintf("%s-%d", base, i)
	}
	return "", fiber.NewError(fiber.StatusConflict, "could not allocate a unique slug")
}
