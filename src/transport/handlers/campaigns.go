package handlers

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

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
	// the tenant. nil skips validation.
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
	// enrichBrief streams an AI-generated campaign brief. nil
	// when the feature is unwired (e.g. tests) → the handler returns 503.
	enrichBrief func(ctx context.Context, req enrich_brief.EnrichBriefRequest, onEvent enrich_brief.OnEventFunc) (*enrich_brief.EnrichBriefResponse, error)
	// messageRepo persists the Campaign Assistant conversation. nil
	// in tests that don't exercise the assistant.
	messageRepo repository.CampaignAssistantMessageRepository
	// assistant is the Campaign Assistant flow callback. nil when
	// unwired (e.g. tests) → the assistant/messages endpoints return 503.
	// Readiness is gated by isContentPlanReady, the same Anthropic-key gate.
	assistant func(ctx context.Context, req campaign_assistant.CampaignAssistantRequest, onEvent campaign_assistant.OnEventFunc) (*campaign_assistant.CampaignAssistantResponse, error)
	// activity records CON-125 user-activity events (campaign_created,
	// content_generated, …). nil is a no-op (analytics disabled / fixtures).
	activity *activity.Recorder
}

// CampaignsOptions carries the handler's nil-safe collaborators.
type CampaignsOptions struct {
	Limiter  *entitlements.Limiter
	Activity *activity.Recorder
	// Brands tenant-validates brand_voice_id/brand_audience_id; nil skips it.
	Brands repository.BrandRepository
}

// baselineCampaignTypeSlug is the one system campaign type every tier can use;
// the other system types are gated by all_campaign_types.
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
	opts CampaignsOptions,
) *CampaignsHandler {
	return &CampaignsHandler{
		limiter:            opts.Limiter,
		activity:           opts.Activity,
		brandRepo:          opts.Brands,
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
	// Targeted membership writes for the content-bank set, so attaching
	// or detaching one document touches only asset_ids (no full-record PUT, no
	// omitted-field reset, atomic under concurrent adds).
	g.Post("/:id/assets", h.auth, h.AddAssets)
	g.Delete("/:id/assets/:assetId", h.auth, h.RemoveAsset)
	g.Post("/:id/generate-draft", h.auth, h.GenerateDraft)
	g.Post("/:id/enrich-brief", h.auth, h.EnrichBrief)
	// Campaign Assistant chat. Tenant-scoped only — any user who can
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
	// Presence-aware: omitting a brand ref on a full-replace save
	// leaves the stored value alone; an explicit null clears it. See Optional.
	BrandVoiceID    Optional[string] `json:"brand_voice_id"`
	BrandAudienceID Optional[string] `json:"brand_audience_id"`
	// UseAssets / AssetIDs are presence-aware: the content-bank set has
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
	// Scheduling settings. All optional; omitted fields fall back to
	// defaults (09:00 / UTC / every day / ±15 min) via normalizeScheduling.
	PublishingTime string             `json:"publishing_time"`
	Timezone       string             `json:"timezone"`
	PublishingDays models.StringSlice `json:"publishing_days"`
	SpreadMinutes  *int               `json:"spread_minutes"`
	// Goal cadence: "week" | "month". Empty falls back to "month".
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

// campaignSchedule is the normalized scheduling + goal settings of a request.
type campaignSchedule struct {
	publishingTime string
	timezone       string
	publishingDays models.StringSlice
	spread         int
	goalCadence    string
}

// normalizeSchedule validates the scheduling fields and the goal cadence,
// returning the effective values with defaults applied. A failure is a
// 400-worthy error.
func (r *campaignRequest) normalizeSchedule() (campaignSchedule, error) {
	publishingTime, timezone, days, spread, err := r.normalizeScheduling()
	if err != nil {
		return campaignSchedule{}, err
	}
	goalCadence, err := campaigngoal.Normalize(strings.TrimSpace(r.GoalCadence))
	if err != nil {
		return campaignSchedule{}, err
	}
	return campaignSchedule{
		publishingTime: publishingTime,
		timezone:       timezone,
		publishingDays: days,
		spread:         spread,
		goalCadence:    goalCadence,
	}, nil
}

// applyTo copies the request's mutable fields onto an existing campaign.
// Brand refs and the content-bank set/flag are presence-aware: omitted leaves
// the stored value (the membership endpoints own the set), present replaces,
// explicit null clears.
func (r *campaignRequest) applyTo(c *models.Campaign, status models.CampaignStatus, sched campaignSchedule) {
	c.Name = r.Name
	c.Description = r.Description
	c.TargetPersona = r.TargetPersona
	c.KeyMessages = r.KeyMessages
	c.ToneGuidelines = r.ToneGuidelines
	r.BrandVoiceID.applyTo(&c.BrandVoiceID)
	r.BrandAudienceID.applyTo(&c.BrandAudienceID)
	r.UseAssets.applyToValue(&c.UseAssets)
	applyOptionalSlice(r.AssetIDs, &c.AssetIDs)
	c.TargetPlatforms = nullCampaignPlatforms(r.TargetPlatforms)
	c.CampaignTypeID = r.CampaignTypeID
	c.Status = status
	c.EstimatedPostCount = r.EstimatedPostCount
	c.StartDate = r.StartDate
	c.EndDate = r.EndDate
	c.Budget = r.Budget
	c.Currency = r.Currency
	c.Language = r.Language
	c.TagIDs = nullSlice(r.TagIDs)
	c.PublishingTime = sched.publishingTime
	c.Timezone = sched.timezone
	c.PublishingDays = sched.publishingDays
	c.SpreadMinutes = sched.spread
	c.GoalCadence = sched.goalCadence
}

// omitColumns lists the presence-aware content-bank columns an omitted field
// must keep out of the whole-record UPDATE: applyTo already left the hydrated
// value in place, but writing it back would clobber a concurrent membership
// write (AddAssetIDs/RemoveAssetID) that landed after the read.
func (r *campaignRequest) omitColumns() []string {
	return omitAbsent(
		columnPresence{"asset_ids", r.AssetIDs.Present},
		columnPresence{"use_assets", r.UseAssets.Present && r.UseAssets.Value != nil},
	)
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
	// The active_campaigns quota gates a new campaign.
	campaignQuota, err := requireQuota(c, h.limiter, "active_campaigns")
	if err != nil {
		return err
	}
	campaignType, err := h.campaignTypeRepo.GetByID(reqCtx(c), req.CampaignTypeID)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid campaign_type_id")
	}
	// Custom / non-baseline campaign types are entitlement-gated.
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

	session, err := sessionFrom(c)
	if err != nil {
		return err
	}

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
	// The campaign now exists — fire any near-limit crossing.
	campaignQuota.dispatch(reqCtx(c))
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
// @Description  campaign_type_id can't change once any post is planned against the type's phases
// @Description  (type_locked; 409 campaign_type_locked). Changing the type, or dates such that a phase
// @Description  window would empty out, drops a manual phase plan back to derived (phase_plan_reset: true).
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
// @Failure      409   {object}  map[string]string  "campaign_type_locked: posts are planned against the current type's phases"
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
	sched, err := req.normalizeSchedule()
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

	typeChanged := req.CampaignTypeID != campaign.CampaignTypeID
	if typeChanged {
		if err := h.checkTypeChange(c, campaign, campaignType); err != nil {
			return err
		}
	}

	// Snapshot the fields we emit change-events for before overwriting them.
	prevStatus := campaign.Status
	prevStart, prevEnd := campaign.StartDate, campaign.EndDate

	req.applyTo(campaign, status, sched)
	campaign.UpdatedAt = time.Now().UTC()

	if err := h.repo.Update(reqCtx(c), campaign, req.omitColumns()...); err != nil {
		if repository.IsConstraintViolation(err, repository.ConstraintCampaignTypeLocked) {
			// A phase was assigned between our read and the write (trigger backstop),
			// so the pre-read count is stale — at least one post now holds a phase.
			return rejectTypeLocked(c, max(campaign.PhasedPostCount, 1))
		}
		return err
	}
	datesChanged := !timePtrEqual(prevStart, campaign.StartDate) || !timePtrEqual(prevEnd, campaign.EndDate)
	// Keep a stored manual phase plan consistent with the new type/dates.
	if maintainPhasePlan(c, h.repo, campaign, typeChanged, datesChanged) {
		campaign.PhaseWindows = nil
		campaign.PhasePlanReset = true
		h.recordActivity(c, activity.CategoryCampaign, "campaign_phase_plan_reset",
			activity.WithEntity("campaign", campaign.ID),
		)
	}
	h.recordCampaignUpdated(c, campaign, prevStatus, datesChanged)
	return c.JSON(campaign)
}

// checkTypeChange guards a campaign-type switch. Once posts are planned
// against the type's phases the type is locked — switching would orphan their
// phase references (a DB trigger backstops a concurrent phase assignment).
// The entitlement gate applies only to a switch, so an unrelated edit of a
// campaign already on a gated type is never blocked.
func (h *CampaignsHandler) checkTypeChange(c *fiber.Ctx, campaign *models.Campaign, target *models.CampaignType) error {
	if campaign.TypeLocked {
		return rejectTypeLocked(c, campaign.PhasedPostCount)
	}
	return h.gateCampaignType(c, target)
}

// recordCampaignUpdated emits the update activity plus the status- and
// date-change events when those fields moved.
func (h *CampaignsHandler) recordCampaignUpdated(c *fiber.Ctx, campaign *models.Campaign, prevStatus models.CampaignStatus, datesChanged bool) {
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
	if datesChanged {
		h.recordActivity(c, activity.CategoryCampaign, "campaign_dates_changed",
			activity.WithEntity("campaign", campaign.ID),
		)
	}
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

	campaign, err := load(c, h.repo.GetByID, "campaign not found")
	if err != nil {
		return err
	}
	session, err := sessionFrom(c)
	if err != nil {
		return err
	}
	if campaign.CreatedBy != session.UserID {
		return fiber.NewError(fiber.StatusForbidden, "forbidden")
	}

	h.recordActivity(c, activity.CategoryCampaign, "content_generated",
		activity.WithEntity("campaign", campaign.ID),
		activity.WithPayload(map[string]any{"mode": "generate_draft"}),
	)

	campaignID := campaign.ID
	generateDraft := h.generateDraft
	// The flow runs after this handler returns; the detached context carries
	// the tenant so usage recording and enforcement attribute correctly.
	flowCtx := detachedContext(c, session.TenantID)
	streamFlow(c, func(emit sseEmit) {
		resp, err := generateDraft(flowCtx, campaignID, func(name content_plan.SSEEventKind, data any) {
			emit(string(name), data)
		})
		if err != nil {
			emit(string(content_plan.SSEEventError), flowError[*content_plan.ValidationError, *content_plan.AIError](err))
			return
		}
		emit(string(content_plan.SSEEventComplete), resp)
	})
	return nil
}

// EnrichBrief godoc
// @Summary      Enrich campaign brief with AI (SSE)
// @Description  Improves the campaign brief (description, target persona, key messages, tone guidelines),
// @Description  streaming progress via Server-Sent Events. A non-empty brief is edited in place, keeping its
// @Description  subject and honouring its tone guidelines and the brand voice; an empty brief is drafted
// @Description  from the campaign's title and type. The optional instruction says what to change.
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

	campaign, err := load(c, h.repo.GetByID, "campaign not found")
	if err != nil {
		return err
	}
	session, err := sessionFrom(c)
	if err != nil {
		return err
	}
	if campaign.CreatedBy != session.UserID {
		return fiber.NewError(fiber.StatusForbidden, "forbidden")
	}

	h.recordActivity(c, activity.CategoryCampaign, "brief_enriched",
		activity.WithEntity("campaign", campaign.ID),
	)

	// The instruction aliases the request buffer, which the stream writer outlives.
	req := enrich_brief.EnrichBriefRequest{CampaignID: campaign.ID, Instruction: strings.Clone(body.Instruction)}
	enrichBrief := h.enrichBrief
	flowCtx := detachedContext(c, session.TenantID)
	streamFlow(c, func(emit sseEmit) {
		resp, err := enrichBrief(flowCtx, req, func(name enrich_brief.SSEEventKind, data any) {
			emit(string(name), data)
		})
		if err != nil {
			emit(string(enrich_brief.SSEEventError), flowError[*enrich_brief.ValidationError, *enrich_brief.AIError](err))
			return
		}
		emit(string(enrich_brief.SSEEventComplete), resp)
	})
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
	session, err := sessionFrom(c)
	if err != nil {
		return err
	}

	h.recordActivity(c, activity.CategoryAIFlow, "campaign_assistant_turn",
		activity.WithEntity("campaign", c.Params("id")),
		activity.WithSource(activity.SourceAssistant),
	)

	// Params and body values alias the fasthttp request buffer, which the
	// stream writer outlives; clone them (see PostAssistantHandler.Assistant).
	flowReq := campaign_assistant.CampaignAssistantRequest{
		CampaignID:  strings.Clone(c.Params("id")),
		Instruction: strings.Clone(req.Instruction),
	}
	assistant := h.assistant
	// The detached context carries the tenant so the tenant-scoped campaign
	// load, brief write, and usage recording attribute correctly.
	flowCtx := detachedContext(c, session.TenantID)
	streamFlow(c, func(emit sseEmit) {
		_, err := assistant(flowCtx, flowReq, func(name campaign_assistant.SSEEventKind, data any) {
			emit(string(name), data)
		})
		if err != nil {
			emit(string(campaign_assistant.SSEEventError), flowError[*campaign_assistant.ValidationError, *campaign_assistant.AIError](err))
		}
		// "complete" is emitted by the runner itself.
	})
	return nil
}

// ListMessages godoc
// @Summary      List campaign assistant messages
// @Description  Returns the most recent Campaign Assistant conversation messages for a campaign.
// @Description  A "model" message's content is the JSON of that turn's "complete" event: action,
// @Description  explanation and the tool results (brief, briefReview with findings, contentPlan,
// @Description  generatedPosts, draftedPosts, dates, redistribute, postsReview).
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
	campaign_assistant.NormalizeHistory(msgs)
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
