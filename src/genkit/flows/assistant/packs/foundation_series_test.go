package packs

import (
	"errors"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/usecase/assistantundo"
	"github.com/ogen-app/ogen/src/usecase/series"
)

func TestSeries_ToolsAndUndo(t *testing.T) {
	db, actions := undoStore(t)
	ctx, _ := turn(t, campaign1, nil)
	for _, m := range []any{
		&models.Account{ID: "acc-1", Email: "jane@example.com", Name: "Jane"},
		&models.User{ID: "u1", AccountID: "acc-1", TenantID: "tenant-1", Name: "Jane", Email: "jane@example.com", Role: models.RoleOwner},
	} {
		if _, err := db.NewInsert().Model(m).Exec(ctx); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	campaigns := repository.NewCampaignRepository(db, repository.NewTagRepository(db), repository.NewPlatformRepository(db), repository.NewCampaignTypeRepository(db))
	if err := campaigns.Create(ctx, &models.Campaign{ID: "c1", Name: "Launch", AssetIDs: models.StringSlice{}, TargetPlatforms: models.CampaignPlatforms{},
		CampaignTypeID: "Uk", Language: "en", PublishingTime: "09:00", PublishingDays: models.StringSlice{}, SpreadMinutes: 15,
		GoalCadence: "month", Status: models.StatusActive, Currency: "USD", TagIDs: models.StringSlice{}, CreatedBy: "u1"}); err != nil {
		t.Fatal(err)
	}
	repo := repository.NewSeriesRepository(db)
	d := Deps{Series: series.New(repo), Actions: actions, Campaigns: campaigns}
	undo := assistantundo.New(db, actions, nil).WithSeries(repo)

	made, err := createSeries(ctx, d, SeriesInput{Name: "Tip Tuesday", Promise: "One tip a week", DefaultRhythm: &RhythmInput{Times: 1, Per: "Week"}})
	if err != nil || made.UndoID == "" || made.Series.Supply != "self" || made.Series.DefaultRhythm.Per != "week" {
		t.Fatalf("created = %+v, %v", made, err)
	}
	id := made.Series.ID
	_, err = createSeries(ctx, d, SeriesInput{Name: "  "})
	refused(t, err)

	// The thread's campaign runs it at its default rhythm, then twice a month.
	ran, err := runSeries(ctx, d, RunSeriesInput{SeriesID: id})
	if err != nil || !ran.Running || ran.Rhythm == nil || ran.Rhythm.Times != 1 || ran.UndoID == "" ||
		ran.CampaignName != "Launch" || ran.SeriesName != "Tip Tuesday" {
		t.Fatalf("run = %+v, %v", ran, err)
	}
	again, err := runSeries(ctx, d, RunSeriesInput{SeriesID: id, Rhythm: &RhythmInput{Times: 2, Per: "month"}})
	if err != nil || again.Rhythm.Per != "month" {
		t.Fatalf("rhythm = %+v, %v", again, err)
	}
	list, err := listSeries(ctx, d, ListSeriesInput{})
	if err != nil || len(list.Series) != 1 || !list.Series[0].Runs || list.Series[0].Rhythm.Times != 2 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	// Each series says where it runs, by campaign name.
	if in := list.Series[0].RunsIn; len(in) != 1 || in[0].CampaignName != "Launch" || in[0].Rhythm.Times != 2 {
		t.Fatalf("runs in = %+v", in)
	}
	// Undo the rhythm change: once a week again.
	if _, err := undo.Undo(ctx, again.UndoID, "u1"); err != nil {
		t.Fatal(err)
	}
	if r, _ := repo.Run(ctx, "c1", id); r == nil || r.Rhythm.Per != "week" {
		t.Fatalf("run after undo = %+v", r)
	}

	edited, err := updateSeries(ctx, d, UpdateSeriesInput{SeriesID: id, Recipe: "A screenshot and two lines", Occasional: true})
	if err != nil || edited.Series.Promise != "One tip a week" || edited.Series.DefaultRhythm != nil {
		t.Fatalf("edited = %+v, %v", edited, err)
	}

	// Removing it stops the campaign running it; undo brings both back.
	gone, err := removeSeries(ctx, d, SeriesRef{SeriesID: id})
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := repo.Run(ctx, "c1", id); r != nil {
		t.Fatal("the campaign still runs a removed series")
	}
	if _, err := undo.Undo(ctx, gone.UndoID, "u1"); err != nil {
		t.Fatal(err)
	}
	if r, _ := repo.Run(ctx, "c1", id); r == nil || r.Rhythm.Per != "week" {
		t.Fatalf("run after restore = %+v", r)
	}
	// The edit still holds, so it can be undone; the create can't while the
	// series has changed since.
	if _, err := undo.Undo(ctx, made.UndoID, "u1"); !errors.Is(err, assistantundo.ErrChangedSince) {
		t.Fatalf("undo create: err = %v, want ErrChangedSince", err)
	}
	if _, err := undo.Undo(ctx, edited.UndoID, "u1"); err != nil {
		t.Fatal(err)
	}
	stopped, err := stopSeries(ctx, d, StopSeriesInput{SeriesID: id})
	if err != nil || stopped.Running {
		t.Fatalf("stop = %+v, %v", stopped, err)
	}
	if _, err := undo.Undo(ctx, made.UndoID, "u1"); err != nil {
		t.Fatalf("undo create: %v", err)
	}
	if _, err := d.Series.Get(ctx, id); !errors.Is(err, series.ErrNotFound) {
		t.Fatalf("get after undo create: %v", err)
	}

	// Made and run in one step: one result, one undo that takes both away.
	both, err := createSeries(ctx, d, SeriesInput{Name: "People of the house", Recipe: "A person who works here",
		RunIn: &RunInInput{Rhythm: &RhythmInput{Times: 2, Per: "month"}}})
	if err != nil || both.Run == nil || both.Run.CampaignName != "Launch" || both.Run.Rhythm.Times != 2 || both.Run.UndoID != "" ||
		len(both.Series.RunsIn) != 1 || both.UndoID == "" {
		t.Fatalf("made and run = %+v, %v", both, err)
	}
	if _, err := undo.Undo(ctx, both.UndoID, "u1"); err != nil {
		t.Fatal(err)
	}
	if r, _ := repo.Run(ctx, "c1", both.Series.ID); r != nil {
		t.Fatal("the campaign still runs an undone series")
	}
	// A campaign that's gone still makes the series, and says why it isn't run.
	orphan, err := createSeries(ctx, d, SeriesInput{Name: "Guest letters", RunIn: &RunInInput{CampaignID: "gone"}})
	if err != nil || orphan.Run != nil || orphan.NotRun == "" || orphan.UndoID == "" {
		t.Fatalf("not run = %+v, %v", orphan, err)
	}
}
