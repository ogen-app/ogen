package repository_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/runslot"
)

func TestRunSlotsClaimHeartbeatFinish(t *testing.T) {
	f := newAnalysisFixture(t)
	distills := repository.NewIdeaDistillRepository(f.db)
	slots := repository.NewIdeaDistillRunSlots(f.db)
	f.seedAsset(t, "asset-1")
	lens, err := f.analyses.Ensure(f.ctx, f.db, "asset-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	run := &models.IdeaDistillRun{AnalysisID: lens.ID, AssetID: "asset-1", Count: 3}
	if err := distills.Create(f.ctx, run); err != nil {
		t.Fatal(err)
	}

	stale := time.Now().Add(-time.Minute)
	if ok, err := slots.Claim(f.ctx, run.ID, "tok-1", stale); err != nil || !ok {
		t.Fatalf("first claim = %v, %v", ok, err)
	}
	if ok, err := slots.Claim(f.ctx, run.ID, "tok-2", stale); err != nil || ok {
		t.Fatalf("claim over a live run = %v, %v, want refused", ok, err)
	}
	slot, err := runslot.Read(f.ctx, slots, run.ID)
	if err != nil || slot.Token != "tok-1" || !slot.Running || slot.Done {
		t.Fatalf("slot = %+v, %v, want tok-1 running", slot, err)
	}

	// Another tenant sees no row and can't touch the slot.
	other := tenantctx.With(context.Background(), "someone-else")
	if _, err := slots.Read(other, run.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-tenant read: %v, want ErrNoRows", err)
	}
	if ok, err := slots.Claim(other, run.ID, "tok-x", time.Now().Add(time.Hour)); err != nil || ok {
		t.Fatalf("cross-tenant claim = %v, %v", ok, err)
	}

	if held, err := slots.Heartbeat(f.ctx, run.ID, "tok-1"); err != nil || !held {
		t.Fatalf("holder's heartbeat = %v, %v; want held", held, err)
	}
	if held, err := slots.Heartbeat(f.ctx, run.ID, "tok-2"); err != nil || held {
		t.Fatalf("a non-holder's heartbeat = %v, %v; want not held", held, err)
	}
	if err := slots.Finish(f.ctx, run.ID, "tok-2", "late", ""); err != nil {
		t.Fatal(err)
	}
	if slot, _ := runslot.Read(f.ctx, slots, run.ID); slot.Done {
		t.Fatal("a stale token finished the slot")
	}
	if err := slots.Finish(f.ctx, run.ID, "tok-1", "model down", "model_unavailable"); err != nil {
		t.Fatal(err)
	}
	slot, err = runslot.Read(f.ctx, slots, run.ID)
	if err != nil || !slot.Done || slot.Err == nil || slot.Err.Error() != "model down" || slot.Code != "model_unavailable" {
		t.Fatalf("finished slot = %+v, %v", slot, err)
	}

	// A finished slot can be claimed again (a retry).
	if ok, err := slots.Claim(f.ctx, run.ID, "tok-3", stale); err != nil || !ok {
		t.Fatalf("claim after finish = %v, %v", ok, err)
	}
}
