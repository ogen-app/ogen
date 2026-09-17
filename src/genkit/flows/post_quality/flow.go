package post_quality

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/firebase/genkit/go/core"
	"github.com/firebase/genkit/go/genkit"

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
const defaultSuggestionCap = 3

// PostQualityFlowConfig holds the settings for the assessPostQuality flow.
type PostQualityFlowConfig struct {
	// Provider resolves the model reference + call config by role; post_quality
	// uses the quality role (cfg.QualityModelID) (CON-86 FR12).
	Provider *llm.Provider
	// Recorder captures usage events; nil disables recording (CON-86 FR5/FR10).
	Recorder *usage.Recorder
	// Checker gates the flow against the tenant's spend caps; nil = no gate.
	Checker *usage.Checker
	// ModelID is the Anthropic model used for scoring — Sonnet 4.5 by
	// default, specified separately from the generation flows.
	ModelID string
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
	// post owner (CON-285). nil is a no-op.
	Notifier *notify.Service

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
	// Versions resolves the latest committed content snapshot to assess
	// (CON-184). Nil disables version resolution — the flow then scores the
	// live posts.content, preserving the pre-CON-184 behaviour.
	Versions repository.PostVersionRepository
}

// postQualityFlow is the registered flow; nil until InitPostQuality runs.
var postQualityFlow *core.Flow[PostQualityRequest, *PostQualityResponse, struct{}]

// postQualityRunner is a closure over (g, cfg, repos) that threads an
// OnEventFunc through for SSE streaming. Set by InitPostQuality.
var postQualityRunner func(ctx context.Context, req PostQualityRequest, onEvent OnEventFunc) (*PostQualityResponse, error)

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

	postQualityFlow = genkit.DefineFlow(g, "assessPostQuality",
		func(ctx context.Context, req PostQualityRequest) (*PostQualityResponse, error) {
			return runPostQuality(ctx, g, req, cfg, repos, nil)
		},
	)
	postQualityRunner = func(ctx context.Context, req PostQualityRequest, onEvent OnEventFunc) (*PostQualityResponse, error) {
		return runPostQuality(ctx, g, req, cfg, repos, onEvent)
	}
	return nil
}

// NewPostQualityCallback returns a callback for the posts handler. onEvent
// is forwarded for SSE streaming; pass nil for a silent call.
func NewPostQualityCallback() func(ctx context.Context, postID string, onEvent OnEventFunc) (*PostQualityResponse, error) {
	return func(ctx context.Context, postID string, onEvent OnEventFunc) (*PostQualityResponse, error) {
		return postQualityRunner(ctx, PostQualityRequest{PostID: postID}, onEvent)
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
		if ownerID == "" {
			return
		}
		publishAssessmentFinalised(cfg.Hub, req.PostID, ownerID, out, retErr)
		// CON-285: a durable assessment finished/failed row for the initiator.
		notifyAssessmentFinalised(cfg.Notifier, tenantID, ownerID, req.PostID, out, retErr)
	}()

	// ── Step 1: validateInput ────────────────────────────────────────────
	post, err := repos.Posts.GetByID(ctx, req.PostID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, &ValidationError{Msg: "post not found"}
		}
		return nil, fmt.Errorf("load post: %w", err)
	}

	// Assess the latest committed version rather than the live editor HEAD
	// (CON-184). Runs before validateInput so the body precondition is
	// checked against the content that is actually scored.
	if err := resolveAssessedContent(ctx, repos, post); err != nil {
		return nil, err
	}

	if err := validateInput(post); err != nil {
		return nil, err
	}
	ownerID = post.CreatedBy
	tenantID = post.TenantID
	emit(onEvent, SSEEventStep, StepEventPayload{Step: "validateInput", Status: "done"})

	// ── Step 2: buildContext ─────────────────────────────────────────────
	prompts, err := buildContext(ctx, post, cfg, repos)
	if err != nil {
		return nil, fmt.Errorf("build context: %w", err)
	}
	emit(onEvent, SSEEventStep, StepEventPayload{Step: "buildContext", Status: "done"})

	// ── Change detection (CON-92): the rendered prompt encodes everything
	// the model sees (post body, platform, type, campaign brief, phase,
	// asset previews); the model id and the resolved weight profile cover
	// the rest of what determines the score. If all of them are unchanged
	// since the stored evaluation, skip the model run and return the cached
	// result — so we re-assess only when something that actually affects the
	// score changed.
	hash := inputHash(prompts, cfg.ModelID, cfg.Weights.For(post.PlatformPostType))
	if cached, cerr := repos.Evaluations.GetByPostID(ctx, post.ID); cerr == nil && cached != nil && cached.InputHash == hash {
		slog.InfoContext(ctx, "inputs unchanged, returning cached evaluation", logging.AttrComponent, "genkit.post_quality", "post_id", req.PostID)
		resp := &PostQualityResponse{
			PostID:      post.ID,
			GeneratedAt: time.Now().UTC(),
			Evaluation:  cached,
			Cached:      true,
		}
		emit(onEvent, SSEEventComplete, resp)
		return resp, nil
	}

	// Enforcement gate (CON-86 FR9): placed AFTER the cache short-circuit so a
	// cached assessment (no provider call) is never blocked. Nil checker = no gate.
	if err := cfg.Checker.Enforce(ctx); err != nil {
		return nil, err
	}

	// ── Step 3: evaluate (single model call, 1-retry/2s-backoff) ─────────
	output, err := evaluate(ctx, g, cfg, prompts)
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
	emit(onEvent, SSEEventStep, StepEventPayload{Step: "composeScore", Status: "done"})

	// ── Step 6: persist (upsert evaluation + PostLog) ────────────────────
	eval, err := persist(ctx, repos, post, result, overall, prompts.captionScoped, cfg.ModelID, hash)
	if err != nil {
		return nil, fmt.Errorf("persist evaluation: %w", err)
	}
	emit(onEvent, SSEEventStep, StepEventPayload{Step: "persist", Status: "done"})

	slog.InfoContext(ctx, "done", logging.AttrComponent, "genkit.post_quality", "post_id", req.PostID, "duration_ms", time.Since(start).Milliseconds(), "overall_pct", overall, "type", post.PlatformPostType)

	resp := &PostQualityResponse{
		PostID:      post.ID,
		GeneratedAt: time.Now().UTC(),
		Evaluation:  eval,
	}
	emit(onEvent, SSEEventComplete, resp)
	return resp, nil
}

// resolveAssessedContent overrides post.Content with the latest committed
// version's content when one exists (CON-184).
//
// posts.content is the live working copy: the editor autosaves keystrokes
// into it without snapshotting, so it is treated as uncommitted
// work-in-progress. A post's saved versions are the states the author
// deliberately checkpointed, and the newest one is what the quality score
// should reflect. Only Content is touched — platform, type, media, and
// campaign context all come from the live post.
//
// It is a no-op when version resolution is disabled (repos.Versions == nil,
// preserving pre-CON-184 behaviour) or when the post has no saved version yet
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
	return nil
}

// inputHash fingerprints everything that determines an assessment's stored
// result: the rendered system and user prompts plus the model id (what the
// model sees), and the resolved weight profile (which ComposeScore folds
// into OverallPct and each dimension's Weight/Contribution — values the
// model never produces). The assess flow compares it against the stored
// hash to decide whether to re-run (CON-92); including the profile means a
// weights config change invalidates the cache rather than serving a stale
// score. A unit-separator between parts prevents one field's content from
// bleeding into the next.
func inputHash(prompts *renderedPrompts, modelID string, profile Profile) string {
	h := sha256.New()
	h.Write([]byte(prompts.system))
	h.Write([]byte{0x1f})
	h.Write([]byte(prompts.user))
	h.Write([]byte{0x1f})
	h.Write([]byte(modelID))
	h.Write([]byte{0x1f})
	fmt.Fprintf(h, "%g,%g,%g,%g", profile.Correctness, profile.Clarity, profile.Engagement, profile.Delivery)
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
	hash string,
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
		InputHash:        hash,
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
		ev.Payload = map[string]any{"postId": postID, "error": err.Error()}
	} else {
		overall := 0.0
		if resp != nil && resp.Evaluation != nil {
			overall = resp.Evaluation.OverallPct
		}
		ev.Type = "assessment.completed"
		ev.Payload = map[string]any{"postId": postID, "overallPct": overall}
	}
	if pubErr := hub.Publish(context.Background(), ev); pubErr != nil {
		slog.Error("hub publish failed", logging.AttrComponent, "genkit.post_quality", "post_id", postID, logging.AttrError, pubErr)
	}
}

// notifyAssessmentFinalised drops a durable "assessment finished / failed"
// notification to the post owner (CON-285): the initiator, who may have walked
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
