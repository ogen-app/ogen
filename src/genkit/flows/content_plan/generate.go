package content_plan

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/campaignphase"
	"github.com/ogen-app/ogen/src/domain/modelconfig"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/flowkit"
	"github.com/ogen-app/ogen/src/genkit/jsonstream"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/usecase/brandresolve"
	"github.com/ogen-app/ogen/src/usecase/campaigngoal"
	"github.com/ogen-app/ogen/src/usecase/scheduling"
	"github.com/ogen-app/ogen/src/usecase/settings"
)

const logComponent = "genkit.content_plan"

// targeting overrides the count, phases, and publish-date window a generation
// run uses. nil = the full-campaign plan (count from
// EstimatedPostCount, all phases, the campaign date range). The platform subset
// is applied by the caller via the platforms argument.
type targeting struct {
	count       int
	phases      []resolvedPhase
	windowStart time.Time
	windowEnd   time.Time
}

// planScope is the count, phases and publish-date window a run generates for.
type planScope struct {
	count      int
	phases     []resolvedPhase
	start, end time.Time
}

// resolveScope returns the full-campaign scope, or tgt's when targeting. The
// full plan generates estimated_post_count posts per goal_cadence period
// times the periods the campaign spans; 0 lets the model decide the count.
func resolveScope(campaign *models.Campaign, tgt *targeting) planScope {
	if tgt != nil {
		return planScope{count: tgt.count, phases: tgt.phases, start: tgt.windowStart, end: tgt.windowEnd}
	}
	loc, _ := settings.ResolveTimezone(campaign.Timezone)
	return planScope{
		count: campaigngoal.EffectiveCount(
			campaign.EstimatedPostCount, campaign.GoalCadence,
			campaign.StartDate, campaign.EndDate, loc,
		),
		phases: campaignPhases(campaign),
		start:  *campaign.StartDate,
		end:    *campaign.EndDate,
	}
}

// campaignPhases lists the campaign type's phases. A stored manual phase plan
// pins each phase's window; otherwise planBatches derives them from the
// campaign dates.
func campaignPhases(campaign *models.Campaign) []resolvedPhase {
	pinned := map[string]*dateWindow{}
	if windows, src := campaignphase.Resolve(campaign); src == campaignphase.SourceManual {
		for _, w := range windows {
			pinned[w.Phase.ID] = &dateWindow{Start: w.Start.Format(time.DateOnly), End: w.End.Format(time.DateOnly)}
		}
	}
	phases := make([]resolvedPhase, len(campaign.CampaignType.Phases))
	for i, p := range campaign.CampaignType.Phases {
		phases[i] = resolvedPhase{
			ID:       p.ID,
			Name:     p.Name,
			Purpose:  p.Purpose,
			Sequence: p.Sequence,
			Window:   pinned[p.ID],
		}
	}
	return phases
}

// resolveBrand returns the brand voice/audience/guardrails block, which
// supersedes the legacy tone/persona prose (falling back to it), plus the
// voice id stamped on each post. A run spans every target platform, so the
// voice's channel notes for all of them are appended. Fails open.
func resolveBrand(ctx context.Context, repos ContentPlanRepos, campaign *models.Campaign, platforms []resolvedPlatform) (string, *string) {
	resolved, err := brandresolve.Resolve(ctx, repos.Brands, campaign, nil)
	if err != nil {
		slog.WarnContext(ctx, "brand resolve failed; using legacy tone",
			logging.AttrComponent, logComponent, "error", err)
	}
	platformIDs := make([]string, len(platforms))
	for i, p := range platforms {
		platformIDs[i] = p.ID
	}
	block := resolved.PromptBlock("")
	if notes := resolved.ChannelNotesBlock(platformIDs); notes != "" {
		block += "\n\n" + notes
	}
	return block, resolved.VoiceID()
}

// batchLimits returns the posts-per-batch and parallelism for a run. A plan
// that fits one batch would leave the other workers idle, so the batch size
// shrinks until the plan splits into about maxParallel batches;
// MaxPostsPerBatch stays the upper cap.
func batchLimits(cfg ContentPlanFlowConfig, count int) (perBatch, parallel int) {
	perBatch = cfg.MaxPostsPerBatch
	if perBatch <= 0 {
		perBatch = 30
	}
	parallel = cfg.MaxParallelBatches
	if parallel <= 0 {
		parallel = 5
	}
	if count > 0 && parallel > 1 {
		if n := (count + parallel - 1) / parallel; n >= 1 && n < perBatch {
			perBatch = n
		}
	}
	return perBatch, parallel
}

func generatePosts(
	ctx context.Context,
	g *genkit.Genkit,
	campaign *models.Campaign,
	platforms []resolvedPlatform,
	assets []resolvedPiece,
	cfg ContentPlanFlowConfig,
	repos ContentPlanRepos,
	onEvent OnEventFunc,
	tgt *targeting,
) ([]DraftPost, []string, error) {
	scope := resolveScope(campaign, tgt)
	brandBlock, brandVoiceID := resolveBrand(ctx, repos, campaign, platforms)
	data := contentPlanTemplateData{
		Name:                    campaign.Name,
		Description:             campaign.Description,
		CampaignTypeLabel:       campaign.CampaignType.Label,
		CampaignTypeDescription: campaign.CampaignType.Description,
		Phases:                  scope.phases,
		TargetPersona:           campaign.TargetPersona,
		KeyMessages:             campaign.KeyMessages,
		ToneGuidelines:          campaign.ToneGuidelines,
		BrandBlock:              brandBlock,
		Language:                campaign.Language,
		StartDate:               scope.start.Format(time.DateOnly),
		EndDate:                 scope.end.Format(time.DateOnly),
		DayCount:                int(scope.end.Sub(scope.start).Hours() / 24),
		EstimatedPostCount:      scope.count,
		Platforms:               platforms,
		Assets:                  assets,
		PublishingDays:          scheduling.DayLabels(campaign.PublishingDays),
	}

	// The system prompt is identical for every batch.
	systemPrompt, err := flowkit.RenderTemplate(cfg.systemTmpl, data)
	if err != nil {
		return nil, nil, fmt.Errorf("render system prompt: %w", err)
	}
	slog.DebugContext(ctx, "system prompt", logging.AttrComponent, logComponent, "prompt", systemPrompt)

	mc := modelconfig.Resolve(ctx, modelconfig.FlowContentPlan, modelconfig.SlotMain)
	// Posts are bound only to the retrieved assets the model reported using,
	// so a hallucinated id never persists and a post citing none records none.
	grounded := idSet(assetIDsOf(assets))
	gen := &postGenerator{
		g:            g,
		modelName:    mc.Ref,
		systemPrompt: systemPrompt,
		modelOpts: []ai.GenerateOption{
			ai.WithMiddleware(cfg.Provider.RefusalGuard(modelconfig.FlowContentPlan)),
			cfg.Provider.CallConfig(mc.Model, cmp.Or(cfg.MaxOutputTokens, 8192)),
		},
		usage:    flowkit.Usage{Recorder: cfg.Recorder, Model: mc, Feature: "content_plan", Component: logComponent},
		validate: newPostValidator(platforms, phaseIDSet(scope.phases), data.StartDate, data.EndDate),
		// Snapping stays inside the active window, so a targeted run never
		// schedules a post outside it.
		persist: func(ctx context.Context, dp *DraftPost) (string, error) {
			dp.BrandVoiceID = brandVoiceID
			return persistOne(ctx, dp, campaign, &scope.start, &scope.end, repos.Posts, repos.Notes, groundedRefs(dp.AssetRefs, grounded))
		},
	}

	perBatch, parallel := batchLimits(cfg, scope.count)
	batches := planBatches(scope.count, scope.phases, platforms, scope.start, scope.end, perBatch)
	if len(batches) == 0 {
		return generateUnplanned(ctx, gen, cfg, data, onEvent)
	}
	slog.InfoContext(ctx, "planned batches", logging.AttrComponent, logComponent, "batches", len(batches), "total_posts", scope.count, "posts_per_batch", perBatch, "parallel", parallel)

	genBatch := func(ctx context.Context, spec batchSpec, emit OnEventFunc) ([]DraftPost, error) {
		batchData := data
		batchData.Batch = &spec
		userPrompt, err := flowkit.RenderTemplate(cfg.userTmpl, batchData)
		if err != nil {
			return nil, fmt.Errorf("render user prompt for batch %d: %w", spec.Index, err)
		}
		slog.DebugContext(ctx, "batch user prompt", logging.AttrComponent, logComponent, "batch", spec.Index+1, "total", len(batches), "posts", spec.PostCount, "window_start", spec.DateWindow.Start, "window_end", spec.DateWindow.End, "prompt", userPrompt)
		// Persistence is capped at the batch's planned size.
		return gen.stream(ctx, userPrompt, spec.GlobalStartIndex, spec.PostCount, emit)
	}
	return runBatchesParallel(ctx, batches, parallel, genBatch, onEvent)
}

// generateUnplanned runs a single uncapped generation when no batch plan
// exists (no count), letting the model decide how many posts the campaign
// warrants. Persisted posts are returned even on failure.
func generateUnplanned(ctx context.Context, gen *postGenerator, cfg ContentPlanFlowConfig, data contentPlanTemplateData, onEvent OnEventFunc) ([]DraftPost, []string, error) {
	userPrompt, err := flowkit.RenderTemplate(cfg.userTmpl, data)
	if err != nil {
		return nil, nil, fmt.Errorf("render user prompt: %w", err)
	}
	slog.DebugContext(ctx, "user prompt (no batch plan)", logging.AttrComponent, logComponent, "prompt", userPrompt)
	posts, err := gen.stream(ctx, userPrompt, 0, 0, onEvent)
	return posts, nil, err
}

func phaseIDSet(phases []resolvedPhase) map[string]bool {
	ids := make(map[string]bool, len(phases))
	for _, ph := range phases {
		ids[ph.ID] = true
	}
	return ids
}

// runBatchesParallel fans the planned batches out to up to maxParallel
// goroutines, serialising the optional emit callback so a single-writer SSE
// sink stays safe. Aggregation is partial-success: a per-batch failure
// becomes a warning and the surviving batches' posts are returned in batch
// order. When every batch fails the function returns an AIError so the
// caller can surface a hard "error" SSE event.
//
// gen is injected so tests can pass a stub that simulates timing, partial
// failures, and emit concurrency without calling Anthropic.
func runBatchesParallel(
	ctx context.Context,
	batches []batchSpec,
	maxParallel int,
	gen func(ctx context.Context, spec batchSpec, emit OnEventFunc) ([]DraftPost, error),
	onEvent OnEventFunc,
) ([]DraftPost, []string, error) {
	if maxParallel <= 0 {
		maxParallel = 1
	}

	// onEvent is called from per-batch goroutines; the SSE writer behind it
	// is single-writer, so we must serialise. The lock is held only for the
	// duration of one event emit — negligible contention.
	var emitMu sync.Mutex
	safeEmit := onEvent
	if onEvent != nil {
		safeEmit = func(name SSEEventKind, payload any) {
			emitMu.Lock()
			defer emitMu.Unlock()
			onEvent(name, payload)
		}
	}

	type batchResult struct {
		posts []DraftPost
		err   error
	}
	results := make([]batchResult, len(batches))

	sem := make(chan struct{}, maxParallel)
	var wg sync.WaitGroup

	for i := range batches {
		spec := batches[i]
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()

			// Per CON-66 generatePostsStreaming returns whatever was
			// persisted before the failure point — keep both the posts
			// AND the err so a batch that streamed 5 then fell back and
			// errored still contributes its 5 persisted rows to the
			// response.
			posts, err := gen(ctx, spec, safeEmit)
			if err != nil {
				slog.ErrorContext(ctx, "batch failed", logging.AttrComponent, "genkit.content_plan", "batch", i+1, "total", len(batches), "persisted", len(posts), logging.AttrError, err)
			} else {
				slog.InfoContext(ctx, "batch done", logging.AttrComponent, "genkit.content_plan", "batch", i+1, "total", len(batches), "posts", len(posts))
			}
			results[i] = batchResult{posts: posts, err: err}
		})
	}
	wg.Wait()

	var allPosts []DraftPost
	var warnings []string
	failures := 0
	for i, r := range results {
		// Always include persisted posts — even from a batch
		// that ultimately errored, the DB has the rows and the response
		// must reflect that.
		allPosts = append(allPosts, r.posts...)
		if r.err != nil {
			failures++
			warnings = append(warnings, fmt.Sprintf(
				"batch %d/%d (slots %d-%d) failed: %v",
				i+1, len(batches),
				batches[i].GlobalStartIndex,
				batches[i].GlobalStartIndex+batches[i].PostCount-1,
				r.err,
			))
		}
	}

	if failures == len(batches) {
		// Even when every batch errored, surviving persisted posts from
		// each batch's streaming phase are returned alongside the
		// AIError so the caller can surface them.
		return allPosts, warnings, &AIError{Msg: fmt.Sprintf("all %d batches failed; first error: %v", len(batches), results[0].err)}
	}
	return allPosts, warnings, nil
}

// postGenerator runs the post-generation calls of one content-plan run. It is
// shared by all batches and safe for concurrent use.
type postGenerator struct {
	g            *genkit.Genkit
	modelName    string
	systemPrompt string
	modelOpts    []ai.GenerateOption // middleware + call config, shared by every call
	usage        flowkit.Usage
	validate     postValidator
	// persist writes one post and returns its row id.
	persist func(ctx context.Context, post *DraftPost) (string, error)
}

// stream generates posts for userPrompt and validates and persists each one
// as soon as it is parsed, before its "post" event fires. On a stream failure
// it falls back to a blocking call, skipping the array positions already
// persisted. startIndex is the global slot index of the first post; emitted
// indexes are compact (failed posts take none). expected caps how many posts
// persist; 0 is uncapped. Whatever was persisted is returned even on error,
// so work survives a failed call.
func (gen *postGenerator) stream(ctx context.Context, userPrompt string, startIndex, expected int, onEvent OnEventFunc) ([]DraftPost, error) {
	opts := append([]ai.GenerateOption{
		ai.WithModelName(gen.modelName),
		ai.WithSystem(gen.systemPrompt),
		ai.WithPrompt(userPrompt),
	}, gen.modelOpts...)
	sink := &postSink{gen: gen, startIndex: startIndex, expected: expected, onEvent: onEvent, persisted: map[int]bool{}}
	res, err := flowkit.StreamObjects(ctx, gen.g, func(pos int, raw string) {
		post, ok := parseAndTrimPost(raw)
		if !ok {
			slog.WarnContext(ctx, "malformed post chunk", logging.AttrComponent, logComponent, "len", len(raw), "raw_preview", logging.Preview(raw, 100))
			emit(onEvent, SSEEventWarning, WarningPayload{Message: fmt.Sprintf("malformed post chunk: %.80s", raw)})
			return
		}
		sink.add(ctx, post, pos)
	}, opts...)
	if err != nil {
		return gen.fallback(ctx, sink, res.Objects, err, opts)
	}
	if resp := res.Response; resp != nil {
		gen.usage.LogTokens(ctx, "tokens", resp)
		text := resp.Text()
		slog.InfoContext(ctx, "stream finished", logging.AttrComponent, logComponent, "finish_reason", resp.FinishReason, "posts_persisted", len(sink.posts), "parsed", res.Objects, "chunks", res.Chunks, "bytes", res.Bytes, "response_len", len(text), "response_tail", tailOf(text, 200))
		gen.usage.Record(ctx, resp)
	}
	return sink.posts, nil
}

// fallback re-issues a broken stream as a blocking call. Positions persisted
// while streaming are skipped (first write wins even if the blocking text
// differs), since the model is close to deterministic in its first posts.
// Both calls record usage, so a partial double count is tolerated.
func (gen *postGenerator) fallback(ctx context.Context, sink *postSink, parsed int, streamErr error, opts []ai.GenerateOption) ([]DraftPost, error) {
	slog.WarnContext(ctx, "stream error, falling back to blocking Generate", logging.AttrComponent, logComponent, "persisted", len(sink.posts), "parsed", parsed, logging.AttrError, streamErr)
	resp, err := genkit.Generate(ctx, gen.g, opts...)
	if err != nil {
		return sink.posts, &AIError{Msg: fmt.Sprintf("model call failed (stream+fallback): %v", err)}
	}
	gen.usage.LogTokens(ctx, "tokens (fallback)", resp)
	gen.usage.Record(ctx, resp)

	text := jsonstream.StripFences(resp.Text())
	var posts []DraftPost
	if err := json.Unmarshal([]byte(text), &posts); err != nil {
		return sink.posts, &AIError{Msg: fmt.Sprintf("model response not valid JSON: %v\nraw: %.200s", err, text)}
	}
	for i, post := range posts {
		if sink.persisted[i] {
			continue
		}
		post.Body = trimBody(post.Body)
		sink.add(ctx, post, i)
	}
	return sink.posts, nil
}

// postSink validates and persists the posts of one stream call.
type postSink struct {
	gen        *postGenerator
	startIndex int
	expected   int
	onEvent    OnEventFunc

	posts []DraftPost
	// persisted holds the raw array positions (counting invalid attempts)
	// that were persisted, so the fallback can skip them.
	persisted map[int]bool
}

// add persists post unless the batch is full; validation and persist
// failures become warnings. The cap matters because the model can
// over-produce (three posts for a "generate exactly 1" batch).
func (s *postSink) add(ctx context.Context, post DraftPost, pos int) {
	if !withinCount(len(s.posts), s.expected) {
		return
	}
	if err := s.gen.validate(post); err != nil {
		emit(s.onEvent, SSEEventWarning, WarningPayload{Message: fmt.Sprintf("post %q dropped: %s", post.Title, err)})
		return
	}
	id, err := s.gen.persist(ctx, &post)
	if err != nil {
		slog.ErrorContext(ctx, "persist failed for post", logging.AttrComponent, logComponent, "title", post.Title, logging.AttrError, err)
		emit(s.onEvent, SSEEventWarning, WarningPayload{Message: fmt.Sprintf("post %q persist failed: %v", post.Title, err)})
		return
	}
	s.persisted[pos] = true
	emit(s.onEvent, SSEEventPost, PostEventPayload{Post: post, Index: s.startIndex + len(s.posts), ID: id})
	s.posts = append(s.posts, post)
}

// parseAndTrimPost unmarshals a raw JSON object string into a DraftPost and
// trims the body to maxBodyRunes. Returns (post, true) on success.
func parseAndTrimPost(raw string) (DraftPost, bool) {
	var post DraftPost
	if err := json.Unmarshal([]byte(raw), &post); err != nil {
		slog.Warn("skipping malformed post chunk", logging.AttrComponent, "genkit.content_plan", logging.AttrError, err)
		return DraftPost{}, false
	}
	post.Body = trimBody(post.Body)
	return post, true
}

// trimBody truncates body to maxBodyRunes Unicode code points.
func trimBody(body string) string {
	const maxBodyRunes = 500
	if runes := []rune(body); len(runes) > maxBodyRunes {
		return string(runes[:maxBodyRunes])
	}
	return body
}

// withinCount reports whether another post may still be persisted for a batch
// that asked for expectedCount posts. expectedCount <= 0 means uncapped — the
// count-less fallback where the generation model decides how many to produce.
func withinCount(persisted, expectedCount int) bool {
	return expectedCount <= 0 || persisted < expectedCount
}

// persistOne inserts a single DraftPost as a new Post row and returns the
// generated row ID. The streaming path calls this for each
// parsed-and-validated post immediately rather than aggregating to a final
// CreateBatch — a client disconnect mid-stream leaves whatever was already
// persisted in the database, and a hard *AIError from one batch never rolls
// back the surviving batches' rows.
//
// The model's bullet-point thesis (dp.Body) is not written into the post
// body. The post is created with an empty body and the thesis is
// stored as a draft_thesis note, so the assistant can later expand it into copy.
func persistOne(ctx context.Context, dp *DraftPost, campaign *models.Campaign, windowStart, windowEnd *time.Time, postRepo repository.PostRepository, noteRepo repository.PostNoteRepository, usedAssetIDs []string) (string, error) {
	id, err := models.NewID()
	if err != nil {
		return "", err
	}

	// Compose scheduled_at from the campaign's scheduling settings —
	// snap the model's date to an enabled publishing day, place it at the
	// publishing time in the campaign timezone, ± deterministic spread. The
	// (possibly snapped) date is written back onto dp so the streamed preview
	// matches the persisted instant. Snapping is bounded by the active generation
	// window (windowStart/windowEnd) — the campaign window, or the CON-114
	// targeting window — so a targeted run never snaps a post outside its window.
	loc, _ := settings.ResolveTimezone(campaign.Timezone)
	scheduledAt, effDate, noEnabledDay := scheduling.ComposeScheduledAt(
		dp.PublishDate, id, loc, campaign.PublishingTime, campaign.PublishingDays,
		campaign.SpreadMinutes, windowStart, windowEnd,
	)
	if noEnabledDay {
		slog.WarnContext(ctx, "no enabled publishing day in window; kept model date",
			logging.AttrComponent, "genkit.content_plan", "post_id", id, "date", dp.PublishDate)
	}
	dp.PublishDate = effDate

	var phaseID *string
	if dp.PhaseID != "" {
		phaseID = &dp.PhaseID
	}

	row := &models.Post{
		ID:                  id,
		CampaignID:          campaign.ID,
		PlatformID:          dp.PlatformID,
		PlatformPostType:    dp.ContentType,
		Title:               dp.Title,
		Content:             "",
		MediaURLs:           models.StringSlice{},
		Status:              models.PostStatusDraft,
		CTAType:             models.CTATypeNone,
		CTAUrl:              "",
		TargetAudienceNotes: dp.ToneNotes,
		UsedAssetIDs:        models.StringSlice(usedAssetIDs),
		CampaignTypePhaseID: phaseID,
		ScheduledAt:         scheduledAt,
		BrandVoiceID:        dp.BrandVoiceID, // Provenance of the voice it was written in
		CreatedBy:           campaign.CreatedBy,
	}
	if err := postRepo.Create(ctx, row); err != nil {
		return "", err
	}

	// Capture the thesis as a draft_thesis note. Best-effort: a
	// note-write failure must not discard the already-persisted post,
	// so it is logged and swallowed rather than returned. An empty thesis
	// creates no note.
	if noteRepo != nil {
		if body := strings.TrimSpace(dp.Body); body != "" {
			if err := createDraftThesisNote(ctx, noteRepo, id, campaign.CreatedBy, body); err != nil {
				slog.ErrorContext(ctx, "draft thesis note create failed", logging.AttrComponent, "genkit.content_plan", "post_id", id, logging.AttrError, err)
			}
		}
	}
	return id, nil
}

// createDraftThesisNote persists the content-plan thesis as a draft_thesis note
// (origin content_plan, authored by the campaign owner).
func createDraftThesisNote(ctx context.Context, noteRepo repository.PostNoteRepository, postID, createdBy, body string) error {
	noteID, err := models.NewID()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	return noteRepo.Create(ctx, &models.PostNote{
		ID:        noteID,
		PostID:    postID,
		Type:      models.PostNoteTypeDraftThesis,
		Title:     "Draft thesis",
		Body:      body,
		Origin:    models.PostNoteOriginContentPlan,
		CreatedBy: createdBy,
		CreatedAt: now,
		UpdatedAt: now,
	})
}

// tailOf returns up to n bytes from the end of s, suitable for log output.
func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
