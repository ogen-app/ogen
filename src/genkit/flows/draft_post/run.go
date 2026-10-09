package draft_post

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/modelconfig"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/flowkit"
	"github.com/ogen-app/ogen/src/genkit/jsonstream"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/usecase/brandresolve"
	"github.com/ogen-app/ogen/src/usecase/notes"
	"github.com/ogen-app/ogen/src/usecase/scheduling"
	"github.com/ogen-app/ogen/src/usecase/settings"
)

const logComponent = "genkit.draft_post"

// defaultMaxOutputTokens caps a single draft generation when the config leaves
// it at 0. Enough for a handful of full-length posts plus the thinking that
// Claude 5.x models do by default, which counts toward the cap.
const defaultMaxOutputTokens int64 = 16384

// contextTemplateData is the view model passed to both prompt blocks.
type contextTemplateData struct {
	CampaignName      string
	CampaignTypeLabel string
	PhaseName         string
	Description       string
	TargetPersona     string
	KeyMessages       string
	ToneGuidelines    string
	// BrandBlock is the resolved brand voice/audience/guardrails block;
	// supersedes TargetPersona/ToneGuidelines in the template.
	BrandBlock     string
	Language       string
	PlatformName   string
	PostType       string
	Constraints    string
	Count          int
	SourceMaterial string
	Instruction    string
}

// resolvedPlatform is the platform metadata the flow needs: the persisted
// post-type slug and the character-limit / format guidance fed to the prompt.
type resolvedPlatform struct {
	ID          string
	Name        string
	PostType    string // resolved slug to persist (may be "")
	Constraints string // character limits, format notes
}

// draftWindow is the validated publish-date window of a request.
type draftWindow struct {
	start, end time.Time
}

func runDraftPost(
	ctx context.Context,
	g *genkit.Genkit,
	req DraftPostRequest,
	cfg DraftPostFlowConfig,
	repos DraftPostRepos,
	onEvent OnEventFunc,
) (*DraftPostResponse, error) {
	start := time.Now()
	slog.InfoContext(ctx, "starting", logging.AttrComponent, logComponent,
		"campaign_id", req.CampaignID, "platform_id", req.PlatformID, "count", req.Count,
		"source_len", len(req.SourceMaterial), "instruction_len", len(req.Instruction))

	if err := cfg.Checker.Enforce(ctx); err != nil {
		return nil, err
	}
	if err := validateRequest(req); err != nil {
		return nil, err
	}
	campaign, err := repos.Campaigns.GetByID(ctx, req.CampaignID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, &ValidationError{Msg: "campaign not found"}
		}
		return nil, fmt.Errorf("load campaign: %w", err)
	}
	platform, err := resolvePlatform(ctx, campaign, req.PlatformID, req.PostType, repos.Platforms)
	if err != nil {
		return nil, err
	}
	window, err := parseWindow(req.WindowStart, req.WindowEnd)
	if err != nil {
		return nil, err
	}

	sink := newDraftSink(ctx, req, campaign, platform, window, repos, onEvent)
	systemPrompt, contextBlock, err := renderPrompts(cfg, sink.promptData(req))
	if err != nil {
		return nil, err
	}
	emit(onEvent, SSEEventStep, StepEventPayload{Step: "buildContext", Status: "done"})

	maxTokens := cmp.Or(cfg.MaxOutputTokens, defaultMaxOutputTokens)
	mc := modelconfig.Resolve(ctx, modelconfig.FlowDraftPost, modelconfig.SlotMain)
	u := flowkit.Usage{Recorder: cfg.Recorder, Model: mc, Feature: "draft_post", Component: logComponent}
	if err := sink.generate(ctx, g, u,
		ai.WithModelName(mc.Ref),
		ai.WithSystem(systemPrompt),
		ai.WithPrompt(contextBlock),
		ai.WithMiddleware(cfg.Provider.RefusalGuard(modelconfig.FlowDraftPost)),
		cfg.Provider.CallConfig(maxTokens),
	); err != nil {
		return nil, err
	}
	emit(onEvent, SSEEventStep, StepEventPayload{Step: "generate", Status: "done"})

	slog.InfoContext(ctx, "done", logging.AttrComponent, logComponent,
		"campaign_id", req.CampaignID, "duration_ms", time.Since(start).Milliseconds(),
		"posts", len(sink.out), "warnings", len(sink.warnings))

	// An empty result is a soft failure (0 posts + warnings), not an error: the
	// assistant surfaces a friendly reply rather than a 502.
	return &DraftPostResponse{Posts: sink.out, Warnings: sink.warnings}, nil
}

func validateRequest(req DraftPostRequest) error {
	if req.CampaignID == "" {
		return &ValidationError{Msg: "campaign id is required"}
	}
	if req.PlatformID == "" {
		return &ValidationError{Msg: "platform id is required"}
	}
	if strings.TrimSpace(req.SourceMaterial) == "" {
		return &ValidationError{Msg: "source material is required to draft a post"}
	}
	return nil
}

func parseWindow(startDate, endDate string) (draftWindow, error) {
	start, err := time.Parse(time.DateOnly, startDate)
	if err != nil {
		return draftWindow{}, &ValidationError{Msg: "windowStart must be an ISO date (YYYY-MM-DD)"}
	}
	end, err := time.Parse(time.DateOnly, endDate)
	if err != nil {
		return draftWindow{}, &ValidationError{Msg: "windowEnd must be an ISO date (YYYY-MM-DD)"}
	}
	if end.Before(start) {
		return draftWindow{}, &ValidationError{Msg: "windowEnd must be on or after windowStart"}
	}
	return draftWindow{start: start, end: end}, nil
}

func renderPrompts(cfg DraftPostFlowConfig, data contextTemplateData) (system, contextBlock string, err error) {
	system, err = flowkit.RenderTemplate(cfg.systemTmpl, data)
	if err != nil {
		return "", "", fmt.Errorf("render system prompt: %w", err)
	}
	contextBlock, err = flowkit.RenderTemplate(cfg.contextTmpl, data)
	if err != nil {
		return "", "", fmt.Errorf("render context block: %w", err)
	}
	return system, contextBlock, nil
}

// draftSink persists finished drafts content-first as they stream in, capped
// at the requested count so an over-producing model can't inflate the result.
type draftSink struct {
	campaign *models.Campaign
	platform resolvedPlatform
	phaseID  string
	window   draftWindow
	dates    []string // one publish date per requested post
	loc      *time.Location
	assetIDs []string
	source   string
	brand    *brandresolve.Resolved
	repos    DraftPostRepos
	onEvent  OnEventFunc

	out      []DraftedPost
	warnings []string
}

// newDraftSink resolves the per-post dates, timezone and brand voice (once
// for the batch; it supersedes the legacy tone prose, falling back to it).
func newDraftSink(ctx context.Context, req DraftPostRequest, campaign *models.Campaign, platform resolvedPlatform, window draftWindow, repos DraftPostRepos, onEvent OnEventFunc) *draftSink {
	count := req.Count
	if count <= 0 {
		count = 1
	}
	brand, err := brandresolve.Resolve(ctx, repos.Brands, campaign, nil)
	if err != nil {
		slog.WarnContext(ctx, "brand resolve failed; using legacy tone",
			logging.AttrComponent, logComponent, "error", err)
	}
	loc, _ := settings.ResolveTimezone(campaign.Timezone)
	return &draftSink{
		campaign: campaign,
		platform: platform,
		phaseID:  req.PhaseID,
		window:   window,
		dates:    spreadDates(window.start, window.end, count),
		loc:      loc,
		assetIDs: req.UsedAssetIDs,
		source:   strings.TrimSpace(req.SourceMaterial),
		brand:    brand,
		repos:    repos,
		onEvent:  onEvent,
	}
}

func (s *draftSink) promptData(req DraftPostRequest) contextTemplateData {
	typeLabel := ""
	if s.campaign.CampaignType != nil {
		typeLabel = s.campaign.CampaignType.Label
	}
	return contextTemplateData{
		CampaignName:      s.campaign.Name,
		CampaignTypeLabel: typeLabel,
		PhaseName:         phaseNameByID(s.campaign, req.PhaseID),
		Description:       s.campaign.Description,
		TargetPersona:     s.campaign.TargetPersona,
		KeyMessages:       s.campaign.KeyMessages,
		ToneGuidelines:    s.campaign.ToneGuidelines,
		BrandBlock:        s.brand.PromptBlock(s.platform.ID),
		Language:          s.campaign.Language,
		PlatformName:      s.platform.Name,
		PostType:          s.platform.PostType,
		Constraints:       s.platform.Constraints,
		Count:             len(s.dates),
		SourceMaterial:    s.source,
		Instruction:       strings.TrimSpace(req.Instruction),
	}
}

// generate streams the drafts, persisting each as it completes. The model's
// JSON array is split into objects and each is parsed on its own, so JSON
// drift elsewhere in the response never loses a finished draft.
func (s *draftSink) generate(ctx context.Context, g *genkit.Genkit, u flowkit.Usage, opts ...ai.GenerateOption) error {
	res, streamErr := flowkit.StreamObjects(ctx, g, func(_ int, raw string) {
		var d modelDraft
		if err := json.Unmarshal([]byte(raw), &d); err != nil {
			slog.WarnContext(ctx, "malformed draft chunk", logging.AttrComponent, logComponent, "raw_preview", logging.Preview(raw, 120))
			return
		}
		s.add(ctx, d)
	}, opts...)
	if streamErr == nil {
		u.LogTokens(ctx, "tokens", res.Response)
		u.Record(ctx, res.Response)
		return nil
	}
	return s.fallback(ctx, g, u, res.Objects, streamErr, opts)
}

// fallback recovers the rest of the batch after a stream failure with one
// blocking call, skipping the array positions already seen while streaming so
// nothing is inserted twice. It fails only when no draft was saved at all.
func (s *draftSink) fallback(ctx context.Context, g *genkit.Genkit, u flowkit.Usage, seen int, streamErr error, opts []ai.GenerateOption) error {
	slog.WarnContext(ctx, "stream error, falling back to blocking Generate", logging.AttrComponent, logComponent, "persisted", len(s.out), "parsed", seen, logging.AttrError, streamErr)
	resp, err := genkit.Generate(ctx, g, opts...)
	if err != nil {
		if len(s.out) == 0 {
			return &AIError{Msg: fmt.Sprintf("model call failed (stream+fallback): %v", err)}
		}
		s.warnings = append(s.warnings, fmt.Sprintf("generation was cut short: %v", err))
		return nil
	}
	u.LogTokens(ctx, "tokens (fallback)", resp)
	u.Record(ctx, resp)

	var drafts []modelDraft
	if err := json.Unmarshal([]byte(jsonstream.StripFences(resp.Text())), &drafts); err != nil {
		if len(s.out) == 0 {
			return &AIError{Msg: fmt.Sprintf("model response not valid JSON: %v", err)}
		}
		return nil
	}
	for _, d := range drafts[min(seen, len(drafts)):] {
		s.add(ctx, d)
	}
	return nil
}

// add persists one finished draft and emits its post event; a draft with
// empty content or a failed write becomes a warning.
func (s *draftSink) add(ctx context.Context, d modelDraft) {
	if len(s.out) >= len(s.dates) {
		return
	}
	title := strings.TrimSpace(d.Title)
	content := strings.TrimSpace(d.Content)
	if content == "" {
		s.warn("dropped a draft with empty content")
		return
	}
	dp, err := s.persist(ctx, s.dates[len(s.out)], title, content)
	if err != nil {
		slog.ErrorContext(ctx, "persist draft failed", logging.AttrComponent, logComponent, "title", title, logging.AttrError, err)
		s.warn(fmt.Sprintf("draft %q could not be saved: %v", title, err))
		return
	}
	emit(s.onEvent, SSEEventPost, PostEventPayload{Post: dp, Index: len(s.out), ID: dp.PostID})
	s.out = append(s.out, dp)
}

func (s *draftSink) warn(msg string) {
	s.warnings = append(s.warnings, msg)
	emit(s.onEvent, SSEEventWarning, WarningPayload{Message: msg})
}

// persist inserts one draft as a content-first draft Post: the generated copy
// is the body, scheduling follows the campaign settings within the window, and
// the source research is kept as a "Source research" note.
func (s *draftSink) persist(ctx context.Context, publishDate, title, content string) (DraftedPost, error) {
	id, err := models.NewID()
	if err != nil {
		return DraftedPost{}, err
	}

	// Snap to an enabled publishing day at the campaign's publishing time in
	// its timezone, ± deterministic spread, bounded by the window.
	scheduledAt, effDate, noEnabledDay := scheduling.ComposeScheduledAt(
		publishDate, id, s.loc, s.campaign.PublishingTime, s.campaign.PublishingDays,
		s.campaign.SpreadMinutes, &s.window.start, &s.window.end,
	)
	if noEnabledDay {
		slog.WarnContext(ctx, "no enabled publishing day in window; kept model date",
			logging.AttrComponent, logComponent, "post_id", id, "date", publishDate)
	}

	row := &models.Post{
		ID:                  id,
		CampaignID:          s.campaign.ID,
		PlatformID:          s.platform.ID,
		PlatformPostType:    s.platform.PostType,
		Title:               title,
		Content:             content,
		MediaURLs:           models.StringSlice{},
		Status:              models.PostStatusDraft,
		CTAType:             models.CTATypeNone,
		CTAUrl:              "",
		UsedAssetIDs:        models.StringSlice(s.assetIDs),
		CampaignTypePhaseID: s.phaseIDFor(ctx, id),
		ScheduledAt:         scheduledAt,
		BrandVoiceID:        s.brand.VoiceID(),
		CreatedBy:           s.campaign.CreatedBy,
	}
	if err := s.repos.Posts.Create(ctx, row); err != nil {
		return DraftedPost{}, err
	}

	// Best-effort: a note-write failure must never discard the persisted post.
	if s.repos.Notes != nil && s.source != "" {
		if err := createSourceNote(ctx, s.repos.Notes, id, s.campaign.CreatedBy, s.source); err != nil {
			slog.ErrorContext(ctx, "source research note create failed", logging.AttrComponent, logComponent, "post_id", id, logging.AttrError, err)
		}
	}

	return DraftedPost{
		Title:       title,
		Content:     content,
		PlatformID:  s.platform.ID,
		PostType:    s.platform.PostType,
		PublishDate: effDate,
		PostID:      id,
	}, nil
}

// phaseIDFor returns the requested phase when it belongs to the campaign's
// type (a DB trigger rejects anything else); an unknown id is dropped rather
// than failing the draft.
func (s *draftSink) phaseIDFor(ctx context.Context, postID string) *string {
	if s.phaseID == "" {
		return nil
	}
	if phaseNameByID(s.campaign, s.phaseID) == "" {
		slog.WarnContext(ctx, "dropping phase that is not a phase of the campaign's type",
			logging.AttrComponent, logComponent, "post_id", postID, "phase_id", s.phaseID)
		return nil
	}
	return new(s.phaseID)
}

// createSourceNote persists the source research as a free-form reference note
// (type note, origin assistant), title "Source research". The body is trimmed
// to the note service's max length.
func createSourceNote(ctx context.Context, noteRepo repository.PostNoteRepository, postID, createdBy, body string) error {
	if r := []rune(body); len(r) > notes.MaxBodyLen {
		body = string(r[:notes.MaxBodyLen])
	}
	noteID, err := models.NewID()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	return noteRepo.Create(ctx, &models.PostNote{
		ID:        noteID,
		PostID:    postID,
		Type:      models.PostNoteTypeNote,
		Title:     "Source research",
		Body:      body,
		Origin:    models.PostNoteOriginAssistant,
		CreatedBy: createdBy,
		CreatedAt: now,
		UpdatedAt: now,
	})
}

// resolvePlatform looks up the platform's metadata (name + constraints)
// and resolves the post-type slug to persist: an explicit slug when given, else
// the campaign's first selected post type for this platform, else the platform's
// first available slug.
func resolvePlatform(ctx context.Context, campaign *models.Campaign, platformID, postType string, platformRepo repository.PlatformRepository) (resolvedPlatform, error) {
	all, err := platformRepo.List(ctx)
	if err != nil {
		return resolvedPlatform{}, fmt.Errorf("list platforms: %w", err)
	}
	var p *models.Platform
	for i := range all {
		if all[i].ID == platformID {
			p = &all[i]
			break
		}
	}
	if p == nil {
		return resolvedPlatform{}, &ValidationError{Msg: fmt.Sprintf("platform %q is not a known platform", platformID)}
	}

	// Prefer the campaign's selected post types for this platform (deterministic
	// order), then fall back to the platform's own available slugs.
	var campaignSlugs []string
	for _, tp := range campaign.TargetPlatforms {
		if tp.ID == platformID {
			campaignSlugs = append(campaignSlugs, tp.PostTypes...)
		}
	}
	resolvedType := strings.TrimSpace(postType)
	if resolvedType == "" {
		if len(campaignSlugs) > 0 {
			resolvedType = campaignSlugs[0]
		} else if len(p.PostTypes) > 0 {
			// Map iteration order is randomised, so pick the lexicographically first
			// available slug for a deterministic default across identical requests.
			slugs := make([]string, 0, len(p.PostTypes))
			for slug := range p.PostTypes {
				slugs = append(slugs, slug)
			}
			slices.Sort(slugs)
			resolvedType = slugs[0]
		}
	}
	return resolvedPlatform{ID: p.ID, Name: p.Name, PostType: resolvedType, Constraints: p.Constraints}, nil
}

// phaseNameByID returns the campaign phase's name for the prompt, or "" when the
// id is unknown (the tool already validated it; the prompt just tolerates a miss).
func phaseNameByID(campaign *models.Campaign, phaseID string) string {
	if phaseID == "" || campaign.CampaignType == nil {
		return ""
	}
	for _, ph := range campaign.CampaignType.Phases {
		if ph.ID == phaseID {
			return ph.Name
		}
	}
	return ""
}

// spreadDates returns n ISO dates evenly distributed across [start, end]
// inclusive. n==1 pins the single date to start; otherwise the first lands on
// start and the last on end. ComposeScheduledAt later snaps each to an enabled
// publishing weekday within the same window.
func spreadDates(start, end time.Time, n int) []string {
	const iso = "2006-01-02"
	out := make([]string, 0, n)
	if n <= 1 {
		return append(out, start.Format(iso))
	}
	totalDays := max(int(end.Sub(start).Hours()/24), 0)
	for i := range n {
		off := 0
		if totalDays > 0 {
			off = int(int64(i) * int64(totalDays) / int64(n-1))
		}
		out = append(out, start.AddDate(0, 0, off).Format(iso))
	}
	return out
}
