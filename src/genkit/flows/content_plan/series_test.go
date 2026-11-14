package content_plan

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/flowkit"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/usecase/campaigngoal"
)

type stubSeriesRepo struct {
	repository.SeriesRepository
	runs    []models.CampaignSeriesRun
	series  map[string]models.Series
	runsErr error
}

func (r stubSeriesRepo) Runs(context.Context, string) ([]models.CampaignSeriesRun, error) {
	return r.runs, r.runsErr
}

func (r stubSeriesRepo) GetByID(_ context.Context, id string) (*models.Series, error) {
	if s, ok := r.series[id]; ok {
		return &s, nil
	}
	return nil, sql.ErrNoRows
}

// fourWeeks is a campaign over four whole weeks, Monday 2 to Sunday 29
// November 2026, in tz.
func fourWeeks(tz string) (*models.Campaign, planScope) {
	start := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 11, 29, 0, 0, 0, 0, time.UTC)
	c := &models.Campaign{ID: "camp1", Timezone: tz, StartDate: &start, EndDate: &end}
	return c, planScope{start: start, end: end}
}

func seriesRepo(runs ...models.CampaignSeriesRun) stubSeriesRepo {
	r := stubSeriesRepo{runs: runs, series: map[string]models.Series{}}
	for _, run := range runs {
		if run.SeriesID != "gone" {
			r.series[run.SeriesID] = models.Series{ID: run.SeriesID, Name: "Series " + run.SeriesID}
		}
	}
	return r
}

// A run every 2 weeks on Tuesday claims the Tuesdays of weeks 1 and 3; a run
// twice a week claims 2 × Periods, as before; an occasional run, or one whose
// series is gone, claims none.
func TestSeriesRuns_ClaimTheRhythmsDays(t *testing.T) {
	for _, tz := range []string{"", "America/Los_Angeles", "Pacific/Auckland"} {
		campaign, scope := fourWeeks(tz)
		repos := ContentPlanRepos{Series: seriesRepo(
			models.CampaignSeriesRun{SeriesID: "tue", Rhythm: &models.SeriesRhythm{Times: 1, Per: "week", Every: 2, Weekday: "tuesday"}},
			models.CampaignSeriesRun{SeriesID: "twice", Rhythm: &models.SeriesRhythm{Times: 2, Per: "week"}},
			models.CampaignSeriesRun{SeriesID: "now-and-then"},
			models.CampaignSeriesRun{SeriesID: "gone", Rhythm: &models.SeriesRhythm{Times: 1, Per: "week"}},
		)}
		runs, warnings := seriesRuns(t.Context(), repos, campaign, scope, false)
		if len(warnings) != 0 || len(runs) != 2 {
			t.Fatalf("%q: runs = %+v, warnings = %v", tz, runs, warnings)
		}
		if got := runs[0].days; !slices.Equal(got, []string{"2026-11-03", "2026-11-17"}) {
			t.Errorf("%q: every-2-weeks Tuesday days = %v", tz, got)
		}
		periods := campaigngoal.Periods("week", campaign.StartDate, campaign.EndDate, time.UTC)
		if got := len(runs[1].days); got != 2*periods {
			t.Errorf("%q: twice a week claims %d, want 2 × %d", tz, got, periods)
		}
		if claims(runs) != 2+2*periods {
			t.Errorf("%q: claims = %d", tz, claims(runs))
		}
	}
}

// Over four whole weeks a run every 2 weeks on Tuesday gets 2 series slots,
// both Tuesdays, out of the goal; the rest of the goal is planned as before,
// numbered after them.
func TestPlanRun_SeriesComeOutOfTheGoal(t *testing.T) {
	campaign, scope := fourWeeks("")
	scope.count = 8
	scope.phases = []resolvedPhase{{ID: "ph1", Name: "Only", Sequence: 1}}
	platforms := []resolvedPlatform{{ID: "linkedin", Name: "LinkedIn"}}
	repos := ContentPlanRepos{Series: seriesRepo(
		models.CampaignSeriesRun{SeriesID: "tue", Rhythm: &models.SeriesRhythm{Times: 1, Per: "week", Every: 2, Weekday: "tuesday"}},
	)}
	runs, _ := seriesRuns(t.Context(), repos, campaign, scope, false)

	batches, warnings := planRun(scope, runs, platforms, 30)
	if len(warnings) != 0 || len(batches) != 2 {
		t.Fatalf("batches = %+v, warnings = %v", batches, warnings)
	}
	series, rest := batches[0], batches[1]
	if series.Series == nil || series.PostCount != 2 || series.GlobalStartIndex != 0 {
		t.Fatalf("series batch = %+v", series)
	}
	for _, s := range series.Slots {
		d, _ := time.Parse(time.DateOnly, s.Date)
		if d.Weekday() != time.Tuesday || s.SeriesID != "tue" {
			t.Errorf("series slot %+v is not a Tuesday of the series", s)
		}
	}
	if rest.Series != nil || rest.PostCount != 6 || rest.GlobalStartIndex != 2 || rest.Index != 1 {
		t.Fatalf("regular batch = %+v; want the other 6 of the goal after the series", rest)
	}

	// Claims beyond the goal plan only the series, and say so.
	scope.count = 1
	if batches, warnings := planRun(scope, runs, platforms, 30); len(batches) != 1 || batches[0].Series == nil || len(warnings) != 1 {
		t.Fatalf("over the goal: batches = %+v, warnings = %v", batches, warnings)
	}
	// Without a goal the model decides the rest, after the series.
	scope.count = 0
	if batches, _ := planRun(scope, runs, platforms, 30); len(batches) != 2 || !batches[1].Unplanned || batches[1].GlobalStartIndex != 2 {
		t.Fatalf("no goal: batches = %+v", batches)
	}
	// Without series either, there is nothing to plan.
	if batches, _ := planRun(scope, nil, platforms, 30); batches != nil {
		t.Fatalf("no goal, no series: batches = %+v", batches)
	}
}

// A targeted run plans no series, and a failed read plans without them and
// says so.
func TestSeriesRuns_TargetedAndFailedRead(t *testing.T) {
	campaign, scope := fourWeeks("")
	repos := ContentPlanRepos{Series: seriesRepo(models.CampaignSeriesRun{SeriesID: "s", Rhythm: &models.SeriesRhythm{Times: 1, Per: "week"}})}
	if runs, _ := seriesRuns(t.Context(), repos, campaign, scope, true); runs != nil {
		t.Fatalf("targeted runs = %+v", runs)
	}
	repos.Series = stubSeriesRepo{runsErr: errors.New("down")}
	if runs, warnings := seriesRuns(t.Context(), repos, campaign, scope, false); runs != nil || len(warnings) != 1 {
		t.Fatalf("failed read: runs = %+v, warnings = %v", runs, warnings)
	}
}

// Each series post is fixed to its day, to the phase whose window holds the
// day, and to platforms in turn across all the series' posts.
func TestSeriesBatches_FixEachSlot(t *testing.T) {
	format := models.ContentFormatDigest
	runs := []seriesRun{
		{series: models.Series{ID: "s1", Name: "Digest", Promise: "The week in five links", ContentFormat: &format}, days: []string{"2026-11-03", "2026-11-17"}},
		{series: models.Series{ID: "s2", Name: "Q&A"}, days: []string{"2026-11-25"}},
	}
	phases := []resolvedPhase{{ID: "late", Name: "Late", Sequence: 2}, {ID: "early", Name: "Early", Sequence: 1}}
	platforms := []resolvedPlatform{{ID: "linkedin", Name: "LinkedIn"}, {ID: "x", Name: "X"}}
	specs := seriesBatches(runs, phases, platforms, mustDate(t, "2026-11-02"), mustDate(t, "2026-11-29"), 30)
	if len(specs) != 2 {
		t.Fatalf("specs = %+v", specs)
	}
	got := specs[0]
	if got.Series.ID != "s1" || got.Series.ContentFormat != "digest" || got.Series.Promise != "The week in five links" || got.PostCount != 2 {
		t.Fatalf("first batch = %+v", got)
	}
	want := []fixedSlot{
		{Number: 1, SeriesID: "s1", Date: "2026-11-03", PlatformID: "linkedin", PlatformName: "LinkedIn", PhaseID: "early", PhaseName: "Early"},
		{Number: 2, SeriesID: "s1", Date: "2026-11-17", PlatformID: "x", PlatformName: "X", PhaseID: "late", PhaseName: "Late"},
	}
	if !slices.Equal(got.Slots, want) {
		t.Fatalf("slots = %+v", got.Slots)
	}
	if s := specs[1].Slots; len(s) != 1 || s[0].PlatformID != "linkedin" || s[0].PhaseID != "late" || s[0].Number != 1 {
		t.Fatalf("second series slots = %+v", s)
	}
	if w := got.DateWindow; w.Start != "2026-11-03" || w.End != "2026-11-17" {
		t.Fatalf("window = %+v", w)
	}

	// A batch holds at most the batch size.
	if specs := seriesBatches(runs[:1], phases, platforms, mustDate(t, "2026-11-02"), mustDate(t, "2026-11-29"), 1); len(specs) != 2 || specs[1].Slots[0].Number != 1 {
		t.Fatalf("chunked = %+v", specs)
	}
}

func TestPhaseOn(t *testing.T) {
	windows := []dateWindow{{"2026-11-02", "2026-11-10"}, {"2026-11-15", "2026-11-29"}}
	for day, want := range map[string]int{
		"2026-11-01": 0, "2026-11-02": 0, "2026-11-10": 0, "2026-11-12": 0,
		"2026-11-15": 1, "2026-11-29": 1, "2026-12-03": 1,
	} {
		if got := phaseOn(day, windows); got != want {
			t.Errorf("phaseOn(%s) = %d, want %d", day, got, want)
		}
	}
}

// A post in a series batch lands on its slot whatever the model wrote; a post
// elsewhere is of no series even if the model names one.
func TestPostSink_PlacesSeriesPosts(t *testing.T) {
	var saved []DraftPost
	gen := &postGenerator{
		validate: func(p DraftPost) error {
			if p.ContentType == "bad" {
				return errors.New("bad type")
			}
			return nil
		},
		persist: func(_ context.Context, dp *DraftPost) (string, error) {
			saved = append(saved, *dp)
			return "id", nil
		},
	}
	slots := []fixedSlot{
		{SeriesID: "s1", Date: "2026-11-03", PlatformID: "x", PhaseID: "early"},
		{SeriesID: "s1", Date: "2026-11-17", PlatformID: "linkedin", PhaseID: "late"},
	}
	sink := &postSink{gen: gen, expected: 2, slots: slots, persisted: map[int]bool{}}
	sink.add(t.Context(), DraftPost{Title: "dropped", ContentType: "bad"}, 0)
	sink.add(t.Context(), DraftPost{Title: "one", PublishDate: "2026-11-05", PlatformID: "linkedin", PhaseID: "late"}, 1)
	sink.add(t.Context(), DraftPost{Title: "two"}, 2)
	if len(saved) != 2 {
		t.Fatalf("saved = %+v", saved)
	}
	for i, p := range saved {
		s := slots[i]
		if p.PublishDate != s.Date || p.PlatformID != s.PlatformID || p.PhaseID != s.PhaseID || p.SeriesID == nil || *p.SeriesID != "s1" {
			t.Errorf("post %d = %+v, want on slot %+v", i, p, s)
		}
	}

	saved = nil
	plain := &postSink{gen: gen, persisted: map[int]bool{}}
	plain.add(t.Context(), DraftPost{Title: "plain", SeriesID: new("made-up")}, 0)
	if saved[0].SeriesID != nil {
		t.Fatalf("a regular post kept series %v", *saved[0].SeriesID)
	}
}

// A series idea is saved with its series on its slot's day, even one the
// campaign doesn't publish on.
func TestPersistIdea_SeriesKeepsItsDay(t *testing.T) {
	campaign, scope := fourWeeks("")
	campaign.CreatedBy, campaign.PublishingTime = "user1", "09:00"
	campaign.PublishingDays = models.StringSlice{"mon", "wed", "fri"}
	ideas := &stubIdeaRepo{}
	dp := DraftPost{Title: "T", PublishDate: "2026-11-03", SeriesID: new("s1")}
	if _, err := persistIdea(t.Context(), &dp, campaign, &scope.start, &scope.end, ideas, nil); err != nil {
		t.Fatal(err)
	}
	i := ideas.created
	if i.SeriesID == nil || *i.SeriesID != "s1" || i.Plan.TargetDate.String() != "2026-11-03" || dp.PublishDate != "2026-11-03" {
		t.Fatalf("idea = %+v, date %s", i, dp.PublishDate)
	}

	dp = DraftPost{Title: "T", PublishDate: "2026-11-03"}
	if _, err := persistIdea(t.Context(), &dp, campaign, &scope.start, &scope.end, ideas, nil); err != nil {
		t.Fatal(err)
	}
	if ideas.created.SeriesID != nil || dp.PublishDate == "2026-11-03" {
		t.Fatalf("a regular idea kept the Tuesday: %+v", ideas.created)
	}
}

// A series batch's prompt names the series and lists each fixed slot; the
// other batches don't mention one.
func TestUserPrompt_SeriesBatch(t *testing.T) {
	raw, err := promptFS.ReadFile("prompts/content_plan.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	tmpl := template.Must(template.New("content_plan").Parse(string(raw))).Lookup("user")
	spec := batchSpec{
		PostCount: 1,
		Series:    &seriesBrief{Name: "Digest", Promise: "The week in five links", Recipe: "Five links, one line each", ContentFormat: "digest"},
		Slots:     []fixedSlot{{Number: 1, Date: "2026-11-03", PlatformID: "x", PlatformName: "X", PhaseID: "early", PhaseName: "Early"}},
	}
	got, err := flowkit.RenderTemplate(tmpl, contentPlanTemplateData{Batch: &spec})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`the series "Digest"`, "Promise to the audience: The week in five links", "Recipe for each episode: Five links, one line each", "Format: digest",
		`1. publishDate "2026-11-03", X (platformId: "x"), phase "Early" (phaseId: "early")`, "exactly 1 posts"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "one slice of a larger plan") {
		t.Error("the series batch got the regular batch's instructions")
	}

	plain, err := flowkit.RenderTemplate(tmpl, contentPlanTemplateData{Batch: &batchSpec{PostCount: 3}})
	if err != nil || strings.Contains(plain, "series") || !strings.Contains(plain, "one slice of a larger plan") {
		t.Fatalf("regular prompt (%v):\n%s", err, plain)
	}
}
