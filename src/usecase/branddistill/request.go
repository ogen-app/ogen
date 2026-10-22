package branddistill

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/urlimport"
)

// Answer is one answered question.
type Answer struct {
	QuestionID string
	Question   string
	Answer     string
}

// RequestInput starts a run: a website, assets and/or answers.
type RequestInput struct {
	TenantID   string
	UserID     *string
	WebsiteURL string
	AssetIDs   []string
	Answers    []Answer
}

// Request validates and queues a run, returned pending. A workspace with an
// open run gets that run with ErrInProgress; a gated tier gets the limiter's
// FeatureNotAvailableError or QuotaExceededError.
func (s *Service) Request(ctx context.Context, in RequestInput) (*models.BrandDistillRun, error) {
	ctx = tenantctx.With(ctx, in.TenantID)
	answers, err := cleanAnswers(in.Answers)
	if err != nil {
		return nil, err
	}
	ids := compactIDs(in.AssetIDs)
	website := strings.TrimSpace(in.WebsiteURL)
	if website == "" && len(ids) == 0 && len(answers) == 0 {
		return nil, &InputError{Code: CodeEmptyRequest, Msg: "give a website, assets or answers"}
	}
	if len(ids) > MaxAssets {
		return nil, &InputError{Code: CodeTooMany, Msg: fmt.Sprintf("at most %d assets", MaxAssets)}
	}
	if !s.Available() {
		return nil, ErrUnavailable
	}
	run := &models.BrandDistillRun{Status: models.BrandDistillStatusPending, Answers: answers, CreatedBy: in.UserID}
	if website != "" {
		if s.deps.Importer == nil || s.deps.Mapper == nil || !s.deps.Importer.Configured(ctx) {
			return nil, ErrScrapingUnavailable
		}
		url, err := urlimport.Normalize(ctx, website)
		if invalid, ok := errors.AsType[*urlimport.InvalidURLError](err); ok {
			return nil, &InputError{Code: CodeInvalidURL, Msg: invalid.Msg}
		}
		if err != nil {
			return nil, err
		}
		run.WebsiteURL = &url
	}
	if run.Sources, err = s.assetSources(ctx, ids); err != nil {
		return nil, err
	}
	if err := s.limiter.RequireGate(ctx, in.TenantID, FeatureGate); err != nil {
		return nil, err
	}
	if _, err := s.limiter.Require(ctx, in.TenantID, FeatureMonthly); err != nil {
		return nil, err
	}
	if err := s.repos.Runs.Create(ctx, s.db, run); err != nil {
		return s.inProgress(ctx, err)
	}
	if err := s.repos.Runs.StampSourceRun(ctx, run.ID, ids); err != nil {
		s.warn(ctx, "stamp the run on its brand sources", run.ID, err)
	}
	return run, s.start(ctx, in.TenantID, run)
}

// start queues a created run's job; a run that cannot be queued is failed.
func (s *Service) start(ctx context.Context, tenantID string, run *models.BrandDistillRun) error {
	if err := s.jobs.EnqueueDistillBrand(ctx, tenantID, run.ID); err != nil {
		s.fail(ctx, run, models.BrandDistillCodeInternal, "The run could not be queued")
		return fmt.Errorf("queue the brand distillation: %w", err)
	}
	runsTotal.Add("started", 1)
	s.publish(ctx, run)
	return nil
}

// inProgress turns a lost race for the open-run slot into ErrInProgress with
// the open run.
func (s *Service) inProgress(ctx context.Context, err error) (*models.BrandDistillRun, error) {
	if !errors.Is(err, repository.ErrBrandDistillOpen) {
		return nil, err
	}
	if open, oerr := s.repos.Runs.Open(ctx); oerr == nil {
		return open, ErrInProgress
	}
	return nil, ErrInProgress
}

// assetSources checks the chosen assets: the tenant's, and not failed. A
// still-ingesting asset is fine; the run waits for it.
func (s *Service) assetSources(ctx context.Context, ids []string) ([]models.DistillSource, error) {
	out := make([]models.DistillSource, 0, len(ids))
	for _, id := range ids {
		a, err := s.repos.Assets.GetByID(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, &InputError{Code: CodeUnknownAsset, Msg: "an asset was not found: " + id}
		}
		if err != nil {
			return nil, err
		}
		if a.Status == models.AssetStatusFailed {
			return nil, &InputError{Code: CodeAssetFailed, Msg: fmt.Sprintf("%q could not be read; upload it again", a.Title)}
		}
		out = append(out, models.DistillSource{
			AssetID: a.ID, Title: a.Title, Type: ptrValue(a.Type), Role: models.DistillRoleAsset,
			Status: sourceStatus(a.Status), URL: ptrValue(a.SourceURL),
		})
	}
	return out, nil
}

// cleanAnswers trims the answers and drops the empty ones.
func cleanAnswers(in []Answer) ([]models.DistillAnswer, error) {
	out := make([]models.DistillAnswer, 0, len(in))
	for _, a := range in {
		a.QuestionID, a.Question, a.Answer = strings.TrimSpace(a.QuestionID), strings.TrimSpace(a.Question), strings.TrimSpace(a.Answer)
		if a.Answer == "" {
			continue
		}
		if a.QuestionID == "" {
			return nil, &InputError{Code: CodeInvalidAnswer, Msg: "every answer needs its questionId"}
		}
		if len(a.Answer) > MaxAnswerBytes || len(a.Question) > MaxQuestionBytes {
			return nil, &InputError{Code: CodeInvalidAnswer, Msg: fmt.Sprintf("an answer is at most %d KB and a question %d characters", MaxAnswerBytes>>10, MaxQuestionBytes)}
		}
		out = append(out, models.DistillAnswer{QuestionID: a.QuestionID, Question: a.Question, Answer: a.Answer})
	}
	if len(out) > MaxAnswers {
		return nil, &InputError{Code: CodeTooMany, Msg: fmt.Sprintf("at most %d answers", MaxAnswers)}
	}
	return out, nil
}

func compactIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// Get returns a run of the tenant; sql.ErrNoRows when it has none by id.
func (s *Service) Get(ctx context.Context, tenantID, runID string) (*models.BrandDistillRun, error) {
	return s.repos.Runs.Get(tenantctx.With(ctx, tenantID), runID)
}

// List returns the tenant's latest runs without their proposals.
func (s *Service) List(ctx context.Context, tenantID string) ([]models.BrandDistillRun, error) {
	return s.repos.Runs.List(tenantctx.With(ctx, tenantID), ListLimit)
}

// Refine answers a ready run's questions: it queues a new run that composes
// again from the same passages and every answer so far, and supersedes the
// old one once it is ready. Refines are not charged.
func (s *Service) Refine(ctx context.Context, tenantID, runID string, userID *string, in []Answer) (*models.BrandDistillRun, error) {
	ctx = tenantctx.With(ctx, tenantID)
	answers, err := cleanAnswers(in)
	if err != nil {
		return nil, err
	}
	if len(answers) == 0 {
		return nil, &InputError{Code: CodeEmptyRequest, Msg: "answer at least one question"}
	}
	if !s.Available() {
		return nil, ErrUnavailable
	}
	var child *models.BrandDistillRun
	err = s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		parent, err := s.repos.Runs.Lock(ctx, tx, runID)
		if err != nil {
			return err
		}
		switch {
		case parent.Status == models.BrandDistillStatusCommitted:
			return ErrCommitted
		case parent.Status != models.BrandDistillStatusReady:
			return ErrNotReady
		case parent.ChildRunID != nil:
			return ErrInProgress
		case parent.Depth >= MaxRefines:
			return ErrRefineLimit
		}
		child = &models.BrandDistillRun{
			ParentRunID: &parent.ID, Depth: parent.Depth + 1, Status: models.BrandDistillStatusPending,
			WebsiteURL: parent.WebsiteURL, Website: parent.Website, Sources: parent.Sources,
			Answers: mergeAnswers(parent.Answers, answers), Candidates: parent.Candidates, CreatedBy: userID,
		}
		if child.Candidates == nil {
			child.Candidates = []models.DistillCandidate{}
		}
		if err := s.repos.Runs.Create(ctx, tx, child); err != nil {
			return err
		}
		// The parent stays ready until the refine is: a refine that fails or is
		// discarded hands it back.
		parent.ChildRunID = &child.ID
		return s.repos.Runs.Save(ctx, tx, parent, "child_run_id")
	})
	if err != nil {
		if errors.Is(err, repository.ErrBrandDistillOpen) {
			return s.inProgress(ctx, err)
		}
		return nil, err
	}
	runsTotal.Add("refined", 1)
	return child, s.start(ctx, tenantID, child)
}

// mergeAnswers keeps every earlier answer and lets a new answer to the same
// question replace it.
func mergeAnswers(prev, next []models.DistillAnswer) []models.DistillAnswer {
	out := make([]models.DistillAnswer, 0, len(prev)+len(next))
	for _, a := range prev {
		if !slices.ContainsFunc(next, func(n models.DistillAnswer) bool { return n.QuestionID == a.QuestionID }) {
			out = append(out, a)
		}
	}
	return append(out, next...)
}

// Discard closes a run that is not committed. An open run's job finds it
// discarded and stops; its charge is released, and a refine's parent gets its
// proposal back.
func (s *Service) Discard(ctx context.Context, tenantID, runID string) (*models.BrandDistillRun, error) {
	ctx = tenantctx.With(ctx, tenantID)
	var (
		run     *models.BrandDistillRun
		wasOpen bool
	)
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var err error
		if run, err = s.repos.Runs.Lock(ctx, tx, runID); err != nil {
			return err
		}
		switch run.Status {
		case models.BrandDistillStatusCommitted:
			return ErrCommitted
		case models.BrandDistillStatusDiscarded:
			return nil
		}
		wasOpen = run.Status.Open()
		run.Status = models.BrandDistillStatusDiscarded
		return s.repos.Runs.Save(ctx, tx, run, "status")
	})
	if err != nil {
		return nil, err
	}
	if wasOpen {
		s.unlinkParent(ctx, run)
	}
	// Only a run that never finished goes back to the allowance; a proposal
	// that was read is spent.
	if run.Charged && wasOpen {
		if err := s.repos.Runs.ReleaseCharge(ctx, run.ID); err != nil {
			s.warn(ctx, "release a discarded run's charge", run.ID, err)
		}
	}
	runsTotal.Add("discarded", 1)
	s.publish(ctx, run)
	return run, nil
}

// Expire marks every tenant's proposals that waited longer than ExpireAfter
// expired and drops their working state.
func (s *Service) Expire(ctx context.Context) (int64, error) {
	n, err := s.repos.Runs.ExpireReadyBefore(tenantctx.WithSystem(ctx), s.now().Add(-ExpireAfter))
	if n > 0 {
		runsTotal.Add("expired", n)
	}
	return n, err
}

func ptrValue(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
