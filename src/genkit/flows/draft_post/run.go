package draft_post

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"text/template"
	"time"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/modelconfig"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/usecase/brandresolve"
	"github.com/ogen-app/ogen/src/usecase/notes"
	"github.com/ogen-app/ogen/src/usecase/scheduling"
	"github.com/ogen-app/ogen/src/usecase/settings"
)

// defaultMaxOutputTokens caps a single draft generation when the config leaves
// it at 0. Enough for a handful of full-length posts.
const defaultMaxOutputTokens int64 = 8192

// contextTemplateData is the view model passed to both prompt blocks.
type contextTemplateData struct {
	CampaignName      string
	CampaignTypeLabel string
	PhaseName         string
	Description       string
	TargetPersona     string
	KeyMessages       string
	ToneGuidelines    string
	// BrandBlock is the resolved brand voice/audience/guardrails block (CON-245);
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
// post-type slug and the CON-91 character-limit / format guidance fed to the
// prompt.
type resolvedPlatform struct {
	ID          string
	Name        string
	PostType    string // resolved slug to persist (may be "")
	Constraints string // CON-91 character limits, format notes
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
	slog.InfoContext(ctx, "starting", logging.AttrComponent, "genkit.draft_post",
		"campaign_id", req.CampaignID, "platform_id", req.PlatformID, "count", req.Count,
		"source_len", len(req.SourceMaterial), "instruction_len", len(req.Instruction))

	// Enforcement gate (CON-86 FR9): block before any provider call when the
	// tenant is already over a cap. Nil checker = no gate.
	if err := cfg.Checker.Enforce(ctx); err != nil {
		return nil, err
	}

	// ── Validate ─────────────────────────────────────────────────────────────
	if req.CampaignID == "" {
		return nil, &ValidationError{Msg: "campaign id is required"}
	}
	if req.PlatformID == "" {
		return nil, &ValidationError{Msg: "platform id is required"}
	}
	source := strings.TrimSpace(req.SourceMaterial)
	if source == "" {
		return nil, &ValidationError{Msg: "source material is required to draft a post"}
	}
	count := req.Count
	if count <= 0 {
		count = 1
	}

	// ── Load campaign (tenant-scoped) ────────────────────────────────────────
	campaign, err := repos.Campaigns.GetByID(ctx, req.CampaignID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, &ValidationError{Msg: "campaign not found"}
		}
		return nil, fmt.Errorf("load campaign: %w", err)
	}

	// ── Resolve platform (+ post type + CON-91 constraints) ──────────────────
	platform, err := resolvePlatform(ctx, campaign, req.PlatformID, req.PostType, repos.Platforms)
	if err != nil {
		return nil, err
	}

	// ── Resolve window + per-post dates ──────────────────────────────────────
	winStart, err := time.Parse("2006-01-02", req.WindowStart)
	if err != nil {
		return nil, &ValidationError{Msg: "windowStart must be an ISO date (YYYY-MM-DD)"}
	}
	winEnd, err := time.Parse("2006-01-02", req.WindowEnd)
	if err != nil {
		return nil, &ValidationError{Msg: "windowEnd must be an ISO date (YYYY-MM-DD)"}
	}
	if winEnd.Before(winStart) {
		return nil, &ValidationError{Msg: "windowEnd must be on or after windowStart"}
	}
	dates := spreadDates(winStart, winEnd, count)

	// ── Build prompts ────────────────────────────────────────────────────────
	typeLabel := ""
	if campaign.CampaignType != nil {
		typeLabel = campaign.CampaignType.Label
	}
	// CON-245: resolve the brand voice/audience/guardrails once for the batch;
	// the block supersedes the legacy tone/persona prose (falling back to it).
	resolved, rerr := brandresolve.Resolve(ctx, repos.Brands, campaign, nil)
	if rerr != nil {
		slog.WarnContext(ctx, "brand resolve failed; using legacy tone",
			logging.AttrComponent, "genkit.draft_post", "error", rerr)
	}
	brandVoiceID := resolved.VoiceID()
	data := contextTemplateData{
		CampaignName:      campaign.Name,
		CampaignTypeLabel: typeLabel,
		PhaseName:         phaseNameByID(campaign, req.PhaseID),
		Description:       campaign.Description,
		TargetPersona:     campaign.TargetPersona,
		KeyMessages:       campaign.KeyMessages,
		ToneGuidelines:    campaign.ToneGuidelines,
		BrandBlock:        resolved.PromptBlock(platform.ID),
		Language:          campaign.Language,
		PlatformName:      platform.Name,
		PostType:          platform.PostType,
		Constraints:       platform.Constraints,
		Count:             count,
		SourceMaterial:    source,
		Instruction:       strings.TrimSpace(req.Instruction),
	}
	systemPrompt, err := renderTemplate(cfg.systemTmpl, data)
	if err != nil {
		return nil, fmt.Errorf("render system prompt: %w", err)
	}
	contextBlock, err := renderTemplate(cfg.contextTmpl, data)
	if err != nil {
		return nil, fmt.Errorf("render context block: %w", err)
	}
	emit(onEvent, SSEEventStep, StepEventPayload{Step: "buildContext", Status: "done"})

	maxTokens := cfg.MaxOutputTokens
	if maxTokens == 0 {
		maxTokens = defaultMaxOutputTokens
	}
	mc := modelconfig.Resolve(ctx, modelconfig.FlowDraftPost, modelconfig.SlotMain)
	modelName := mc.Ref
	modelCfg := cfg.Provider.CallConfig(maxTokens)
	usageVendor := mc.Vendor
	usageModel := mc.Model

	loc, _ := settings.ResolveTimezone(campaign.Timezone)

	var out []DraftedPost
	var warnings []string

	// appendDraft persists one finished draft content-first (CON-207) and emits
	// its post event, capped at the requested count so an over-producing model
	// can't inflate the result.
	appendDraft := func(d modelDraft) {
		if len(out) >= count {
			return
		}
		title := strings.TrimSpace(d.Title)
		content := strings.TrimSpace(d.Content)
		if content == "" {
			warnings = append(warnings, "dropped a draft with empty content")
			emit(onEvent, SSEEventWarning, WarningPayload{Message: "dropped a draft with empty content"})
			return
		}
		publishDate := dates[len(out)] // len(out) < count == len(dates)
		dp, id, perr := persistDraft(ctx, campaign, platform, req.PhaseID, publishDate, loc, &winStart, &winEnd, title, content, req.UsedAssetIDs, source, brandVoiceID, repos)
		if perr != nil {
			slog.ErrorContext(ctx, "persist draft failed", logging.AttrComponent, "genkit.draft_post", "title", title, logging.AttrError, perr)
			msg := fmt.Sprintf("draft %q could not be saved: %v", title, perr)
			warnings = append(warnings, msg)
			emit(onEvent, SSEEventWarning, WarningPayload{Message: msg})
			return
		}
		emit(onEvent, SSEEventPost, PostEventPayload{Post: dp, Index: len(out), ID: id})
		out = append(out, dp)
	}

	// ── Stream generation ────────────────────────────────────────────────────
	// No ai.WithOutputType: genkit's strict validator drops the whole response on
	// common Claude JSON drift. We scan complete objects out of the stream and
	// parse each tolerantly instead (mirrors content_plan).
	scanner := newJSONObjScanner()
	parsedCount := 0
	var streamErr error
	for result, err := range genkit.GenerateStream(ctx, g,
		ai.WithModelName(modelName),
		ai.WithSystem(systemPrompt),
		ai.WithPrompt(contextBlock),
		modelCfg,
	) {
		if err != nil {
			streamErr = err
			break
		}
		if result.Done {
			if result.Response != nil {
				if result.Response.Usage != nil {
					u := result.Response.Usage
					slog.InfoContext(ctx, "tokens", logging.AttrComponent, "genkit.draft_post", "input", u.InputTokens, "output", u.OutputTokens, "total", u.InputTokens+u.OutputTokens)
				}
				cfg.Recorder.RecordResp(ctx, usageVendor, usageModel, "draft_post", result.Response)
			}
			break
		}
		for _, raw := range scanner.push(result.Chunk.Text()) {
			parsedCount++
			var d modelDraft
			if uerr := json.Unmarshal([]byte(raw), &d); uerr != nil {
				slog.WarnContext(ctx, "malformed draft chunk", logging.AttrComponent, "genkit.draft_post", "raw_preview", logging.Preview(raw, 120))
				continue
			}
			appendDraft(d)
		}
	}

	// ── Blocking fallback on stream failure ──────────────────────────────────
	// Recover the rest of the batch with a single blocking call, skipping the
	// objects already seen during streaming so we never double-insert.
	if streamErr != nil {
		slog.WarnContext(ctx, "stream error, falling back to blocking Generate", logging.AttrComponent, "genkit.draft_post", "persisted", len(out), "parsed", parsedCount, logging.AttrError, streamErr)
		resp, gerr := genkit.Generate(ctx, g,
			ai.WithModelName(modelName),
			ai.WithSystem(systemPrompt),
			ai.WithPrompt(contextBlock),
			modelCfg,
		)
		if gerr != nil {
			if len(out) == 0 {
				return nil, &AIError{Msg: fmt.Sprintf("model call failed (stream+fallback): %v", gerr)}
			}
			warnings = append(warnings, fmt.Sprintf("generation was cut short: %v", gerr))
		} else {
			if resp.Usage != nil {
				slog.InfoContext(ctx, "tokens (fallback)", logging.AttrComponent, "genkit.draft_post", "input", resp.Usage.InputTokens, "output", resp.Usage.OutputTokens, "total", resp.Usage.InputTokens+resp.Usage.OutputTokens)
			}
			cfg.Recorder.RecordResp(ctx, usageVendor, usageModel, "draft_post", resp)
			var arr []modelDraft
			if uerr := json.Unmarshal([]byte(stripFences(resp.Text())), &arr); uerr != nil {
				if len(out) == 0 {
					return nil, &AIError{Msg: fmt.Sprintf("model response not valid JSON: %v", uerr)}
				}
			} else {
				for i, d := range arr {
					if i < parsedCount {
						continue // already handled during streaming
					}
					appendDraft(d)
				}
			}
		}
	}
	emit(onEvent, SSEEventStep, StepEventPayload{Step: "generate", Status: "done"})

	slog.InfoContext(ctx, "done", logging.AttrComponent, "genkit.draft_post",
		"campaign_id", req.CampaignID, "duration_ms", time.Since(start).Milliseconds(),
		"posts", len(out), "warnings", len(warnings))

	// An empty result is a soft failure (0 posts + warnings), not an error — the
	// assistant runner surfaces a friendly reply rather than a 502 (CON-207 §9).
	return &DraftPostResponse{Posts: out, Warnings: warnings}, nil
}

// persistDraft inserts one finished draft as a content-first Post row (CON-207):
// the generated copy goes into Post.Content, status=draft, scheduled per CON-181,
// and the source research is kept as a "Source research" reference note. Returns
// the DraftedPost view + the new row id.
func persistDraft(
	ctx context.Context,
	campaign *models.Campaign,
	platform resolvedPlatform,
	phaseID, publishDate string,
	loc *time.Location,
	windowStart, windowEnd *time.Time,
	title, content string,
	usedAssetIDs []string,
	source string,
	brandVoiceID *string,
	repos DraftPostRepos,
) (DraftedPost, string, error) {
	id, err := models.NewID()
	if err != nil {
		return DraftedPost{}, "", err
	}

	// CON-181: snap the publish date to an enabled publishing day, place it at
	// the campaign's publishing time in its timezone, ± deterministic spread —
	// bounded by the generation window.
	scheduledAt, effDate, noEnabledDay := scheduling.ComposeScheduledAt(
		publishDate, id, loc, campaign.PublishingTime, campaign.PublishingDays,
		campaign.SpreadMinutes, windowStart, windowEnd,
	)
	if noEnabledDay {
		slog.WarnContext(ctx, "no enabled publishing day in window; kept model date",
			logging.AttrComponent, "genkit.draft_post", "post_id", id, "date", publishDate)
	}

	// CON-166: a post's phase must belong to its campaign's type (a DB trigger
	// rejects anything else). The calling tool resolves the phase against the
	// campaign, but drop an unknown id rather than fail the whole draft.
	var phaseIDPtr *string
	if phaseID != "" {
		if phaseNameByID(campaign, phaseID) != "" {
			phaseIDPtr = &phaseID
		} else {
			slog.WarnContext(ctx, "dropping phase that is not a phase of the campaign's type",
				logging.AttrComponent, "genkit.draft_post", "post_id", id, "phase_id", phaseID)
		}
	}

	row := &models.Post{
		ID:                  id,
		CampaignID:          campaign.ID,
		PlatformID:          platform.ID,
		PlatformPostType:    platform.PostType,
		Title:               title,
		Content:             content, // CON-207: content-first — the finished copy, not ""
		MediaURLs:           models.StringSlice{},
		Status:              models.PostStatusDraft,
		CTAType:             models.CTATypeNone,
		CTAUrl:              "",
		UsedAssetIDs:        models.StringSlice(usedAssetIDs),
		CampaignTypePhaseID: phaseIDPtr,
		ScheduledAt:         scheduledAt,
		BrandVoiceID:        brandVoiceID, // CON-245
		CreatedBy:           campaign.CreatedBy,
	}
	if err := repos.Posts.Create(ctx, row); err != nil {
		return DraftedPost{}, "", err
	}

	// CON-207: keep the source research as a reference note. Best-effort — a
	// note-write failure must never discard the persisted post (CON-66/CON-188).
	if repos.Notes != nil {
		if body := strings.TrimSpace(source); body != "" {
			if err := createSourceNote(ctx, repos.Notes, id, campaign.CreatedBy, body); err != nil {
				slog.ErrorContext(ctx, "source research note create failed", logging.AttrComponent, "genkit.draft_post", "post_id", id, logging.AttrError, err)
			}
		}
	}

	return DraftedPost{
		Title:       title,
		Content:     content,
		PlatformID:  platform.ID,
		PostType:    platform.PostType,
		PublishDate: effDate,
		PostID:      id,
	}, id, nil
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

// resolvePlatform looks up the platform's metadata (name + CON-91 constraints)
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
	totalDays := int(end.Sub(start).Hours() / 24)
	if totalDays < 0 {
		totalDays = 0
	}
	for i := range n {
		off := 0
		if totalDays > 0 {
			off = int(int64(i) * int64(totalDays) / int64(n-1))
		}
		out = append(out, start.AddDate(0, 0, off).Format(iso))
	}
	return out
}

// stripFences removes a leading ```lang fence and trailing ``` from a blocking
// model response, so a fenced JSON array still parses (mirrors content_plan).
func stripFences(s string) string {
	text := strings.TrimSpace(s)
	if strings.HasPrefix(text, "```") {
		if _, after, found := strings.Cut(text, "\n"); found {
			text = after
		}
		text = strings.TrimSuffix(strings.TrimSpace(text), "```")
		text = strings.TrimSpace(text)
	}
	return text
}

func renderTemplate(tmpl *template.Template, data any) (string, error) {
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}
