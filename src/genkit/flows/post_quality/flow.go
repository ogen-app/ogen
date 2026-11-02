package post_quality

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/entitlements"
	"github.com/ogen-app/ogen/src/domain/modelconfig"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/kernel/usage"
	"github.com/ogen-app/ogen/src/usecase/notify"
	"github.com/ogen-app/ogen/src/usecase/post_actions/logs"
)

// defaultSuggestionCap is the top-N suggestions per dimension when the
// config leaves SuggestionCap at zero.
const defaultSuggestionCap = 2

// postSuggestionCap is the most suggestions one assessment keeps across all
// four dimensions, so the author reads a short list of the fixes that matter.
const postSuggestionCap = 5

// PostQualityFlowConfig holds the settings for the assessPostQuality flow.
type PostQualityFlowConfig struct {
	// Provider builds the model call config; the model itself comes from the
	// modelconfig resolver (FlowPostQuality, SlotMain).
	Provider *llm.Provider
	// Recorder captures usage events; nil disables recording.
	Recorder *usage.Recorder
	// Checker gates the flow against the tenant's spend caps; nil = no gate.
	Checker *usage.Checker
	// MaxOutputTokens caps the model response; 0 falls back to
	// defaultMaxOutputTokens. Keep it under the Anthropic non-streaming
	// limit — evaluate issues a blocking GenerateData call.
	MaxOutputTokens int64
	// SuggestionCap is the top-N suggestions per dimension; 0 falls back to
	// defaultSuggestionCap.
	SuggestionCap int
	// Weights selects the per-PlatformPostType weight profile used to
	// compose the overall score.
	Weights Weights
	// Hub publishes the "assessment finalised" event on success/failure.
	// nil = silent.
	Hub eventhub.Hub
	// Notifier drops a durable "assessment finished / failed" notification to the
	// post owner after a manual run. nil is a no-op.
	Notifier *notify.Service
	// Limiter enforces the quality_reviews_per_post entitlement against the
	// stored run count. nil = no cap.
	Limiter *entitlements.Limiter

	tmpl *templates
}

// PostQualityRepos bundles the repository dependencies for the flow.
type PostQualityRepos struct {
	Posts       repository.PostRepository
	Campaigns   repository.CampaignRepository
	Assets      repository.AssetRepository
	Chunks      repository.AssetChunksRepository
	Platforms   repository.PlatformRepository
	Evaluations repository.PostEvaluationRepository
	PostLogs    repository.PostLogRepository
	// Versions resolves the latest committed content snapshot to assess.
	// Nil disables version resolution — the flow then scores the
	// live posts.content.
	Versions repository.PostVersionRepository
}

// postQualityRunner is a closure over (g, cfg, repos) that threads an
// OnEventFunc through for SSE streaming. Set by InitPostQuality.
var postQualityRunner func(ctx context.Context, req PostQualityRequest, onEvent OnEventFunc) (*PostQualityResponse, error)

// postQualityStatus is a closure over (cfg, repos) that reads a post's stored
// assessment and whether it is stale. Set by InitPostQuality.
var postQualityStatus func(ctx context.Context, postID string) (*Status, error)

// InitPostQuality registers the assessPostQuality Genkit flow. It must be
// called after the Genkit instance is initialised with the Anthropic
// plugin.
func InitPostQuality(g *genkit.Genkit, cfg PostQualityFlowConfig, repos PostQualityRepos) error {
	tmpl, err := loadTemplates()
	if err != nil {
		return err
	}
	cfg.tmpl = tmpl
	if cfg.SuggestionCap <= 0 {
		cfg.SuggestionCap = defaultSuggestionCap
	}

	genkit.DefineFlow(g, "assessPostQuality",
		func(ctx context.Context, req PostQualityRequest) (*PostQualityResponse, error) {
			return runPostQuality(ctx, g, req, cfg, repos, nil)
		},
	)
	postQualityRunner = func(ctx context.Context, req PostQualityRequest, onEvent OnEventFunc) (*PostQualityResponse, error) {
		return runPostQuality(ctx, g, req, cfg, repos, onEvent)
	}
	postQualityStatus = func(ctx context.Context, postID string) (*Status, error) {
		return readStatus(ctx, cfg, repos, postID)
	}
	return nil
}

// NewPostQualityCallback returns a callback for the posts handler: a manual
// assessment. onEvent is forwarded for SSE streaming; pass nil for a silent
// call.
func NewPostQualityCallback() func(ctx context.Context, postID string, onEvent OnEventFunc) (*PostQualityResponse, error) {
	return func(ctx context.Context, postID string, onEvent OnEventFunc) (*PostQualityResponse, error) {
		return postQualityRunner(ctx, PostQualityRequest{PostID: postID, Trigger: models.QualityTriggerManual}, onEvent)
	}
}

// NewAutoAssessCallback returns the entry point for automatic assessments
// started by a lifecycle checkpoint. It runs silently (no SSE).
func NewAutoAssessCallback() func(ctx context.Context, postID string, trigger models.QualityTrigger) (*PostQualityResponse, error) {
	return func(ctx context.Context, postID string, trigger models.QualityTrigger) (*PostQualityResponse, error) {
		return postQualityRunner(ctx, PostQualityRequest{PostID: postID, Trigger: trigger}, nil)
	}
}

// NewStatusCallback returns the read side for the posts handler: the stored
// assessment plus whether it is stale.
func NewStatusCallback() func(ctx context.Context, postID string) (*Status, error) {
	return func(ctx context.Context, postID string) (*Status, error) {
		return postQualityStatus(ctx, postID)
	}
}

// emit calls onEvent when non-nil; a safe no-op otherwise.
func emit(onEvent OnEventFunc, name SSEEventKind, data any) {
	if onEvent != nil {
		onEvent(name, data)
	}
}

// runPostQuality executes the six steps of the flow.
func runPostQuality(
	ctx context.Context,
	g *genkit.Genkit,
	req PostQualityRequest,
	cfg PostQualityFlowConfig,
	repos PostQualityRepos,
	onEvent OnEventFunc,
) (out *PostQualityResponse, retErr error) {
	start := time.Now()
	slog.InfoContext(ctx, "starting", logging.AttrComponent, "genkit.post_quality", "post_id", req.PostID)

	// Captured once the post is loaded, so the finalisation event + durable
	// notification are scoped to the post owner and tenant. Empty before
	// validateInput → very-early failures emit no finalisation.
	var ownerID, tenantID string
	defer func() {
		if ownerID == "" || quietFinish(req.Trigger, out, retErr) {
			return
		}
		publishAssessmentFinalised(cfg.Hub, req.PostID, ownerID, req.Trigger, out, retErr)
		// A durable assessment finished/failed row for the author who asked;
		// automatic runs only update the open page.
		if !req.Trigger.IsAuto() {
			notifyAssessmentFinalised(cfg.Notifier, tenantID, ownerID, req.PostID, out, retErr)
		}
	}()

	// ── Step 1: validateInput ────────────────────────────────────────────
	post, err := loadAssessedPost(ctx, repos, req.PostID)
	if err != nil {
		return nil, err
	}
	if !triggerStillApplies(req.Trigger, post.Status) {
		return nil, &SkippedError{Reason: "post is no longer " + string(models.PostStatusReadyForPublish)}
	}
	ownerID = post.CreatedBy
	tenantID = post.TenantID
	emit(onEvent, SSEEventStep, StepEventPayload{Step: "validateInput", Status: "done"})

	// ── Step 2: buildContext ─────────────────────────────────────────────
	in, err := assessmentInputs(ctx, cfg, repos, post)
	if err != nil {
		return nil, fmt.Errorf("build context: %w", err)
	}
	prompts, mc := in.prompts, in.model
	emit(onEvent, SSEEventStep, StepEventPayload{Step: "buildContext", Status: "done"})

	// ── Change detection: skip the model when nothing that affects the
	// score changed since the stored evaluation. Automatic runs also skip
	// when only the context changed (brief, assets, model, rubric): that
	// marks the assessment stale for the author to re-run, rather than
	// re-scoring every post of a campaign on one brief edit.
	cached, err := repos.Evaluations.GetByPostID(ctx, post.ID)
	if err != nil {
		return nil, fmt.Errorf("load evaluation: %w", err)
	}
	if reuse(cached, in, req.Trigger) {
		slog.InfoContext(ctx, "reusing stored evaluation", logging.AttrComponent, "genkit.post_quality", "post_id", req.PostID, "trigger", req.Trigger)
		resp := &PostQualityResponse{
			PostID:      post.ID,
			GeneratedAt: time.Now().UTC(),
			Evaluation:  cached,
			Cached:      true,
		}
		emit(onEvent, SSEEventComplete, resp)
		return resp, nil
	}

	// Enforcement gates: placed AFTER the cache short-circuit so a cached
	// assessment (no provider call) is never blocked. A blocked automatic run
	// is skipped quietly; a blocked manual run reports the cap to the author.
	runs, err := allowRun(ctx, cfg, tenantID, cached, req.Trigger)
	if err != nil {
		return nil, err
	}
	if req.Trigger.IsAuto() {
		publishAssessmentStarted(cfg.Hub, post.ID, ownerID, req.Trigger)
	}

	// ── Step 3: evaluate (single model call, 1-retry/2s-backoff) ─────────
	output, err := evaluate(ctx, g, cfg, mc, prompts)
	if err != nil {
		return nil, err
	}
	emit(onEvent, SSEEventStep, StepEventPayload{Step: "evaluate", Status: "done"})

	// ── Step 4: validateOutput (already enforced inside evaluate's retry) ─
	emit(onEvent, SSEEventStep, StepEventPayload{Step: "validateOutput", Status: "done"})

	// ── Step 5: composeScore (deterministic, backend-owned) ──────────────
	result := toEvaluationResult(output)
	profile := cfg.Weights.For(post.PlatformPostType)
	overall := ComposeScore(&result, profile)
	capPostSuggestions(&result, postSuggestionCap)
	emit(onEvent, SSEEventStep, StepEventPayload{Step: "composeScore", Status: "done"})

	// ── Step 6: persist (upsert evaluation + PostLog) ────────────────────
	eval, err := persist(ctx, repos, post, result, overall, prompts.captionScoped, mc.Ref, in, req.Trigger)
	if err != nil {
		return nil, fmt.Errorf("persist evaluation: %w", err)
	}
	emit(onEvent, SSEEventStep, StepEventPayload{Step: "persist", Status: "done"})
	if !req.Trigger.IsAuto() {
		cfg.Limiter.DispatchCrossing(ctx, tenantID, runs)
	}

	slog.InfoContext(ctx, "done", logging.AttrComponent, "genkit.post_quality", "post_id", req.PostID, "duration_ms", time.Since(start).Milliseconds(), "overall_pct", overall, "type", post.PlatformPostType)

	resp := &PostQualityResponse{
		PostID:      post.ID,
		GeneratedAt: time.Now().UTC(),
		Evaluation:  eval,
	}
	emit(onEvent, SSEEventComplete, resp)
	return resp, nil
}

// QualityRunsFeature is the entitlement capping model runs per post.
const QualityRunsFeature = "quality_reviews_per_post"

// allowRun applies the per-post run allowance and the tenant spend cap before
// a model run. For an automatic run a denial is a *SkippedError.
func allowRun(ctx context.Context, cfg PostQualityFlowConfig, tenantID string, cached *models.PostEvaluation, trigger models.QualityTrigger) (entitlements.Decision, error) {
	var used int64
	if cached != nil {
		used = int64(cached.ModelRuns)
	}
	dec, err := cfg.Limiter.AllowCounted(ctx, tenantID, QualityRunsFeature, used)
	if err == nil {
		err = dec.Err()
	}
	if err == nil {
		err = cfg.Checker.Enforce(ctx)
	}
	if err != nil && trigger.IsAuto() {
		return dec, &SkippedError{Reason: err.Error()}
	}
	return dec, err
}

// quietFinish reports whether a run ends without a finalisation event: an
// automatic run that was skipped or reused the stored assessment changed
// nothing the author could see.
func quietFinish(trigger models.QualityTrigger, out *PostQualityResponse, err error) bool {
	if !trigger.IsAuto() {
		return false
	}
	if _, skipped := errors.AsType[*SkippedError](err); skipped {
		return true
	}
	return out != nil && out.Cached
}

// resolveAssessedContent overrides post.Content with the latest committed
// version's content when one exists.
//
// posts.content is the live working copy: the editor autosaves keystrokes
// into it without snapshotting, so it is treated as uncommitted
// work-in-progress. A post's saved versions are the states the author
// deliberately checkpointed, and the newest one is what the quality score
// should reflect. Only Content is touched — platform, type, media, and
// campaign context all come from the live post.
//
// It is a no-op when version resolution is disabled (repos.Versions == nil)
// or when the post has no saved version yet
// (never opened in the assistant, never manually snapshotted), so a fresh
// post is still assessable against its posts.content.
func resolveAssessedContent(ctx context.Context, repos PostQualityRepos, post *models.Post) error {
	if repos.Versions == nil {
		return nil
	}
	latest, err := repos.Versions.GetLatestByPostID(ctx, post.ID)
	if err != nil {
		return fmt.Errorf("load latest version: %w", err)
	}
	if latest != nil {
		post.Content = latest.Content
	}
	// A link post publishes with its URL in the message; versions saved by
	// hand hold the bare text, so the URL is added the way it goes out.
	post.Content = post.AppendLink(post.Content)
	return nil
}

// loadAssessedPost loads the post with its content resolved to the version
// being scored, and checks it can be assessed.
func loadAssessedPost(ctx context.Context, repos PostQualityRepos, postID string) (*models.Post, error) {
	post, err := repos.Posts.GetByID(ctx, postID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, &ValidationError{Msg: "post not found"}
		}
		return nil, fmt.Errorf("load post: %w", err)
	}
	// Runs before validateInput so the body precondition is checked against
	// the content that is actually scored.
	if err := resolveAssessedContent(ctx, repos, post); err != nil {
		return nil, err
	}
	if err := validateInput(post); err != nil {
		return nil, err
	}
	return post, nil
}

// inputs is everything one assessment of a post depends on: the rendered
// prompts, the resolved model and weight profile, and their fingerprints.
type inputs struct {
	prompts     *renderedPrompts
	model       modelconfig.Resolved
	profile     Profile
	contentHash string
	contextHash string
}

// inputHash is the combined fingerprint the cache compares.
func (in *inputs) inputHash() string {
	return hashParts(in.contentHash, in.contextHash)
}

// assessmentInputs renders the prompts for post and fingerprints them. It is
// DB reads only, so the read side can call it to report staleness.
func assessmentInputs(ctx context.Context, cfg PostQualityFlowConfig, repos PostQualityRepos, post *models.Post) (*inputs, error) {
	prompts, err := buildContext(ctx, post, cfg, repos)
	if err != nil {
		return nil, err
	}
	// Resolved once per run so the cache key, the scoring calls and the
	// persisted model id always agree.
	mc := modelconfig.Resolve(ctx, modelconfig.FlowPostQuality, modelconfig.SlotMain)
	profile := cfg.Weights.For(post.PlatformPostType)
	return &inputs{
		prompts:     prompts,
		model:       mc,
		profile:     profile,
		contentHash: contentHash(post, prompts.captionScoped),
		contextHash: contextHash(prompts, mc.Ref, profile),
	}, nil
}

// reuse reports whether the stored evaluation can be returned instead of
// running the model. Identical inputs always reuse. An automatic run also
// reuses when only the context changed, and the published checkpoint runs
// only for a post that was never assessed. A row stored before content
// hashes existed has an empty one: it is unknown whether the content changed,
// so an automatic run leaves it for the author to re-run rather than spend
// a run on what may be the same text.
func reuse(cached *models.PostEvaluation, in *inputs, trigger models.QualityTrigger) bool {
	switch {
	case cached == nil:
		return false
	case cached.InputHash == in.inputHash():
		return true
	case !trigger.IsAuto():
		return false
	case trigger == models.QualityTriggerPublished, cached.ContentHash == "":
		return true
	}
	return cached.ContentHash == in.contentHash
}

// triggerStillApplies reports whether a queued checkpoint still holds when
// it runs: a post marked ready and moved back to draft before its delay
// passed is no longer assessed for being ready.
func triggerStillApplies(trigger models.QualityTrigger, status models.PostStatus) bool {
	return trigger != models.QualityTriggerReady || status == models.PostStatusReadyForPublish
}

// contentHash fingerprints the scored post itself: title, body, platform,
// post type and whether it carries media the model cannot see.
func contentHash(post *models.Post, captionScoped bool) string {
	return hashParts(post.Title, post.Content, post.PlatformID, post.PlatformPostType, strconv.FormatBool(captionScoped))
}

// contextHash fingerprints everything else that determines the stored
// result: the rubric, the campaign/phase/asset context, the model id, and
// the resolved weight profile (which ComposeScore folds into OverallPct and
// each dimension's Weight/Contribution, values the model never produces),
// so a weights config change marks the assessment stale too.
func contextHash(prompts *renderedPrompts, modelID string, profile Profile) string {
	return hashParts(prompts.system, prompts.userContext, modelID,
		fmt.Sprintf("%g,%g,%g,%g", profile.Correctness, profile.Clarity, profile.Engagement, profile.Delivery))
}

// hashParts is sha256 over parts, each prefixed with its length, so one
// part's content cannot bleed into the next whatever bytes it holds.
func hashParts(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write(strconv.AppendInt(nil, int64(len(p)), 10))
		h.Write([]byte{0x1f})
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// persist upserts the evaluation (overwriting any prior run for the post)
// and records the operation in PostLog. The PostLog append is best-effort:
// a logging failure does not fail the evaluation.
func persist(
	ctx context.Context,
	repos PostQualityRepos,
	post *models.Post,
	result models.EvaluationResult,
	overall float64,
	captionScoped bool,
	modelID string,
	in *inputs,
	trigger models.QualityTrigger,
) (*models.PostEvaluation, error) {
	id, err := models.NewID()
	if err != nil {
		return nil, fmt.Errorf("mint evaluation id: %w", err)
	}
	eval := &models.PostEvaluation{
		ID:               id,
		PostID:           post.ID,
		PlatformID:       post.PlatformID,
		PlatformPostType: post.PlatformPostType,
		CaptionScoped:    captionScoped,
		OverallPct:       overall,
		Result:           result,
		ModelID:          modelID,
		InputHash:        in.inputHash(),
		ContentHash:      in.contentHash,
		ContextHash:      in.contextHash,
		Trigger:          triggerOrManual(trigger),
	}
	if err := repos.Evaluations.Upsert(ctx, eval); err != nil {
		return nil, err
	}
	appendQualityLog(ctx, repos, post.ID, eval)
	return eval, nil
}

// appendQualityLog records the assessment in the Post's audit history.
// Best-effort — failures are logged, not propagated.
func appendQualityLog(ctx context.Context, repos PostQualityRepos, postID string, eval *models.PostEvaluation) {
	if repos.PostLogs == nil {
		return
	}
	logID, err := models.NewID()
	if err != nil {
		slog.ErrorContext(ctx, "cannot mint log id", logging.AttrComponent, "genkit.post_quality", "post_id", postID, logging.AttrError, err)
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"overallPct":  eval.OverallPct,
		"modelId":     eval.ModelID,
		"correctness": eval.Result.Correctness.Score,
		"clarity":     eval.Result.Clarity.Score,
		"engagement":  eval.Result.Engagement.Score,
		"delivery":    eval.Result.Delivery.Score,
		"trigger":     eval.Trigger,
	})
	if err := repos.PostLogs.Append(ctx, &models.PostLog{
		ID:        logID,
		PostID:    postID,
		EventType: models.PostLogEventQualityAssessed,
		Actor:     models.ActorSystem,
		Summary:   fmt.Sprintf("quality assessed: %.0f%%", eval.OverallPct),
		Payload:   logs.SanitizeAndCap(string(payload)),
	}); err != nil {
		slog.ErrorContext(ctx, "logs append failed", logging.AttrComponent, "genkit.post_quality", "post_id", postID, logging.AttrError, err)
	}
}

// publishAssessmentFinalised announces the end of an assessment run on the
// shared event hub. Topic is "entity:post:<id>"; type is
// "assessment.completed" on success, "assessment.failed" on error (dotted
// convention, CON-285).
func publishAssessmentFinalised(
	hub eventhub.Hub,
	postID, ownerID string,
	trigger models.QualityTrigger,
	resp *PostQualityResponse,
	err error,
) {
	if hub == nil {
		return
	}
	id, idErr := models.NewID()
	if idErr != nil {
		slog.Error("cannot mint event id", logging.AttrComponent, "genkit.post_quality", logging.AttrError, idErr)
		return
	}
	ev := eventhub.Event{
		ID:     id,
		Topic:  "entity:post:" + postID,
		UserID: ownerID,
	}
	if err != nil {
		ev.Type = "assessment.failed"
		ev.Payload = map[string]any{"postId": postID, "error": err.Error(), "trigger": triggerOrManual(trigger)}
	} else {
		overall := 0.0
		if resp != nil && resp.Evaluation != nil {
			overall = resp.Evaluation.OverallPct
		}
		ev.Type = "assessment.completed"
		ev.Payload = map[string]any{"postId": postID, "overallPct": overall, "trigger": triggerOrManual(trigger)}
	}
	if pubErr := hub.Publish(context.Background(), ev); pubErr != nil {
		slog.Error("hub publish failed", logging.AttrComponent, "genkit.post_quality", "post_id", postID, logging.AttrError, pubErr)
	}
}

// publishAssessmentStarted tells the open post page that an automatic
// assessment began, so it can show progress until assessment.completed.
func publishAssessmentStarted(hub eventhub.Hub, postID, ownerID string, trigger models.QualityTrigger) {
	if hub == nil {
		return
	}
	id, err := models.NewID()
	if err != nil {
		slog.Error("cannot mint event id", logging.AttrComponent, "genkit.post_quality", logging.AttrError, err)
		return
	}
	ev := eventhub.Event{
		ID:      id,
		Topic:   "entity:post:" + postID,
		UserID:  ownerID,
		Type:    "assessment.started",
		Payload: map[string]any{"postId": postID, "trigger": triggerOrManual(trigger)},
	}
	if err := hub.Publish(context.Background(), ev); err != nil {
		slog.Error("hub publish failed", logging.AttrComponent, "genkit.post_quality", "post_id", postID, logging.AttrError, err)
	}
}

func triggerOrManual(t models.QualityTrigger) models.QualityTrigger {
	return cmp.Or(t, models.QualityTriggerManual)
}

// notifyAssessmentFinalised drops a durable "assessment finished / failed"
// notification to the post owner: the initiator, who may have walked
// away while the assessment ran. The client suppresses the live echo for the tab
// that started it; this row is for other devices and a later return. The
// dedupe_key collapses repeats for the same post while still unread. Uses a
// fresh tenant-scoped, bounded context because the request ctx may already be
// cancelled by the time this deferred call runs.
func notifyAssessmentFinalised(n *notify.Service, tenantID, ownerID, postID string, resp *PostQualityResponse, runErr error) {
	if n == nil || ownerID == "" || tenantID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(tenantctx.With(context.Background(), tenantID), 5*time.Second)
	defer cancel()
	spec := notify.Spec{
		EntityType: "post",
		EntityID:   postID,
		ActionURL:  "/posts/" + postID,
	}
	if runErr != nil {
		spec.Level = models.NotificationLevelError
		spec.Type = "assessment.failed"
		spec.Title = "Quality assessment failed"
		spec.Body = "We couldn't assess your post's quality."
		spec.DedupeKey = "assessment.failed:" + postID
	} else {
		overall := 0.0
		if resp != nil && resp.Evaluation != nil {
			overall = resp.Evaluation.OverallPct
		}
		spec.Level = models.NotificationLevelSuccess
		spec.Type = "assessment.completed"
		spec.Title = "Quality assessment ready"
		spec.Body = "Your post's quality assessment is ready."
		spec.Data = map[string]any{"overall_pct": overall}
		spec.DedupeKey = "assessment.completed:" + postID
	}
	_ = n.Emit(ctx, ownerID, spec)
}
