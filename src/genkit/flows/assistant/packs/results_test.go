package packs

import (
	"context"
	"errors"
	"maps"
	"slices"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/usecase/brandfacts"
)

func TestResults_LastOutputByTool(t *testing.T) {
	ctx, _, _ := beginResults("foundation")(t.Context(), assistant.Turn{})
	end := endResults("foundation")
	if res, _ := end(ctx); res != nil {
		t.Fatalf("nothing ran: %+v", res)
	}

	run := recorded("foundation", "runSeries", nil, func(_ context.Context, in RunSeriesInput) (*RunOut, error) {
		switch in.SeriesID {
		case "refused":
			return nil, assistantkit.Refused("no")
		case "broken":
			return nil, errors.New("db down")
		}
		return &RunOut{SeriesID: in.SeriesID, Running: true}, nil
	})
	for _, id := range []string{"s1", "s2", "refused", "broken"} {
		_, _ = run(ctx, RunSeriesInput{SeriesID: id})
	}
	get := recorded("foundation", "getBrand", brandResult, func(context.Context, struct{}) (*BrandOut, error) {
		return &BrandOut{Voices: []BrandVoiceOut{{ID: "v1"}}, ExpiredFacts: []models.BrandFact{{ID: "f1"}}}, nil
	})
	if _, err := get(ctx, struct{}{}); err != nil {
		t.Fatal(err)
	}
	// Another pack's collector isn't this one's.
	_, _ = recorded("repair", "checkBrand", nil, func(context.Context, CheckBrandInput) (*CheckBrandOutput, error) {
		return &CheckBrandOutput{}, nil
	})(ctx, CheckBrandInput{})

	res, err := end(ctx)
	if err != nil || res == nil {
		t.Fatalf("result = %+v, %v", res, err)
	}
	got := res.Value.(map[string]any)
	if keys := slices.Sorted(maps.Keys(got)); !slices.Equal(keys, []string{"getBrand", "runSeries"}) {
		t.Fatalf("keys = %v", keys)
	}
	if r := got["runSeries"].(*RunOut); r.SeriesID != "s2" {
		t.Fatalf("runSeries = %+v; want the last that ran", r)
	}
	if b, ok := got["getBrand"].(BrandResult); !ok || len(b.ExpiredFacts) != 1 || len(b.Voices) != 1 {
		t.Fatalf("getBrand = %#v; want the client result", got["getBrand"])
	}
}

// editBrand reports under its own name only: the single-item tools it runs
// aren't recorded.
func TestResults_EditBrandIsOneResult(t *testing.T) {
	brand := &memBrand{facts: map[string]*models.BrandFact{}}
	d := Deps{Brand: brand}
	facts := brandfacts.New(brand)
	ctx, _ := turn(t, workspace, nil)
	ctx, _, _ = beginResults("foundation")(ctx, assistant.Turn{})
	edit := recorded("foundation", "editBrand", nil, func(ctx context.Context, in EditBrandInput) (*EditBrandOutput, error) {
		return editBrand(ctx, d, facts, nil, in)
	})
	if _, err := edit(ctx, EditBrandInput{AddFacts: []FactInput{{Statement: "Founded in 2019"}}}); err != nil {
		t.Fatal(err)
	}
	res, _ := endResults("foundation")(ctx)
	if keys := slices.Collect(maps.Keys(res.Value.(map[string]any))); !slices.Equal(keys, []string{"editBrand"}) {
		t.Fatalf("keys = %v", keys)
	}
}
