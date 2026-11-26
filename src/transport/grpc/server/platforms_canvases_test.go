package server

import (
	"maps"
	"slices"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	platformsv1 "github.com/ogen-app/ogen/gen/platforms/v1"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/pgtest"
)

const instagram = "rzgpTkARLH0L"

func TestValidatePlatformWriteCanvases(t *testing.T) {
	limits := models.DefaultPlatformGlobalLimits()
	cases := []struct {
		name   string
		slug   string
		canvas *platformsv1.Canvas
		want   codes.Code
	}{
		{name: "image post", slug: "image-post", canvas: &platformsv1.Canvas{Width: 1080, Height: 1350}, want: codes.OK},
		{name: "story at the ceiling", slug: "story", canvas: &platformsv1.Canvas{Width: 8192, Height: 8192}, want: codes.OK},
		{name: "zero width", slug: "reel", canvas: &platformsv1.Canvas{Width: 0, Height: 1920}, want: codes.InvalidArgument},
		{name: "negative height", slug: "reel", canvas: &platformsv1.Canvas{Width: 1080, Height: -1}, want: codes.InvalidArgument},
		{name: "above the ceiling", slug: "image-post", canvas: &platformsv1.Canvas{Width: 8193, Height: 1080}, want: codes.InvalidArgument},
		{name: "nil canvas", slug: "image-post", canvas: nil, want: codes.InvalidArgument},
		{name: "text-only type", slug: "text-post", canvas: &platformsv1.Canvas{Width: 1080, Height: 1080}, want: codes.InvalidArgument},
		{name: "long-form type takes no artwork", slug: "long-form-post", canvas: &platformsv1.Canvas{Width: 1080, Height: 1080}, want: codes.InvalidArgument},
		{name: "whitelist-only type", slug: "live-video", canvas: &platformsv1.Canvas{Width: 1080, Height: 1080}, want: codes.InvalidArgument},
		{name: "not a post type of the platform", slug: "carousel", canvas: &platformsv1.Canvas{Width: 1080, Height: 1080}, want: codes.InvalidArgument},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			postTypes := map[string]string{
				"image-post": "Photo", "story": "Story", "reel": "Reel",
				"text-post": "Text", "long-form-post": "Long form", "live-video": "Live",
			}
			pb := &platformsv1.Platform{
				Name:             "Instagram",
				ZernioId:         "instagram",
				PostTypes:        postTypes,
				PostTypeCanvases: &platformsv1.PostTypeCanvases{BySlug: map[string]*platformsv1.Canvas{tc.slug: tc.canvas}},
			}
			if got := status.Code(validatePlatformWrite(pb, limits)); got != tc.want {
				t.Fatalf("code = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPostTypeCanvasesProtoRoundTrip(t *testing.T) {
	in := models.PostTypeCanvases{"story": {Width: 1080, Height: 1920}, "image-post": {Width: 1080, Height: 1350}}
	if out := fromCanvasesProto(toCanvasesProto(in)); !maps.Equal(out, in) {
		t.Fatalf("round trip = %v, want %v", out, in)
	}
	if out := fromCanvasesProto(nil); out == nil || len(out) != 0 {
		t.Fatalf("fromCanvasesProto(nil) = %#v, want an empty map", out)
	}
}

func TestUpdatePlatformCanvases(t *testing.T) {
	db := pgtest.MustDB()
	t.Cleanup(func() { _ = db.Close() })
	repo := repository.NewPlatformRepository(db)
	svc := newPlatformAdminService(repo, repository.NewPlatformGlobalLimitsRepository(db))

	seeded, err := repo.GetByID(t.Context(), instagram)
	if err != nil {
		t.Fatalf("get platform: %v", err)
	}
	if len(seeded.PostTypeCanvases) == 0 {
		t.Fatal("seeded platform has no canvases")
	}

	// update sends pb, then returns the stored canvases.
	update := func(t *testing.T, pb *platformsv1.Platform) models.PostTypeCanvases {
		t.Helper()
		if _, err := svc.UpdatePlatform(t.Context(), &platformsv1.UpdatePlatformRequest{Platform: pb}); err != nil {
			t.Fatalf("UpdatePlatform: %v", err)
		}
		after, err := repo.GetByID(t.Context(), instagram)
		if err != nil {
			t.Fatalf("get platform: %v", err)
		}
		return after.PostTypeCanvases
	}
	current := func(t *testing.T) *platformsv1.Platform {
		t.Helper()
		p, err := repo.GetByID(t.Context(), instagram)
		if err != nil {
			t.Fatalf("get platform: %v", err)
		}
		return toPlatformProto(p, nil)
	}

	// Runs in order: each step starts from what the previous one stored.
	t.Run("unset keeps the stored canvases", func(t *testing.T) {
		pb := current(t)
		pb.PostTypeCanvases = nil
		pb.Cadence = "daily"
		if got := update(t, pb); !maps.Equal(got, seeded.PostTypeCanvases) {
			t.Fatalf("canvases = %v, want %v", got, seeded.PostTypeCanvases)
		}
	})

	t.Run("unset drops a removed post type's canvas", func(t *testing.T) {
		pb := current(t)
		pb.PostTypeCanvases = nil
		delete(pb.PostTypes, "story")
		pb.SupportedPostTypes = slices.DeleteFunc(pb.SupportedPostTypes, func(s string) bool { return s == "story" })
		got := update(t, pb)
		if _, ok := got["story"]; ok {
			t.Fatalf("canvases = %v, want no story", got)
		}
		if got["reel"] != seeded.PostTypeCanvases["reel"] {
			t.Fatalf("reel canvas = %v, want %v", got["reel"], seeded.PostTypeCanvases["reel"])
		}
	})

	t.Run("set replaces them", func(t *testing.T) {
		pb := current(t)
		pb.PostTypeCanvases = &platformsv1.PostTypeCanvases{BySlug: map[string]*platformsv1.Canvas{
			"image-post": {Width: 1080, Height: 1080},
		}}
		want := models.PostTypeCanvases{"image-post": {Width: 1080, Height: 1080}}
		if got := update(t, pb); !maps.Equal(got, want) {
			t.Fatalf("canvases = %v, want %v", got, want)
		}
	})

	t.Run("set empty clears them", func(t *testing.T) {
		pb := current(t)
		pb.PostTypeCanvases = &platformsv1.PostTypeCanvases{}
		if got := update(t, pb); len(got) != 0 {
			t.Fatalf("canvases = %v, want none", got)
		}
	})
}
