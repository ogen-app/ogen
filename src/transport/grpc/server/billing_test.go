package server

import (
	"context"
	"fmt"
	"net"
	"strings"
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
	setRequired      []any
	setComplimentary []any
	invalidErr       error
	offerErr         error
	setMapping       []any
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
func (f *fakeBillingAdmin) SetComplimentary(_ context.Context, tenantID string, complimentary bool, reason string) error {
	if reason == "" {
		return billing.ErrReasonRequired
	}
	f.setComplimentary = []any{tenantID, complimentary, reason}
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

var fakeVariant = billing.ProviderVariant{
	Variant: billingdom.Variant{ID: "101", ProductID: "10", ProductName: "Pro", Name: "Monthly", PriceMinor: 5000, Interval: "month",
		HasFreeTrial: true, TrialInterval: "month", TrialIntervalCount: 1, Status: "published", TestMode: true},
	Currency: "EUR", MappedPriceID: "p1",
}

var fakePrice = billingdom.Price{ID: "p1", TierID: "pro", TierName: "Pro", TierVersionID: "ttv-pro-2", TierVersion: 2, Currency: "EUR", Interval: "month", NetMinor: 5000}

func (f *fakeBillingAdmin) ProviderVariants(_ context.Context, testMode bool) ([]billing.ProviderVariant, error) {
	if !testMode {
		return nil, fmt.Errorf("%w: set lemonsqueezy_api_key_live", billing.ErrModeUnreadable)
	}
	return []billing.ProviderVariant{fakeVariant}, nil
}
func (f *fakeBillingAdmin) VariantMappingList(_ context.Context, testMode bool) (billing.VariantMappings, error) {
	v := fakeVariant
	return billing.VariantMappings{TestMode: testMode, Checked: true, Mappings: []billing.VariantMapping{
		{Price: fakePrice, FreeTrialTier: true, VariantID: "101", Variant: &v, Status: billingdom.MappingOK,
			Row: &models.BillingVariantMapping{UpdatedAt: time.Now(), UpdatedBy: models.VariantMappingByHarbor}},
		{Price: billingdom.Price{ID: "p2", TierName: "Max", Interval: "year"}, Status: billingdom.MappingUnmapped, Message: "Not mapped"},
	}}, nil
}
func (f *fakeBillingAdmin) SetVariantMapping(_ context.Context, priceID string, testMode bool, variantID, reason string) (billing.VariantMapping, error) {
	f.setMapping = []any{priceID, testMode, variantID, reason}
	switch {
	case reason == "":
		return billing.VariantMapping{}, billing.ErrReasonRequired
	case priceID == "missing":
		return billing.VariantMapping{}, billing.ErrUnknownPrice
	case variantID == "999":
		return billing.VariantMapping{}, &billing.MappingRefusedError{Status: billingdom.MappingPriceMismatch, Message: "Variant 999 costs 49.00 EUR; the price is 50.00 EUR excluding tax."}
	case variantID == "":
		return billing.VariantMapping{Price: fakePrice, Status: billingdom.MappingUnmapped}, nil
	}
	v := fakeVariant
	return billing.VariantMapping{Price: fakePrice, VariantID: variantID, Variant: &v, Status: billingdom.MappingOK}, nil
}
func (f *fakeBillingAdmin) SuggestVariantMappings(context.Context, bool) ([]billing.VariantSuggestion, error) {
	return []billing.VariantSuggestion{{PriceID: "p2", Variant: fakeVariant}}, nil
}
func (f *fakeBillingAdmin) CheckVariants(context.Context) (int, error) { return 0, nil }

func TestBillingVariantMappingRoundTrip(t *testing.T) {
	fake := &fakeBillingAdmin{}
	cli := dialBillingAdmin(t, fake)
	ctx := t.Context()

	vs, err := cli.ListProviderVariants(ctx, &billingv1.ListProviderVariantsRequest{TestMode: true})
	if err != nil || len(vs.GetVariants()) != 1 {
		t.Fatalf("variants = %v %v", vs, err)
	}
	if v := vs.GetVariants()[0]; v.GetId() != "101" || v.GetProductName() != "Pro" || v.GetPriceMinor() != 5000 || v.GetCurrency() != "EUR" ||
		v.GetTrialIntervalCount() != 1 || v.GetMappedPriceId() != "p1" || v.GetStatus() != "published" {
		t.Fatalf("variant = %v", v)
	}
	_, err = cli.ListProviderVariants(ctx, &billingv1.ListProviderVariantsRequest{TestMode: false})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(status.Convert(err).Message(), "lemonsqueezy_api_key_live") {
		t.Fatalf("live without key: %v", err)
	}

	list, err := cli.ListVariantMappings(ctx, &billingv1.ListVariantMappingsRequest{TestMode: true})
	if err != nil || !list.GetChecked() || !list.GetTestMode() || len(list.GetMappings()) != 2 {
		t.Fatalf("mappings = %v %v", list, err)
	}
	m := list.GetMappings()[0]
	if m.GetTierVersionPriceId() != "p1" || m.GetTierName() != "Pro" || m.GetTierVersion() != 2 || m.GetNetMinor() != 5000 || !m.GetFreeTrialTier() ||
		m.GetVariantId() != "101" || m.GetVariant().GetId() != "101" || m.GetStatus() != "ok" || m.GetUpdatedAt() == nil || m.GetUpdatedBy() != "harbor" {
		t.Fatalf("mapped row = %v", m)
	}
	if u := list.GetMappings()[1]; u.GetStatus() != "unmapped" || u.GetVariant() != nil || u.GetUpdatedAt() != nil || u.GetMessage() == "" {
		t.Fatalf("unmapped row = %v", u)
	}

	set, err := cli.SetVariantMapping(ctx, &billingv1.SetVariantMappingRequest{TierVersionPriceId: "p1", TestMode: true, VariantId: "101", Reason: "new tier"})
	if err != nil || set.GetMapping().GetStatus() != "ok" || set.GetMapping().GetVariantId() != "101" || fake.setMapping[3] != "new tier" {
		t.Fatalf("set = %v %v %v", set, err, fake.setMapping)
	}
	if _, err := cli.SetVariantMapping(ctx, &billingv1.SetVariantMappingRequest{TestMode: true, VariantId: "101", Reason: "x"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("no price: %v", err)
	}
	if _, err := cli.SetVariantMapping(ctx, &billingv1.SetVariantMappingRequest{TierVersionPriceId: "p1", VariantId: "101"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("no reason: %v", err)
	}
	if _, err := cli.SetVariantMapping(ctx, &billingv1.SetVariantMappingRequest{TierVersionPriceId: "missing", VariantId: "101", Reason: "x"}); status.Code(err) != codes.NotFound {
		t.Fatalf("unknown price: %v", err)
	}
	_, err = cli.SetVariantMapping(ctx, &billingv1.SetVariantMappingRequest{TierVersionPriceId: "p1", TestMode: true, VariantId: "999", Reason: "x"})
	if status.Code(err) != codes.FailedPrecondition || status.Convert(err).Message() != "Variant 999 costs 49.00 EUR; the price is 50.00 EUR excluding tax." {
		t.Fatalf("refused mapping: %v", err)
	}
	unset, err := cli.SetVariantMapping(ctx, &billingv1.SetVariantMappingRequest{TierVersionPriceId: "p1", TestMode: true, Reason: "retire"})
	if err != nil || unset.GetMapping().GetStatus() != "unmapped" || fake.setMapping[2] != "" {
		t.Fatalf("unmap = %v %v", unset, err)
	}

	sg, err := cli.SuggestVariantMappings(ctx, &billingv1.SuggestVariantMappingsRequest{TestMode: true})
	if err != nil || len(sg.GetSuggestions()) != 1 || sg.GetSuggestions()[0].GetTierVersionPriceId() != "p2" || sg.GetSuggestions()[0].GetVariant().GetId() != "101" {
		t.Fatalf("suggestions = %v %v", sg, err)
	}
}

func TestBillingAdminRoundTrip(t *testing.T) {
	fake := &fakeBillingAdmin{}
	cli := dialBillingAdmin(t, fake)
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
	if _, err := cli.SetComplimentary(ctx, &billingv1.SetComplimentaryRequest{TenantId: "t1", Complimentary: true}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("complimentary without reason: %v", err)
	}
	if _, err := cli.SetComplimentary(ctx, &billingv1.SetComplimentaryRequest{Complimentary: true, Reason: "tester"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("complimentary without tenant: %v", err)
	}
	if _, err := cli.SetComplimentary(ctx, &billingv1.SetComplimentaryRequest{TenantId: "t1", Complimentary: true, Reason: "tester"}); err != nil || fake.setComplimentary[1] != true || fake.setComplimentary[2] != "tester" {
		t.Fatalf("set complimentary: %v %v", err, fake.setComplimentary)
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

// dialBillingAdmin serves the billing admin surface over fake and returns an
// authenticated client.
func dialBillingAdmin(t *testing.T, fake BillingAdmin) billingv1.BillingAdminServiceClient {
	t.Helper()
	const token = "billing-token"
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
	return billingv1.NewBillingAdminServiceClient(conn)
}
