package handlers

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/valyala/fasthttp"

	"github.com/ogen-app/ogen/src/domain/entitlements"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/campaign_assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/content_plan"
	"github.com/ogen-app/ogen/src/genkit/flows/enrich_brief"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/campaigngoal"
	"github.com/ogen-app/ogen/src/usecase/scheduling"
	"github.com/ogen-app/ogen/src/usecase/settings"
)

var validStatuses = map[models.CampaignStatus]bool{
	models.StatusDraft:     true,
	models.StatusScheduled: true,
	models.StatusActive:    true,
	models.StatusPaused:    true,
	models.StatusCompleted: true,
	models.StatusArchived:  true,
}

type CampaignsHandler struct {
	repo             repository.CampaignRepository
	campaignTypeRepo repository.CampaignTypeRepository
	limiter          *entitlements.Limiter // CON-295 entitlement quota gate (nil-safe)
	// brandRepo validates campaign brand_voice_id/brand_audience_id belong to
	// the tenant (CON-245). Optional (SetBrandRepo); nil skips validation.
	brandRepo     repository.BrandRepository
	auth          fiber.Handler
	generateDraft func(ctx context.Context, campaignID string, onEvent content_plan.OnEventFunc) (*content_plan.ContentPlanResponse, error)
	// isContentPlanReady reports whether the underlying Anthropic key
	// is currently configured. Decoupled from generateDraft so we can
	// return 503 before opening the SSE stream rather than emitting
	// an error event mid-stream when the runtime is unavailable.
	// May be nil in tests; nil is treated as "always available" so
	// existing fixture wiring keeps working. The same readiness gate
	// covers enrichBrief — both flows live behind the one Anthropic key.
	isContentPlanReady func() bool
	// enrichBrief streams an AI-generated campaign brief (CON-56). nil
	// when the feature is unwired (e.g. tests) → the handler returns 503.
	enrichBrief func(ctx context.Context, req enrich_brief.EnrichBriefRequest, onEvent enrich_brief.OnEventFunc) (*enrich_brief.EnrichBriefResponse, error)
	// messageRepo persists the Campaign Assistant conversation (CON-112). nil
	// in tests that don't exercise the assistant.
	messageRepo repository.CampaignAssistantMessageRepository
	// assistant is the Campaign Assistant flow callback (CON-112). nil when
	// unwired (e.g. tests) → the assistant/messages endpoints return 503.
	// Readiness is gated by isContentPlanReady, the same Anthropic-key gate.
	assistant func(ctx context.Context, req campaign_assistant.CampaignAssistantRequest, onEvent campaign_assistant.OnEventFunc) (*campaign_assistant.CampaignAssistantResponse, error)
	// activity records CON-125 user-activity events (campaign_created,
	// content_generated, …). nil is a no-op (analytics disabled / fixtures).
	// Wired via SetActivityRecorder.
	activity *activity.Recorder
}

// SetBrandRepo wires the CON-245 brand repository so campaign brand refs can be
// tenant-validated. Optional; nil skips validation.
func (h *CampaignsHandler) SetBrandRepo(r repository.BrandRepository) {
	h.brandRepo = r
}

// SetLimiter wires the CON-295 entitlement limiter (nil-safe no-op).
func (h *CampaignsHandler) SetLimiter(l *entitlements.Limiter) { h.limiter = l }

// baselineCampaignTypeSlug is the one system campaign type every tier can use;
// the other system types are gated by all_campaign_types (CON-295).
const baselineCampaignTypeSlug = "evergreen"

// gateCampaignType enforces the campaign-type entitlement gates for the selected
// type: a custom (non-system) type requires custom_campaign_types, a non-baseline
// system type requires all_campaign_types. The Evergreen baseline is always
// allowed. Returns a *FeatureNotAvailableError (rendered 403) when gated off.
func (h *CampaignsHandler) gateCampaignType(c *fiber.Ctx, ct *models.CampaignType) error {
	tenantID, ok := tenantctx.From(reqCtx(c))
	if !ok {
		return nil
	}
	switch {
	case !ct.IsSystem:
		return h.limiter.RequireGate(reqCtx(c), tenantID, "custom_campaign_types")
	case ct.Name != baselineCampaignTypeSlug:
		return h.limiter.RequireGate(reqCtx(c), tenantID, "all_campaign_types")
	}
	return nil
}

// SetActivityRecorder wires the CON-125 activity recorder. nil (analytics
// disabled) makes every activity emission a no-op.
func (h *CampaignsHandler) SetActivityRecorder(r *activity.Recorder) {
	h.activity = r
}

// recordActivity emits a best-effort CON-125 activity event. Tenant + user are
// resolved from the request context (set by the auth middleware); source
// defaults to api and can be overridden by a later option.
func (h *CampaignsHandler) recordActivity(c *fiber.Ctx, category, typ string, opts ...activity.Option) {
	h.activity.Record(reqCtx(c), category, typ,
		append([]activity.Option{activity.WithSource(activity.SourceAPI)}, opts...)...)
}

// timePtrEqual compares two optional timestamps by value (the fields are
// *time.Time, so == would compare pointers).
func timePtrEqual(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

func NewCampaignsHandler(
	repo repository.CampaignRepository,
	campaignTypeRepo repository.CampaignTypeRepository,
	auth fiber.Handler,
	generateDraft func(ctx context.Context, campaignID string, onEvent content_plan.OnEventFunc) (*content_plan.ContentPlanResponse, error),
	isContentPlanReady func() bool,
	enrichBrief func(ctx context.Context, req enrich_brief.EnrichBriefRequest, onEvent enrich_brief.OnEventFunc) (*enrich_brief.EnrichBriefResponse, error),
	messageRepo repository.CampaignAssistantMessageRepository,
	assistant func(ctx context.Context, req campaign_assistant.CampaignAssistantRequest, onEvent campaign_assistant.OnEventFunc) (*campaign_assistant.CampaignAssistantResponse, error),
) *CampaignsHandler {
	return &CampaignsHandler{
		repo:               repo,
		campaignTypeRepo:   campaignTypeRepo,
		auth:               auth,
		generateDraft:      generateDraft,
		isContentPlanReady: isContentPlanReady,
		enrichBrief:        enrichBrief,
		messageRepo:        messageRepo,
		assistant:          assistant,
	}
}

func (h *CampaignsHandler) Register(app *fiber.App) {
	g := app.Group("/api/campaigns")
	g.Get("/", h.auth, h.List)
	g.Post("/", h.auth, h.Create)
	g.Get("/:id", h.auth, h.Get)
	g.Put("/:id", h.auth, h.Update)
	g.Delete("/:id", h.auth, h.Delete)
	// CON-156 (BE 6): campaign lifecycle. Archive removes a campaign from the
	// active list (reversible); unarchive returns it. Both tenant-scoped.
	g.Post("/:id/archive", h.auth, h.Archive)
	g.Post("/:id/unarchive", h.auth, h.Unarchive)
	// CON-233: targeted membership writes for the content-bank set, so attaching
	// or detaching one document touches only asset_ids (no full-record PUT, no
	// omitted-field reset, atomic under concurrent adds).
	g.Post("/:id/assets", h.auth, h.AddAssets)
	g.Delete("/:id/assets/:assetId", h.auth, h.RemoveAsset)
	g.Post("/:id/generate-draft", h.auth, h.GenerateDraft)
	g.Post("/:id/enrich-brief", h.auth, h.EnrichBrief)
	// CON-112: Campaign Assistant chat. Tenant-scoped only — any user who can
	// see the campaign can use the assistant (no owner-only guard).
	g.Post("/:id/assistant", h.auth, h.Assistant)
	g.Get("/:id/messages", h.auth, h.ListMessages)
}

type campaignRequest struct {
	Name           string `json:"name"                validate:"required"`
	Description    string `json:"description"`
	TargetPersona  string `json:"target_persona"`
	KeyMessages    string `json:"key_messages"`
	ToneGuidelines string `json:"tone_guidelines"`
	// Presence-aware (CON-245): omitting a brand ref on a full-replace save
	// leaves the stored value alone; an explicit null clears it. See Optional.
	BrandVoiceID    Optional[string] `json:"brand_voice_id"`
	BrandAudienceID Optional[string] `json:"brand_audience_id"`
	// UseAssets / AssetIDs are presence-aware (CON-233): the content-bank set has
	// its own membership endpoints (POST/DELETE /campaigns/:id/assets) that keep
	// use_assets derived from it, so an ordinary whole-record save that omits
	// these must leave them alone rather than restate the set (and reset the flag)
	// over an in-flight membership write. Present replaces; explicit null on
	// asset_ids clears. See Optional, applyToValue and applyOptionalSlice.
	UseAssets          Optional[bool]               `json:"use_assets"`
	AssetIDs           Optional[models.StringSlice] `json:"asset_ids"`
	TargetPlatforms    models.CampaignPlatforms     `json:"target_platforms"`
	CampaignTypeID     string                       `json:"campaign_type_id"    validate:"required"`
	Status             models.CampaignStatus        `json:"status"`
	StartDate          *time.Time                   `json:"start_date"`
	EndDate            *time.Time                   `json:"end_date"`
	EstimatedPostCount *int                         `json:"estimated_post_count"`
	Budget             *float64                     `json:"budget"`
	Currency           string                       `json:"currency"`
	Language           string                       `json:"language"`
	TagIDs             models.StringSlice           `json:"tag_ids"`
	// Scheduling settings (CON-181). All optional; omitted fields fall back to
	// defaults (09:00 / UTC / every day / ±15 min) via normalizeScheduling.
	PublishingTime string             `json:"publishing_time"`
	Timezone       string             `json:"timezone"`
	PublishingDays models.StringSlice `json:"publishing_days"`
	SpreadMinutes  *int               `json:"spread_minutes"`
	// Goal cadence (CON-182): "week" | "month". Empty falls back to "month".
	// estimated_post_count is the target posts per one of these periods.
	GoalCadence string `json:"goal_cadence"`
}

// normalizeScheduling validates the request's scheduling fields and returns the
// effective values with defaults applied. A validation failure is returned as a
// 400-worthy error.
func (r *campaignRequest) normalizeScheduling() (publishingTime string, timezone string, days models.StringSlice, spread int, err error) {
	publishingTime = strings.TrimSpace(r.PublishingTime)
	if publishingTime == "" {
		publishingTime = scheduling.DefaultPublishingTime
	} else if !scheduling.ValidClock(publishingTime) {
		return "", "", nil, 0, fmt.Errorf("publishing_time must be HH:MM (24-hour), got %q", r.PublishingTime)
	}

	timezone = strings.TrimSpace(r.Timezone)
	if timezone != "" {
		if _, tzErr := settings.ResolveTimezone(timezone); tzErr != nil {
			return "", "", nil, 0, fmt.Errorf("invalid timezone: %s", timezone)
		}
	}

	if len(r.PublishingDays) == 0 {
		days = scheduling.DefaultPublishingDays()
	} else {
		seen := make(map[string]bool, len(r.PublishingDays))
		days = make(models.StringSlice, 0, len(r.PublishingDays))
		for _, d := range r.PublishingDays {
			tok := strings.ToLower(strings.TrimSpace(d))
			if !scheduling.ValidWeekday(tok) {
				return "", "", nil, 0, fmt.Errorf("invalid publishing day: %q", d)
			}
			if seen[tok] {
				return "", "", nil, 0, fmt.Errorf("duplicate publishing day: %q", tok)
			}
			seen[tok] = true
			days = append(days, tok)
		}
	}

	spread = scheduling.DefaultSpreadMinutes
	if r.SpreadMinutes != nil {
		spread = *r.SpreadMinutes
		if spread < 0 || spread > scheduling.MaxSpreadMinutes {
			return "", "", nil, 0, fmt.Errorf("spread_minutes must be between 0 and %d", scheduling.MaxSpreadMinutes)
		}
	}
	return publishingTime, timezone, days, spread, nil
}

// toStatus resolves the campaign's status, defaulting to active. CON-156 BE 6:
// draft is not a user-facing distinction (nothing behaves differently), so a
// campaign with no explicit status is created active rather than draft. Existing
// draft campaigns keep working — nothing in the backend branches on the value.
func (r *campaignRequest) toStatus() models.CampaignStatus {
	if r.Status == "" {
		return models.StatusActive
	}
	return r.Status
}

// List godoc
// @Summary      List campaigns
// @Description  Returns the active set (neither archived nor deleted) ordered by creation date. Pass ?archived=true to list archived campaigns instead.
// @Tags         campaigns
// @Produce      json
// @Security     CookieAuth
// @Param        archived  query     bool  false  "List archived campaigns instead of the active set"
// @Success      200  {array}   models.Campaign
// @Failure      401  {object}  map[string]string
// @Router       /api/campaigns [get]
func (h *CampaignsHandler) List(c *fiber.Ctx) error {
	list := h.repo.List
	if c.QueryBool("archived", false) {
		list = h.repo.ListArchived
	}
	campaigns, err := list(reqCtx(c))
	if err != nil {
		return err
	}
	return c.JSON(campaigns)
}

// Create godoc
// @Summary      Create campaign
// @Description  Creates a new campaign. The created_by field is set from the authenticated session.
// @Tags         campaigns
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        body  body      campaignRequest  true  "Campaign payload"
// @Success      201   {object}  models.Campaign
// @Failure      400   {object}  map[string]string
// @Failure      401   {object}  map[string]string
// @Router       /api/campaigns [post]
func (h *CampaignsHandler) Create(c *fiber.Ctx) error {
	var req campaignRequest
	if err := bindAndValidate(c, &req); err != nil {
		return err
	}
	status := req.toStatus()
	if !validStatuses[status] {
		return fiber.NewError(fiber.StatusBadRequest, "invalid status")
	}
	// CON-295: the active_campaigns quota gates a new campaign.
	var campaignQuota entitlements.Decision
	tenantID, hasTenant := tenantctx.From(reqCtx(c))
	if hasTenant {
		dec, qErr := h.limiter.Require(reqCtx(c), tenantID, "active_campaigns")
		if qErr != nil {
			return qErr
		}
		campaignQuota = dec
	}
	campaignType, err := h.campaignTypeRepo.GetByID(reqCtx(c), req.CampaignTypeID)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid campaign_type_id")
	}
	// CON-295: custom / non-baseline campaign types are entitlement-gated.
	if err := h.gateCampaignType(c, campaignType); err != nil {
		return err
	}
	publishingTime, timezone, publishingDays, spread, err := req.normalizeScheduling()
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	goalCadence, err := campaigngoal.Normalize(strings.TrimSpace(req.GoalCadence))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}

	session := c.Locals("session").(*models.Session)

	id, err := models.NewID()
	if err != nil {
		return err
	}

	if err := validateBrandRefs(reqCtx(c), h.brandRepo, req.BrandVoiceID.Value, req.BrandAudienceID.Value); err != nil {
		return err
	}

	campaign := &models.Campaign{
		ID:                 id,
		Name:               req.Name,
		Description:        req.Description,
		TargetPersona:      req.TargetPersona,
		KeyMessages:        req.KeyMessages,
		ToneGuidelines:     req.ToneGuidelines,
		BrandVoiceID:       req.BrandVoiceID.Value,
		BrandAudienceID:    req.BrandAudienceID.Value,
		UseAssets:          req.UseAssets.orZero(),
		AssetIDs:           nullSlice(req.AssetIDs.orZero()),
		TargetPlatforms:    nullCampaignPlatforms(req.TargetPlatforms),
		CampaignTypeID:     req.CampaignTypeID,
		Status:             status,
		EstimatedPostCount: req.EstimatedPostCount,
		StartDate:          req.StartDate,
		EndDate:            req.EndDate,
		Budget:             req.Budget,
		Currency:           req.Currency,
		Language:           req.Language,
		TagIDs:             nullSlice(req.TagIDs),
		Tags:               []models.Tag{},
		PublishingTime:     publishingTime,
		Timezone:           timezone,
		PublishingDays:     publishingDays,
		SpreadMinutes:      spread,
		GoalCadence:        goalCadence,
		CreatedBy:          session.UserID,
	}
	if err := h.repo.Create(reqCtx(c), campaign); err != nil {
		return err
	}
	// CON-295: the campaign now exists — fire any near-limit crossing.
	if hasTenant {
		h.limiter.DispatchCrossing(reqCtx(c), tenantID, campaignQuota)
	}
	h.recordActivity(c, activity.CategoryCampaign, "campaign_created",
		activity.WithEntity("campaign", campaign.ID),
		activity.WithPayload(map[string]any{"status": string(campaign.Status), "campaign_type_id": campaign.CampaignTypeID}),
	)
	return c.Status(fiber.StatusCreated).JSON(campaign)
}

// Get godoc
// @Summary      Get campaign
// @Description  Returns a single campaign by Sqid.
// @Tags         campaigns
// @Produce      json
// @Security     CookieAuth
// @Param        id   path      string  true  "Campaign Sqid"
// @Success      200  {object}  models.Campaign
// @Failure      401  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Router       /api/campaigns/{id} [get]
func (h *CampaignsHandler) Get(c *fiber.Ctx) error {
	campaign, err := h.repo.GetByID(reqCtx(c), c.Params("id"))
	if err != nil {
		return notFound(err, "campaign not found")
	}
	return c.JSON(campaign)
}

// Update godoc
// @Summary      Update campaign
// @Description  Replaces all mutable fields of an existing campaign.
// @Tags         campaigns
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        id    path      string          true  "Campaign Sqid"
// @Param        body  body      campaignRequest true  "Campaign payload"
// @Success      200   {object}  models.Campaign
// @Failure      400   {object}  map[string]string
// @Failure      401   {object}  map[string]string
// @Failure      404   {object}  map[string]string
// @Router       /api/campaigns/{id} [put]
func (h *CampaignsHandler) Update(c *fiber.Ctx) error {
	var req campaignRequest
	if err := bindAndValidate(c, &req); err != nil {
		return err
	}
	status := req.toStatus()
	if !validStatuses[status] {
		return fiber.NewError(fiber.StatusBadRequest, "invalid status")
	}
	campaignType, err := h.campaignTypeRepo.GetByID(reqCtx(c), req.CampaignTypeID)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid campaign_type_id")
	}
	publishingTime, timezone, publishingDays, spread, err := req.normalizeScheduling()
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	goalCadence, err := campaigngoal.Normalize(strings.TrimSpace(req.GoalCadence))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}

	if err := validateBrandRefs(reqCtx(c), h.brandRepo, req.BrandVoiceID.Value, req.BrandAudienceID.Value); err != nil {
		return err
	}

	campaign, err := h.repo.GetByID(reqCtx(c), c.Params("id"))
	if err != nil {
		return notFound(err, "campaign not found")
	}

	// CON-295: only gate when the caller is switching to a gated campaign type,
	// so an unrelated edit of a campaign that already uses one is never blocked.
	if req.CampaignTypeID != campaign.CampaignTypeID {
		if err := h.gateCampaignType(c, campaignType); err != nil {
			return err
		}
	}

	// Snapshot the fields we emit change-events for before overwriting them.
	prevStatus := campaign.Status
	prevStart, prevEnd := campaign.StartDate, campaign.EndDate

	campaign.Name = req.Name
	campaign.Description = req.Description
	campaign.TargetPersona = req.TargetPersona
	campaign.KeyMessages = req.KeyMessages
	campaign.ToneGuidelines = req.ToneGuidelines
	// Presence-aware (CON-245): omit to leave alone, explicit null to clear.
	req.BrandVoiceID.applyTo(&campaign.BrandVoiceID)
	req.BrandAudienceID.applyTo(&campaign.BrandAudienceID)
	// Presence-aware (CON-233): omit to leave the content-bank set + derived flag
	// alone (the membership endpoints own them), present to replace, explicit null
	// on asset_ids to clear.
	req.UseAssets.applyToValue(&campaign.UseAssets)
	applyOptionalSlice(req.AssetIDs, &campaign.AssetIDs)
	campaign.TargetPlatforms = nullCampaignPlatforms(req.TargetPlatforms)
	campaign.CampaignTypeID = req.CampaignTypeID
	campaign.Status = status
	campaign.EstimatedPostCount = req.EstimatedPostCount
	campaign.StartDate = req.StartDate
	campaign.EndDate = req.EndDate
	campaign.Budget = req.Budget
	campaign.Currency = req.Currency
	campaign.Language = req.Language
	campaign.TagIDs = nullSlice(req.TagIDs)
	campaign.PublishingTime = publishingTime
	campaign.Timezone = timezone
	campaign.PublishingDays = publishingDays
	campaign.SpreadMinutes = spread
	campaign.GoalCadence = goalCadence
	campaign.UpdatedAt = time.Now().UTC()

	// Presence-aware content-bank fields (CON-233): the in-memory apply above
	// already left an omitted field at its hydrated value, but the whole-record
	// UPDATE would still write that stale value back and clobber a concurrent
	// membership write (AddAssetIDs/RemoveAssetID) that landed after GetByID.
	// Drop the omitted columns from the write so "leave alone" holds at the DB.
	var omit []string
	if !req.AssetIDs.Present {
		omit = append(omit, "asset_ids")
	}
	if !(req.UseAssets.Present && req.UseAssets.Value != nil) {
		omit = append(omit, "use_assets")
	}
	if err := h.repo.Update(reqCtx(c), campaign, omit...); err != nil {
		return err
	}
	h.recordActivity(c, activity.CategoryCampaign, "campaign_updated",
		activity.WithEntity("campaign", campaign.ID),
		activity.WithStatus(string(campaign.Status)),
	)
	if prevStatus != campaign.Status {
		h.recordActivity(c, activity.CategoryCampaign, "campaign_status_changed",
			activity.WithEntity("campaign", campaign.ID),
			activity.WithStatus(string(prevStatus)+"->"+string(campaign.Status)),
		)
	}
	if !timePtrEqual(prevStart, campaign.StartDate) || !timePtrEqual(prevEnd, campaign.EndDate) {
		h.recordActivity(c, activity.CategoryCampaign, "campaign_dates_changed",
			activity.WithEntity("campaign", campaign.ID),
		)
	}
	return c.JSON(campaign)
}

// assetMembershipRequest is the body of the CON-233 membership-add endpoints on
// both campaigns and posts: only the ids to attach, so the client never restates
// the whole record.
type assetMembershipRequest struct {
	AssetIDs models.StringSlice `json:"asset_ids"`
}

// AddAssets godoc
// @Summary      Attach content-bank assets to a campaign
// @Description  Unions the given asset ids into the campaign's content-bank set
// @Description  (campaigns.asset_ids) and turns use_assets on, touching no other
// @Description  field. The write is a single atomic UPDATE, so concurrent
// @Description  attaches of different documents both survive and omitted fields
// @Description  are never reset (CON-233). Adding an already-present id is a
// @Description  no-op. Returns the updated campaign.
// @Tags         campaigns
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        id    path      string                 true  "Campaign Sqid"
// @Param        body  body      assetMembershipRequest true  "Asset ids to attach"
// @Success      200   {object}  models.Campaign
// @Failure      400   {object}  map[string]string
// @Failure      401   {object}  map[string]string
// @Failure      404   {object}  map[string]string
// @Router       /api/campaigns/{id}/assets [post]
func (h *CampaignsHandler) AddAssets(c *fiber.Ctx) error {
	var req assetMembershipRequest
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	campaign, err := h.repo.AddAssetIDs(reqCtx(c), c.Params("id"), req.AssetIDs)
	if err != nil {
		return notFound(err, "campaign not found")
	}
	h.recordActivity(c, activity.CategoryCampaign, "campaign_updated",
		activity.WithEntity("campaign", campaign.ID),
		activity.WithStatus(string(campaign.Status)),
	)
	return c.JSON(campaign)
}

// RemoveAsset godoc
// @Summary      Detach a content-bank asset from a campaign
// @Description  Removes one asset id from the campaign's content-bank set
// @Description  (campaigns.asset_ids) and re-derives use_assets from it, so
// @Description  detaching the last source turns use_assets off (CON-233).
// @Description  Removing an id that is not present is a no-op. Returns the
// @Description  updated campaign.
// @Tags         campaigns
// @Produce      json
// @Security     CookieAuth
// @Param        id       path      string  true  "Campaign Sqid"
// @Param        assetId  path      string  true  "Asset Sqid to detach"
// @Success      200      {object}  models.Campaign
// @Failure      401      {object}  map[string]string
// @Failure      404      {object}  map[string]string
// @Router       /api/campaigns/{id}/assets/{assetId} [delete]
func (h *CampaignsHandler) RemoveAsset(c *fiber.Ctx) error {
	campaign, err := h.repo.RemoveAssetID(reqCtx(c), c.Params("id"), c.Params("assetId"))
	if err != nil {
		return notFound(err, "campaign not found")
	}
	h.recordActivity(c, activity.CategoryCampaign, "campaign_updated",
		activity.WithEntity("campaign", campaign.ID),
		activity.WithStatus(string(campaign.Status)),
	)
	return c.JSON(campaign)
}

// Delete godoc
// @Summary      Delete campaign
// @Description  Soft-deletes a campaign by Sqid. The row is retained as a safety net (no self-serve restore); it disappears from lists and reads.
// @Tags         campaigns
// @Security     CookieAuth
// @Param        id   path  string  true  "Campaign Sqid"
// @Success      204
// @Failure      401  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Router       /api/campaigns/{id} [delete]
func (h *CampaignsHandler) Delete(c *fiber.Ctx) error {
	deleted, err := h.repo.Delete(reqCtx(c), c.Params("id"))
	if err != nil {
		return err
	}
	if !deleted {
		return fiber.NewError(fiber.StatusNotFound, "campaign not found")
	}
	h.recordActivity(c, activity.CategoryCampaign, "campaign_deleted",
		activity.WithEntity("campaign", c.Params("id")),
	)
	return c.SendStatus(fiber.StatusNoContent)
}

// Archive godoc
// @Summary      Archive campaign
// @Description  Removes a campaign from the active list. Reversible via unarchive.
// @Tags         campaigns
// @Security     CookieAuth
// @Param        id   path  string  true  "Campaign Sqid"
// @Success      204
// @Failure      401  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Router       /api/campaigns/{id}/archive [post]
func (h *CampaignsHandler) Archive(c *fiber.Ctx) error {
	ok, err := h.repo.Archive(reqCtx(c), c.Params("id"))
	if err != nil {
		return err
	}
	if !ok {
		return fiber.NewError(fiber.StatusNotFound, "campaign not found")
	}
	h.recordActivity(c, activity.CategoryCampaign, "campaign_archived",
		activity.WithEntity("campaign", c.Params("id")),
	)
	return c.SendStatus(fiber.StatusNoContent)
}

// Unarchive godoc
// @Summary      Unarchive campaign
// @Description  Returns an archived campaign to the active list.
// @Tags         campaigns
// @Security     CookieAuth
// @Param        id   path  string  true  "Campaign Sqid"
// @Success      204
// @Failure      401  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Router       /api/campaigns/{id}/unarchive [post]
func (h *CampaignsHandler) Unarchive(c *fiber.Ctx) error {
	ok, err := h.repo.Unarchive(reqCtx(c), c.Params("id"))
	if err != nil {
		return err
	}
	if !ok {
		return fiber.NewError(fiber.StatusNotFound, "campaign not found")
	}
	h.recordActivity(c, activity.CategoryCampaign, "campaign_unarchived",
		activity.WithEntity("campaign", c.Params("id")),
	)
	return c.SendStatus(fiber.StatusNoContent)
}

// GenerateDraft godoc
// @Summary      Generate draft posts (SSE)
// @Description  Calls the AI content plan flow and streams progress via Server-Sent Events.
// @Description  Each step emits an SSE event of type "step" with payload {"step":"<name>","status":"done"}.
// @Description  On success a final "complete" event carries the full ContentPlanResponse payload.
// @Description  On failure an "error" event carries {"message":"<text>","code":<http_code>}.
// @Tags         campaigns
// @Produce      text/event-stream
// @Security     CookieAuth
// @Param        id   path  string  true  "Campaign Sqid"
// @Success      200  "SSE stream: step / complete / error events"
// @Failure      401  {object}  map[string]string
// @Failure      403  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      503  {object}  map[string]string
// @Router       /api/campaigns/{id}/generate-draft [post]
func (h *CampaignsHandler) GenerateDraft(c *fiber.Ctx) error {
	if h.generateDraft == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "content plan feature is not enabled")
	}
	if h.isContentPlanReady != nil && !h.isContentPlanReady() {
		return fiber.NewError(fiber.StatusServiceUnavailable, "content plan feature is not enabled")
	}

	campaign, err := h.repo.GetByID(reqCtx(c), c.Params("id"))
	if err != nil {
		return notFound(err, "campaign not found")
	}

	session := c.Locals("session").(*models.Session)
	if campaign.CreatedBy != session.UserID {
		return fiber.NewError(fiber.StatusForbidden, "forbidden")
	}

	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")
	c.Set("X-Accel-Buffering", "no")

	h.recordActivity(c, activity.CategoryCampaign, "content_generated",
		activity.WithEntity("campaign", campaign.ID),
		activity.WithPayload(map[string]any{"mode": "generate_draft"}),
	)

	campaignID := campaign.ID
	generateDraft := h.generateDraft
	// Carry the tenant into the detached flow context (the StreamWriter runs
	// after this handler returns) so usage recording + enforcement attribute
	// to the right tenant (CON-86).
	flowCtx := detachedContext(c, session.TenantID)

	c.Context().SetBodyStreamWriter(fasthttp.StreamWriter(func(w *bufio.Writer) {
		writeEvent := func(event string, data any) {
			b, _ := json.Marshal(data)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
			_ = w.Flush()
		}

		onEvent := content_plan.OnEventFunc(func(name content_plan.SSEEventKind, data any) {
			writeEvent(string(name), data)
		})

		resp, err := generateDraft(flowCtx, campaignID, onEvent)
		if err != nil {
			code := fiber.StatusInternalServerError
			msg := err.Error()
			var ve *content_plan.ValidationError
			var ae *content_plan.AIError
			switch {
			case errors.As(err, &ve):
				code = fiber.StatusBadRequest
				msg = ve.Msg
			case errors.As(err, &ae):
				code = fiber.StatusBadGateway
				msg = ae.Msg
			}
			writeEvent(string(content_plan.SSEEventError), content_plan.ErrorEventPayload{Message: msg, Code: code})
			return
		}

		writeEvent(string(content_plan.SSEEventComplete), resp)
	}))

	return nil
}

// EnrichBrief godoc
// @Summary      Enrich campaign brief with AI (SSE)
// @Description  Generates a campaign brief (description, target persona, key messages, tone guidelines)
// @Description  from the campaign's title and type, streaming progress via Server-Sent Events.
// @Description  Per-field "*_delta" events preview each value as it is written; a final "complete"
// @Description  event carries the full EnrichBriefResponse and is the signal that the brief is ready.
// @Description  On failure an "error" event carries {"message":"<text>","code":<http_code>}.
// @Description  The brief is returned as a suggestion only — it is not persisted to the campaign.
// @Tags         campaigns
// @Accept       json
// @Produce      text/event-stream
// @Security     CookieAuth
// @Param        id    path  string  true  "Campaign Sqid"
// @Param        body  body  object  false "Optional steering: {\"instruction\":\"...\"}"
// @Success      200  "SSE stream: step / *_delta / complete / error events"
// @Failure      400  {object}  map[string]string
// @Failure      401  {object}  map[string]string
// @Failure      403  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      503  {object}  map[string]string
// @Router       /api/campaigns/{id}/enrich-brief [post]
func (h *CampaignsHandler) EnrichBrief(c *fiber.Ctx) error {
	if h.enrichBrief == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "brief enrichment feature is not enabled")
	}
	if h.isContentPlanReady != nil && !h.isContentPlanReady() {
		return fiber.NewError(fiber.StatusServiceUnavailable, "brief enrichment feature is not enabled")
	}

	// Body is optional; only parse when present so an empty POST is valid.
	var body struct {
		Instruction string `json:"instruction"`
	}
	if len(c.Body()) > 0 {
		if err := c.BodyParser(&body); err != nil {
			return fiber.NewError(fiber.StatusBadRequest, err.Error())
		}
	}

	campaign, err := h.repo.GetByID(reqCtx(c), c.Params("id"))
	if err != nil {
		return notFound(err, "campaign not found")
	}

	session := c.Locals("session").(*models.Session)
	if campaign.CreatedBy != session.UserID {
		return fiber.NewError(fiber.StatusForbidden, "forbidden")
	}

	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")
	c.Set("X-Accel-Buffering", "no")

	h.recordActivity(c, activity.CategoryCampaign, "brief_enriched",
		activity.WithEntity("campaign", campaign.ID),
	)

	req := enrich_brief.EnrichBriefRequest{CampaignID: campaign.ID, Instruction: body.Instruction}
	enrichBrief := h.enrichBrief
	flowCtx := detachedContext(c, session.TenantID)

	c.Context().SetBodyStreamWriter(fasthttp.StreamWriter(func(w *bufio.Writer) {
		writeEvent := func(event string, data any) {
			b, _ := json.Marshal(data)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
			_ = w.Flush()
		}

		onEvent := enrich_brief.OnEventFunc(func(name enrich_brief.SSEEventKind, data any) {
			writeEvent(string(name), data)
		})

		resp, err := enrichBrief(flowCtx, req, onEvent)
		if err != nil {
			code := fiber.StatusInternalServerError
			msg := err.Error()
			var ve *enrich_brief.ValidationError
			var ae *enrich_brief.AIError
			switch {
			case errors.As(err, &ve):
				code = fiber.StatusBadRequest
				msg = ve.Msg
			case errors.As(err, &ae):
				code = fiber.StatusBadGateway
				msg = ae.Msg
			}
			writeEvent(string(enrich_brief.SSEEventError), enrich_brief.ErrorEventPayload{Message: msg, Code: code})
			return
		}

		writeEvent(string(enrich_brief.SSEEventComplete), resp)
	}))

	return nil
}

// Assistant godoc
// @Summary      Campaign assistant (SSE)
// @Description  Sends an instruction to the Campaign Assistant and streams progress via Server-Sent Events.
// @Description  "explanation_delta" carries {"delta":"..."} fragments as the reply streams. "tool_call"/"tool_result"
// @Description  signal tool invocations. When a content plan runs, "content_plan_started", "content_plan_post"
// @Description  (etc.) and "content_plan_complete" are forwarded; when the brief is enriched, "enrich_brief_started",
// @Description  the per-field "enrich_brief_*_delta" events and "enrich_brief_complete" are forwarded. A final
// @Description  "complete" event carries the CampaignAssistantResponse. "error" carries {"message":"...","code":<http_code>}.
// @Description  Available to any user in the campaign's tenant.
// @Tags         campaigns
// @Accept       json
// @Produce      text/event-stream
// @Security     CookieAuth
// @Param        id    path      string           true  "Campaign Sqid"
// @Param        body  body      assistantRequest true  "Instruction payload"
// @Success      200  "SSE stream: delta / tool_call / tool_result / *_complete / complete / error events"
// @Failure      400   {object}  map[string]string
// @Failure      401   {object}  map[string]string
// @Failure      503   {object}  map[string]string
// @Router       /api/campaigns/{id}/assistant [post]
func (h *CampaignsHandler) Assistant(c *fiber.Ctx) error {
	if h.assistant == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "campaign assistant is not available")
	}
	if h.isContentPlanReady != nil && !h.isContentPlanReady() {
		return fiber.NewError(fiber.StatusServiceUnavailable, "campaign assistant is not available")
	}

	var req assistantRequest
	if err := bindAndValidate(c, &req); err != nil {
		return err
	}

	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")
	c.Set("X-Accel-Buffering", "no")

	h.recordActivity(c, activity.CategoryAIFlow, "campaign_assistant_turn",
		activity.WithEntity("campaign", c.Params("id")),
		activity.WithSource(activity.SourceAssistant),
	)

	// Copy the buffer-backed request values: the StreamWriter runs after this
	// handler returns, by which point fasthttp may have recycled the request
	// buffer into a concurrent request and corrupted them. See the post
	// assistant handler for the failure this prevents.
	campaignID := strings.Clone(c.Params("id"))
	instruction := strings.Clone(req.Instruction)
	assistant := h.assistant
	session := c.Locals("session").(*models.Session)
	// Carry the tenant into the detached flow context (the StreamWriter runs
	// after this handler returns) so the tenant-scoped campaign load, brief
	// write, and usage recording all attribute to the right tenant (CON-86/97).
	flowCtx := detachedContext(c, session.TenantID)

	c.Context().SetBodyStreamWriter(fasthttp.StreamWriter(func(w *bufio.Writer) {
		writeEvent := func(event string, data any) {
			b, _ := json.Marshal(data)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
			_ = w.Flush()
		}

		onEvent := campaign_assistant.OnEventFunc(func(name campaign_assistant.SSEEventKind, data any) {
			writeEvent(string(name), data)
		})

		_, err := assistant(flowCtx, campaign_assistant.CampaignAssistantRequest{
			CampaignID:  campaignID,
			Instruction: instruction,
		}, onEvent)
		if err != nil {
			code := fiber.StatusInternalServerError
			msg := err.Error()
			var ve *campaign_assistant.ValidationError
			var ae *campaign_assistant.AIError
			switch {
			case errors.As(err, &ve):
				code = fiber.StatusBadRequest
				msg = ve.Msg
			case errors.As(err, &ae):
				code = fiber.StatusBadGateway
				msg = ae.Msg
			}
			writeEvent(string(campaign_assistant.SSEEventError), campaign_assistant.ErrorEventPayload{Message: msg, Code: code})
			return
		}
		// "complete" is emitted by the runner itself; nothing to write here.
	}))

	return nil
}

// ListMessages godoc
// @Summary      List campaign assistant messages
// @Description  Returns the most recent Campaign Assistant conversation messages for a campaign.
// @Tags         campaigns
// @Produce      json
// @Security     CookieAuth
// @Param        id   path      string  true  "Campaign Sqid"
// @Success      200  {array}   models.CampaignAssistantMessage
// @Failure      401  {object}  map[string]string
// @Router       /api/campaigns/{id}/messages [get]
func (h *CampaignsHandler) ListMessages(c *fiber.Ctx) error {
	if h.messageRepo == nil {
		return c.JSON([]models.CampaignAssistantMessage{})
	}
	msgs, err := h.messageRepo.ListRecentByCampaignID(reqCtx(c), c.Params("id"), 50)
	if err != nil {
		return err
	}
	// Preserve the array contract: an empty history serializes as [] not null.
	if msgs == nil {
		msgs = []models.CampaignAssistantMessage{}
	}
	return c.JSON(msgs)
}

// nullSlice returns an empty StringSlice instead of nil so the JSON column
// always stores "[]" rather than null.
func nullSlice(s models.StringSlice) models.StringSlice {
	if s == nil {
		return models.StringSlice{}
	}
	return s
}

// nullCampaignPlatforms returns an empty CampaignPlatforms instead of nil.
func nullCampaignPlatforms(p models.CampaignPlatforms) models.CampaignPlatforms {
	if p == nil {
		return models.CampaignPlatforms{}
	}
	return p
}

// nullMap returns an empty PostTypeMap instead of nil so the JSON column
// always stores "{}" rather than null.
func nullMap(m models.PostTypeMap) models.PostTypeMap {
	if m == nil {
		return models.PostTypeMap{}
	}
	return m
}
