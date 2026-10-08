package handlers

import (
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/storage"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/usecase/plugins"
)

// Pairing poll cadence and per-key limits. A plugin polls every 2s for at
// most the pairing TTL, well inside 60 polls a minute.
const (
	pairingPollInterval    = 2 * time.Second
	pairingStartsPerMinute = 10
	pairingPollsPerMinute  = 60
)

// Pairing reject codes the plugin matches on.
const (
	CodePairingDenied     = "pairing_denied"
	CodePairingExpired    = "pairing_expired"
	CodePairingNotPending = "pairing_not_pending"
	CodeInvalidLabel      = "invalid_client_label"
)

// FigmaPluginHandler is the API the Figma plugin calls. Pairing routes are
// unauthenticated (they are how a plugin gets a token); the rest take a
// plugin token. CORS for these routes is set up in the server wiring.
type FigmaPluginHandler struct {
	pairing     *plugins.Service
	appBaseURL  string
	auth        fiber.Handler
	users       repository.UserRepository
	posts       repository.PostRepository
	platforms   repository.PlatformRepository
	assets      *AssetsHandler
	attachments *PostAttachmentsHandler
	activity    *activity.Recorder
	media       *pluginMediaResolver
	// maxPluginVideoBytes caps one plugin video; 0 leaves the web app's cap.
	maxPluginVideoBytes int64

	startLimiter *keyedRateLimiter
	pollLimiter  *keyedRateLimiter
	tokenLimiter *keyedRateLimiter
}

// FigmaPluginDeps are the plugin API's collaborators. Assets and Attachments
// supply the content bank's image ingest and bank-to-post attach, so a frame
// sent by the plugin takes exactly the path an upload does. Activity may be
// nil. Platforms supplies the media rules the campaign tree carries for
// pre-flight checks; nil leaves them out. MaxVideoBytes caps one video send
// (0 = the web app's video cap). PostAttachments, MediaPreviews, Storage and
// Previews supply the post media of a single campaign: without Storage no
// previews are signed, without Previews (image-service) only media that Figma
// takes as stored get one.
type FigmaPluginDeps struct {
	Pairing       *plugins.Service
	AppBaseURL    string
	Tokens        repository.PluginTokenRepository
	Users         repository.UserRepository
	Posts         repository.PostRepository
	Platforms     repository.PlatformRepository
	Assets        *AssetsHandler
	Attachments   *PostAttachmentsHandler
	Activity      *activity.Recorder
	MaxVideoBytes int64

	PostAttachments repository.PostAttachmentRepository
	MediaPreviews   repository.MediaPreviewRepository
	Storage         storage.Storage
	Previews        PreviewRenderer
}

func NewFigmaPluginHandler(d FigmaPluginDeps) *FigmaPluginHandler {
	return &FigmaPluginHandler{
		pairing:     d.Pairing,
		appBaseURL:  strings.TrimRight(d.AppBaseURL, "/"),
		auth:        RequirePluginToken(d.Tokens, d.Users),
		users:       d.Users,
		posts:       d.Posts,
		platforms:   d.Platforms,
		assets:      d.Assets,
		attachments: d.Attachments,
		activity:    d.Activity,
		media: &pluginMediaResolver{
			attachments: d.PostAttachments, previews: d.MediaPreviews, store: d.Storage, renderer: d.Previews,
		},
		maxPluginVideoBytes: d.MaxVideoBytes,
		startLimiter:        newKeyedRateLimiter(pairingStartsPerMinute, time.Minute),
		pollLimiter:         newKeyedRateLimiter(pairingPollsPerMinute, time.Minute),
		tokenLimiter:        newKeyedRateLimiter(pluginRequestsPerMinute, time.Minute),
	}
}

func (h *FigmaPluginHandler) Register(app *fiber.App) {
	g := app.Group(PluginRoutePrefix + "/figma")
	g.Post("/pairings", h.StartPairing)
	g.Get("/pairings/:read_key", h.PollPairing)

	authed := []fiber.Handler{h.auth, h.limitPerToken}
	g.Get("/me", append(authed, h.Me)...)
	g.Get("/posts", append(authed, h.ListPosts)...)
	g.Get("/campaigns", append(authed, h.ListCampaigns)...)
	g.Get("/campaigns/:id", append(authed, h.GetCampaign)...)
	g.Post("/images", append(authed, h.SendImage)...)
	g.Post("/posts/:post_id/videos/presign", append(authed, h.PresignVideo)...)
	g.Post("/posts/:post_id/videos/finalize", append(authed, h.FinalizeVideo)...)
	g.Delete("/token", append(authed, h.RevokeToken)...)
}

type startPairingRequest struct {
	ClientLabel string `json:"client_label"`
}

type startPairingResponse struct {
	ReadKey        string    `json:"read_key"`
	WriteKey       string    `json:"write_key"`
	ApproveURL     string    `json:"approve_url"`
	ExpiresAt      time.Time `json:"expires_at"`
	PollIntervalMS int64     `json:"poll_interval_ms"`
}

type pairingStatusResponse struct {
	Status string `json:"status"`
}

type pluginWorkspace struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type pluginUser struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email,omitempty"`
}

type collectPairingResponse struct {
	Token     string          `json:"token"`
	Workspace pluginWorkspace `json:"workspace"`
	User      pluginUser      `json:"user"`
}

// StartPairing godoc
// @Summary     Open a plugin pairing
// @Description Returns a read key the plugin keeps and polls with, and an approve_url (carrying the write key) to open in the browser. Expires after 10 minutes. Rate limited per client IP.
// @Tags        plugins
// @Accept      json
// @Produce     json
// @Param       body body startPairingRequest true "label shown on the approval page, 1-80 characters"
// @Success     201 {object} startPairingResponse
// @Failure     400 {object} map[string]string
// @Failure     429 {object} map[string]string
// @Router      /api/plugins/figma/pairings [post]
func (h *FigmaPluginHandler) StartPairing(c *fiber.Ctx) error {
	// Cloned: the limiter keeps the key past this request's buffers.
	ip := strings.Clone(c.IP())
	if ok, retry := h.startLimiter.allow(ip); !ok {
		return tooManyRequests(c, retry, "too many pairing attempts, try again shortly")
	}
	var req startPairingRequest
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	started, err := h.pairing.Start(reqCtx(c), models.PluginClientFigma, req.ClientLabel, ip)
	if errors.Is(err, plugins.ErrInvalidLabel) {
		return rejectCode(c, fiber.StatusBadRequest, CodeInvalidLabel, err.Error())
	}
	if err != nil {
		return err
	}
	return c.Status(fiber.StatusCreated).JSON(startPairingResponse{
		ReadKey:        started.ReadKey,
		WriteKey:       started.WriteKey,
		ApproveURL:     h.appBaseURL + "/integrations/figma/connect?key=" + url.QueryEscape(started.WriteKey),
		ExpiresAt:      started.ExpiresAt,
		PollIntervalMS: pairingPollInterval.Milliseconds(),
	})
}

// PollPairing godoc
// @Summary     Poll a plugin pairing
// @Description 202 while waiting for approval; 200 with the plugin token exactly once after approval (the pairing is then gone); 403 pairing_denied; 410 pairing_expired for an expired, unknown or already-collected key. Rate limited per read key.
// @Tags        plugins
// @Produce     json
// @Param       read_key path string true "read key from POST /pairings"
// @Success     200 {object} collectPairingResponse
// @Success     202 {object} pairingStatusResponse
// @Failure     403 {object} map[string]string
// @Failure     410 {object} map[string]string
// @Failure     429 {object} map[string]string
// @Router      /api/plugins/figma/pairings/{read_key} [get]
func (h *FigmaPluginHandler) PollPairing(c *fiber.Ctx) error {
	readKey := strings.Clone(c.Params("read_key"))
	if ok, retry := h.pollLimiter.allow(readKey); !ok {
		return tooManyRequests(c, retry, "polling too fast")
	}
	res, err := h.pairing.Collect(reqCtx(c), readKey)
	if err != nil {
		return pairingError(c, err)
	}
	if res.Pending {
		return c.Status(fiber.StatusAccepted).JSON(pairingStatusResponse{Status: models.PluginPairingPending})
	}
	workspace := pluginWorkspace{ID: res.Connection.TenantID}
	if res.Workspace != nil {
		workspace.Name = res.Workspace.Name
	}
	return c.JSON(collectPairingResponse{
		Token:     res.Token,
		Workspace: workspace,
		User:      pluginUser{ID: res.User.ID, Name: res.User.Name, Email: res.User.Email},
	})
}

// pairingError maps the pairing use case's errors to coded responses.
func pairingError(c *fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, plugins.ErrPairingGone):
		return rejectCode(c, fiber.StatusGone, CodePairingExpired, "pairing expired or already used, start again")
	case errors.Is(err, plugins.ErrPairingDenied):
		return rejectCode(c, fiber.StatusForbidden, CodePairingDenied, "the connection was declined")
	case errors.Is(err, plugins.ErrPairingNotPending):
		return rejectCode(c, fiber.StatusConflict, CodePairingNotPending, "this connection request was already answered")
	}
	return err
}
