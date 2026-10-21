package server

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	billingv1 "github.com/ogen-app/ogen/gen/billing/v1"
	billingdom "github.com/ogen-app/ogen/src/domain/billing"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/usecase/billing"
)

type fakeBillingAdmin struct {
	setRequired []any
	invalidErr  error
	offerErr    error
}

func (f *fakeBillingAdmin) Enabled() bool { return true }
func (f *fakeBillingAdmin) Settings() billingdom.Settings {
	return billingdom.Settings{Provider: "lemonsqueezy", TestMode: true, StoreID: "7", TrialTeardownLive: true}
}
func (f *fakeBillingAdmin) Configured(context.Context) bool { return true }
func (f *fakeBillingAdmin) TenantBilling(_ context.Context, tenantID string) (billing.TenantBilling, error) {
	if tenantID == "missing" {
		return billing.TenantBilling{}, billing.ErrTenantNotFound
	}
	price := "p1"
	now := time.Now()
	return billing.TenantBilling{
		TenantID: tenantID, BillingRequired: true,
		Subscriptions:    []models.BillingSubscription{{ID: "bs1", Provider: "lemonsqueezy", ProviderSubscriptionID: "9", Status: "past_due", TierVersionPriceID: &price, FirstPaidAt: &now, CreatedAt: now}},
		Duplicates:       map[string]bool{},
		Prices:           map[string]billingdom.Price{"p1": {ID: "p1", TierID: "pro", TierVersionID: "ttv-pro-2"}},
		ScheduledChanges: []models.BillingScheduledChange{{ID: "sc1", BillingSubscriptionID: "bs1", Status: "pending", EffectiveAt: now}},
		WebhookEvents:    []models.BillingWebhookEvent{{ID: "we1", EventName: "subscription_updated", Outcome: "applied", ReceivedAt: now}},
		Owners:           []billing.OwnerTrial{{UserID: "u1", Email: "o@x", TrialEligible: true}},
	}, nil
}
func (f *fakeBillingAdmin) Attention(context.Context, int, string) ([]billing.AttentionItem, string, error) {
	return []billing.AttentionItem{{TenantID: "t1", TenantName: "Acme", Reason: billing.AttentionExpiredNeverPaid, TeardownStatus: "dry_run",
		Subscription: &models.BillingSubscription{ID: "bs1"}, Since: time.Now()}}, "50", nil
}
func (f *fakeBillingAdmin) SetBillingRequired(_ context.Context, tenantID string, required bool, reason string) error {
	if reason == "" {
		return billing.ErrReasonRequired
	}
	f.setRequired = []any{tenantID, required, reason}
	return nil
}
func (f *fakeBillingAdmin) ResyncTenant(context.Context, string) (int, error) { return 2, nil }
func (f *fakeBillingAdmin) FreeTrialOffer(context.Context) (billing.FreeTrialOffer, error) {
	return billing.FreeTrialOffer{TierID: "pro", TierName: "Pro", TrialDays: 30, EligibleTierIDs: []string{"pro"}}, nil
}
func (f *fakeBillingAdmin) SetFreeTrialTier(context.Context, string, string) (billing.FreeTrialOffer, error) {
	if f.offerErr != nil {
		return billing.FreeTrialOffer{}, f.offerErr
	}
	return billing.FreeTrialOffer{TierID: "max", TrialDays: 30}, nil
}
func (f *fakeBillingAdmin) InvalidateTestSubscriptions(_ context.Context, dryRun bool) ([]string, error) {
	return []string{"t1", "t2"}, f.invalidErr
}
func (f *fakeBillingAdmin) BillingStatus(context.Context, *models.Tenant) (string, error) {
	return "on_trial", nil
}

func TestBillingAdminRoundTrip(t *testing.T) {
	const token = "billing-token"
	fake := &fakeBillingAdmin{}
	srv, err := New(Deps{Token: token, Secrets: newFakeStore(), Billing: fake})
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			return invoker(metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token), method, req, reply, cc, opts...)
		}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	cli := billingv1.NewBillingAdminServiceClient(conn)
	ctx := t.Context()

	st, err := cli.GetBillingSettings(ctx, &billingv1.GetBillingSettingsRequest{})
	if err != nil || st.GetProvider() != "lemonsqueezy" || !st.GetTestMode() || !st.GetConfigured() || st.GetStoreId() != "7" || !st.GetTrialTeardownLive() {
		t.Fatalf("settings = %v %v", st, err)
	}

	tb, err := cli.GetTenantBilling(ctx, &billingv1.GetTenantBillingRequest{TenantId: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	b := tb.GetBilling()
	if !b.GetBillingRequired() || len(b.GetSubscriptions()) != 1 || b.GetSubscriptions()[0].GetTierId() != "pro" || b.GetSubscriptions()[0].GetFirstPaidAt() == nil ||
		len(b.GetScheduledChanges()) != 1 || len(b.GetRecentWebhookEvents()) != 1 || !b.GetOwners()[0].GetTrialEligible() {
		t.Fatalf("tenant billing = %v", b)
	}
	if _, err := cli.GetTenantBilling(ctx, &billingv1.GetTenantBillingRequest{TenantId: "missing"}); status.Code(err) != codes.NotFound {
		t.Fatalf("missing tenant: %v", err)
	}
	if _, err := cli.GetTenantBilling(ctx, &billingv1.GetTenantBillingRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty tenant: %v", err)
	}

	att, err := cli.ListBillingAttention(ctx, &billingv1.ListBillingAttentionRequest{PageSize: 50})
	if err != nil || len(att.GetItems()) != 1 || att.GetItems()[0].GetReason() != "expired_never_paid" || att.GetItems()[0].GetTeardownStatus() != "dry_run" || att.GetNextPageToken() != "50" {
		t.Fatalf("attention = %v %v", att, err)
	}

	if _, err := cli.SetBillingRequired(ctx, &billingv1.SetBillingRequiredRequest{TenantId: "t1"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("no reason: %v", err)
	}
	if _, err := cli.SetBillingRequired(ctx, &billingv1.SetBillingRequiredRequest{TenantId: "t1", Required: false, Reason: "partner"}); err != nil || fake.setRequired[2] != "partner" {
		t.Fatalf("set required: %v %v", err, fake.setRequired)
	}
	rs, err := cli.ResyncSubscription(ctx, &billingv1.ResyncSubscriptionRequest{TenantId: "t1"})
	if err != nil || rs.GetCorrected() != 2 {
		t.Fatalf("resync = %v %v", rs, err)
	}

	offer, err := cli.GetFreeTrialTier(ctx, &billingv1.GetFreeTrialTierRequest{})
	if err != nil || offer.GetOffer().GetTierId() != "pro" || offer.GetOffer().GetTrialDays() != 30 {
		t.Fatalf("offer = %v %v", offer, err)
	}
	fake.offerErr = billing.ErrTrialVariantMissing
	if _, err := cli.SetFreeTrialTier(ctx, &billingv1.SetFreeTrialTierRequest{TierId: "max", Reason: "x"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("missing trial variant: %v", err)
	}

	inv, err := cli.InvalidateTestSubscriptions(ctx, &billingv1.InvalidateTestSubscriptionsRequest{DryRun: true})
	if err != nil || inv.GetCount() != 2 || !inv.GetDryRun() {
		t.Fatalf("invalidate = %v %v", inv, err)
	}
	fake.invalidErr = billing.ErrStillTestMode
	if _, err := cli.InvalidateTestSubscriptions(ctx, &billingv1.InvalidateTestSubscriptionsRequest{}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("test-mode cutover: %v", err)
	}
}
