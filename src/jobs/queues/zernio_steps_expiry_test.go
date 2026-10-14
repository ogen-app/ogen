package queues

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
)

// TestExpiringConnectionSteps checks the remote sweep: it fans out one job per
// tenant, and each tenant job asks for that profile's account health, then
// records it and looks up the owner's notification exactly as the in-process
// sweep does.
func TestExpiringConnectionSteps(t *testing.T) {
	accounts := &healthAccountRepo{
		tenantID:  "t1",
		profileID: "p1",
		accounts:  map[string]*models.SocialAccount{"acc1": {ID: "acc1", Platform: "youtube"}},
	}
	logs := &seenKeyLog{}
	steps := NewZernioSteps(Deps{
		Zernio: ZernioDeps{SocialAccountRepo: accounts},
		Users:  ownerRepo{owner: models.User{ID: "owner1"}},
		Email:  EmailDeps{Logs: logs},
	})
	ctx := context.Background()

	sweep, err := steps.Step(ctx, ZernioStepInput{Kind: DetectExpiringConnectionsQueue, Args: []byte(`{}`)})
	if err != nil || sweep.Done == nil || len(sweep.Done.Enqueue) != 1 {
		t.Fatalf("sweep = %+v, %v; want one child", sweep, err)
	}
	child := sweep.Done.Enqueue[0]
	if child.Kind != ExpiringConnectionsTenantKind {
		t.Fatalf("child kind = %q", child.Kind)
	}

	first, err := steps.Step(ctx, ZernioStepInput{Kind: child.Kind, Args: child.Args})
	if err != nil || first.Call == nil || first.Call.Path != "/accounts/health" || first.Call.Query.Get("profileId") != "p1" {
		t.Fatalf("first step = %+v, %v; want GET /accounts/health for p1", first.Call, err)
	}

	expiry := time.Now().Add(30 * time.Hour)
	body, _ := json.Marshal(map[string]any{"accounts": []zernio.AccountHealth{{
		AccountID: "acc1", ProfileID: "p1", Platform: "youtube", Status: "warning", TokenValid: true, TokenExpiresAt: &expiry,
	}}})
	last, err := steps.Step(ctx, ZernioStepInput{Kind: child.Kind, Args: child.Args, State: first.State, Result: &zernio.Result{Status: 200, Body: body}})
	if err != nil || last.Done == nil {
		t.Fatalf("last step = %+v, %v", last, err)
	}
	if accounts.accounts["acc1"].HealthStatus != "warning" || accounts.accounts["acc1"].UnhealthySince == nil {
		t.Errorf("health not recorded: %+v", accounts.accounts["acc1"])
	}
	if len(logs.keys) != 1 {
		t.Errorf("owner dedupe lookups = %d, want 1", len(logs.keys))
	}
}
