package content_plan

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/usecase/brandresolve"
	"github.com/ogen-app/ogen/src/usecase/campaigngoal"
	"github.com/ogen-app/ogen/src/usecase/scheduling"
	"github.com/ogen-app/ogen/src/usecase/settings"
)

// jsonPostScanner incrementally scans a stream of JSON text and yields complete
// top-level JSON objects (i.e. each element of the outer array) as they arrive.
type jsonPostScanner struct {
	buf      []byte
	depth    int
	inStr    bool
	escaped  bool
	objStart int // index of the opening '{' of the current object; -1 when not inside one
}

func newJSONPostScanner() *jsonPostScanner {
	return &jsonPostScanner{objStart: -1}
}

// push appends chunk to the internal buffer and returns all newly-complete
// JSON object strings found since the last call.
func (s *jsonPostScanner) push(chunk string) []string {
	var complete []string
	for i := 0; i < len(chunk); i++ {
		c := chunk[i]
		s.buf = append(s.buf, c)
		pos := len(s.buf) - 1

		if s.escaped {
			s.escaped = false
			continue
		}
		if s.inStr {
			switch c {
			case '\\':
				s.escaped = true
			case '"':
				s.inStr = false
			}
			continue
		}
		switch c {
		case '"':
			s.inStr = true
		case '{':
			if s.depth == 0 {
				s.objStart = pos
			}
			s.depth++
		case '}':
			s.depth--
			if s.depth == 0 && s.objStart >= 0 {
				complete = append(complete, string(s.buf[s.objStart:pos+1]))
				s.objStart = -1
			}
		}
	}
	return complete
}

// targeting overrides the count, phases, and publish-date window a generation
// run uses (CON-114). nil = the full-campaign plan (count from
// EstimatedPostCount, all phases, the campaign date range). The platform subset
// is applied by the caller via the platforms argument.
type targeting struct {
	count       int
	phases      []resolvedPhase
	windowStart time.Time
	windowEnd   time.Time
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
	// CON-182: the full-campaign plan generates estimated_post_count posts PER
	// goal_cadence period × the number of periods the campaign spans (0 when no
	// per-period count is set → the model decides the count, as before). The
	// targeting path below overrides this with its explicit count.
	loc, _ := settings.ResolveTimezone(campaign.Timezone)
	estCount := campaigngoal.EffectiveCount(
		campaign.EstimatedPostCount, campaign.GoalCadence,
		campaign.StartDate, campaign.EndDate, loc,
	)
	startDate, endDate := *campaign.StartDate, *campaign.EndDate

	phases := make([]resolvedPhase, len(campaign.CampaignType.Phases))
	for i, p := range campaign.CampaignType.Phases {
		phases[i] = resolvedPhase{
			ID:       p.ID,
			Name:     p.Name,
			Purpose:  p.Purpose,
			Sequence: p.Sequence,
		}
	}

	// CON-114 targeting: restrict the run to an explicit count, a single phase,
	// and a custom publish-date window (platforms are already the caller's subset).
	if tgt != nil {
		estCount = tgt.count
		phases = tgt.phases
		startDate, endDate = tgt.windowStart, tgt.windowEnd
	}

	dayCount := int(endDate.Sub(startDate).Hours() / 24)

	// CON-245: resolve the campaign's brand voice/audience/guardrails and inject
	// them as one block; it supersedes the legacy tone/persona prose (and falls
	// back to that prose when no brand material is set). Fails open on error.
	resolved, rerr := brandresolve.Resolve(ctx, repos.Brands, campaign, nil)
	if rerr != nil {
		slog.WarnContext(ctx, "brand resolve failed; using legacy tone",
			logging.AttrComponent, "genkit.content_plan", "error", rerr)
	}
	brandVoiceID := resolved.VoiceID()
	// This batch spans every target platform, so append the voice's per-channel
	// notes for all of them (PromptBlock's single-platform note can't cover a
	// multi-platform run).
	platformIDs := make([]string, len(platforms))
	for i, p := range platforms {
		platformIDs[i] = p.ID
	}
	brandBlock := resolved.PromptBlock("")
	if notes := resolved.ChannelNotesBlock(platformIDs); notes != "" {
		brandBlock += "\n\n" + notes
	}

	data := contentPlanTemplateData{
		Name:                    campaign.Name,
		Description:             campaign.Description,
		CampaignTypeLabel:       campaign.CampaignType.Label,
		CampaignTypeDescription: campaign.CampaignType.Description,
		Phases:                  phases,
		TargetPersona:           campaign.TargetPersona,
		KeyMessages:             campaign.KeyMessages,
		ToneGuidelines:          campaign.ToneGuidelines,
		BrandBlock:              brandBlock,
		Language:                campaign.Language,
		StartDate:               startDate.Format("2006-01-02"),
		EndDate:                 endDate.Format("2006-01-02"),
		DayCount:                dayCount,
		EstimatedPostCount:      estCount,
		Platforms:               platforms,
		Assets:                  assets,
		PublishingDays:          scheduling.DayLabels(campaign.PublishingDays),
	}

	// System prompt is identical for every batch — render once.
	systemPrompt, err := renderTemplate(cfg.systemTmpl, data)
	if err != nil {
		return nil, nil, fmt.Errorf("render system prompt: %w", err)
	}
	slog.DebugContext(ctx, "system prompt", logging.AttrComponent, "genkit.content_plan", "prompt", systemPrompt)

	maxTokens := cfg.MaxOutputTokens
	if maxTokens == 0 {
		maxTokens = 8192
	}
	maxPostsPerBatch := cfg.MaxPostsPerBatch
	if maxPostsPerBatch <= 0 {
		maxPostsPerBatch = 30
	}
	maxParallel := cfg.MaxParallelBatches
	if maxParallel <= 0 {
		maxParallel = 5
	}

	modelName := cfg.Provider.Ref(llm.RoleGeneration)
	modelCfg := cfg.Provider.CallConfig(maxTokens)
	usageVendor := cfg.Provider.Vendor()
	usageModel := cfg.Provider.Model(llm.RoleGeneration)
	// recordUsage records one usage event per model call — the stream Done path
	// and the blocking fallback each call it once (a partial double-count on
	// fallback is tolerated, CON-86 §10). Nil recorder = no-op.
	recordUsage := func(ctx context.Context, resp *ai.ModelResponse) {
		cfg.Recorder.RecordResp(ctx, usageVendor, usageModel, "content_plan", resp)
	}

	// Per CON-66 every parsed post is validated and persisted inline,
	// before its post-event fires — the validator is built once and
	// shared across all batches; the persist closure binds the campaign
	// + post repository for the duration of the run.
	validPhaseIDs := make(map[string]bool, len(phases))
	for _, ph := range phases {
		validPhaseIDs[ph.ID] = true
	}
	validate := newPostValidator(platforms, validPhaseIDs, data.StartDate, data.EndDate)
	// CON-118: bind each post to the subset of retrieved assets the model
	// reported drawing on for that post (dp.AssetRefs), filtered to the ids
	// actually retrieved into context so a hallucinated id never persists. Posts
	// no longer all inherit the full retrieved set — a post that cited no asset
	// records an empty list.
	grounded := idSet(assetIDsOf(assets))
	// startDate/endDate are the active generation window — the campaign window, or
	// the CON-114 targeting window when tgt != nil. persistOne snaps each post's
	// publishing day within these bounds so a targeted run stays inside its window.
	persistFn := func(ctx context.Context, dp *DraftPost) (string, error) {
		dp.BrandVoiceID = brandVoiceID // CON-245: stamp the resolved voice on the post
		return persistOne(ctx, dp, campaign, &startDate, &endDate, repos.Posts, repos.Notes, groundedRefs(dp.AssetRefs, grounded))
	}

	// Fill the parallel budget (CON-112 perf): a plan that fits in one batch is
	// a single long Sonnet call that ignores maxParallel entirely — e.g. 30
	// posts generated as one 9.5k-token call runs ~170s while 4 of the 5 worker
	// slots sit idle. Shrink the effective batch size so the plan splits into
	// ~maxParallel batches that run concurrently (30 posts → 5×6 ≈ 5× faster).
	// MaxPostsPerBatch stays the upper cap; this only ever makes batches smaller.
	if estCount > 0 && maxParallel > 1 {
		if perBatch := (estCount + maxParallel - 1) / maxParallel; perBatch >= 1 && perBatch < maxPostsPerBatch {
			maxPostsPerBatch = perBatch
		}
	}

	// Single-shot fallback: no estimated count, or fewer slots than a single
	// batch. We still go through the batched path for consistency, but with
	// a single batch covering everything.
	batches := planBatches(estCount, phases, platforms, startDate, endDate, maxPostsPerBatch)
	if len(batches) == 0 {
		// EstimatedPostCount is 0 or otherwise unplannable — preserve the
		// pre-CON-67 behaviour of asking the model to decide the count
		// from campaign context.
		userPrompt, err := renderTemplate(cfg.userTmpl, data)
		if err != nil {
			return nil, nil, fmt.Errorf("render user prompt: %w", err)
		}
		slog.DebugContext(ctx, "user prompt (no batch plan)", logging.AttrComponent, "genkit.content_plan", "prompt", userPrompt)
		// expectedCount 0 = uncapped: no batch plan, so the model decides how
		// many posts the campaign warrants (pre-CON-67 behaviour).
		posts, genErr := generatePostsStreaming(ctx, g, modelName, systemPrompt, userPrompt, modelCfg, recordUsage, 0, 0, validate, persistFn, onEvent)
		if genErr != nil {
			// Even on hard failure, return what was persisted so the
			// caller's partial-success aggregation has the rows.
			return posts, nil, genErr
		}
		return posts, nil, nil
	}

	slog.InfoContext(ctx, "planned batches", logging.AttrComponent, "genkit.content_plan", "batches", len(batches), "total_posts", estCount, "posts_per_batch", maxPostsPerBatch, "parallel", maxParallel)

	gen := func(ctx context.Context, spec batchSpec, emit OnEventFunc) ([]DraftPost, error) {
		batchData := data
		batchData.Batch = &spec
		userPrompt, err := renderTemplate(cfg.userTmpl, batchData)
		if err != nil {
			return nil, fmt.Errorf("render user prompt for batch %d: %w", spec.Index, err)
		}
		slog.DebugContext(ctx, "batch user prompt", logging.AttrComponent, "genkit.content_plan", "batch", spec.Index+1, "total", len(batches), "posts", spec.PostCount, "window_start", spec.DateWindow.Start, "window_end", spec.DateWindow.End, "prompt", userPrompt)
		// Cap persistence at the batch's planned size so an over-producing model
		// can't inflate the count (CON-114).
		return generatePostsStreaming(ctx, g, modelName, systemPrompt, userPrompt, modelCfg, recordUsage, spec.GlobalStartIndex, spec.PostCount, validate, persistFn, emit)
	}
	return runBatchesParallel(ctx, batches, maxParallel, gen, onEvent)
}

// runBatchesParallel fans the planned batches out to up to maxParallel
// goroutines, serialising the optional emit callback so a single-writer SSE
// sink stays safe. Aggregation is partial-success: a per-batch failure
// becomes a warning and the surviving batches' posts are returned in batch
// order. When every batch fails the function returns an AIError so the
// caller can surface a hard "error" SSE event.
//
// Extracted for unit-testability — the production gen calls into Anthropic;
// tests pass a stub that simulates timing, partial failures, and emit
// concurrency without touching the network.
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
		// Always include persisted posts (CON-66) — even from a batch
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

// generatePostsStreaming attempts a streaming model call and inline-persists
// each parsed-and-validated DraftPost via persistFn before emitting it as a
// "post" SSE event with the new row's ID. Validation failures and persist
// failures emit "warning" SSE events; the run continues. On any stream
// failure the function falls back to a blocking genkit.Generate call —
// posts that were already persisted during streaming are tracked by raw
// response position and skipped in the fallback so we never double-insert.
//
// globalStartIndex is the slot index assigned to the first post produced by
// this call; emitted PostEventPayload.Index values are globalStartIndex +
// within-batch offset (compact — failed posts don't consume an index).
// Single-shot callers pass 0; batched callers pass the batch's
// GlobalStartIndex so the UI can place posts in deterministic order.
//
// Returns whatever was persisted, even on hard fallback failure — CON-66's
// guarantee is that work survives the call, so a downstream AIError must
// still hand back the persisted slice.
func generatePostsStreaming(
	ctx context.Context,
	g *genkit.Genkit,
	modelName, systemPrompt, userPrompt string,
	modelCfg ai.GenerateOption,
	recordUsage func(context.Context, *ai.ModelResponse),
	globalStartIndex int,
	expectedCount int,
	validate postValidator,
	persistFn func(ctx context.Context, post *DraftPost) (string, error),
	onEvent OnEventFunc,
) ([]DraftPost, error) {
	scanner := newJSONPostScanner()
	var posts []DraftPost
	// persistedPositions tracks the raw parse position (0-based, includes
	// invalid attempts) of every successfully persisted post in the
	// streaming phase, so the blocking-fallback below can skip them. The
	// model is approximately deterministic in its first-N positions, so a
	// position-based skip is more reliable than a content-equality check.
	persistedPositions := map[int]bool{}
	parsedPosition := 0
	var streamErr error
	var chunkCount int
	var totalBytes int

	tryPersist := func(post DraftPost, position int) {
		// CON-114: never persist more than this batch asked for. The generation
		// model can over-produce (e.g. stream 3 posts for a "generate exactly 1"
		// batch); without this cap every extra valid post is persisted, so a
		// request for 1 post yielded 3. expectedCount <= 0 = uncapped (the
		// count-less fallback where the model decides how many to produce).
		if !withinCount(len(posts), expectedCount) {
			return
		}
		if err := validate(post); err != nil {
			emit(onEvent, SSEEventWarning, WarningPayload{
				Message: fmt.Sprintf("post %q dropped: %s", post.Title, err),
			})
			return
		}
		id, err := persistFn(ctx, &post)
		if err != nil {
			slog.ErrorContext(ctx, "persist failed for post", logging.AttrComponent, "genkit.content_plan", "title", post.Title, logging.AttrError, err)
			emit(onEvent, SSEEventWarning, WarningPayload{
				Message: fmt.Sprintf("post %q persist failed: %v", post.Title, err),
			})
			return
		}
		persistedPositions[position] = true
		emit(onEvent, SSEEventPost, PostEventPayload{
			Post:  post,
			Index: globalStartIndex + len(posts),
			ID:    id,
		})
		posts = append(posts, post)
	}

	for result, err := range genkit.GenerateStream(ctx, g,
		ai.WithModelName(modelName),
		ai.WithSystem(systemPrompt),
		ai.WithPrompt(userPrompt),
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
					slog.InfoContext(ctx, "tokens", logging.AttrComponent, "genkit.content_plan", "input", u.InputTokens, "output", u.OutputTokens, "total", u.InputTokens+u.OutputTokens)
				}
				respText := result.Response.Text()
				slog.InfoContext(ctx, "stream finished", logging.AttrComponent, "genkit.content_plan", "finish_reason", result.Response.FinishReason, "posts_persisted", len(posts), "parsed", parsedPosition, "chunks", chunkCount, "bytes", totalBytes, "response_len", len(respText), "response_tail", tailOf(respText, 200))
				recordUsage(ctx, result.Response)
			}
			break
		}

		chunkCount++
		chunkText := result.Chunk.Text()
		totalBytes += len(chunkText)
		for _, raw := range scanner.push(chunkText) {
			position := parsedPosition
			parsedPosition++
			post, ok := parseAndTrimPost(raw)
			if !ok {
				slog.WarnContext(ctx, "malformed post chunk", logging.AttrComponent, "genkit.content_plan", "len", len(raw), "raw_preview", logging.Preview(raw, 100))
				emit(onEvent, SSEEventWarning, WarningPayload{
					Message: fmt.Sprintf("malformed post chunk: %.80s", raw),
				})
				continue
			}
			tryPersist(post, position)
		}
	}

	if streamErr == nil {
		return posts, nil
	}

	// Stream failed (connection dropped mid-generation). Re-issue as a
	// blocking call to recover the rest of the batch; deduplicate against
	// what the streaming path already persisted so we never double-insert.
	slog.WarnContext(ctx, "stream error, falling back to blocking Generate", logging.AttrComponent, "genkit.content_plan", "persisted", len(posts), "parsed", parsedPosition, logging.AttrError, streamErr)
	resp, err := genkit.Generate(ctx, g,
		ai.WithModelName(modelName),
		ai.WithSystem(systemPrompt),
		ai.WithPrompt(userPrompt),
		modelCfg,
	)
	if err != nil {
		// Hand back whatever was already persisted so the caller can
		// surface partial success rather than discarding the run.
		return posts, &AIError{Msg: fmt.Sprintf("model call failed (stream+fallback): %v", err)}
	}

	if resp.Usage != nil {
		slog.InfoContext(ctx, "tokens (fallback)", logging.AttrComponent, "genkit.content_plan", "input", resp.Usage.InputTokens, "output", resp.Usage.OutputTokens, "total", resp.Usage.InputTokens+resp.Usage.OutputTokens)
	}
	recordUsage(ctx, resp)

	text := strings.TrimSpace(resp.Text())
	if strings.HasPrefix(text, "```") {
		if _, after, found := strings.Cut(text, "\n"); found {
			text = after
		}
		text = strings.TrimSuffix(strings.TrimSpace(text), "```")
		text = strings.TrimSpace(text)
	}

	var fallbackPosts []DraftPost
	if err := json.Unmarshal([]byte(text), &fallbackPosts); err != nil {
		return posts, &AIError{Msg: fmt.Sprintf("model response not valid JSON: %v\nraw: %.200s", err, text)}
	}

	for i, post := range fallbackPosts {
		if persistedPositions[i] {
			// Already persisted via the streaming path — first-write
			// wins, even if the blocking response produces a slightly
			// different text for this slot (e.g. non-zero temperature).
			continue
		}
		post.Body = trimBody(post.Body)
		tryPersist(post, i)
	}

	return posts, nil
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
// generated row ID. Per CON-66 the streaming path calls this for each
// parsed-and-validated post immediately rather than aggregating to a final
// CreateBatch — a client disconnect mid-stream now leaves whatever was
// already persisted in the database, and a hard *AIError from one batch
// no longer rolls back the surviving batches' rows.
//
// CON-188: the model's bullet-point thesis (dp.Body) is no longer written into
// the post body. The post is created with an empty body and the thesis is
// stored as a draft_thesis note, so the assistant can later expand it into copy.
func persistOne(ctx context.Context, dp *DraftPost, campaign *models.Campaign, windowStart, windowEnd *time.Time, postRepo repository.PostRepository, noteRepo repository.PostNoteRepository, usedAssetIDs []string) (string, error) {
	id, err := models.NewID()
	if err != nil {
		return "", err
	}

	// CON-181: compose scheduled_at from the campaign's scheduling settings —
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
		BrandVoiceID:        dp.BrandVoiceID, // CON-245: provenance of the voice it was written in
		CreatedBy:           campaign.CreatedBy,
	}
	if err := postRepo.Create(ctx, row); err != nil {
		return "", err
	}

	// Capture the thesis as a draft_thesis note (CON-188). Best-effort: a
	// note-write failure must not discard the already-persisted post (CON-66),
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

func renderTemplate(tmpl *template.Template, data any) (string, error) {
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}
