package billing

import (
	"errors"
	"testing"
	"time"

	billingdom "github.com/ogen-app/ogen/src/domain/billing"
	"github.com/ogen-app/ogen/src/domain/models"
)

func planReq(tenantID, userID, accountID, tier string) PlanRequest {
	return PlanRequest{Buyer: Buyer{TenantID: tenantID, UserID: userID, AccountID: accountID, Email: "x@example.com"}, TierID: tier}
}

func TestComplimentaryWorkspaceSwitchesTierWithoutCheckout(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	tenantID, userID, accountID := h.workspace("c")
	if err := h.svc.SetComplimentary(ctx, tenantID, true, "alpha tester"); err != nil {
		t.Fatal(err)
	}
	if g, _ := h.svc.Gated(ctx, tenantID); g {
		t.Fatal("complimentary workspace is gated")
	}
	st, err := h.svc.PlanState(ctx, tenantID, accountID)
	if err != nil || !st.Complimentary || st.BillingRequired {
		t.Fatalf("plan state = %+v %v", st, err)
	}

	res, err := h.svc.ChangePlan(ctx, planReq(tenantID, userID, accountID, "pro"))
	if err != nil || res.Checkout != nil {
		t.Fatalf("change to pro = %+v %v", res, err)
	}
	if len(h.provider.checkouts) != 0 || len(h.provider.changes) != 0 {
		t.Fatal("a complimentary switch called the provider")
	}
	if got := h.currentVersion(tenantID); got != h.proPrice.TierVersionID+"/"+models.AssignmentReasonComp {
		t.Fatalf("assignment = %s", got)
	}
	// Choosing the current plan again changes nothing; another plan switches.
	if _, err := h.svc.ChangePlan(ctx, planReq(tenantID, userID, accountID, "pro")); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(time.Minute)
	if _, err := h.svc.ChangePlan(ctx, planReq(tenantID, userID, accountID, h.maxPrice.TierVersionID)); err != nil {
		t.Fatal(err)
	}
	if got := h.currentVersion(tenantID); got != h.maxPrice.TierVersionID+"/"+models.AssignmentReasonComp {
		t.Fatalf("assignment after max = %s", got)
	}
	// Hidden (non-purchasable) tiers stay operator-only.
	if _, err := h.svc.ChangePlan(ctx, planReq(tenantID, userID, accountID, models.DefaultTierID)); !errors.Is(err, ErrTierNotAvailable) {
		t.Fatalf("hidden tier err = %v", err)
	}
	if _, err := h.svc.ChangePlan(ctx, planReq(tenantID, userID, accountID, "nope")); !errors.Is(err, ErrUnknownTier) {
		t.Fatalf("unknown tier err = %v", err)
	}
	if status, _ := h.svc.BillingStatus(ctx, &models.Tenant{ID: tenantID, Complimentary: true}); status != BillingStatusComplimentary {
		t.Fatalf("billing status = %q", status)
	}
}

func TestGrandfatheredWorkspaceStillChecksOut(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	tenantID, userID, accountID := h.workspace("g")
	if err := h.svc.SetBillingRequired(ctx, tenantID, false, "grandfathered"); err != nil {
		t.Fatal(err)
	}
	res, err := h.svc.ChangePlan(ctx, planReq(tenantID, userID, accountID, "pro"))
	if err != nil || res.Checkout == nil {
		t.Fatalf("grandfathered change = %+v %v", res, err)
	}
	if st, _ := h.svc.PlanState(ctx, tenantID, accountID); st.Complimentary {
		t.Fatal("grandfathered workspace reported complimentary")
	}
}

func TestComplimentarySwitchWorksWithBillingOff(t *testing.T) {
	h := newHarness(t, func(s *billingdom.Settings) { s.Provider = billingdom.ProviderNone })
	ctx := t.Context()
	tenantID, userID, accountID := h.workspace("off")
	if err := h.svc.SetComplimentary(ctx, tenantID, true, "beta"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.ChangePlan(ctx, planReq(tenantID, userID, accountID, "pro")); err != nil {
		t.Fatalf("comp switch with billing off: %v", err)
	}
	st, err := h.svc.PlanState(ctx, tenantID, accountID)
	if err != nil || !st.Complimentary {
		t.Fatalf("plan state with billing off = %+v %v", st, err)
	}
	other, otherUser, otherAccount := h.workspace("paying")
	if _, err := h.svc.ChangePlan(ctx, planReq(other, otherUser, otherAccount, "pro")); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("non-comp change with billing off err = %v", err)
	}
}

func TestEndingComplimentaryAccessGatesAndKeepsFreeMonth(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	tenantID, _, accountID := h.workspace("end")
	if err := h.svc.SetComplimentary(ctx, tenantID, true, "beta"); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.SetBillingRequired(ctx, tenantID, true, "gate it"); !errors.Is(err, ErrComplimentary) {
		t.Fatalf("SetBillingRequired on comp err = %v", err)
	}
	if err := h.svc.SetComplimentary(ctx, tenantID, false, ""); !errors.Is(err, ErrReasonRequired) {
		t.Fatalf("missing reason err = %v", err)
	}
	if err := h.svc.SetComplimentary(ctx, tenantID, false, "beta wave 1 ended"); err != nil {
		t.Fatal(err)
	}
	if g, _ := h.svc.Gated(ctx, tenantID); !g {
		t.Fatal("ending comp did not gate the workspace")
	}
	tb, err := h.svc.TenantBilling(ctx, tenantID)
	if err != nil || tb.Complimentary || !tb.BillingRequired {
		t.Fatalf("tenant billing = %+v %v", tb, err)
	}
	if eligible, _ := h.svc.TrialEligible(ctx, accountID); !eligible {
		t.Fatal("ex-tester lost the free month")
	}
	if err := h.svc.SetComplimentary(ctx, "missing", true, "x"); !errors.Is(err, ErrTenantNotFound) {
		t.Fatalf("unknown tenant err = %v", err)
	}
}
