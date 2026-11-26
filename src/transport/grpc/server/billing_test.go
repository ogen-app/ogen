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
	"google.golang.org/protobuf/types/known/timestamppb"

	billingv1 "github.com/ogen-app/ogen/gen/billing/v1"
	billingdom "github.com/ogen-app/ogen/src/domain/billing"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/usecase/billing"
)

type fakeBillingAdmin struct {
	setRequired      []any
	setComplimentary []any
	invalidErr       error
	invalidScope     billing.InvalidationScope
	offerErr         error
	setMapping       []any
	listedPayments   []any
}

func (f *fakeBillingAdmin) Enabled() bool { return true }
func (f *fakeBillingAdmin) Settings() billingdom.Settings {
	return billingdom.Settings{Provider: "polar", TestMode: true, OrganizationID: "org_7", APIVersion: "2026-10", TrialTeardownLive: true}
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
		Subscriptions: []models.BillingSubscription{{ID: "bs1", Provider: "polar", ProviderSubscriptionID: "9", Status: "past_due", ProviderStatus: "past_due",
			ProviderProductID: "prod_max", PendingProductID: "prod_pro", PendingEffectiveAt: &now, CancelAtPeriodEnd: true,
			TierVersionPriceID: &price, FirstPaidAt: &now, CreatedAt: now}},
		Duplicates:       map[string]bool{},
		Prices:           map[string]billingdom.Price{"p1": {ID: "p1", TierID: "pro", TierVersionID: "ttv-pro-2"}},
		ScheduledChanges: []models.BillingScheduledChange{{ID: "sc1", BillingSubscriptionID: "bs1", Status: "provider_applied", EffectiveAt: now}},
		WebhookEvents:    []models.BillingWebhookEvent{{ID: "we1", EventName: "subscription.updated", Outcome: "applied", ReceivedAt: now}},
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
func (f *fakeBillingAdmin) InvalidateSubscriptions(_ context.Context, _ bool, scope billing.InvalidationScope) ([]string, error) {
	f.invalidScope = scope
	if scope == 0 {
		return nil, billing.ErrBadScope
	}
	return []string{"t1", "t2"}, f.invalidErr
}
func (f *fakeBillingAdmin) BillingStatus(context.Context, *models.Tenant) (string, error) {
	return "on_trial", nil
}

var fakeProduct = billing.ProviderProduct{
	Product: billingdom.Product{ID: "prod_pro", Name: "Pro monthly", Interval: "month", IntervalCount: 1,
		Prices:       []billingdom.ProductPrice{{Currency: "EUR", AmountMinor: 5000}, {Currency: "USD", AmountMinor: 5500}},
		HasFreeTrial: true, TrialInterval: "month", TrialIntervalCount: 1, TestMode: true},
	MappedPriceIDs: []string{"p1"},
}

var fakePrice = billingdom.Price{ID: "p1", TierID: "pro", TierName: "Pro", TierVersionID: "ttv-pro-2", TierVersion: 2, Currency: "EUR", Interval: "month", NetMinor: 5000}

func (f *fakeBillingAdmin) ProviderProducts(_ context.Context, testMode bool) ([]billing.ProviderProduct, error) {
	if !testMode {
		return nil, fmt.Errorf("%w: set polar_access_token_live", billing.ErrModeUnreadable)
	}
	return []billing.ProviderProduct{fakeProduct}, nil
}
func (f *fakeBillingAdmin) ProductMappingList(_ context.Context, testMode bool) (billing.ProductMappings, error) {
	p := fakeProduct
	return billing.ProductMappings{TestMode: testMode, Checked: true, Mappings: []billing.ProductMappingView{
		{Price: fakePrice, FreeTrialTier: true, ProductID: "prod_pro", Product: &p, Status: billingdom.MappingOK,
			Row: &models.BillingProductMapping{UpdatedAt: time.Now(), UpdatedBy: models.ProductMappingByHarbor}},
		{Price: billingdom.Price{ID: "p2", TierName: "Max", Interval: "year"}, Status: billingdom.MappingUnmapped, Message: "Not mapped"},
	}}, nil
}
func (f *fakeBillingAdmin) SetProductMapping(_ context.Context, priceID string, testMode bool, productID, reason string) (billing.ProductMappingView, error) {
	f.setMapping = []any{priceID, testMode, productID, reason}
	switch {
	case reason == "":
		return billing.ProductMappingView{}, billing.ErrReasonRequired
	case priceID == "missing":
		return billing.ProductMappingView{}, billing.ErrUnknownPrice
	case productID == "prod_bad":
		return billing.ProductMappingView{}, &billing.MappingRefusedError{Status: billingdom.MappingPriceMismatch, Message: "Product prod_bad costs 49.00 EUR; the price is 50.00 EUR excluding tax."}
	case productID == "":
		return billing.ProductMappingView{Price: fakePrice, Status: billingdom.MappingUnmapped}, nil
	}
	p := fakeProduct
	return billing.ProductMappingView{Price: fakePrice, ProductID: productID, Product: &p, Status: billingdom.MappingOK}, nil
}
func (f *fakeBillingAdmin) SuggestProductMappings(context.Context, bool) ([]billing.ProductSuggestion, error) {
	return []billing.ProductSuggestion{{PriceID: "p2", Product: fakeProduct}}, nil
}
func (f *fakeBillingAdmin) CheckProducts(context.Context) (int, error) { return 0, nil }
func (f *fakeBillingAdmin) ListPayments(_ context.Context, testMode bool, from, to *time.Time, pageSize int, pageToken string) ([]billing.PaymentRow, string, error) {
	f.listedPayments = []any{testMode, from, to, pageSize, pageToken}
	if pageToken == "bad" {
		return nil, "", billing.ErrBadPageToken
	}
	at := time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)
	rate, source, day := "1.1666666667", "ecb", time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	return []billing.PaymentRow{
		{
			BillingPayment: models.BillingPayment{
				ID: "pay1", Provider: "polar", ProviderID: "77", Kind: "refund", Currency: "EUR", ProviderProductID: "prod_pro",
				Subtotal: 900, Tax: 100, Total: 1000, SubtotalUSD: new(int64(1050)), TaxUSD: new(int64(117)), TotalUSD: new(int64(1167)),
				FXRate: &rate, FXSource: &source, FXDate: &day,
				Status: "partial_refund", OccurredAt: at, TestMode: true,
			},
			ResolvedTenantID: "t1", TierID: "pro",
		},
		{
			// Not converted yet: the USD amounts are unset.
			BillingPayment: models.BillingPayment{ID: "pay2", Provider: "polar", ProviderID: "78", Kind: "order", Currency: "GBP", Total: 500, OccurredAt: at},
		},
	}, "next", nil
}

func TestBillingListPaymentsRoundTrip(t *testing.T) {
	fake := &fakeBillingAdmin{}
	cli := dialBillingAdmin(t, fake)
	ctx := t.Context()
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	res, err := cli.ListPayments(ctx, &billingv1.ListPaymentsRequest{From: timestamppb.New(from), To: timestamppb.New(to), TestMode: true, PageSize: 10, PageToken: "tok"})
	if err != nil || res.GetNextPageToken() != "next" || len(res.GetPayments()) != 2 {
		t.Fatalf("list = %v %v", res, err)
	}
	p := res.GetPayments()[0]
	if p.GetId() != "pay1" || p.GetKind() != "refund" || p.GetProviderId() != "77" || p.GetTenantId() != "t1" || p.GetTierId() != "pro" || p.GetProviderProductId() != "prod_pro" ||
		p.GetTotal() != 1000 || p.GetTaxUsd() != 117 || !p.GetTestMode() || !p.GetOccurredAt().AsTime().Equal(time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)) ||
		p.GetFxRate() != "1.1666666667" || p.GetFxSource() != "ecb" || p.GetFxDate() != "2026-09-03" {
		t.Fatalf("payment = %v", p)
	}
	if u := res.GetPayments()[1]; u.TotalUsd != nil || u.FeeUsd != nil || u.GetFxSource() != "" {
		t.Fatalf("unconverted payment = %v", u)
	}
	if got := fake.listedPayments; got[0] != true || !got[1].(*time.Time).Equal(from) || !got[2].(*time.Time).Equal(to) || got[3] != 10 || got[4] != "tok" {
		t.Fatalf("listed with %v", got)
	}

	if _, err := cli.ListPayments(ctx, &billingv1.ListPaymentsRequest{}); err != nil || fake.listedPayments[1].(*time.Time) != nil {
		t.Fatalf("open range: %v %v", err, fake.listedPayments)
	}
	if _, err := cli.ListPayments(ctx, &billingv1.ListPaymentsRequest{From: timestamppb.New(to), To: timestamppb.New(from)}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("reversed range: %v", err)
	}
	if _, err := cli.ListPayments(ctx, &billingv1.ListPaymentsRequest{PageToken: "bad"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad token: %v", err)
	}
}

func TestBillingProductMappingRoundTrip(t *testing.T) {
	fake := &fakeBillingAdmin{}
	cli := dialBillingAdmin(t, fake)
	ctx := t.Context()

	ps, err := cli.ListProviderProducts(ctx, &billingv1.ListProviderProductsRequest{TestMode: true})
	if err != nil || len(ps.GetProducts()) != 1 {
		t.Fatalf("products = %v %v", ps, err)
	}
	if p := ps.GetProducts()[0]; p.GetId() != "prod_pro" || p.GetName() != "Pro monthly" || len(p.GetPrices()) != 2 || p.GetPrices()[1].GetCurrency() != "USD" ||
		p.GetPrices()[1].GetAmountMinor() != 5500 || p.GetIntervalCount() != 1 || p.GetTrialIntervalCount() != 1 || p.GetMappedPriceIds()[0] != "p1" || p.GetArchived() {
		t.Fatalf("product = %v", p)
	}
	_, err = cli.ListProviderProducts(ctx, &billingv1.ListProviderProductsRequest{TestMode: false})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(status.Convert(err).Message(), "polar_access_token_live") {
		t.Fatalf("live without token: %v", err)
	}

	list, err := cli.ListProductMappings(ctx, &billingv1.ListProductMappingsRequest{TestMode: true})
	if err != nil || !list.GetChecked() || !list.GetTestMode() || len(list.GetMappings()) != 2 {
		t.Fatalf("mappings = %v %v", list, err)
	}
	m := list.GetMappings()[0]
	if m.GetTierVersionPriceId() != "p1" || m.GetTierName() != "Pro" || m.GetTierVersion() != 2 || m.GetNetMinor() != 5000 || !m.GetFreeTrialTier() ||
		m.GetProductId() != "prod_pro" || m.GetProduct().GetId() != "prod_pro" || m.GetStatus() != "ok" || m.GetUpdatedAt() == nil || m.GetUpdatedBy() != "harbor" {
		t.Fatalf("mapped row = %v", m)
	}
	if u := list.GetMappings()[1]; u.GetStatus() != "unmapped" || u.GetProduct() != nil || u.GetUpdatedAt() != nil || u.GetMessage() == "" {
		t.Fatalf("unmapped row = %v", u)
	}

	set, err := cli.SetProductMapping(ctx, &billingv1.SetProductMappingRequest{TierVersionPriceId: "p1", TestMode: true, ProductId: "prod_pro", Reason: "new tier"})
	if err != nil || set.GetMapping().GetStatus() != "ok" || set.GetMapping().GetProductId() != "prod_pro" || fake.setMapping[3] != "new tier" {
		t.Fatalf("set = %v %v %v", set, err, fake.setMapping)
	}
	if _, err := cli.SetProductMapping(ctx, &billingv1.SetProductMappingRequest{TestMode: true, ProductId: "prod_pro", Reason: "x"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("no price: %v", err)
	}
	if _, err := cli.SetProductMapping(ctx, &billingv1.SetProductMappingRequest{TierVersionPriceId: "p1", ProductId: "prod_pro"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("no reason: %v", err)
	}
	if _, err := cli.SetProductMapping(ctx, &billingv1.SetProductMappingRequest{TierVersionPriceId: "missing", ProductId: "prod_pro", Reason: "x"}); status.Code(err) != codes.NotFound {
		t.Fatalf("unknown price: %v", err)
	}
	_, err = cli.SetProductMapping(ctx, &billingv1.SetProductMappingRequest{TierVersionPriceId: "p1", TestMode: true, ProductId: "prod_bad", Reason: "x"})
	if status.Code(err) != codes.FailedPrecondition || status.Convert(err).Message() != "Product prod_bad costs 49.00 EUR; the price is 50.00 EUR excluding tax." {
		t.Fatalf("refused mapping: %v", err)
	}
	unset, err := cli.SetProductMapping(ctx, &billingv1.SetProductMappingRequest{TierVersionPriceId: "p1", TestMode: true, Reason: "retire"})
	if err != nil || unset.GetMapping().GetStatus() != "unmapped" || fake.setMapping[2] != "" {
		t.Fatalf("unmap = %v %v", unset, err)
	}

	sg, err := cli.SuggestProductMappings(ctx, &billingv1.SuggestProductMappingsRequest{TestMode: true})
	if err != nil || len(sg.GetSuggestions()) != 1 || sg.GetSuggestions()[0].GetTierVersionPriceId() != "p2" || sg.GetSuggestions()[0].GetProduct().GetId() != "prod_pro" {
		t.Fatalf("suggestions = %v %v", sg, err)
	}
}

func TestBillingAdminRoundTrip(t *testing.T) {
	fake := &fakeBillingAdmin{}
	cli := dialBillingAdmin(t, fake)
	ctx := t.Context()

	st, err := cli.GetBillingSettings(ctx, &billingv1.GetBillingSettingsRequest{})
	if err != nil || st.GetProvider() != "polar" || !st.GetTestMode() || !st.GetConfigured() || st.GetOrganizationId() != "org_7" || st.GetApiVersion() != "2026-10" || !st.GetTrialTeardownLive() {
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
	if s := b.GetSubscriptions()[0]; s.GetProviderProductId() != "prod_max" || s.GetPendingProductId() != "prod_pro" || s.GetPendingEffectiveAt() == nil ||
		!s.GetCancelAtPeriodEnd() || s.GetProviderStatus() != "past_due" {
		t.Fatalf("subscription = %v", s)
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
	fake.offerErr = billing.ErrTrialProductMissing
	if _, err := cli.SetFreeTrialTier(ctx, &billingv1.SetFreeTrialTierRequest{TierId: "max", Reason: "x"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("missing trial product: %v", err)
	}

	inv, err := cli.InvalidateSubscriptions(ctx, &billingv1.InvalidateSubscriptionsRequest{DryRun: true, Scope: billingv1.InvalidationScope_INVALIDATION_SCOPE_PROVIDER_SWITCH})
	if err != nil || inv.GetCount() != 2 || !inv.GetDryRun() || fake.invalidScope != billing.ScopeProviderSwitch {
		t.Fatalf("invalidate = %v %v", inv, err)
	}
	if _, err := cli.InvalidateSubscriptions(ctx, &billingv1.InvalidateSubscriptionsRequest{DryRun: true}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("no scope: %v", err)
	}
	fake.invalidErr = billing.ErrStillTestMode
	if _, err := cli.InvalidateSubscriptions(ctx, &billingv1.InvalidateSubscriptionsRequest{Scope: billingv1.InvalidationScope_INVALIDATION_SCOPE_MODE_CUTOVER}); status.Code(err) != codes.FailedPrecondition ||
		fake.invalidScope != billing.ScopeModeCutover {
		t.Fatalf("sandbox mode cutover: %v", err)
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
