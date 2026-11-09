package assistantundo

import (
	"context"
	"errors"
	"fmt"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// seriesSnapshot is a series' state as an undo log stores it: nil for one
// that doesn't exist, and the runs a removed one had.
type seriesSnapshot struct {
	Series *models.Series `json:"series"`
	Runs   []runJSON      `json:"runs,omitempty"`
}

// runJSON keeps a run's campaign, which its own JSON leaves out.
type runJSON struct {
	CampaignID string               `json:"campaignId"`
	Rhythm     *models.SeriesRhythm `json:"rhythm"`
}

// runSnapshot is a campaign's run of a series; Run nil is a series it
// doesn't run.
type runSnapshot struct {
	CampaignID string                    `json:"campaignId"`
	SeriesID   string                    `json:"seriesId"`
	Run        *models.CampaignSeriesRun `json:"run"`
}

// Series is a series' state for the undo log; nil is one that doesn't
// exist. runs are the campaign runs a removed series had.
func Series(s *models.Series, runs []models.CampaignSeriesRun) models.JSONMap {
	snap := seriesSnapshot{Series: s}
	for _, r := range runs {
		snap.Runs = append(snap.Runs, runJSON{CampaignID: r.CampaignID, Rhythm: r.Rhythm})
	}
	return toMap(snap)
}

func seriesOf(m models.JSONMap) (*models.Series, []models.CampaignSeriesRun, error) {
	var snap seriesSnapshot
	if err := fromMap(m, &snap); err != nil {
		return nil, nil, err
	}
	runs := make([]models.CampaignSeriesRun, len(snap.Runs))
	for i, r := range snap.Runs {
		runs[i] = models.CampaignSeriesRun{CampaignID: r.CampaignID, Rhythm: r.Rhythm}
	}
	return snap.Series, runs, nil
}

// CampaignSeries is a campaign's run of a series for the undo log; nil run is
// a series the campaign doesn't run.
func CampaignSeries(campaignID, seriesID string, run *models.CampaignSeriesRun) models.JSONMap {
	return toMap(runSnapshot{CampaignID: campaignID, SeriesID: seriesID, Run: run})
}

// CampaignSeriesID is the entity id a run change is logged under.
func CampaignSeriesID(campaignID, seriesID string) string { return campaignID + "/" + seriesID }

// WithSeries lets s undo series changes and changes to what campaigns run.
func (s *Service) WithSeries(series repository.SeriesRepository) *Service {
	s.undoers[models.AssistantActionSeries] = seriesUndo{series}
	s.undoers[models.AssistantActionCampaignSeries] = campaignSeriesUndo{series}
	return s
}

type seriesUndo struct{ series repository.SeriesRepository }

// Undo restores a series as it was, within tx, while it still holds what the
// change left. An added series is taken away only while no post uses it; a
// removed one comes back with its runs on the campaigns still live.
func (u seriesUndo) Undo(ctx context.Context, tx bun.IDB, a *models.AssistantAction) error {
	restored, runs, err := seriesOf(a.Before)
	if err != nil {
		return fmt.Errorf("restore series: %w", err)
	}
	was, _, err := seriesOf(a.After)
	if err != nil {
		return fmt.Errorf("read changed series: %w", err)
	}
	switch {
	case was != nil && restored == nil:
		used, err := u.series.Usage(ctx, []string{a.EntityID})
		if err != nil {
			return err
		}
		if n := used[a.EntityID]; n.Drafts+n.Published > 0 {
			return ErrChangedSince
		}
		_, err = u.series.SoftDeleteIf(ctx, tx, a.EntityID, was)
		return seriesChanged(err)
	case was != nil:
		restored.ID = a.EntityID
		return seriesChanged(u.series.UpdateIf(ctx, tx, restored, was))
	case restored != nil:
		restored.ID = a.EntityID
		return seriesChanged(u.series.Undelete(ctx, tx, restored, runs))
	}
	return errors.New("series change logs no series")
}

type campaignSeriesUndo struct{ series repository.SeriesRepository }

// Undo puts a campaign's run of a series back as it was, within tx, while it
// still is what the change left.
func (u campaignSeriesUndo) Undo(ctx context.Context, tx bun.IDB, a *models.AssistantAction) error {
	var before, after runSnapshot
	if err := fromMap(a.Before, &before); err != nil {
		return fmt.Errorf("restore run: %w", err)
	}
	if err := fromMap(a.After, &after); err != nil {
		return fmt.Errorf("read changed run: %w", err)
	}
	return seriesChanged(u.series.SetRunIf(ctx, tx, after.CampaignID, after.SeriesID, before.Run, after.Run))
}

func seriesChanged(err error) error {
	if errors.Is(err, repository.ErrSeriesChanged) || errors.Is(err, repository.ErrSeriesNotLive) ||
		errors.Is(err, repository.ErrCampaignNotLive) {
		return ErrChangedSince
	}
	return err
}
