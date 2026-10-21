package server

import (
	"context"
	"errors"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	billingv1 "github.com/ogen-app/ogen/gen/billing/v1"
	billingdom "github.com/ogen-app/ogen/src/domain/billing"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/usecase/billing"
)

// BillingAdmin is the billing usecase surface Harbor drives.
type BillingAdmin interface {
	Enabled() bool
	Settings() billingdom.Settings
	Configured(ctx context.Context) bool
	TenantBilling(ctx context.Context, tenantID string) (billing.TenantBilling, error)
	Attention(ctx context.Context, pageSize int, pageToken string) ([]billing.AttentionItem, string, error)
	SetBillingRequired(ctx context.Context, tenantID string, required bool, reason string) error
	ResyncTenant(ctx context.Context, tenantID string) (int, error)
	FreeTrialOffer(ctx context.Context) (billing.FreeTrialOffer, error)
	SetFreeTrialTier(ctx context.Context, tierID, reason string) (billing.FreeTrialOffer, error)
	InvalidateTestSubscriptions(ctx context.Context, dryRun bool) ([]string, error)
	BillingStatus(ctx context.Context, t *models.Tenant) (string, error)
}

// billingAdminService implements billingv1.BillingAdminServiceServer.
type billingAdminService struct {
	billingv1.UnimplementedBillingAdminServiceServer
	svc BillingAdmin
}

func newBillingAdminService(svc BillingAdmin) *billingAdminService {
	return &billingAdminService{svc: svc}
}

// billingStatusErr maps usecase errors to gRPC codes.
func billingStatusErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, billing.ErrUnavailable):
		return status.Error(codes.Unavailable, "billing is not configured")
	case errors.Is(err, billing.ErrTenantNotFound):
		return status.Error(codes.NotFound, "tenant not found")
	case errors.Is(err, billing.ErrUnknownTier):
		return status.Error(codes.NotFound, "tier not found")
	case errors.Is(err, billing.ErrReasonRequired):
		return status.Error(codes.InvalidArgument, "reason is required")
	case errors.Is(err, billing.ErrStillTestMode):
		return status.Error(codes.FailedPrecondition, "billing still runs in test mode; switch LEMONSQUEEZY_TEST_MODE off first")
	case errors.Is(err, billing.ErrTrialVariantMissing):
		return status.Error(codes.FailedPrecondition, "the tier's Lemon Squeezy variants do not all carry a one-month free trial")
	case errors.Is(err, billing.ErrProvider):
		return status.Error(codes.Unavailable, "billing provider error")
	default:
		return status.Error(codes.Internal, "billing operation failed")
	}
}

func (s *billingAdminService) GetBillingSettings(ctx context.Context, _ *billingv1.GetBillingSettingsRequest) (*billingv1.GetBillingSettingsResponse, error) {
	st := s.svc.Settings()
	return &billingv1.GetBillingSettingsResponse{
		Provider:          st.Provider,
		TestMode:          st.TestMode,
		Configured:        s.svc.Configured(ctx),
		StoreId:           st.StoreID,
		TrialTeardownLive: st.TrialTeardownLive,
	}, nil
}

func (s *billingAdminService) GetTenantBilling(ctx context.Context, req *billingv1.GetTenantBillingRequest) (*billingv1.GetTenantBillingResponse, error) {
	tb, err := s.tenantBilling(ctx, req.GetTenantId())
	if err != nil {
		return nil, err
	}
	return &billingv1.GetTenantBillingResponse{Billing: tb}, nil
}

func (s *billingAdminService) tenantBilling(ctx context.Context, tenantID string) (*billingv1.TenantBilling, error) {
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		return nil, status.Error(codes.InvalidArgument, "tenant_id is required")
	}
	tb, err := s.svc.TenantBilling(ctx, tenantID)
	if err != nil {
		return nil, billingStatusErr(err)
	}
	out := &billingv1.TenantBilling{TenantId: tb.TenantID, BillingRequired: tb.BillingRequired, Gated: tb.Gated}
	for i := range tb.Subscriptions {
		out.Subscriptions = append(out.Subscriptions, toSubscriptionProto(&tb.Subscriptions[i], tb.Prices, tb.Duplicates[tb.Subscriptions[i].ID]))
	}
	for _, c := range tb.ScheduledChanges {
		out.ScheduledChanges = append(out.ScheduledChanges, &billingv1.ScheduledChange{
			Id: c.ID, SubscriptionId: c.BillingSubscriptionID, FromTierVersionPriceId: c.FromTierVersionPriceID,
			ToTierVersionPriceId: c.ToTierVersionPriceID, EffectiveAt: timestamppb.New(c.EffectiveAt), Status: c.Status,
			CreatedBy: c.CreatedBy, CreatedAt: timestamppb.New(c.CreatedAt),
		})
	}
	for i := range tb.WebhookEvents {
		out.RecentWebhookEvents = append(out.RecentWebhookEvents, toWebhookEventProto(&tb.WebhookEvents[i]))
	}
	for _, o := range tb.Owners {
		out.Owners = append(out.Owners, &billingv1.OwnerTrial{UserId: o.UserID, AccountId: o.AccountID, Email: o.Email, TrialEligible: o.TrialEligible})
	}
	return out, nil
}

func (s *billingAdminService) ListBillingAttention(ctx context.Context, req *billingv1.ListBillingAttentionRequest) (*billingv1.ListBillingAttentionResponse, error) {
	items, next, err := s.svc.Attention(ctx, int(req.GetPageSize()), req.GetPageToken())
	if err != nil {
		return nil, billingStatusErr(err)
	}
	out := &billingv1.ListBillingAttentionResponse{NextPageToken: next}
	for _, it := range items {
		p := &billingv1.AttentionItem{TenantId: it.TenantID, TenantName: it.TenantName, Reason: it.Reason, Since: timestamppb.New(it.Since), TeardownStatus: it.TeardownStatus}
		if it.Subscription != nil {
			p.Subscription = toSubscriptionProto(it.Subscription, nil, it.Reason == billing.AttentionDuplicate)
		}
		if it.WebhookEvent != nil {
			p.WebhookEvent = toWebhookEventProto(it.WebhookEvent)
		}
		out.Items = append(out.Items, p)
	}
	return out, nil
}

func (s *billingAdminService) SetBillingRequired(ctx context.Context, req *billingv1.SetBillingRequiredRequest) (*billingv1.SetBillingRequiredResponse, error) {
	tenantID := strings.TrimSpace(req.GetTenantId())
	if tenantID == "" {
		return nil, status.Error(codes.InvalidArgument, "tenant_id is required")
	}
	if err := s.svc.SetBillingRequired(ctx, tenantID, req.GetRequired(), strings.TrimSpace(req.GetReason())); err != nil {
		return nil, billingStatusErr(err)
	}
	tb, err := s.tenantBilling(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	return &billingv1.SetBillingRequiredResponse{Billing: tb}, nil
}

func (s *billingAdminService) ResyncSubscription(ctx context.Context, req *billingv1.ResyncSubscriptionRequest) (*billingv1.ResyncSubscriptionResponse, error) {
	tenantID := strings.TrimSpace(req.GetTenantId())
	if tenantID == "" {
		return nil, status.Error(codes.InvalidArgument, "tenant_id is required")
	}
	n, err := s.svc.ResyncTenant(ctx, tenantID)
	if err != nil {
		return nil, billingStatusErr(err)
	}
	tb, err := s.tenantBilling(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	return &billingv1.ResyncSubscriptionResponse{Billing: tb, Corrected: int32(n)}, nil
}

func (s *billingAdminService) GetFreeTrialTier(ctx context.Context, _ *billingv1.GetFreeTrialTierRequest) (*billingv1.GetFreeTrialTierResponse, error) {
	offer, err := s.svc.FreeTrialOffer(ctx)
	if err != nil {
		return nil, billingStatusErr(err)
	}
	return &billingv1.GetFreeTrialTierResponse{Offer: toOfferProto(offer)}, nil
}

func (s *billingAdminService) SetFreeTrialTier(ctx context.Context, req *billingv1.SetFreeTrialTierRequest) (*billingv1.SetFreeTrialTierResponse, error) {
	offer, err := s.svc.SetFreeTrialTier(ctx, strings.TrimSpace(req.GetTierId()), strings.TrimSpace(req.GetReason()))
	if err != nil {
		return nil, billingStatusErr(err)
	}
	return &billingv1.SetFreeTrialTierResponse{Offer: toOfferProto(offer)}, nil
}

func (s *billingAdminService) InvalidateTestSubscriptions(ctx context.Context, req *billingv1.InvalidateTestSubscriptionsRequest) (*billingv1.InvalidateTestSubscriptionsResponse, error) {
	ids, err := s.svc.InvalidateTestSubscriptions(ctx, req.GetDryRun())
	if err != nil {
		return nil, billingStatusErr(err)
	}
	return &billingv1.InvalidateTestSubscriptionsResponse{DryRun: req.GetDryRun(), Count: int32(len(ids)), TenantIds: ids}, nil
}

func toOfferProto(o billing.FreeTrialOffer) *billingv1.FreeTrialTier {
	return &billingv1.FreeTrialTier{TierId: o.TierID, TierName: o.TierName, TrialDays: int32(o.TrialDays), EligibleTierIds: o.EligibleTierIDs}
}

func ts(t *time.Time) *timestamppb.Timestamp {
	if t == nil {
		return nil
	}
	return timestamppb.New(*t)
}

func toSubscriptionProto(s *models.BillingSubscription, prices map[string]billingdom.Price, duplicate bool) *billingv1.Subscription {
	p := &billingv1.Subscription{
		Id: s.ID, Provider: s.Provider, ProviderSubscriptionId: s.ProviderSubscriptionID, ProviderCustomerId: s.ProviderCustomerID,
		ProviderVariantId: s.ProviderVariantID, Status: s.Status, TestMode: s.TestMode,
		RenewsAt: ts(s.RenewsAt), EndsAt: ts(s.EndsAt), TrialEndsAt: ts(s.TrialEndsAt), FirstPaidAt: ts(s.FirstPaidAt),
		CardBrand: s.CardBrand, CardLastFour: s.CardLastFour, InvalidatedAt: ts(s.InvalidatedAt), InvalidatedReason: s.InvalidatedReason,
		TeardownAt: ts(s.TeardownAt), Duplicate: duplicate, CreatedAt: timestamppb.New(s.CreatedAt), UpdatedAt: timestamppb.New(s.UpdatedAt),
	}
	if s.AccountID != nil {
		p.AccountId = *s.AccountID
	}
	if s.TierVersionPriceID != nil {
		p.TierVersionPriceId = *s.TierVersionPriceID
		if price, ok := prices[*s.TierVersionPriceID]; ok {
			p.TierId, p.TierVersionId = price.TierID, price.TierVersionID
		}
	}
	return p
}

func toWebhookEventProto(e *models.BillingWebhookEvent) *billingv1.WebhookEvent {
	return &billingv1.WebhookEvent{
		Id: e.ID, EventName: e.EventName, ProviderObjectId: e.ProviderObjectID, TestMode: e.TestMode,
		Outcome: e.Outcome, Error: e.Error, ReceivedAt: timestamppb.New(e.ReceivedAt), ProcessedAt: ts(e.ProcessedAt),
	}
}
