package consistency

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/modelconfig"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

const defaultMaxPosts = 20

func runCheckBrief(
	ctx context.Context,
	g *genkit.Genkit,
	campaignID string,
	cfg ConsistencyFlowConfig,
	repos ConsistencyRepos,
	onEvent OnEventFunc,
) (*BriefReview, error) {
	start := time.Now()
	slog.InfoContext(ctx, "starting brief review", logging.AttrComponent, "genkit.consistency", "campaign_id", campaignID)

	if err := cfg.Checker.Enforce(ctx); err != nil {
		return nil, err
	}
	campaign, err := repos.Campaigns.GetByID(ctx, campaignID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, &ValidationError{Msg: "campaign not found"}
		}
		return nil, fmt.Errorf("load campaign: %w", err)
	}
	emit(onEvent, SSEEventStep, StepEventPayload{Step: "buildContext", Status: "done"})

	systemPrompt, err := cfg.tmpl.renderBriefSystem()
	if err != nil {
		return nil, err
	}
	userPrompt, err := cfg.tmpl.renderBriefUser(campaign)
	if err != nil {
		return nil, err
	}

	maxTokens := cfg.MaxOutputTokens
	if maxTokens == 0 {
		maxTokens = 8192
	}
	// WithSystem/WithPrompt Sprintf their first arg; pass text as a "%s" value
	// so a brief containing "%" verbs is never interpreted (mirrors post_quality).
	mc := modelconfig.Resolve(ctx, modelconfig.FlowConsistency, modelconfig.SlotMain)
	out, resp, err := genkit.GenerateData[briefReviewOutput](ctx, g,
		ai.WithModelName(mc.Ref),
		ai.WithSystem("%s", systemPrompt),
		ai.WithPrompt("%s", userPrompt),
		cfg.Provider.CallConfig(maxTokens),
	)
	if err != nil {
		slog.ErrorContext(ctx, "model call failed", logging.AttrComponent, "genkit.consistency", "campaign_id", campaignID, "duration_ms", time.Since(start).Milliseconds(), logging.AttrError, err)
		return nil, &AIError{Msg: fmt.Sprintf("model call failed: %v", err)}
	}
	cfg.Recorder.RecordResp(ctx, mc.Vendor, mc.Model, "consistency", resp)
	emit(onEvent, SSEEventStep, StepEventPayload{Step: "analyze", Status: "done"})

	findings := make([]Finding, 0, len(out.Findings))
	consistent := true
	for _, f := range out.Findings {
		findings = append(findings, Finding{Aspect: f.Aspect, Severity: f.Severity, Issue: f.Issue, Suggestion: f.Suggestion})
		if severityHigh(f.Severity) {
			consistent = false
		}
	}

	slog.InfoContext(ctx, "brief review done", logging.AttrComponent, "genkit.consistency", "campaign_id", campaignID, "duration_ms", time.Since(start).Milliseconds(), "findings", len(findings), "consistent", consistent)
	return &BriefReview{
		CampaignID:  campaign.ID,
		Consistent:  consistent,
		Findings:    findings,
		Summary:     out.Summary,
		GeneratedAt: time.Now().UTC(),
	}, nil
}

func runCheckPosts(
	ctx context.Context,
	g *genkit.Genkit,
	req PostsCheckRequest,
	cfg ConsistencyFlowConfig,
	repos ConsistencyRepos,
	onEvent OnEventFunc,
) (*PostsReview, error) {
	start := time.Now()
	slog.InfoContext(ctx, "starting posts review", logging.AttrComponent, "genkit.consistency", "campaign_id", req.CampaignID)

	if err := cfg.Checker.Enforce(ctx); err != nil {
		return nil, err
	}
	campaign, err := repos.Campaigns.GetByID(ctx, req.CampaignID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, &ValidationError{Msg: "campaign not found"}
		}
		return nil, fmt.Errorf("load campaign: %w", err)
	}
	all, err := repos.Posts.ListByCampaign(ctx, req.CampaignID)
	if err != nil {
		return nil, fmt.Errorf("list posts: %w", err)
	}

	eligible := eligiblePosts(all)
	total := len(eligible)
	// Effective cap: the configured max when set, else the built-in default.
	// A caller-supplied req.Max may request fewer posts but must never exceed
	// the cap.
	maxPosts := cfg.MaxPosts
	if maxPosts <= 0 {
		maxPosts = defaultMaxPosts
	}
	limit := maxPosts
	if req.Max > 0 && req.Max < limit {
		limit = req.Max
	}
	checked := eligible
	capped := false
	if total > limit {
		checked = eligible[:limit]
		capped = true
	}
	emit(onEvent, SSEEventStep, StepEventPayload{Step: "buildContext", Status: "done"})

	if len(checked) == 0 {
		return &PostsReview{
			CampaignID:  campaign.ID,
			Findings:    []PostFinding{},
			Summary:     "There are no non-published posts to check.",
			GeneratedAt: time.Now().UTC(),
		}, nil
	}

	systemPrompt, err := cfg.tmpl.renderPostsSystem()
	if err != nil {
		return nil, err
	}
	userPrompt, err := cfg.tmpl.renderPostsUser(campaign, checked)
	if err != nil {
		return nil, err
	}

	mc := modelconfig.Resolve(ctx, modelconfig.FlowConsistency, modelconfig.SlotMain)
	out, resp, err := genkit.GenerateData[postsReviewOutput](ctx, g,
		ai.WithModelName(mc.Ref),
		ai.WithSystem("%s", systemPrompt),
		ai.WithPrompt("%s", userPrompt),
		cfg.Provider.CallConfig(maxOutputTokens(cfg)),
	)
	if err != nil {
		slog.ErrorContext(ctx, "model call failed", logging.AttrComponent, "genkit.consistency", "campaign_id", req.CampaignID, "duration_ms", time.Since(start).Milliseconds(), logging.AttrError, err)
		return nil, &AIError{Msg: fmt.Sprintf("model call failed: %v", err)}
	}
	cfg.Recorder.RecordResp(ctx, mc.Vendor, mc.Model, "consistency", resp)
	emit(onEvent, SSEEventStep, StepEventPayload{Step: "analyze", Status: "done"})

	// Keep only findings that reference a checked post (drop hallucinated / dup ids).
	byID := make(map[string]models.Post, len(checked))
	for _, p := range checked {
		byID[p.ID] = p
	}
	seen := make(map[string]bool)
	findings := make([]PostFinding, 0, len(out.Findings))
	for _, f := range out.Findings {
		p, ok := byID[f.PostID]
		if !ok || seen[f.PostID] {
			continue
		}
		seen[f.PostID] = true
		findings = append(findings, PostFinding{
			PostID:     p.ID,
			Title:      p.Title,
			Severity:   f.Severity,
			Issue:      f.Issue,
			Suggestion: f.Suggestion,
		})
	}

	slog.InfoContext(ctx, "posts review done", logging.AttrComponent, "genkit.consistency", "campaign_id", req.CampaignID, "duration_ms", time.Since(start).Milliseconds(), "checked", len(checked), "total", total, "capped", capped, "drift", len(findings))
	return &PostsReview{
		CampaignID:  campaign.ID,
		Checked:     len(checked),
		Total:       total,
		Capped:      capped,
		Aligned:     len(checked) - len(findings),
		Findings:    findings,
		Summary:     out.Summary,
		GeneratedAt: time.Now().UTC(),
	}, nil
}

func maxOutputTokens(cfg ConsistencyFlowConfig) int64 {
	if cfg.MaxOutputTokens == 0 {
		return 8192
	}
	return cfg.MaxOutputTokens
}

// eligiblePosts returns the non-published posts with non-empty content — the
// only ones a posts-vs-brief review considers (CON-115/116 eligibility).
func eligiblePosts(posts []models.Post) []models.Post {
	var out []models.Post
	for _, p := range posts {
		switch p.Status {
		case models.PostStatusPublished, models.PostStatusNotPublished:
			continue
		}
		if strings.TrimSpace(p.Content) == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}
