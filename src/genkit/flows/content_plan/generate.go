package content_plan

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/usecase/brandresolve"
	"github.com/ogen-app/ogen/src/usecase/campaigngoal"
	"github.com/ogen-app/ogen/src/usecase/scheduling"
	"github.com/ogen-app/ogen/src/usecase/seriesplan"
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

// seriesRuns is the campaign's series runs that claim plan slots, each with
// the days its rhythm puts its posts on over the scope. A targeted run plans
// no series. Days are counted on the campaign's calendar dates in its
// timezone, the dates the plan's window checks. It fails open: a failed
// read plans without series and says so.
func seriesRuns(ctx context.Context, repos ContentPlanRepos, campaign *models.Campaign, scope planScope, targeted bool) ([]seriesRun, []string) {
	if repos.Series == nil || targeted {
		return nil, nil
	}
	const unread = "the campaign's series could not be read, so the plan reserves no series slots"
	runs, err := repos.Series.Runs(ctx, campaign.ID)
	if err != nil {
		slog.WarnContext(ctx, "series runs read failed; planning without series",
			logging.AttrComponent, logComponent, logging.AttrError, err)
		return nil, []string{unread}
	}
	loc, err := settings.ResolveTimezone(campaign.Timezone)
	if err != nil {
		loc = time.UTC
	}
	start, end := calendarDay(scope.start, loc), calendarDay(scope.end, loc)
	var out []seriesRun
	for _, run := range runs {
		if run.Rhythm == nil {
			continue // occasional: claims no slots
		}
		series, err := repos.Series.GetByID(ctx, run.SeriesID)
		if errors.Is(err, sql.ErrNoRows) {
			continue // deleted since
		}
		if err != nil {
			slog.WarnContext(ctx, "series read failed; planning without series",
				logging.AttrComponent, logComponent, "series_id", run.SeriesID, logging.AttrError, err)
			return nil, []string{unread}
		}
		days := seriesplan.Days(run.Rhythm, &start, &end, loc)
		if len(days) == 0 {
			continue
		}
		r := seriesRun{series: *series, days: make([]string, len(days))}
		for i, d := range days {
			r.days[i] = d.Format(time.DateOnly)
		}
		out = append(out, r)
	}
	return out, nil
}

// calendarDay is t's calendar date at midnight in loc.
func calendarDay(t time.Time, loc *time.Location) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
}

// claims is how many plan slots the runs take.
func claims(runs []seriesRun) int {
	n := 0
	for _, r := range runs {
		n += len(r.days)
	}
	return n
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
	runs, warnings := seriesRuns(ctx, repos, campaign, scope, tgt != nil)
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
	usage := flowkit.Usage{Recorder: cfg.Recorder, Model: mc, Feature: "content_plan", Component: logComponent}
	gen := &postGenerator{
		g:            g,
		modelName:    mc.Ref,
		systemPrompt: systemPrompt,
		modelOpts: []ai.GenerateOption{
			ai.WithMiddleware(cfg.Provider.CallMiddleware(modelconfig.FlowContentPlan, usage.Record)),
			cfg.Provider.CallConfig(mc.Model, cmp.Or(cfg.MaxOutputTokens, 8192)),
		},
		usage:    usage,
		validate: newPostValidator(platforms, phaseIDSet(scope.phases), data.StartDate, data.EndDate),
		persist:  persistCited(campaign, scope, repos, assets, brandVoiceID),
	}

	perBatch, parallel := batchLimits(cfg, max(scope.count, claims(runs)))
	batches, planWarnings := planRun(scope, runs, platforms, perBatch)
	warnings = append(warnings, planWarnings...)
	if len(batches) == 0 {
		posts, _, err := generateUnplanned(ctx, gen, cfg, data, onEvent)
		return posts, warnings, err
	}
	slog.InfoContext(ctx, "planned batches", logging.AttrComponent, logComponent, "batches", len(batches), "total_posts", scope.count, "series_posts", claims(runs), "posts_per_batch", perBatch, "parallel", parallel)

	genBatch := func(ctx context.Context, spec batchSpec, emit OnEventFunc) ([]DraftPost, error) {
		batchData := data
		if !spec.Unplanned {
			batchData.Batch = &spec
		}
		userPrompt, err := flowkit.RenderTemplate(cfg.userTmpl, batchData)
		if err != nil {
			return nil, fmt.Errorf("render user prompt for batch %d: %w", spec.Index, err)
		}
		slog.DebugContext(ctx, "batch user prompt", logging.AttrComponent, logComponent, "batch", spec.Index+1, "total", len(batches), "posts", spec.PostCount, "window_start", spec.DateWindow.Start, "window_end", spec.DateWindow.End, "prompt", userPrompt)
		// Persistence is capped at the batch's planned size.
		return gen.stream(ctx, userPrompt, spec.GlobalStartIndex, spec.PostCount, spec.Slots, emit)
	}
	posts, batchWarnings, err := runBatchesParallel(ctx, batches, parallel, genBatch, onEvent)
	return posts, append(warnings, batchWarnings...), err
}

// planRun lays out a run's batches: the series' posts first, on their
// rhythms' days, then the rest of the goal, which the series' slots come out
// of. A run without a goal leaves the rest to the model, so beside series it
// ends on an unplanned batch, and with none it has no batches at all. Batches
// are numbered across the whole plan.
func planRun(scope planScope, runs []seriesRun, platforms []resolvedPlatform, perBatch int) ([]batchSpec, []string) {
	var warnings []string
	claimed := claims(runs)
	regular := scope.count
	if regular > 0 {
		if claimed > regular {
			warnings = append(warnings, fmt.Sprintf("the series claim %d slots, more than the goal's %d, so the plan holds only series ideas", claimed, regular))
		}
		regular = max(regular-claimed, 0)
	}
	batches := seriesBatches(runs, scope.phases, platforms, scope.start, scope.end, perBatch)
	batches = append(batches, planBatches(regular, scope.phases, platforms, scope.start, scope.end, perBatch)...)
	if len(batches) == 0 {
		return nil, warnings
	}
	if scope.count == 0 {
		batches = append(batches, batchSpec{Unplanned: true})
	}
	next := 0
	for i := range batches {
		batches[i].Index, batches[i].GlobalStartIndex = i, next
		next += batches[i].PostCount
	}
	return batches, warnings
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
	posts, err := gen.stream(ctx, userPrompt, 0, 0, nil, onEvent)
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
// persist; 0 is uncapped. slots, when set, places each post in turn on its
// series slot. Whatever was persisted is returned even on error, so work
// survives a failed call.
func (gen *postGenerator) stream(ctx context.Context, userPrompt string, startIndex, expected int, slots []fixedSlot, onEvent OnEventFunc) ([]DraftPost, error) {
	opts := append([]ai.GenerateOption{
		ai.WithModelName(gen.modelName),
		ai.WithSystem(gen.systemPrompt),
		ai.WithPrompt(userPrompt),
	}, gen.modelOpts...)
	sink := &postSink{gen: gen, startIndex: startIndex, expected: expected, slots: slots, onEvent: onEvent, persisted: map[int]bool{}}
	res, err := flowkit.StreamObjects(ctx, gen.g, func(pos int, raw string) {
		post, ok := parseAndTrimPost(raw)
		if !ok {
			slog.WarnContext(ctx, "malformed post chunk", logging.AttrComponent, logComponent, "len", len(raw), "raw_preview", logging.Preview(raw, 100))
			emit(onEvent, SSEEventWarning, WarningPayload{Message: fmt.Sprintf("malformed post chunk: %.80s", raw)})
			return
		}
		sink.add(ctx, post, pos)
	}, opts...)
	if errors.Is(err, llm.ErrRefused) {
		// The blocking retry would be refused (and billed) again.
		return sink.posts, &AIError{Msg: fmt.Sprintf("model call failed: %v", err)}
	}
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
	// slots are a series batch's slots, the next one taken by each post
	// that persists; nil for the other batches.
	slots   []fixedSlot
	onEvent OnEventFunc

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
	// A series post goes on the slot at its place in the model's array, the
	// one it was written for, so a dropped post leaves only its own slot
	// empty; a post past the last slot has none. No other post is of a
	// series, whatever the model wrote.
	post.SeriesID = nil
	if s.slots != nil {
		if pos >= len(s.slots) {
			emit(s.onEvent, SSEEventWarning, WarningPayload{Message: fmt.Sprintf("post %q dropped: the series has no slot left for it", post.Title)})
			return
		}
		s.slots[pos].place(&post)
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
	emit(s.onEvent, SSEEventIdea, IdeaEventPayload{Idea: post, Index: s.startIndex + len(s.posts), ID: id})
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

// persistCited returns the generator's persist step. Ideas are bound only to
// the retrieved assets the model reported using, so a hallucinated id never
// persists and an idea citing none records none (and is exploratory).
// Snapping stays inside the active window, so a targeted run never schedules
// an idea outside it.
func persistCited(campaign *models.Campaign, scope planScope, repos ContentPlanRepos, assets []resolvedPiece, brandVoiceID *string) func(context.Context, *DraftPost) (string, error) {
	grounded := idSet(assetIDsOf(assets))
	return func(ctx context.Context, dp *DraftPost) (string, error) {
		dp.BrandVoiceID = brandVoiceID
		// The streamed idea cites what the saved one is bound to.
		dp.AssetRefs = groundedRefs(dp.AssetRefs, grounded)
		return persistIdea(ctx, dp, campaign, &scope.start, &scope.end, repos.Ideas, dp.AssetRefs)
	}
}

// persistIdea saves one planned item as an idea and returns its id. The
// streaming path calls this for each parsed-and-validated item immediately
// rather than aggregating to a final batch — a client disconnect mid-stream
// leaves whatever was already saved, and a hard *AIError from one batch never
// rolls back the surviving batches' ideas.
//
// The idea is the campaign's, written down for its owner: the model's
// bullet-point thesis is its note, and the platform, post type, phase and
// (snapped) publish date are its plan. It records the assets it drew on and
// whether it was grounded in them; nothing is on the calendar as a post
// until someone compiles or drafts it.
func persistIdea(ctx context.Context, dp *DraftPost, campaign *models.Campaign, windowStart, windowEnd *time.Time, ideas repository.IdeaRepository, usedAssetIDs []string) (string, error) {
	id, err := models.NewID()
	if err != nil {
		return "", err
	}

	// Snap the model's date to an enabled publishing day inside the active
	// window — the campaign window, or the CON-114 targeting window — so a
	// targeted run never plans outside it. The snapped date is written back
	// onto dp so the streamed preview matches the saved plan.
	//
	// A series idea keeps its slot's day: the series' weekday outranks the
	// campaign's publishing days.
	loc, _ := settings.ResolveTimezone(campaign.Timezone)
	days := campaign.PublishingDays
	if dp.SeriesID != nil {
		days = nil
	}
	_, effDate, noEnabledDay := scheduling.ComposeScheduledAt(
		dp.PublishDate, id, loc, campaign.PublishingTime, days,
		campaign.SpreadMinutes, windowStart, windowEnd,
	)
	if noEnabledDay {
		slog.WarnContext(ctx, "no enabled publishing day in window; kept model date",
			logging.AttrComponent, "genkit.content_plan", "idea_id", id, "date", dp.PublishDate)
	}
	dp.PublishDate = effDate

	note := strings.TrimSpace(dp.Body)
	if tone := strings.TrimSpace(dp.ToneNotes); tone != "" {
		note = strings.TrimSpace(note + "\n\nTone: " + tone)
	}
	idea := &models.Idea{
		ID:            id,
		Title:         dp.Title,
		Note:          note,
		CampaignID:    &campaign.ID,
		Origin:        models.IdeaOriginContentPlan,
		AssetIDs:      append([]string{}, usedAssetIDs...),
		GroundingMode: models.GroundingFor(len(usedAssetIDs)),
		SeriesID:      dp.SeriesID,
		CreatedBy:     &campaign.CreatedBy,
	}
	if len(usedAssetIDs) > 0 {
		idea.SourceAssetID = &usedAssetIDs[0]
	}
	if dp.PlatformID != "" {
		idea.Plan.PlatformID = &dp.PlatformID
	}
	if dp.ContentType != "" {
		idea.Plan.PostType = &dp.ContentType
	}
	if dp.PhaseID != "" {
		idea.Plan.PhaseID = &dp.PhaseID
	}
	if d, err := models.ParseCalendarDate(effDate); err == nil {
		idea.Plan.TargetDate = &d
	}
	if err := ideas.Create(ctx, idea); err != nil {
		return "", err
	}
	saved := *idea
	dp.Saved = &saved
	return id, nil
}

// tailOf returns up to n bytes from the end of s, suitable for log output.
func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
