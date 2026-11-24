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
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// MaxReads caps the reads of one incremental run.
const MaxReads = MaxAssets + 4

var (
	// ErrNotCollecting means the run takes no more sources: it is not an
	// incremental run, or it is composing or done.
	ErrNotCollecting = errors.New("the run takes no more sources")
	// ErrReadsOpen means a read is still in progress; stop it or wait.
	ErrReadsOpen = errors.New("a source is still being read")
)

// Input error codes of reads.
const (
	CodeAlreadyRead = "already_read"
	CodeNothingRead = "nothing_read"
	CodeOneSource   = "one_source"
)

// ReadInput is one source to read: a website, an asset or answers.
type ReadInput struct {
	TenantID   string
	RunID      string
	UserID     *string
	WebsiteURL string
	AssetID    string
	Answers    []Answer
}

// startIncremental opens an incremental run and starts a read for each source
// given. The run takes more sources until it is composed.
func (s *Service) startIncremental(ctx context.Context, in RequestInput) (*models.BrandDistillRun, error) {
	run := &models.BrandDistillRun{Status: models.BrandDistillStatusCollecting, Incremental: true, CreatedBy: in.UserID}
	if err := s.repos.Runs.Create(ctx, s.db, run); err != nil {
		return s.inProgress(ctx, err)
	}
	runsTotal.Add("started_incremental", 1)
	reads := make([]ReadInput, 0, len(in.AssetIDs)+2)
	if strings.TrimSpace(in.WebsiteURL) != "" {
		reads = append(reads, ReadInput{WebsiteURL: in.WebsiteURL})
	}
	for _, id := range compactIDs(in.AssetIDs) {
		reads = append(reads, ReadInput{AssetID: id})
	}
	if len(in.Answers) > 0 {
		reads = append(reads, ReadInput{Answers: in.Answers})
	}
	for _, r := range reads {
		r.TenantID, r.RunID, r.UserID = in.TenantID, run.ID, in.UserID
		if _, err := s.AddRead(ctx, r); err != nil {
			s.warn(ctx, "start a read of a new run", run.ID, err)
		}
	}
	s.publish(ctx, run)
	return run, nil
}

// AddRead starts reading one more source of an incremental run. A website or
// an asset is read by the job; answers need no reading and are settled at
// once.
func (s *Service) AddRead(ctx context.Context, in ReadInput) (*models.BrandDistillRun, error) {
	ctx = tenantctx.With(ctx, in.TenantID)
	read, err := s.newRead(ctx, in)
	if err != nil {
		return nil, err
	}
	err = s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		run, err := s.repos.Runs.Lock(ctx, tx, in.RunID)
		if err != nil {
			return err
		}
		if !run.Incremental || run.Status != models.BrandDistillStatusCollecting {
			return ErrNotCollecting
		}
		reads, err := s.repos.Runs.ListReads(ctx, tx, run.ID)
		if err != nil {
			return err
		}
		if err := admitRead(read, reads); err != nil {
			return err
		}
		read.ReadOf = &run.ID
		if err := s.repos.Runs.Create(ctx, tx, read); err != nil {
			return err
		}
		// Touching the run keeps an active setup from expiring.
		return s.repos.Runs.Save(ctx, tx, run)
	})
	if err != nil {
		return nil, err
	}
	runsTotal.Add("read_started/"+read.ReadKind(), 1)
	if read.ReadKind() == models.DistillReadAnswers {
		return read, s.settleRead(ctx, read)
	}
	if read.ReadKind() == models.DistillReadAsset {
		if err := s.repos.Runs.StampSourceRun(ctx, in.RunID, []string{in.AssetID}); err != nil {
			s.warn(ctx, "stamp the run on its brand source", read.ID, err)
		}
	}
	return read, s.start(ctx, in.TenantID, read)
}

// newRead checks one source and builds its read, still unsaved.
func (s *Service) newRead(ctx context.Context, in ReadInput) (*models.BrandDistillRun, error) {
	website, asset := strings.TrimSpace(in.WebsiteURL), strings.TrimSpace(in.AssetID)
	answers, err := cleanAnswers(in.Answers)
	if err != nil {
		return nil, err
	}
	given := 0
	for _, ok := range []bool{website != "", asset != "", len(answers) > 0} {
		if ok {
			given++
		}
	}
	switch {
	case given == 0:
		return nil, &InputError{Code: CodeEmptyRequest, Msg: "give a website, an asset or answers"}
	case given > 1:
		return nil, &InputError{Code: CodeOneSource, Msg: "a read is one source: a website, an asset or answers"}
	case !s.Available():
		return nil, ErrUnavailable
	}
	read := &models.BrandDistillRun{Status: models.BrandDistillStatusPending, Answers: answers, CreatedBy: in.UserID}
	switch {
	case website != "":
		if read.WebsiteURL, err = s.websiteURL(ctx, website); err != nil {
			return nil, err
		}
	case asset != "":
		if read.Sources, err = s.assetSources(ctx, []string{asset}); err != nil {
			return nil, err
		}
	}
	return read, nil
}

// admitRead refuses a read the run already has, or one past its limits.
func admitRead(read *models.BrandDistillRun, reads []models.BrandDistillRun) error {
	live := slices.DeleteFunc(slices.Clone(reads), func(r models.BrandDistillRun) bool {
		return r.Status != models.BrandDistillStatusReady && !r.Status.Open()
	})
	if len(live) >= MaxReads {
		return &InputError{Code: CodeTooMany, Msg: fmt.Sprintf("at most %d sources", MaxReads)}
	}
	for _, r := range live {
		switch read.ReadKind() {
		case models.DistillReadWebsite:
			if r.WebsiteURL != nil && *r.WebsiteURL == *read.WebsiteURL {
				return &InputError{Code: CodeAlreadyRead, Msg: "this website is already being read"}
			}
		case models.DistillReadAsset:
			if r.ReadKind() == models.DistillReadAsset && r.Sources[0].AssetID == read.Sources[0].AssetID {
				return &InputError{Code: CodeAlreadyRead, Msg: "this asset is already being read"}
			}
		}
	}
	return nil
}

// settleRead ends a read whose sources were read: it is ready, its coverage
// is what it found, and its run's coverage takes it in.
func (s *Service) settleRead(ctx context.Context, read *models.BrandDistillRun) error {
	read.Coverage = countCoverage([]models.BrandDistillRun{*read})
	read.Status, read.FailureCode, read.FailureReason = models.BrandDistillStatusReady, "", ""
	if read.Candidates == nil {
		read.Candidates = []models.DistillCandidate{}
	}
	if err := s.repos.Runs.SaveOpen(ctx, read, "status", "coverage", "candidates", "failure_code", "failure_reason"); err != nil {
		return err
	}
	runsTotal.Add("read_ready", 1)
	s.publish(ctx, read)
	return s.refreshCoverage(ctx, *read.ReadOf)
}

// refreshCoverage counts the run's finished reads into its coverage and
// publishes the run. The run's row lock orders reads that finish together, so
// the last write counts every one of them.
func (s *Service) refreshCoverage(ctx context.Context, runID string) error {
	var run *models.BrandDistillRun
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var err error
		if run, err = s.repos.Runs.Lock(ctx, tx, runID); err != nil {
			return err
		}
		reads, err := s.repos.Runs.ListReads(ctx, tx, runID)
		if err != nil {
			return err
		}
		run.Coverage = countCoverage(readyReads(reads))
		return s.repos.Runs.Save(ctx, tx, run, "coverage")
	})
	if err != nil {
		return fmt.Errorf("count the run's coverage: %w", err)
	}
	s.publish(ctx, run)
	return nil
}

func readyReads(reads []models.BrandDistillRun) []models.BrandDistillRun {
	return slices.DeleteFunc(reads, func(r models.BrandDistillRun) bool { return r.Status != models.BrandDistillStatusReady })
}

// StopRead stops one read of an incremental run, or takes a finished one out
// of it; its coverage leaves the run's.
func (s *Service) StopRead(ctx context.Context, tenantID, runID, readID string) (*models.BrandDistillRun, error) {
	ctx = tenantctx.With(ctx, tenantID)
	read, err := s.repos.Runs.Get(ctx, readID)
	if err != nil {
		return nil, err
	}
	if read.ReadOf == nil || *read.ReadOf != runID {
		return nil, sql.ErrNoRows
	}
	run, err := s.repos.Runs.Get(ctx, runID)
	if err != nil {
		return nil, err
	}
	if run.Status != models.BrandDistillStatusCollecting {
		return nil, ErrNotCollecting
	}
	if read, err = s.Discard(ctx, tenantID, readID); err != nil {
		return nil, err
	}
	return read, s.refreshCoverage(ctx, runID)
}

// Reads returns an incremental run's reads, oldest first.
func (s *Service) Reads(ctx context.Context, tenantID, runID string) ([]models.BrandDistillRun, error) {
	return s.repos.Runs.ListReads(tenantctx.With(ctx, tenantID), nil, runID)
}

// Compose writes the proposal of an incremental run from its finished reads.
// Every read must be done: one still in progress is ErrReadsOpen.
func (s *Service) Compose(ctx context.Context, tenantID, runID string) (*models.BrandDistillRun, error) {
	ctx = tenantctx.With(ctx, tenantID)
	if !s.Available() {
		return nil, ErrUnavailable
	}
	var run *models.BrandDistillRun
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var err error
		if run, err = s.repos.Runs.Lock(ctx, tx, runID); err != nil {
			return err
		}
		if !run.Incremental || run.Status != models.BrandDistillStatusCollecting {
			return ErrNotCollecting
		}
		reads, err := s.repos.Runs.ListReads(ctx, tx, runID)
		if err != nil {
			return err
		}
		if slices.ContainsFunc(reads, func(r models.BrandDistillRun) bool { return r.Status.Open() }) {
			return ErrReadsOpen
		}
		if !gather(run, readyReads(reads)) {
			return &InputError{Code: CodeNothingRead, Msg: "no source gave anything to read; add a document or answer a few questions"}
		}
		run.Status = models.BrandDistillStatusPending
		return s.repos.Runs.Save(ctx, tx, run, "status", "website", "sources", "answers", "candidates")
	})
	if err != nil {
		return run, err
	}
	return run, s.start(ctx, tenantID, run)
}

// gather puts the finished reads' sources, passages and answers on the run,
// renumbering each read's source refs after the ones before it so every
// passage keeps its own. It reports whether there is anything to compose
// from.
func gather(run *models.BrandDistillRun, reads []models.BrandDistillRun) bool {
	run.Sources, run.Candidates, run.Answers = []models.DistillSource{}, []models.DistillCandidate{}, []models.DistillAnswer{}
	for _, r := range reads {
		offset := len(run.Sources)
		run.Sources = append(run.Sources, r.Sources...)
		for _, c := range r.Candidates {
			var i, j int
			if _, err := fmt.Sscanf(c.Ref, "s%d.%d", &i, &j); err != nil {
				continue
			}
			c.Ref = fmt.Sprintf("s%d.%d", i+offset, j)
			run.Candidates = append(run.Candidates, c)
		}
		run.Answers = mergeAnswers(run.Answers, r.Answers)
		if r.Website != nil && run.Website == nil {
			run.Website = r.Website
		}
	}
	return len(run.Candidates) > 0 || len(run.Answers) > 0
}

// discardReads closes an incremental run's open reads with it.
func (s *Service) discardReads(ctx context.Context, tx bun.Tx, runID string) error {
	reads, err := s.repos.Runs.ListReads(ctx, tx, runID)
	if err != nil {
		return err
	}
	for i := range reads {
		if r := &reads[i]; r.Status.Open() {
			r.Status = models.BrandDistillStatusDiscarded
			if err := s.repos.Runs.Save(ctx, tx, r, "status"); err != nil {
				return err
			}
		}
	}
	return nil
}
