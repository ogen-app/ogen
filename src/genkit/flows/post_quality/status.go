package post_quality

import (
	"context"
	"errors"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// readStatus returns the post's stored assessment and whether it is stale,
// or (nil, nil) when the post was never assessed. It renders the prompts the
// next run would use and compares their fingerprints with the stored ones;
// no model is called. A post that can no longer be assessed (its body was
// emptied, say) reads as content-stale.
func readStatus(ctx context.Context, cfg PostQualityFlowConfig, repos PostQualityRepos, postID string) (*Status, error) {
	eval, err := repos.Evaluations.GetByPostID(ctx, postID)
	if err != nil || eval == nil {
		return nil, err
	}
	st := &Status{PostEvaluation: eval, RunsLimit: runsLimit(ctx, cfg)}

	post, err := loadAssessedPost(ctx, repos, postID)
	if _, invalid := errors.AsType[*ValidationError](err); invalid {
		st.Stale, st.StaleReason = true, models.QualityStaleContent
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	in, err := assessmentInputs(ctx, cfg, repos, post)
	if err != nil {
		return nil, err
	}
	st.StaleReason = eval.StaleReason(in.contentHash, in.contextHash)
	st.Stale = st.StaleReason != ""
	return st, nil
}

// runsLimit is the tenant's quality_reviews_per_post cap, nil when unlimited.
func runsLimit(ctx context.Context, cfg PostQualityFlowConfig) *int {
	tenantID, ok := tenantctx.From(ctx)
	if !ok {
		return nil
	}
	limit := cfg.Limiter.NumericLimit(ctx, tenantID, QualityRunsFeature)
	if limit == nil {
		return nil
	}
	return new(int(*limit))
}
