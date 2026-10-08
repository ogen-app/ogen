package database

import (
	"fmt"
	"slices"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/platforms"
)

// TestPlatformCanvasSeed checks the CON-351 seed over every seeded platform:
// each media-bearing post type gets a canvas, text-only types get none, and a
// video type's canvas is one the platform's own video rules accept.
func TestPlatformCanvasSeed(t *testing.T) {
	ctx := t.Context()
	db, m := throwawayDB(t, "con351")
	if _, err := m.Migrate(ctx); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	var rows []models.Platform
	if err := db.NewSelect().Model(&rows).Scan(ctx); err != nil {
		t.Fatalf("list platforms: %v", err)
	}
	if len(rows) != 9 {
		t.Fatalf("seeded platforms = %d, want 9", len(rows))
	}

	for i := range rows {
		p := &rows[i]
		for _, v := range platforms.ResolvePostTypeRules(p) {
			media := v.Rule != nil && (slices.Contains(v.Rule.AllowedKinds, platforms.KindImage) ||
				slices.Contains(v.Rule.AllowedKinds, platforms.KindVideo))
			switch {
			case media && v.Canvas == nil:
				t.Errorf("%s %s: no canvas", p.Name, v.Slug)
			case !media && v.Canvas != nil:
				t.Errorf("%s %s: canvas %v on a type without artwork", p.Name, v.Slug, *v.Canvas)
			case media && slices.Equal(v.Rule.AllowedKinds, []string{platforms.KindVideo}):
				checkVideoCanvas(t, p, v.Slug, *v.Canvas)
			}
		}
	}
}

func checkVideoCanvas(t *testing.T, p *models.Platform, slug string, c models.Canvas) {
	t.Helper()
	vc := p.VideoConstraints
	if vc.MaxWidth > 0 && c.Width > vc.MaxWidth || vc.MaxHeight > 0 && c.Height > vc.MaxHeight {
		t.Errorf("%s %s: canvas %d×%d exceeds the %d×%d video cap", p.Name, slug, c.Width, c.Height, vc.MaxWidth, vc.MaxHeight)
	}
	if len(vc.AllowedAspectRatios) == 0 {
		return
	}
	g := gcd(c.Width, c.Height)
	ratio := fmt.Sprintf("%d:%d", c.Width/g, c.Height/g)
	if !slices.Contains(vc.AllowedAspectRatios, ratio) {
		t.Errorf("%s %s: canvas ratio %s not in %v", p.Name, slug, ratio, vc.AllowedAspectRatios)
	}
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}
