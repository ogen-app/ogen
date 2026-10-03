// Package series implements series — standing instructions for a recurring kind
// of post — and the campaign runs of them. It owns the scope rules: a series
// lives in the workspace library or inside one campaign, only promote changes
// that (one-way), and a campaign can only run library series or its own.
//
// A campaign's series are written one at a time (attach, detach, set rhythm);
// there is deliberately no call that restates the whole set, so a stale form
// cannot put back a run that was just removed or drop one that was just added.
package series

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// Length bounds guard the unbounded TEXT columns.
const (
	MaxNameLen    = 120
	MaxPromiseLen = 280
	MaxRecipeLen  = 4000
)

// Validation errors. The handler maps these to HTTP 400.
var (
	ErrEmptyName          = errors.New("series name is required")
	ErrNameTooLong        = errors.New("series name is too long")
	ErrPromiseTooLong     = errors.New("series promise is too long")
	ErrRecipeTooLong      = errors.New("series recipe is too long")
	ErrInvalidSupply      = errors.New("supply must be self or idea")
	ErrInvalidFormat      = errors.New("invalid content_format")
	ErrInvalidRhythm      = errors.New("rhythm must be null or {times: 1-31, per: week|month}")
	ErrSeriesOutOfScope   = errors.New("series belongs to another campaign")
	ErrPostSeriesNotFound = errors.New("series_id not found")
)

// Lookup errors. The handler maps these to HTTP 404.
var (
	ErrNotFound         = errors.New("series not found")
	ErrCampaignNotFound = errors.New("campaign not found")
	ErrNotAttached      = errors.New("campaign does not run this series")
)

// IsValidation reports whether err is a 400-worthy input error.
func IsValidation(err error) bool {
	for _, target := range []error{
		ErrEmptyName, ErrNameTooLong, ErrPromiseTooLong, ErrRecipeTooLong,
		ErrInvalidSupply, ErrInvalidFormat, ErrInvalidRhythm, ErrSeriesOutOfScope,
		ErrPostSeriesNotFound,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// IsNotFound reports whether err is a 404-worthy lookup miss.
func IsNotFound(err error) bool {
	return errors.Is(err, ErrNotFound) || errors.Is(err, ErrCampaignNotFound) || errors.Is(err, ErrNotAttached)
}

// Service is the series use case over the series repository.
type Service struct {
	repo repository.SeriesRepository
	now  func() time.Time
}

func New(repo repository.SeriesRepository) *Service {
	return &Service{repo: repo, now: time.Now}
}

// stamp is the current time at the microsecond resolution Postgres stores, so
// a value echoed on a write matches what a later read returns.
func (s *Service) stamp() time.Time {
	return s.now().UTC().Truncate(time.Microsecond)
}

// Input is the editable part of a series. Usage is server-owned and never an
// input; CampaignID is read on create only.
type Input struct {
	Name          string
	Promise       string
	Recipe        string
	Supply        models.SeriesSupply
	ContentFormat *models.ContentFormat
	DefaultRhythm *models.SeriesRhythm
	CampaignID    *string
}

// List returns every live series in the workspace, both scopes, with usage.
func (s *Service) List(ctx context.Context) ([]models.Series, error) {
	list, err := s.repo.List(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.withUsage(ctx, list); err != nil {
		return nil, err
	}
	return list, nil
}

// Get returns one live series with usage, or ErrNotFound.
func (s *Service) Get(ctx context.Context, id string) (*models.Series, error) {
	series, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, notFound(err)
	}
	one := []models.Series{*series}
	if err := s.withUsage(ctx, one); err != nil {
		return nil, err
	}
	return &one[0], nil
}

// Create validates and stores a new series. A campaign-local one is attached
// to its campaign at its default rhythm.
func (s *Service) Create(ctx context.Context, in Input) (*models.Series, error) {
	in = normalize(in)
	if err := validate(in); err != nil {
		return nil, err
	}
	if in.CampaignID != nil {
		if err := s.checkCampaign(ctx, *in.CampaignID); err != nil {
			return nil, err
		}
	}
	id, err := models.NewID()
	if err != nil {
		return nil, err
	}
	now := s.stamp()
	series := &models.Series{
		ID:            id,
		Name:          in.Name,
		Promise:       in.Promise,
		Recipe:        in.Recipe,
		Supply:        in.Supply,
		ContentFormat: in.ContentFormat,
		DefaultRhythm: in.DefaultRhythm,
		CampaignID:    in.CampaignID,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := s.repo.Create(ctx, series); err != nil {
		return nil, err
	}
	return series, nil
}

// Update replaces the series' editable fields. The scope is not one of them:
// in.CampaignID is ignored, and only Promote moves a series.
func (s *Service) Update(ctx context.Context, id string, in Input) (*models.Series, error) {
	in = normalize(in)
	if err := validate(in); err != nil {
		return nil, err
	}
	series, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	series.Name = in.Name
	series.Promise = in.Promise
	series.Recipe = in.Recipe
	series.Supply = in.Supply
	series.ContentFormat = in.ContentFormat
	series.DefaultRhythm = in.DefaultRhythm
	series.UpdatedAt = s.stamp()
	if err := s.write(ctx, series, "name", "promise", "recipe", "supply", "content_format", "default_rhythm"); err != nil {
		return nil, err
	}
	return series, nil
}

// Delete soft-deletes a series and removes its campaign runs. Posts keep their
// series_id. Returns the series as it was, or ErrNotFound.
func (s *Service) Delete(ctx context.Context, id string) (*models.Series, error) {
	series, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	ok, err := s.repo.SoftDelete(ctx, id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotFound
	}
	return series, nil
}

// Promote moves a campaign-local series into the workspace library. One-way,
// and a no-op for a series already there. Its existing runs are kept.
func (s *Service) Promote(ctx context.Context, id string) (*models.Series, bool, error) {
	series, err := s.Get(ctx, id)
	if err != nil {
		return nil, false, err
	}
	if series.CampaignID == nil {
		return series, false, nil
	}
	series.CampaignID = nil
	series.UpdatedAt = s.stamp()
	if err := s.write(ctx, series, "campaign_id"); err != nil {
		return nil, false, err
	}
	return series, true, nil
}

// CampaignSeries returns the series one campaign runs.
func (s *Service) CampaignSeries(ctx context.Context, campaignID string) (*models.CampaignSeries, error) {
	if err := s.checkCampaign(ctx, campaignID); err != nil {
		return nil, err
	}
	return s.runs(ctx, campaignID)
}

// Attach adds one series to a campaign at the series' default rhythm.
// Idempotent: a series the campaign already runs keeps its row and rhythm.
func (s *Service) Attach(ctx context.Context, campaignID, seriesID string) (*models.CampaignSeries, error) {
	if err := s.checkCampaign(ctx, campaignID); err != nil {
		return nil, err
	}
	series, err := s.Get(ctx, seriesID)
	if err != nil {
		return nil, err
	}
	if series.CampaignID != nil && *series.CampaignID != campaignID {
		return nil, ErrSeriesOutOfScope
	}
	run := &models.CampaignSeriesRun{
		CampaignID: campaignID,
		SeriesID:   seriesID,
		Rhythm:     series.DefaultRhythm,
		CreatedAt:  s.stamp(),
	}
	if err := s.repo.Attach(ctx, run); err != nil {
		return nil, err
	}
	return s.runs(ctx, campaignID)
}

// Detach removes one series from a campaign. Detaching one it does not run
// answers with the campaign unchanged.
func (s *Service) Detach(ctx context.Context, campaignID, seriesID string) (*models.CampaignSeries, error) {
	if err := s.checkCampaign(ctx, campaignID); err != nil {
		return nil, err
	}
	if err := s.repo.Detach(ctx, campaignID, seriesID); err != nil {
		return nil, err
	}
	return s.runs(ctx, campaignID)
}

// SetRhythm sets how often a campaign runs one of its series. nil is
// occasional. Returns ErrNotAttached when the campaign does not run it.
func (s *Service) SetRhythm(ctx context.Context, campaignID, seriesID string, rhythm *models.SeriesRhythm) (*models.CampaignSeries, error) {
	if rhythm != nil && !rhythm.Valid() {
		return nil, ErrInvalidRhythm
	}
	if err := s.checkCampaign(ctx, campaignID); err != nil {
		return nil, err
	}
	ok, err := s.repo.SetRhythm(ctx, campaignID, seriesID, rhythm)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotAttached
	}
	return s.runs(ctx, campaignID)
}

// CheckPostSeries verifies a post in campaignID may carry seriesID: the series
// must be live in the workspace and either in the library or local to that
// campaign.
func (s *Service) CheckPostSeries(ctx context.Context, seriesID, campaignID string) error {
	series, err := s.repo.GetByID(ctx, seriesID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrPostSeriesNotFound
	}
	if err != nil {
		return err
	}
	if series.CampaignID != nil && *series.CampaignID != campaignID {
		return ErrSeriesOutOfScope
	}
	return nil
}

func (s *Service) runs(ctx context.Context, campaignID string) (*models.CampaignSeries, error) {
	runs, err := s.repo.Runs(ctx, campaignID)
	if err != nil {
		return nil, err
	}
	return &models.CampaignSeries{CampaignID: campaignID, Runs: runs}, nil
}

func (s *Service) withUsage(ctx context.Context, list []models.Series) error {
	if len(list) == 0 {
		return nil
	}
	ids := make([]string, len(list))
	for i := range list {
		ids[i] = list[i].ID
	}
	usage, err := s.repo.Usage(ctx, ids)
	if err != nil {
		return err
	}
	for i := range list {
		list[i].Usage = usage[list[i].ID]
	}
	return nil
}

func (s *Service) write(ctx context.Context, series *models.Series, columns ...string) error {
	ok, err := s.repo.Update(ctx, series, columns...)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	return nil
}

func (s *Service) checkCampaign(ctx context.Context, campaignID string) error {
	ok, err := s.repo.LiveCampaignExists(ctx, campaignID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrCampaignNotFound
	}
	return nil
}

func normalize(in Input) Input {
	in.Name = strings.TrimSpace(in.Name)
	in.Promise = strings.TrimSpace(in.Promise)
	in.Recipe = strings.TrimSpace(in.Recipe)
	return in
}

func validate(in Input) error {
	switch {
	case in.Name == "":
		return ErrEmptyName
	case len([]rune(in.Name)) > MaxNameLen:
		return ErrNameTooLong
	case len([]rune(in.Promise)) > MaxPromiseLen:
		return ErrPromiseTooLong
	case len([]rune(in.Recipe)) > MaxRecipeLen:
		return ErrRecipeTooLong
	case !in.Supply.Valid():
		return ErrInvalidSupply
	case in.ContentFormat != nil && !in.ContentFormat.IsValid():
		return ErrInvalidFormat
	case in.DefaultRhythm != nil && !in.DefaultRhythm.Valid():
		return ErrInvalidRhythm
	}
	return nil
}

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
