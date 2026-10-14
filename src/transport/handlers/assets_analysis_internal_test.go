package handlers

import (
	"context"
	"errors"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// digestRepo serves LensDigests from a map and counts the calls; any
// other method panics on the nil embed.
type digestRepo struct {
	repository.AssetAnalysisRepository
	digests map[string]repository.LensDigest
	err     error
	calls   int
}

func (r *digestRepo) LensDigests(_ context.Context, _ []string, _ *string) (map[string]repository.LensDigest, error) {
	r.calls++
	return r.digests, r.err
}

func TestDecorateAnalysis(t *testing.T) {
	repo := &digestRepo{digests: map[string]repository.LensDigest{
		"gist":     {Status: models.AssetAnalysisStatusReady, Gist: "Reference architectures for RAG."},
		"fallback": {Status: models.AssetAnalysisStatusReady, SummaryHead: "## Overview\n\nThe call agreed Q3 pricing. More follows."},
		"bullets":  {Status: models.AssetAnalysisStatusReady, SummaryHead: "- one\n- two"},
		"running":  {Status: models.AssetAnalysisStatusRunning, Gist: "Not shown yet."},
		"failed":   {Status: models.AssetAnalysisStatusFailed, SummaryHead: "Old prose. More."},
	}}
	h := &AssetsHandler{analyses: repo}
	assets := []models.Asset{{ID: "gist"}, {ID: "fallback"}, {ID: "bullets"}, {ID: "running"}, {ID: "failed"}, {ID: "image"}}

	h.decorateAnalysis(t.Context(), assets)

	if repo.calls != 1 {
		t.Fatalf("LensDigests called %d times for %d assets, want 1", repo.calls, len(assets))
	}
	want := map[string]struct {
		status models.AssetAnalysisStatus
		gist   string
	}{
		"gist":     {models.AssetAnalysisStatusReady, "Reference architectures for RAG."},
		"fallback": {models.AssetAnalysisStatusReady, "The call agreed Q3 pricing."},
		"bullets":  {models.AssetAnalysisStatusReady, ""},
		"running":  {models.AssetAnalysisStatusRunning, ""},
		"failed":   {models.AssetAnalysisStatusFailed, ""},
		"image":    {"", ""},
	}
	for _, a := range assets {
		w := want[a.ID]
		var status models.AssetAnalysisStatus
		if a.AnalysisStatus != nil {
			status = *a.AnalysisStatus
		}
		var gist string
		if a.Gist != nil {
			gist = *a.Gist
			if gist == "" {
				t.Errorf("%s: gist is an empty string, want null", a.ID)
			}
		}
		if status != w.status || gist != w.gist {
			t.Errorf("%s: status %q gist %q, want %q and %q", a.ID, status, gist, w.status, w.gist)
		}
	}
}

// TestDecorateAnalysis_ReadFailure: a failed read leaves status and gist null
// rather than failing the list.
func TestDecorateAnalysis_ReadFailure(t *testing.T) {
	h := &AssetsHandler{analyses: &digestRepo{err: errors.New("db down")}}
	assets := []models.Asset{{ID: "a1"}}
	h.decorateAnalysis(t.Context(), assets)
	if assets[0].AnalysisStatus != nil || assets[0].Gist != nil {
		t.Fatalf("status %v gist %v, want both nil", assets[0].AnalysisStatus, assets[0].Gist)
	}
}
