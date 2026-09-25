package post_assistant

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"text/template"
	"time"
	"unicode/utf8"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/platforms"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/usecase/brandresolve"
	"github.com/ogen-app/ogen/src/usecase/settings"
)

// maxSummaryChars caps the combined asset summaries at ~1000 tokens.
const maxSummaryChars = 4000

// previewChars is the target length for a single asset preview.
const previewChars = 200

// contextCacheTTL controls how long a cached context block stays valid.
const contextCacheTTL = 5 * time.Minute

// assetSummary is a lightweight representation of an attached asset.
type assetSummary struct {
	ID         string
	Name       string
	Type       string
	ChunkCount int
	Preview    string
}

// assistantContext holds the rendered prompts ready for the model call.
type assistantContext struct {
	SystemPrompt string // system instructions (stable across turns)
	ContextBlock string // campaign + phase + assets (stable across turns)
}

// contextCacheEntry is a cached assistantContext keyed by post ID.
// fingerprint captures every post field that feeds into the rendered
// prompt — any change to it invalidates the entry. Tracked explicitly
// rather than diffing field-by-field so adding a new field is a one-line
// update to postFingerprint.
type contextCacheEntry struct {
	ctx         *assistantContext
	fingerprint string
	expiresAt   time.Time
}

var (
	contextCache = map[string]*contextCacheEntry{}
	// contextCacheGen tracks a per-post generation number, bumped by every
	// invalidateContextCache call. assembleContextCached captures it before the
	// (unlocked) assemble and only writes the result back if it is still
	// unchanged — otherwise an invalidation that raced an in-flight assembly
	// would be silently undone by the stale result repopulating the cache.
	// Kept in its own map so the generation survives deletion of the entry.
	contextCacheGen = map[string]uint64{}
	contextCacheMu  sync.Mutex
)

// postFingerprint returns a stable string that changes whenever any
// prompt-affecting field of the post changes (content, attached assets,
// phase). Asset IDs are joined without sorting so a reorder also busts
// the cache — the order is surfaced to the model via listAssets output.
func postFingerprint(post *models.Post) string {
	var b strings.Builder
	b.WriteString(post.Content)
	b.WriteByte('\x1f')
	// CON-245: the resolved brand block renders the voice's channel note for
	// post.PlatformID, so a platform change must invalidate the cached context.
	b.WriteString(post.PlatformID)
	b.WriteByte('\x1f')
	b.WriteString(strings.Join(post.UsedAssetIDs, ","))
	b.WriteByte('\x1f')
	if post.CampaignTypePhaseID != nil {
		b.WriteString(*post.CampaignTypePhaseID)
	}
	return b.String()
}

// brandFingerprint captures the identity (id + updatedAt) of the resolved brand
// material so the context cache invalidates when a voice/audience/guardrails is
// edited (CON-245 FR6), not just when the post itself changes.
func brandFingerprint(r *brandresolve.Resolved) string {
	if r == nil {
		return ""
	}
	var b strings.Builder
	if r.Voice != nil {
		b.WriteString(r.Voice.ID)
		b.WriteByte(':')
		b.WriteString(r.Voice.UpdatedAt.String())
	}
	b.WriteByte('\x1f')
	if r.Audience != nil {
		b.WriteString(r.Audience.ID)
		b.WriteByte(':')
		b.WriteString(r.Audience.UpdatedAt.String())
	}
	b.WriteByte('\x1f')
	if r.Guardrails != nil {
		b.WriteString(r.Guardrails.UpdatedAt.String())
	}
	// CON-316: facts are their own rows. The list is already filtered for
	// expiry, so a fact going off also changes the fingerprint.
	for _, f := range r.Facts {
		b.WriteByte('\x1f')
		b.WriteString(f.ID)
		b.WriteByte(':')
		b.WriteString(f.UpdatedAt.String())
	}
	return b.String()
}

// assembleContextCached returns the cached context for the given post if
// the fingerprint matches and the TTL hasn't expired. Otherwise it
// assembles a fresh context, caches it, and returns it.
func assembleContextCached(
	ctx context.Context,
	post *models.Post,
	repos PostAssistantRepos,
	systemTmpl, contextTmpl *template.Template,
) (*assistantContext, error) {
	// Load the campaign once — both the brand fingerprint and the rendered
	// context need it.
	campaign := post.Campaign
	if campaign == nil {
		c, err := repos.Campaigns.GetByID(ctx, post.CampaignID)
		if err != nil {
			return nil, fmt.Errorf("load campaign: %w", err)
		}
		campaign = c
	}
	// CON-245: resolve the brand and fold its identity into the fingerprint, so
	// an edited voice/audience/guardrails invalidates the cached context (FR6).
	// Fails open — resolved is usable even on a repo error.
	resolved, _ := brandresolve.Resolve(ctx, repos.Brands, campaign, post)
	fp := postFingerprint(post) + "\x1f" + brandFingerprint(resolved)

	contextCacheMu.Lock()
	if entry, ok := contextCache[post.ID]; ok {
		if time.Now().Before(entry.expiresAt) && entry.fingerprint == fp {
			contextCacheMu.Unlock()
			return entry.ctx, nil
		}
		delete(contextCache, post.ID)
	}
	gen := contextCacheGen[post.ID]
	contextCacheMu.Unlock()

	actx, err := assembleContext(ctx, post, campaign, resolved, repos, systemTmpl, contextTmpl)
	if err != nil {
		return nil, err
	}

	// Only cache the result if no invalidation raced this assembly. If the
	// generation moved, the underlying data changed (e.g. a note was added)
	// after we read it, so actx is already stale — drop it rather than
	// repopulate the cache with data an invalidation just cleared.
	contextCacheMu.Lock()
	if contextCacheGen[post.ID] == gen {
		contextCache[post.ID] = &contextCacheEntry{
			ctx:         actx,
			fingerprint: fp,
			expiresAt:   time.Now().Add(contextCacheTTL),
		}
	}
	contextCacheMu.Unlock()

	return actx, nil
}

// invalidateContextCache drops any cached context block for a post so the next
// turn re-reads fresh data. Notes are not part of the cache fingerprint (only
// content/assets/phase are), so a note-only change would otherwise stay unseen
// for up to the TTL — the createNote tool calls this after persisting a note.
func invalidateContextCache(postID string) {
	contextCacheMu.Lock()
	// Bump the generation so any assembly already in flight (which captured the
	// old generation before this invalidation) refuses to write its now-stale
	// result back into the cache.
	contextCacheGen[postID]++
	delete(contextCache, postID)
	contextCacheMu.Unlock()
}

func assembleContext(
	ctx context.Context,
	post *models.Post,
	campaign *models.Campaign,
	resolved *brandresolve.Resolved,
	repos PostAssistantRepos,
	systemTmpl, contextTmpl *template.Template,
) (*assistantContext, error) {
	var phaseName, phaseDescription string
	if post.CampaignTypePhase != nil {
		phaseName = post.CampaignTypePhase.Name
		phaseDescription = post.CampaignTypePhase.Purpose
	}

	summaries, err := buildAssetSummaries(ctx, post.UsedAssetIDs, repos)
	if err != nil {
		return nil, err
	}

	versions, err := buildVersionSummaries(ctx, post, repos)
	if err != nil {
		return nil, err
	}

	noteSummaries, err := buildNoteSummaries(ctx, post, repos)
	if err != nil {
		return nil, err
	}

	// Available platforms power the clonePost tool's platform resolution.
	// Best-effort: a load failure (or no platforms repo) just omits the
	// section — the tool still validates server-side.
	var platforms []platformOption
	if repos.Platforms != nil {
		if ps, perr := repos.Platforms.List(ctx); perr == nil {
			platforms = toPlatformOptions(ps)
		}
	}

	data := contextTemplateData{
		CampaignName:        campaign.Name,
		CampaignDescription: campaign.Description,
		TargetPersona:       campaign.TargetPersona,
		KeyMessages:         campaign.KeyMessages,
		ToneGuidelines:      campaign.ToneGuidelines,
		BrandBlock:          resolved.PromptBlock(post.PlatformID),
		Language:            campaign.Language,
		PhaseName:           phaseName,
		PhaseDescription:    phaseDescription,
		PostContent:         post.Content,
		Assets:              summaries,
		Platforms:           platforms,
		Versions:            versions,
		Notes:               noteSummaries,
	}

	systemPrompt, err := renderTemplate(systemTmpl, data)
	if err != nil {
		return nil, fmt.Errorf("render system prompt: %w", err)
	}
	contextBlock, err := renderTemplate(contextTmpl, data)
	if err != nil {
		return nil, fmt.Errorf("render context block: %w", err)
	}

	return &assistantContext{
		SystemPrompt: systemPrompt,
		ContextBlock: contextBlock,
	}, nil
}

type contextTemplateData struct {
	CampaignName        string
	CampaignDescription string
	TargetPersona       string
	KeyMessages         string
	ToneGuidelines      string
	// BrandBlock is the resolved brand voice/audience/guardrails block (CON-245),
	// which supersedes TargetPersona/ToneGuidelines in the template.
	BrandBlock       string
	Language         string
	PhaseName        string
	PhaseDescription string
	PostContent      string
	Assets           []assetSummary
	Platforms        []platformOption
	Versions         []versionSummary
	Notes            []noteSummary
}

// noteBodyPreviewChars bounds a single note's body in the context block so a
// long note can't blow the prompt budget. Draft theses are already short
// (≤500 chars); image prompts and side notes are typically short too.
const noteBodyPreviewChars = 800

// maxNotesInContext and maxNotesContextRunes bound the aggregate note section so
// a post with many (or many long) notes can't blow the prompt budget. Notes are
// consumed in repository order (draft_thesis first), so the pinned thesis is
// never the one dropped.
const (
	maxNotesInContext    = 20
	maxNotesContextRunes = 4000
)

// noteSummary is a single note surfaced to the model (CON-188). Body is
// included (notes are short) so the assistant can act on the captured thesis or
// prompt directly.
type noteSummary struct {
	Type  string
	Title string
	Body  string
}

// buildNoteSummaries lists the post's notes for the context block, draft
// theses first then oldest-first (the repo's ordering). Best-effort: a load
// error is propagated; no notes simply omits the section. Rendered into the
// cached context block — the same 5-minute TTL staleness window as the version
// history applies (a note added mid-session may not appear until the cache
// entry expires or the post content changes).
func buildNoteSummaries(ctx context.Context, post *models.Post, repos PostAssistantRepos) ([]noteSummary, error) {
	if repos.Notes == nil {
		return nil, nil
	}
	list, err := repos.Notes.ListByPostID(ctx, post.ID)
	if err != nil {
		return nil, err
	}
	out := make([]noteSummary, 0, len(list))
	total := 0
	for _, n := range list {
		if len(out) >= maxNotesInContext {
			break
		}
		body := truncateRunes(n.Body, noteBodyPreviewChars)
		bodyRunes := utf8.RuneCountInString(body)
		// Always include the first note (draft_thesis); after that, stop once the
		// combined body budget would be exceeded.
		if len(out) > 0 && total+bodyRunes > maxNotesContextRunes {
			break
		}
		out = append(out, noteSummary{Type: string(n.Type), Title: n.Title, Body: body})
		total += bodyRunes
	}
	return out, nil
}

// versionSummary is a single entry in the post's version history,
// surfaced to the model so it can map "v2" / "the previous version" to a
// concrete version number for the restoreVersion tool. Content is
// deliberately omitted to keep the prompt small — only the metadata the
// model needs to choose a number is included.
type versionSummary struct {
	Number    int
	Note      string
	Creator   string
	IsCurrent bool
}

// buildVersionSummaries lists the post's saved versions (oldest first),
// flagging the latest (highest version_number) as the current HEAD.
// Best-effort: a load failure is propagated, but no versions simply
// omits the section. Rendered into the cached context block; a content
// change (the common case) busts the cache, so the only staleness window
// is a manual snapshot within the TTL — harmless, since the restore tool
// and service always validate the chosen number against the live DB.
func buildVersionSummaries(ctx context.Context, post *models.Post, repos PostAssistantRepos) ([]versionSummary, error) {
	if repos.Versions == nil {
		return nil, nil
	}
	versions, err := repos.Versions.ListByPostID(ctx, post.ID)
	if err != nil {
		return nil, err
	}
	out := make([]versionSummary, 0, len(versions))
	for i, v := range versions {
		out = append(out, versionSummary{
			Number:  v.VersionNumber,
			Note:    v.Note,
			Creator: v.Creator,
			// The latest snapshot is the current HEAD. ListByPostID is
			// ordered ascending, so that's the last element. Content
			// equality would wrongly flag every snapshot that shares the
			// restored content (e.g. after restoring v1, both v1 and the
			// appended restore version match the live content).
			IsCurrent: i == len(versions)-1,
		})
	}
	return out, nil
}

// platformOption is a platform the user can clone a post onto, with its
// available post-type slugs — surfaced to the model for the clonePost tool.
type platformOption struct {
	Name      string
	PostTypes []string
}

func toPlatformOptions(platforms []models.Platform) []platformOption {
	opts := make([]platformOption, 0, len(platforms))
	for _, p := range platforms {
		types := make([]string, 0, len(p.PostTypes))
		for slug := range p.PostTypes {
			types = append(types, slug)
		}
		slices.Sort(types)
		opts = append(opts, platformOption{Name: p.Name, PostTypes: types})
	}
	return opts
}

func buildAssetSummaries(ctx context.Context, assetIDs []string, repos PostAssistantRepos) ([]assetSummary, error) {
	if len(assetIDs) == 0 {
		return nil, nil
	}

	// Fetch all assets in parallel.
	type result struct {
		index   int
		summary assetSummary
		ok      bool
	}
	results := make([]result, len(assetIDs))
	var wg sync.WaitGroup
	for i, id := range assetIDs {
		wg.Go(func() {
			idx, assetID := i, id
			asset, err := repos.Assets.GetByID(ctx, assetID)
			if err != nil {
				return
			}
			chunks, err := repos.Chunks.GetByAssetID(ctx, assetID)
			if err != nil {
				return
			}
			preview := ""
			if len(chunks) > 0 {
				preview = truncateRunes(chunks[0].Content, previewChars)
			}
			assetType := ""
			if asset.Type != nil {
				assetType = *asset.Type
			}
			results[idx] = result{
				index: idx,
				summary: assetSummary{
					ID:         asset.ID,
					Name:       asset.Title,
					Type:       assetType,
					ChunkCount: len(chunks),
					Preview:    preview,
				},
				ok: true,
			}
		})
	}
	wg.Wait()

	summaries := make([]assetSummary, 0, len(assetIDs))
	for _, r := range results {
		if r.ok {
			summaries = append(summaries, r.summary)
		}
	}

	// Shorten previews proportionally if combined size exceeds budget.
	totalChars := 0
	for _, s := range summaries {
		totalChars += len(s.Preview)
	}
	if totalChars > maxSummaryChars && len(summaries) > 0 {
		budget := maxSummaryChars / len(summaries)
		for i := range summaries {
			summaries[i].Preview = truncateRunes(summaries[i].Preview, budget)
		}
	}

	return summaries, nil
}

// buildSchedulingContext renders the per-turn scheduling context block
// (CON-78) injected into the user turn: the current time in both the
// workspace timezone and UTC, the post's status, how its platform routes
// (auto vs manual publish), and a readiness summary so the model can
// confirm the right mode and pre-empt a promote-time validation failure
// before it ever calls schedulePost. Computed fresh every turn (current
// time changes), so it is deliberately kept out of the cached context.
func buildSchedulingContext(ctx context.Context, post *models.Post, repos PostAssistantRepos) string {
	loc, tzName := settings.WorkspaceTimezone(ctx, repos.Settings)
	now := time.Now()

	var b strings.Builder
	b.WriteString("## Scheduling context\n")
	b.WriteString(fmt.Sprintf("- Current time: %s (%s) — UTC %s\n",
		now.In(loc).Format("Mon Jan 2, 2006 15:04"), tzName, now.UTC().Format(time.RFC3339)))
	b.WriteString(fmt.Sprintf("- Workspace timezone: %s. Resolve relative times (\"tomorrow 9am\", \"next Monday morning\") against THIS zone and pass the result as ISO-8601 with its offset. Defaults: morning=09:00, afternoon=14:00, evening=18:00 unless the user says otherwise — state the assumed time when you confirm.\n", tzName))
	b.WriteString(fmt.Sprintf("- Post status: %s\n", post.Status))

	if post.PlatformID == "" {
		b.WriteString("- Platform: none set — the post cannot be scheduled until a platform is chosen.\n")
	} else {
		platform := post.Platform
		if (platform == nil || platform.ID != post.PlatformID) && repos.Platforms != nil {
			if p, err := repos.Platforms.GetByID(ctx, post.PlatformID); err == nil {
				platform = p
			}
		}
		name := post.PlatformID
		if platform != nil {
			name = platform.Name
		}
		autoPublish := false
		if supported := zernio.LookupSupportedBySqid(post.PlatformID); supported != nil && repos.Allowlist != nil {
			if ok, err := repos.Allowlist.Contains(ctx, supported.ZernioID); err == nil {
				autoPublish = ok
			}
		}
		mode := "MANUAL publishing — it will be scheduled_for_manual_publishing and will NOT auto-send. Tell the user it won't auto-publish."
		if autoPublish {
			mode = "AUTO-publish — it will be sent automatically at the scheduled time."
		}
		b.WriteString(fmt.Sprintf("- Platform: %s → %s\n", name, mode))
	}

	b.WriteString("- Readiness: ")
	switch post.Status {
	case models.PostStatusReadyForPublish:
		b.WriteString("ready — schedule directly (no allowPromote needed).\n")
	case models.PostStatusDraft:
		reasons := schedulingReadinessReasons(ctx, post, repos)
		switch {
		case post.PlatformID == "":
			b.WriteString("draft with no platform — cannot schedule.\n")
		case len(reasons) == 0:
			b.WriteString("draft, but it passes pre-publish validation — call schedulePost with allowPromote:true to promote and schedule in one step.\n")
		default:
			b.WriteString("draft that is NOT yet publishable. Problems: " + strings.Join(reasons, "; ") + ". Decline and tell the user exactly what to fix; do NOT schedule.\n")
		}
	default:
		b.WriteString(fmt.Sprintf("status %q is not schedulable (only ready_for_publish, or a draft you promote). If it's already scheduled, it must be cancelled first to reschedule.\n", post.Status))
	}

	b.WriteString("\nTo schedule: resolve the time, then CONFIRM the absolute time + the auto/manual mode and WAIT for the user's \"yes\" before calling schedulePost. Never call schedulePost on the first turn.\n")
	return b.String()
}

// schedulingReadinessReasons runs the CON-74 pre-publish rules read-only
// so the scheduling context can list what a draft is missing before the
// model attempts a promote. Mirrors the checks the shared service
// enforces; best-effort (a load failure yields no reasons).
func schedulingReadinessReasons(ctx context.Context, post *models.Post, repos PostAssistantRepos) []string {
	if repos.Platforms == nil || post.PlatformID == "" {
		return nil
	}
	platform := post.Platform
	if platform == nil || platform.ID != post.PlatformID {
		p, err := repos.Platforms.GetByID(ctx, post.PlatformID)
		if err != nil {
			return nil
		}
		platform = p
	}
	var atts []models.PostAttachment
	if repos.Attachments != nil {
		if a, err := repos.Attachments.ListByPostID(ctx, post.ID); err == nil {
			atts = a
		}
	}
	// CON-284: the per-segment gate for threads, whole-post gate otherwise.
	errsByPlatform := platforms.ValidatePublishReadiness(post, platform, atts)
	var reasons []string
	for _, errs := range errsByPlatform {
		for _, e := range errs {
			reasons = append(reasons, e.Message)
		}
	}
	return reasons
}

func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	return string(runes[:max]) + "…"
}

func renderTemplate(tmpl *template.Template, data any) (string, error) {
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}
