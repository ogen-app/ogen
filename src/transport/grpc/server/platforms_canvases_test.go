package server

import (
	"testing"

	platformsv1 "github.com/ogen-app/ogen/gen/platforms/v1"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/pgtest"
)

// An operator edit through the admin contract, which has no canvas field yet,
// must keep the platform's stored canvases.
func TestUpdatePlatformKeepsCanvases(t *testing.T) {
	db := pgtest.MustDB()
	t.Cleanup(func() { _ = db.Close() })
	repo := repository.NewPlatformRepository(db)
	svc := newPlatformAdminService(repo, repository.NewPlatformGlobalLimitsRepository(db))

	const instagram = "rzgpTkARLH0L"
	before, err := repo.GetByID(t.Context(), instagram)
	if err != nil {
		t.Fatalf("get platform: %v", err)
	}
	if len(before.PostTypeCanvases) == 0 {
		t.Fatal("seeded platform has no canvases")
	}

	pb := toPlatformProto(before, nil)
	pb.Cadence = "daily"
	if _, err := svc.UpdatePlatform(t.Context(), &platformsv1.UpdatePlatformRequest{Platform: pb}); err != nil {
		t.Fatalf("UpdatePlatform: %v", err)
	}

	after, err := repo.GetByID(t.Context(), instagram)
	if err != nil {
		t.Fatalf("get platform: %v", err)
	}
	if after.Cadence != "daily" {
		t.Fatalf("cadence = %q, want the edit applied", after.Cadence)
	}
	if len(after.PostTypeCanvases) != len(before.PostTypeCanvases) {
		t.Fatalf("canvases = %v, want %v", after.PostTypeCanvases, before.PostTypeCanvases)
	}
	for slug, c := range before.PostTypeCanvases {
		if after.PostTypeCanvases[slug] != c {
			t.Errorf("%s canvas = %v, want %v", slug, after.PostTypeCanvases[slug], c)
		}
	}
}
