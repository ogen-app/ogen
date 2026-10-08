package platforms

import (
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
)

func TestResolvePostTypeRules_Canvas(t *testing.T) {
	p := &models.Platform{
		ID: "canvasplat", Name: "CanvasPlat",
		PostTypes: models.PostTypeMap{
			"image-post": "Image post",
			"story":      "Story",
			"reel":       "Reel",
			"text-post":  "Text post",
			"live-video": "Live video",
		},
		PostTypeCanvases: models.PostTypeCanvases{
			"image-post": {Width: 1080, Height: 1350},
			"story":      {Width: 1080, Height: 1920},
			"text-post":  {Width: 1200, Height: 630},  // ignored: text-only type
			"live-video": {Width: 1920, Height: 1080}, // ignored: whitelist-only
		},
	}
	byslug := map[string]PostTypeRuleView{}
	for _, v := range ResolvePostTypeRules(p) {
		byslug[v.Slug] = v
	}

	if c := byslug["image-post"].Canvas; c == nil || *c != (models.Canvas{Width: 1080, Height: 1350}) {
		t.Errorf("image-post canvas = %v, want 1080×1350", c)
	}
	if c := byslug["story"].Canvas; c == nil || *c != (models.Canvas{Width: 1080, Height: 1920}) {
		t.Errorf("story canvas = %v, want 1080×1920", c)
	}
	for _, slug := range []string{"reel", "text-post", "live-video"} {
		if c := byslug[slug].Canvas; c != nil {
			t.Errorf("%s canvas = %v, want nil", slug, c)
		}
	}
}

func TestResolvePostTypeRules_CanvasIgnoresNonPositive(t *testing.T) {
	p := &models.Platform{
		PostTypes:        models.PostTypeMap{"image-post": "Image post"},
		PostTypeCanvases: models.PostTypeCanvases{"image-post": {Width: 1080}},
	}
	if c := ResolvePostTypeRules(p)[0].Canvas; c != nil {
		t.Errorf("canvas = %v, want nil for a zero height", c)
	}
}
