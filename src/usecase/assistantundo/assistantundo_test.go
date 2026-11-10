package assistantundo_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/campaignphase"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/pgtest"
	"github.com/ogen-app/ogen/src/usecase/assistantundo"
)

type fixture struct {
	db        *bun.DB
	ctx       context.Context
	actions   repository.AssistantActionRepository
	campaigns repository.CampaignRepository
	svc       *assistantundo.Service
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db := pgtest.MustDB()
	t.Cleanup(func() { _ = db.Close() })
	ctx := tenantctx.With(context.Background(), models.DefaultTenantID)
	for _, m := range []any{
		&models.Account{ID: "acc-1", Email: "jane@example.com", Name: "Jane"},
		&models.User{ID: "user-1", AccountID: "acc-1", TenantID: models.DefaultTenantID, Name: "Jane", Email: "jane@example.com", Role: models.RoleOwner},
	} {
		if _, err := db.NewInsert().Model(m).Exec(ctx); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	f := &fixture{
		db:      db,
		ctx:     ctx,
		actions: repository.NewAssistantActionRepository(db),
		campaigns: repository.NewCampaignRepository(db, repository.NewTagRepository(db),
			repository.NewPlatformRepository(db), repository.NewCampaignTypeRepository(db)),
	}
	f.svc = assistantundo.New(db, f.actions, f.campaigns)
	return f
}

// seedCampaign stores a campaign of the seeded two-phase type "Uk".
func (f *fixture) seedCampaign(t *testing.T, edit func(*models.Campaign)) *models.Campaign {
	t.Helper()
	c := &models.Campaign{
		ID: "c1", Name: "Launch", AssetIDs: models.StringSlice{}, TargetPlatforms: models.CampaignPlatforms{},
		CampaignTypeID: "Uk", Language: "en", PublishingTime: "09:00", PublishingDays: models.StringSlice{},
		SpreadMinutes: 15, GoalCadence: "month", Status: models.StatusActive, Currency: "USD",
		TagIDs: models.StringSlice{}, CreatedBy: "user-1",
	}
	if edit != nil {
		edit(c)
	}
	if err := f.campaigns.Create(f.ctx, c); err != nil {
		t.Fatal(err)
	}
	return f.load(t)
}

func (f *fixture) load(t *testing.T) *models.Campaign {
	t.Helper()
	c, err := f.campaigns.GetByID(f.ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// save writes the whole campaign, as an edit elsewhere does.
func (f *fixture) save(t *testing.T, c *models.Campaign) {
	t.Helper()
	if err := f.campaigns.Update(f.ctx, c); err != nil {
		t.Fatal(err)
	}
}

// logChange applies change to the stored campaign and logs it, as a tool does.
func (f *fixture) logChange(t *testing.T, kind models.AssistantActionKind,
	snapshot func(*models.Campaign) models.JSONMap, change func(*models.Campaign),
) string {
	t.Helper()
	c := f.load(t)
	before := snapshot(c)
	change(c)
	f.save(t, c)
	id, err := assistantundo.Record(f.ctx, f.actions, &models.AssistantAction{
		ScopeKind: models.AssistantScopeCampaign, ScopeID: "c1", Kind: kind, EntityID: "c1",
		Before: before, After: snapshot(c), CreatedBy: "user-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (f *fixture) undone(t *testing.T, id string) bool {
	t.Helper()
	a, err := f.actions.GetByID(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return a.UndoneAt != nil
}

func day(s string) *time.Time {
	t, _ := time.Parse(time.DateOnly, s)
	return &t
}

func TestUndo_CampaignDates(t *testing.T) {
	f := newFixture(t)
	f.seedCampaign(t, func(c *models.Campaign) { c.EndDate = day("2026-06-30") })
	id := f.logChange(t, models.AssistantActionCampaignDates, assistantundo.CampaignDates, func(c *models.Campaign) {
		c.StartDate, c.EndDate = day("2026-06-01"), day("2026-07-31")
	})

	a, err := f.svc.Undo(f.ctx, id, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	c := f.load(t)
	if c.StartDate != nil || !c.EndDate.Equal(*day("2026-06-30")) {
		t.Fatalf("restored dates = %v, %v", c.StartDate, c.EndDate)
	}
	if a.UndoneBy != "user-1" || a.UndoneAt == nil || !f.undone(t, id) {
		t.Fatalf("action = %+v", a)
	}
	if _, err := f.svc.Undo(f.ctx, id, "user-1"); !errors.Is(err, assistantundo.ErrAlreadyUndone) {
		t.Fatalf("second undo err = %v", err)
	}
}

func TestUndo_CampaignBrief(t *testing.T) {
	f := newFixture(t)
	f.seedCampaign(t, func(c *models.Campaign) { c.Description, c.KeyMessages = "old", "km" })
	id := f.logChange(t, models.AssistantActionCampaignBrief, assistantundo.CampaignBrief, func(c *models.Campaign) {
		c.Description, c.TargetPersona, c.KeyMessages, c.ToneGuidelines = "new", "persona", "km2", "calm"
	})
	// A field outside the brief changes after the assistant's edit.
	c := f.load(t)
	c.Name = "Renamed"
	f.save(t, c)

	if _, err := f.svc.Undo(f.ctx, id, "user-1"); err != nil {
		t.Fatal(err)
	}
	c = f.load(t)
	if c.Description != "old" || c.TargetPersona != "" || c.KeyMessages != "km" || c.ToneGuidelines != "" {
		t.Fatalf("restored brief = %+v", c)
	}
	if c.Name != "Renamed" {
		t.Fatalf("name = %q, undo rewrote a field outside the brief", c.Name)
	}
}

// An edit made after the assistant's change is never overwritten.
func TestUndo_RefusesWhenChangedSince(t *testing.T) {
	f := newFixture(t)
	f.seedCampaign(t, func(c *models.Campaign) { c.Description = "old" })
	id := f.logChange(t, models.AssistantActionCampaignBrief, assistantundo.CampaignBrief, func(c *models.Campaign) {
		c.Description = "assistant"
	})
	c := f.load(t)
	c.Description = "user edit"
	f.save(t, c)

	if _, err := f.svc.Undo(f.ctx, id, "user-1"); !errors.Is(err, assistantundo.ErrChangedSince) {
		t.Fatalf("err = %v, want ErrChangedSince", err)
	}
	if got := f.load(t).Description; got != "user edit" {
		t.Fatalf("description = %q, the later edit was overwritten", got)
	}
	if f.undone(t, id) {
		t.Fatal("a refused undo was recorded")
	}
}

func TestUndo_DatesRefuseWhenChangedSince(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*models.Campaign)
	}{
		{"start date", func(c *models.Campaign) { c.StartDate = day("2026-06-05") }},
		{"end date", func(c *models.Campaign) { c.EndDate = day("2026-08-15") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.seedCampaign(t, func(c *models.Campaign) { c.StartDate, c.EndDate = day("2026-06-01"), day("2026-06-30") })
			id := f.logChange(t, models.AssistantActionCampaignDates, assistantundo.CampaignDates, func(c *models.Campaign) {
				c.EndDate = day("2026-07-31")
			})
			c := f.load(t)
			tc.edit(c)
			f.save(t, c)
			want := assistantundo.CampaignDates(c)

			if _, err := f.svc.Undo(f.ctx, id, "user-1"); !errors.Is(err, assistantundo.ErrChangedSince) {
				t.Fatalf("err = %v, want ErrChangedSince", err)
			}
			if got := assistantundo.CampaignDates(f.load(t)); got["startDate"] != want["startDate"] || got["endDate"] != want["endDate"] {
				t.Fatalf("dates = %v, want the later edit %v", got, want)
			}
			if f.undone(t, id) {
				t.Fatal("a refused undo was recorded")
			}
		})
	}
}

// An edit that commits while the undo is checking is seen, not overwritten.
func TestUndo_WaitsForAConcurrentEdit(t *testing.T) {
	f := newFixture(t)
	f.seedCampaign(t, func(c *models.Campaign) { c.Description = "old" })
	id := f.logChange(t, models.AssistantActionCampaignBrief, assistantundo.CampaignBrief, func(c *models.Campaign) {
		c.Description = "assistant"
	})

	tx, err := f.db.BeginTx(f.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.NewUpdate().Model((*models.Campaign)(nil)).Set("description = ?", "user edit").
		Where("id = ?", "c1").Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := f.svc.Undo(f.ctx, id, "user-1")
		done <- err
	}()
	// Give the undo time to block on the edit's row lock, then commit it.
	time.Sleep(300 * time.Millisecond)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	if err := <-done; !errors.Is(err, assistantundo.ErrChangedSince) {
		t.Fatalf("err = %v, want ErrChangedSince", err)
	}
	if got := f.load(t).Description; got != "user edit" {
		t.Fatalf("description = %q, the concurrent edit was overwritten", got)
	}
}

// failingMark records the undo, then fails, as a dropped connection would.
type failingMark struct {
	repository.AssistantActionRepository
}

func (m failingMark) MarkUndone(ctx context.Context, db bun.IDB, id, by string, now time.Time) (bool, error) {
	if _, err := m.AssistantActionRepository.MarkUndone(ctx, db, id, by, now); err != nil {
		return false, err
	}
	return false, errors.New("connection lost")
}

// A restore whose record fails is rolled back, so the change stays undoable.
func TestUndo_RestoreAndRecordCommitTogether(t *testing.T) {
	f := newFixture(t)
	f.seedCampaign(t, func(c *models.Campaign) { c.Description = "old" })
	id := f.logChange(t, models.AssistantActionCampaignBrief, assistantundo.CampaignBrief, func(c *models.Campaign) {
		c.Description = "assistant"
	})

	svc := assistantundo.New(f.db, failingMark{f.actions}, f.campaigns)
	if _, err := svc.Undo(f.ctx, id, "user-1"); err == nil {
		t.Fatal("undo succeeded with a failed record")
	}
	if got := f.load(t).Description; got != "assistant" {
		t.Fatalf("description = %q, the restore outlived the failed record", got)
	}
	if f.undone(t, id) {
		t.Fatal("the failed undo was recorded")
	}
	if _, err := f.svc.Undo(f.ctx, id, "user-1"); err != nil {
		t.Fatalf("retry err = %v", err)
	}
}

// Undoing dates moves a manual plan's outer bounds with them, keeping the
// boundary the user set between the phases.
func TestUndo_DatesReanchorManualPlan(t *testing.T) {
	f := newFixture(t)
	f.seedCampaign(t, func(c *models.Campaign) { c.StartDate, c.EndDate = day("2026-06-01"), day("2026-06-30") })
	id := f.logChange(t, models.AssistantActionCampaignDates, assistantundo.CampaignDates, func(c *models.Campaign) {
		c.EndDate = day("2026-07-31")
	})
	// The user plans the extended campaign by hand.
	if err := f.campaigns.ReplacePhaseWindows(f.ctx, "c1", []models.CampaignPhaseWindow{
		{PhaseID: "98", StartDate: *day("2026-06-01"), EndDate: *day("2026-06-20")},
		{PhaseID: "xh", StartDate: *day("2026-06-21"), EndDate: *day("2026-07-31")},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := f.svc.Undo(f.ctx, id, "user-1"); err != nil {
		t.Fatal(err)
	}
	windows, source := campaignphase.Resolve(f.load(t))
	if source != campaignphase.SourceManual || len(windows) != 2 {
		t.Fatalf("plan = %v %+v, want the manual plan kept", source, windows)
	}
	if !windows[0].End.Equal(*day("2026-06-20")) || !windows[1].End.Equal(*day("2026-06-30")) {
		t.Fatalf("windows = %+v, want 06-01..06-20, 06-21..06-30", windows)
	}
}

func TestUndo_CampaignPlatforms(t *testing.T) {
	f := newFixture(t)
	f.seedCampaign(t, func(c *models.Campaign) {
		c.TargetPlatforms = models.CampaignPlatforms{{ID: "pl1", PostTypes: []string{"article"}}}
	})
	id := f.logChange(t, models.AssistantActionCampaignPlatforms, assistantundo.CampaignPlatforms, func(c *models.Campaign) {
		c.TargetPlatforms = append(c.TargetPlatforms, models.CampaignPlatform{ID: "pl2"})
	})
	// The content bank changes after the assistant's edit.
	if _, err := f.campaigns.AddAssetIDs(f.ctx, "c1", []string{"asset-1"}); err != nil {
		t.Fatal(err)
	}

	if _, err := f.svc.Undo(f.ctx, id, "user-1"); err != nil {
		t.Fatal(err)
	}
	c := f.load(t)
	tp := c.TargetPlatforms
	if len(tp) != 1 || tp[0].ID != "pl1" || len(tp[0].PostTypes) != 1 || tp[0].PostTypes[0] != "article" {
		t.Fatalf("restored platforms = %+v", tp)
	}
	if !c.UseAssets || len(c.AssetIDs) != 1 {
		t.Fatalf("assets = %v (use %t), undo rewrote the content bank", c.AssetIDs, c.UseAssets)
	}
}

// A later edit to the platforms, or to a platform's post types, is kept.
func TestUndo_PlatformsRefuseWhenChangedSince(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*models.Campaign)
	}{
		{"platform", func(c *models.Campaign) { c.TargetPlatforms = c.TargetPlatforms[:1] }},
		{"post types", func(c *models.Campaign) { c.TargetPlatforms[0].PostTypes = []string{"article", "carousel"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.seedCampaign(t, func(c *models.Campaign) {
				c.TargetPlatforms = models.CampaignPlatforms{{ID: "pl1", PostTypes: []string{"article"}}}
			})
			id := f.logChange(t, models.AssistantActionCampaignPlatforms, assistantundo.CampaignPlatforms, func(c *models.Campaign) {
				c.TargetPlatforms = append(c.TargetPlatforms, models.CampaignPlatform{ID: "pl2"})
			})
			c := f.load(t)
			tc.edit(c)
			f.save(t, c)
			want := assistantundo.CampaignPlatforms(c)

			if _, err := f.svc.Undo(f.ctx, id, "user-1"); !errors.Is(err, assistantundo.ErrChangedSince) {
				t.Fatalf("err = %v, want ErrChangedSince", err)
			}
			if got := assistantundo.CampaignPlatforms(f.load(t)); fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("platforms = %v, want the later edit %v", got, want)
			}
			if f.undone(t, id) {
				t.Fatal("a refused undo was recorded")
			}
		})
	}
}

// failingUpdate writes the columns, then fails, as a dropped connection would.
type failingUpdate struct {
	repository.CampaignRepository
}

func (r failingUpdate) UpdateColumns(ctx context.Context, db bun.IDB, c *models.Campaign, columns ...string) error {
	if err := r.CampaignRepository.UpdateColumns(ctx, db, c, columns...); err != nil {
		return err
	}
	return errors.New("connection lost")
}

// A restore that fails after its write is rolled back with the record.
func TestUndo_FailedRestoreRollsBack(t *testing.T) {
	f := newFixture(t)
	f.seedCampaign(t, func(c *models.Campaign) { c.Description = "old" })
	id := f.logChange(t, models.AssistantActionCampaignBrief, assistantundo.CampaignBrief, func(c *models.Campaign) {
		c.Description = "assistant"
	})

	svc := assistantundo.New(f.db, f.actions, failingUpdate{f.campaigns})
	if _, err := svc.Undo(f.ctx, id, "user-1"); err == nil {
		t.Fatal("undo succeeded with a failed restore")
	}
	if got := f.load(t).Description; got != "assistant" {
		t.Fatalf("description = %q, the failed restore was kept", got)
	}
	a, err := f.actions.GetByID(f.ctx, id)
	if err != nil || a.UndoneAt != nil || a.UndoneBy != "" {
		t.Fatalf("action = %+v, %v; want it still undoable", a, err)
	}
}

// An undo holds one connection: on a pool of one it still completes.
func TestUndo_RunsOnItsTransactionAlone(t *testing.T) {
	f := newFixture(t)
	f.seedCampaign(t, func(c *models.Campaign) {
		c.EndDate, c.TagIDs = day("2026-06-30"), models.StringSlice{"tag-1"}
		c.TargetPlatforms = models.CampaignPlatforms{{ID: "linkedin"}}
	})
	id := f.logChange(t, models.AssistantActionCampaignDates, assistantundo.CampaignDates, func(c *models.Campaign) {
		c.EndDate = day("2026-07-31")
	})
	f.db.SetMaxOpenConns(1)

	ctx, cancel := context.WithTimeout(f.ctx, 5*time.Second)
	defer cancel()
	if _, err := f.svc.Undo(ctx, id, "user-1"); err != nil {
		t.Fatalf("undo on a one-connection pool: %v", err)
	}
	if got := f.load(t).EndDate; !got.Equal(*day("2026-06-30")) {
		t.Fatalf("end = %v, want 2026-06-30", got)
	}
}

// A manual-plan edit takes the campaign's row lock before the plan's rows,
// as a date undo does, so the two queue instead of deadlocking.
func TestReplacePhaseWindows_LocksTheCampaignFirst(t *testing.T) {
	f := newFixture(t)
	f.seedCampaign(t, func(c *models.Campaign) { c.StartDate, c.EndDate = day("2026-06-01"), day("2026-06-30") })
	plan := func() []models.CampaignPhaseWindow {
		return []models.CampaignPhaseWindow{
			{PhaseID: "98", StartDate: *day("2026-06-01"), EndDate: *day("2026-06-15")},
			{PhaseID: "xh", StartDate: *day("2026-06-16"), EndDate: *day("2026-06-30")},
		}
	}
	if err := f.campaigns.ReplacePhaseWindows(f.ctx, "c1", plan()); err != nil {
		t.Fatal(err)
	}

	// The undo's lock.
	undo, err := f.db.BeginTx(f.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = undo.Rollback() }()
	if _, err := f.campaigns.GetForUpdate(f.ctx, undo, "c1"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- f.campaigns.ReplacePhaseWindows(f.ctx, "c1", plan()) }()

	// Wait for the edit to block on a lock.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting int
		if err := f.db.NewRaw("SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'").
			Scan(f.ctx, &waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("the edit ran while the campaign was locked: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the edit never waited for the campaign's lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// It holds none of the plan's rows while it waits.
	var ids []string
	if err := f.db.NewRaw("SELECT phase_id FROM campaign_phase_windows WHERE campaign_id = ? FOR UPDATE NOWAIT", "c1").
		Scan(f.ctx, &ids); err != nil {
		t.Fatalf("the waiting edit holds the plan's rows: %v", err)
	}
	if err := undo.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestUndo_NotFound(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.Undo(f.ctx, "nope", "user-1"); !errors.Is(err, assistantundo.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// logCreated logs the seeded campaign as one the assistant created.
func (f *fixture) logCreated(t *testing.T) string {
	t.Helper()
	id, err := assistantundo.Record(f.ctx, f.actions, &models.AssistantAction{
		ScopeKind: models.AssistantScopeWorkspace, Kind: models.AssistantActionCampaignCreated, EntityID: "c1",
		Before: models.JSONMap{}, After: assistantundo.CampaignCreated(f.load(t)), CreatedBy: "user-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestUndo_CampaignCreated(t *testing.T) {
	f := newFixture(t)
	f.seedCampaign(t, func(c *models.Campaign) {
		c.StartDate, c.EndDate, c.AssetIDs = day("2026-03-02"), day("2026-04-26"), models.StringSlice{"a2", "a1"}
	})
	id := f.logCreated(t)
	// An idea the sources' analyses distilled on their own isn't work.
	if _, err := f.db.NewInsert().Model(&models.Idea{ID: "auto", Title: "t", CampaignID: new("c1"),
		Origin: models.IdeaOriginAssetAnalysis, CreatedByName: "Ogen"}).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}

	if _, err := f.svc.Undo(f.ctx, id, "user-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.campaigns.GetByID(f.ctx, "c1"); err == nil {
		t.Fatal("the campaign is still there")
	}
}

func TestUndo_CampaignCreatedRefusesOnceUsed(t *testing.T) {
	for name, use := range map[string]func(t *testing.T, f *fixture){
		"renamed": func(t *testing.T, f *fixture) {
			c := f.load(t)
			c.Name = "Spring"
			f.save(t, c)
		},
		"an idea written in it": func(t *testing.T, f *fixture) {
			if _, err := f.db.NewInsert().Model(&models.Idea{ID: "mine", Title: "t", CampaignID: new("c1"), CreatedByName: "Jane"}).Exec(f.ctx); err != nil {
				t.Fatal(err)
			}
		},
		"its brief written": func(t *testing.T, f *fixture) {
			c := f.load(t)
			c.KeyMessages = "Slow down at the villa"
			f.save(t, c)
		},
		"an analysis idea planned": func(t *testing.T, f *fixture) {
			if _, err := f.db.NewInsert().Model(&models.Idea{ID: "planned", Title: "t", CampaignID: new("c1"),
				Origin: models.IdeaOriginAssetAnalysis, Plan: models.IdeaPlan{PostType: new("text-post")}, CreatedByName: "Ogen"}).Exec(f.ctx); err != nil {
				t.Fatal(err)
			}
		},
		"a series run in it": func(t *testing.T, f *fixture) {
			if _, err := f.db.NewInsert().Model(&models.Series{ID: "s1", Name: "Fridays", Promise: "p", Recipe: "r",
				Supply: models.SeriesSupplySelf, CampaignID: new("c1")}).Exec(f.ctx); err != nil {
				t.Fatal(err)
			}
		},
		"an analysis idea decided": func(t *testing.T, f *fixture) {
			if _, err := f.db.NewInsert().Model(&models.Idea{ID: "kept", Title: "t", CampaignID: new("c1"),
				Origin: models.IdeaOriginAssetAnalysis, Verdict: new(models.IdeaVerdictYes), DecidedAt: new(time.Now()), DecidedBy: new("user-1"),
				CreatedByName: "Ogen"}).Exec(f.ctx); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.seedCampaign(t, nil)
			id := f.logCreated(t)
			use(t, f)
			if _, err := f.svc.Undo(f.ctx, id, "user-1"); !errors.Is(err, assistantundo.ErrChangedSince) {
				t.Fatalf("err = %v, want ErrChangedSince", err)
			}
			if _, err := f.campaigns.GetByID(f.ctx, "c1"); err != nil {
				t.Fatalf("the campaign is gone: %v", err)
			}
		})
	}
}

// The tool logs the campaign as it built it, before the database read it
// back: that snapshot must match the stored row, or no creation could be
// undone.
func TestUndo_CampaignCreatedFromTheBuiltCampaign(t *testing.T) {
	f := newFixture(t)
	c := &models.Campaign{
		ID: "c1", Name: "Spring reopening", Description: "Around the opening.", CampaignTypeID: "Uk", Status: models.StatusActive,
		AssetIDs: models.StringSlice{"a2", "a1"}, UseAssets: true, TargetPlatforms: models.CampaignPlatforms{},
		TagIDs: models.StringSlice{}, Tags: []models.Tag{}, StartDate: day("2026-03-02"), EndDate: day("2026-04-26"),
		PublishingTime: "09:00", PublishingDays: models.StringSlice{"mon"}, SpreadMinutes: 15, GoalCadence: "month", CreatedBy: "user-1",
	}
	after := assistantundo.CampaignCreated(c)
	if err := f.campaigns.Create(f.ctx, c); err != nil {
		t.Fatal(err)
	}
	id, err := assistantundo.Record(f.ctx, f.actions, &models.AssistantAction{
		ScopeKind: models.AssistantScopeWorkspace, Kind: models.AssistantActionCampaignCreated, EntityID: "c1",
		Before: models.JSONMap{}, After: after, CreatedBy: "user-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Undo(f.ctx, id, "user-1"); err != nil {
		t.Fatalf("undo = %v; the built campaign's snapshot must match its row", err)
	}
}

// Undoing a goal change restores the posts per period and the period,
// unless the goal was changed again since.
func TestUndo_CampaignGoal(t *testing.T) {
	f := newFixture(t)
	f.seedCampaign(t, func(c *models.Campaign) { c.EstimatedPostCount, c.GoalCadence = new(3), "week" })
	id := f.logChange(t, models.AssistantActionCampaignGoal, assistantundo.CampaignGoal, func(c *models.Campaign) {
		c.EstimatedPostCount, c.GoalCadence = new(5), "month"
	})
	if _, err := f.svc.Undo(f.ctx, id, "user-1"); err != nil {
		t.Fatal(err)
	}
	if c := f.load(t); c.EstimatedPostCount == nil || *c.EstimatedPostCount != 3 || c.GoalCadence != "week" {
		t.Fatalf("restored goal = %v %s", c.EstimatedPostCount, c.GoalCadence)
	}

	id = f.logChange(t, models.AssistantActionCampaignGoal, assistantundo.CampaignGoal, func(c *models.Campaign) { c.EstimatedPostCount = new(8) })
	c := f.load(t)
	c.EstimatedPostCount = new(9)
	f.save(t, c)
	if _, err := f.svc.Undo(f.ctx, id, "user-1"); !errors.Is(err, assistantundo.ErrChangedSince) {
		t.Fatalf("undo over a later change = %v", err)
	}
}
