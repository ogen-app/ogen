package packs

import (
	"cmp"
	"context"
	"errors"
	"strings"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/usecase/assistantundo"
	"github.com/ogen-app/ogen/src/usecase/series"
)

// RhythmInput is how often a series runs.
type RhythmInput struct {
	Times int    `json:"times" jsonschema:"description=1-31"`
	Per   string `json:"per"   jsonschema:"description=week or month"`
}

func (r *RhythmInput) rhythm() *models.SeriesRhythm {
	if r == nil {
		return nil
	}
	return &models.SeriesRhythm{Times: r.Times, Per: strings.ToLower(strings.TrimSpace(r.Per))}
}

// SeriesOut is a series: what it promises, how one is made, its default
// rhythm, and how many posts it has produced; with the campaign's run of it
// when a campaign is asked about.
type SeriesOut struct {
	ID            string               `json:"id"`
	Name          string               `json:"name"`
	Promise       string               `json:"promise,omitempty"`
	Recipe        string               `json:"recipe,omitempty"`
	Supply        string               `json:"supply"`
	ContentFormat string               `json:"contentFormat,omitempty"`
	DefaultRhythm *models.SeriesRhythm `json:"defaultRhythm,omitempty"`
	// CampaignID is set on a series kept inside one campaign; otherwise it
	// is in the workspace library.
	CampaignID string             `json:"campaignId,omitempty"`
	Usage      models.SeriesUsage `json:"usage"`
	// Runs is set when the campaign asked about runs it; Rhythm nil is
	// occasional.
	Runs   bool                 `json:"runs,omitzero"`
	Rhythm *models.SeriesRhythm `json:"rhythm,omitempty"`
}

func seriesOut(s *models.Series) SeriesOut {
	out := SeriesOut{ID: s.ID, Name: s.Name, Promise: s.Promise, Recipe: s.Recipe, Supply: string(s.Supply),
		DefaultRhythm: s.DefaultRhythm, Usage: s.Usage}
	if s.ContentFormat != nil {
		out.ContentFormat = string(*s.ContentFormat)
	}
	if s.CampaignID != nil {
		out.CampaignID = *s.CampaignID
	}
	return out
}

// ListSeriesInput asks for the series, and a campaign's runs of them.
type ListSeriesInput struct {
	CampaignID string `json:"campaignId,omitempty" jsonschema:"description=Also say which series this campaign runs; the thread's campaign when omitted"`
}

// ListSeriesOutput is the series the workspace has.
type ListSeriesOutput struct {
	CampaignID string      `json:"campaignId,omitempty"`
	Series     []SeriesOut `json:"series"`
}

// SeriesInput is a new series.
type SeriesInput struct {
	Name          string       `json:"name"`
	Promise       string       `json:"promise,omitempty"       jsonschema:"description=What each post in it gives the reader, one line"`
	Recipe        string       `json:"recipe,omitempty"        jsonschema:"description=How one is made"`
	Supply        string       `json:"supply,omitempty"        jsonschema:"description=self (default): it brings its own subject; idea: each post takes an idea from the queue"`
	ContentFormat string       `json:"contentFormat,omitempty"`
	DefaultRhythm *RhythmInput `json:"defaultRhythm,omitempty"`
	CampaignID    string       `json:"campaignId,omitempty"    jsonschema:"description=Keep it inside this campaign instead of the workspace library"`
}

// UpdateSeriesInput edits a series; empty fields keep their value.
type UpdateSeriesInput struct {
	SeriesID      string       `json:"seriesId,omitempty" jsonschema:"description=The series; the open one when omitted"`
	Name          string       `json:"name,omitempty"`
	Promise       string       `json:"promise,omitempty"`
	Recipe        string       `json:"recipe,omitempty"`
	Supply        string       `json:"supply,omitempty"`
	ContentFormat string       `json:"contentFormat,omitempty"`
	DefaultRhythm *RhythmInput `json:"defaultRhythm,omitempty"`
	// Occasional clears the default rhythm.
	Occasional bool `json:"occasional,omitempty"`
}

// SeriesRef names a series.
type SeriesRef struct {
	SeriesID string `json:"seriesId,omitempty" jsonschema:"description=The series; the open one when omitted"`
}

// SeriesChangeOut is a series changed and the undo handle.
type SeriesChangeOut struct {
	Series SeriesOut `json:"series"`
	UndoID string    `json:"undoId,omitempty"`
}

// RunSeriesInput makes a campaign run a series, or run it at another rhythm.
type RunSeriesInput struct {
	SeriesID   string       `json:"seriesId,omitempty"   jsonschema:"description=The series; the open one when omitted"`
	CampaignID string       `json:"campaignId,omitempty" jsonschema:"description=The campaign; the thread's when omitted"`
	Rhythm     *RhythmInput `json:"rhythm,omitempty"     jsonschema:"description=How often; the series' default when omitted"`
	Occasional bool         `json:"occasional,omitempty" jsonschema:"description=Run it with no set rhythm"`
}

// StopSeriesInput makes a campaign stop running a series.
type StopSeriesInput struct {
	SeriesID   string `json:"seriesId,omitempty"   jsonschema:"description=The series; the open one when omitted"`
	CampaignID string `json:"campaignId,omitempty" jsonschema:"description=The campaign; the thread's when omitted"`
}

// RunOut is a campaign's run of a series after a change; Running false is
// stopped, and Rhythm nil occasional.
type RunOut struct {
	CampaignID string               `json:"campaignId"`
	SeriesID   string               `json:"seriesId"`
	Running    bool                 `json:"running"`
	Rhythm     *models.SeriesRhythm `json:"rhythm,omitempty"`
	UndoID     string               `json:"undoId,omitempty"`
}

func seriesErr(err error) error {
	switch {
	case series.IsValidation(err), series.IsNotFound(err):
		return assistantkit.Refused(err.Error())
	case errors.Is(err, repository.ErrSeriesChanged):
		return assistantkit.Refused("it was changed while you were editing it; read it again with listSeries")
	}
	return err
}

func campaignOf(ctx context.Context, id string) (string, error) {
	if id = cmp.Or(strings.TrimSpace(id), assistant.CampaignID(ctx)); id == "" {
		return "", assistantkit.Refused("say which campaign")
	}
	return id, nil
}

func listSeries(ctx context.Context, d Deps, in ListSeriesInput) (*ListSeriesOutput, error) {
	list, err := d.Series.List(ctx)
	if err != nil {
		return nil, err
	}
	out := &ListSeriesOutput{CampaignID: cmp.Or(strings.TrimSpace(in.CampaignID), assistant.CampaignID(ctx)), Series: []SeriesOut{}}
	runs := map[string]*models.CampaignSeriesRun{}
	if out.CampaignID != "" {
		cs, err := d.Series.CampaignSeries(ctx, out.CampaignID)
		if err != nil {
			return nil, seriesErr(err)
		}
		for i := range cs.Runs {
			runs[cs.Runs[i].SeriesID] = &cs.Runs[i]
		}
	}
	for _, s := range list {
		// A series kept inside another campaign isn't this campaign's to run.
		if out.CampaignID != "" && s.CampaignID != nil && *s.CampaignID != out.CampaignID {
			continue
		}
		o := seriesOut(&s)
		if r := runs[s.ID]; r != nil {
			o.Runs, o.Rhythm = true, r.Rhythm
		}
		out.Series = append(out.Series, o)
	}
	return out, nil
}

func formatOf(s string) *models.ContentFormat {
	if s = strings.TrimSpace(s); s == "" {
		return nil
	}
	f := models.ContentFormat(s)
	return &f
}

func createSeries(ctx context.Context, d Deps, in SeriesInput) (*SeriesChangeOut, error) {
	si := series.Input{Name: in.Name, Promise: in.Promise, Recipe: in.Recipe,
		Supply:        models.SeriesSupply(cmp.Or(strings.TrimSpace(in.Supply), string(models.SeriesSupplySelf))),
		ContentFormat: formatOf(in.ContentFormat), DefaultRhythm: in.DefaultRhythm.rhythm()}
	if id := strings.TrimSpace(in.CampaignID); id != "" {
		si.CampaignID = &id
	}
	s, err := d.Series.Create(ctx, si)
	if err != nil {
		return nil, seriesErr(err)
	}
	assistantkit.Touch(ctx, "series", s.ID)
	undo := recordUndo(ctx, d, models.AssistantActionSeries, s.ID, assistantundo.Series(nil, nil), assistantundo.Series(s, nil))
	return &SeriesChangeOut{Series: seriesOut(s), UndoID: undo}, nil
}

func updateSeries(ctx context.Context, d Deps, in UpdateSeriesInput) (*SeriesChangeOut, error) {
	id, err := pick(ctx, in.SeriesID, viewcontext.KindSeries, "series")
	if err != nil {
		return nil, err
	}
	cur, err := d.Series.Get(ctx, id)
	if err != nil {
		return nil, seriesErr(err)
	}
	si := series.Input{Name: cmp.Or(in.Name, cur.Name), Promise: cmp.Or(in.Promise, cur.Promise), Recipe: cmp.Or(in.Recipe, cur.Recipe),
		Supply: models.SeriesSupply(cmp.Or(strings.TrimSpace(in.Supply), string(cur.Supply))), ContentFormat: cur.ContentFormat,
		DefaultRhythm: cur.DefaultRhythm}
	if f := formatOf(in.ContentFormat); f != nil {
		si.ContentFormat = f
	}
	switch {
	case in.Occasional:
		si.DefaultRhythm = nil
	case in.DefaultRhythm != nil:
		si.DefaultRhythm = in.DefaultRhythm.rhythm()
	}
	after, err := d.Series.UpdateIf(ctx, cur, si)
	if err != nil {
		return nil, seriesErr(err)
	}
	assistantkit.Touch(ctx, "series", after.ID)
	undo := recordUndo(ctx, d, models.AssistantActionSeries, after.ID, assistantundo.Series(cur, nil), assistantundo.Series(after, nil))
	return &SeriesChangeOut{Series: seriesOut(after), UndoID: undo}, nil
}

func removeSeries(ctx context.Context, d Deps, in SeriesRef) (*SeriesChangeOut, error) {
	id, err := pick(ctx, in.SeriesID, viewcontext.KindSeries, "series")
	if err != nil {
		return nil, err
	}
	gone, runs, err := d.Series.Remove(ctx, id)
	if err != nil {
		return nil, seriesErr(err)
	}
	assistantkit.Touch(ctx, "series", gone.ID)
	undo := recordUndo(ctx, d, models.AssistantActionSeries, gone.ID, assistantundo.Series(gone, runs), assistantundo.Series(nil, nil))
	return &SeriesChangeOut{Series: seriesOut(gone), UndoID: undo}, nil
}

func runSeries(ctx context.Context, d Deps, in RunSeriesInput) (*RunOut, error) {
	sid, err := pick(ctx, in.SeriesID, viewcontext.KindSeries, "series")
	if err != nil {
		return nil, err
	}
	cid, err := campaignOf(ctx, in.CampaignID)
	if err != nil {
		return nil, err
	}
	want := &models.CampaignSeriesRun{Rhythm: in.Rhythm.rhythm()}
	if in.Rhythm == nil && !in.Occasional {
		s, err := d.Series.Get(ctx, sid)
		if err != nil {
			return nil, seriesErr(err)
		}
		want.Rhythm = s.DefaultRhythm
	}
	return setRun(ctx, d, cid, sid, want)
}

func stopSeries(ctx context.Context, d Deps, in StopSeriesInput) (*RunOut, error) {
	sid, err := pick(ctx, in.SeriesID, viewcontext.KindSeries, "series")
	if err != nil {
		return nil, err
	}
	cid, err := campaignOf(ctx, in.CampaignID)
	if err != nil {
		return nil, err
	}
	return setRun(ctx, d, cid, sid, nil)
}

func setRun(ctx context.Context, d Deps, campaignID, seriesID string, want *models.CampaignSeriesRun) (*RunOut, error) {
	before, err := d.Series.SetRun(ctx, campaignID, seriesID, want)
	if err != nil {
		return nil, seriesErr(err)
	}
	out := &RunOut{CampaignID: campaignID, SeriesID: seriesID, Running: want != nil}
	if want != nil {
		out.Rhythm = want.Rhythm
	}
	if (before == nil) == (want == nil) && repository.SameRun(before, want) {
		return out, nil
	}
	assistantkit.Touch(ctx, "campaign", campaignID)
	out.UndoID = recordUndo(ctx, d, models.AssistantActionCampaignSeries, assistantundo.CampaignSeriesID(campaignID, seriesID),
		assistantundo.CampaignSeries(campaignID, seriesID, before), assistantundo.CampaignSeries(campaignID, seriesID, want))
	return out, nil
}

// seriesSpecs are the series tools; nil without the series service.
func seriesSpecs(g *genkit.Genkit, d Deps) []assistantkit.Spec {
	if d.Series == nil {
		return nil
	}
	list := genkit.DefineTool(g, "listSeries",
		"Lists the workspace's series — standing instructions for a recurring kind of post — with what each promises, how one is made, its default rhythm and the posts it produced; with a campaign, which of them it runs and how often.",
		assistantkit.Wrap(recorded(foundationPackName, "listSeries", nil, func(ctx context.Context, in ListSeriesInput) (*ListSeriesOutput, error) {
			return listSeries(ctx, d, in)
		})), ai.WithStrictSchema(false))
	create := genkit.DefineTool(g, "createSeries", "Adds a series to the workspace library, or inside one campaign. Returns an undo id.",
		assistantkit.Wrap(recorded(foundationPackName, "createSeries", nil, func(ctx context.Context, in SeriesInput) (*SeriesChangeOut, error) { return createSeries(ctx, d, in) })), ai.WithStrictSchema(false))
	update := genkit.DefineTool(g, "updateSeries", "Edits a series: only the fields passed change. Returns an undo id.",
		assistantkit.Wrap(recorded(foundationPackName, "updateSeries", nil, func(ctx context.Context, in UpdateSeriesInput) (*SeriesChangeOut, error) {
			return updateSeries(ctx, d, in)
		})), ai.WithStrictSchema(false))
	remove := genkit.DefineTool(g, "removeSeries", "Removes a series; campaigns stop running it, its posts keep it. Returns an undo id.",
		assistantkit.Wrap(recorded(foundationPackName, "removeSeries", nil, func(ctx context.Context, in SeriesRef) (*SeriesChangeOut, error) { return removeSeries(ctx, d, in) })), ai.WithStrictSchema(false))
	run := genkit.DefineTool(g, "runSeries", "Makes a campaign run a series, or run it at another rhythm. Returns an undo id.",
		assistantkit.Wrap(recorded(foundationPackName, "runSeries", nil, func(ctx context.Context, in RunSeriesInput) (*RunOut, error) { return runSeries(ctx, d, in) })), ai.WithStrictSchema(false))
	stop := genkit.DefineTool(g, "stopSeries", "Makes a campaign stop running a series. Returns an undo id.",
		assistantkit.Wrap(recorded(foundationPackName, "stopSeries", nil, func(ctx context.Context, in StopSeriesInput) (*RunOut, error) { return stopSeries(ctx, d, in) })), ai.WithStrictSchema(false))
	return []assistantkit.Spec{
		spec(foundationPackName, list, assistantkit.TierRead,
			`Call this when the user asks about series — "which series do we have?", "what does this campaign run?" — or before changing one.`),
		spec(foundationPackName, create, assistantkit.TierReversible,
			`Call this when the user asks for a new recurring kind of post — "start a weekly 'Tip Tuesday' series". It goes in the workspace library unless they want it in one campaign only. It can be undone.`),
		spec(foundationPackName, update, assistantkit.TierReversible,
			`Call this to change a series itself — its name, promise, recipe, format or default rhythm. To change how often one campaign runs it, use runSeries. It can be undone.`),
		spec(foundationPackName, remove, assistantkit.TierReversible,
			`Call this when the user asks to delete a series. It can be undone.`),
		spec(foundationPackName, run, assistantkit.TierReversible,
			`Call this when the user wants a campaign to run a series — "add Tip Tuesday to this campaign", "run it twice a month here". It can be undone.`),
		spec(foundationPackName, stop, assistantkit.TierReversible,
			`Call this when the user wants a campaign to stop running a series. It can be undone.`),
	}
}
