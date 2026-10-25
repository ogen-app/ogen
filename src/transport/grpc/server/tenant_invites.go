package server

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	tenantinvitesv1 "github.com/ogen-app/ogen/gen/tenantinvites/v1"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/usecase/signupaccess"
)

// TenantInvites is the signup-access usecase surface Harbor drives.
// *signupaccess.Service satisfies it.
type TenantInvites interface {
	CreateInvite(ctx context.Context, in signupaccess.CreateInviteInput) (*models.TenantInviteView, bool, error)
	GetInvite(ctx context.Context, id string) (*models.TenantInviteView, error)
	ListInvites(ctx context.Context, in signupaccess.ListInvitesInput) ([]models.TenantInviteView, bool, error)
	ResendInvite(ctx context.Context, id string, expiresInHours int) (*models.TenantInviteView, error)
	RevokeInvite(ctx context.Context, id string) (*models.TenantInviteView, error)
	Settings(ctx context.Context) (models.SignupSettings, error)
	UpdateSettings(ctx context.Context, in signupaccess.UpdateSettingsInput) (models.SignupSettings, error)
}

// tenantInviteAdminService implements tenantinvitesv1.TenantInviteAdminServiceServer.
type tenantInviteAdminService struct {
	tenantinvitesv1.UnimplementedTenantInviteAdminServiceServer
	svc TenantInvites
	now func() time.Time
}

func newTenantInviteAdminService(svc TenantInvites) *tenantInviteAdminService {
	return &tenantInviteAdminService{svc: svc, now: time.Now}
}

// tenantInviteStatusErr maps usecase errors to gRPC codes. Unexpected errors
// are logged and answer a generic Internal, so DB internals never cross the
// wire.
func tenantInviteStatusErr(ctx context.Context, op string, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, signupaccess.ErrInvalidArgument):
		return status.Error(codes.InvalidArgument, strings.TrimPrefix(err.Error(), signupaccess.ErrInvalidArgument.Error()+": "))
	case errors.Is(err, signupaccess.ErrAccountExists):
		return status.Error(codes.FailedPrecondition, "account_exists: this email already has an Ogen account; make their workspace complimentary instead")
	case errors.Is(err, signupaccess.ErrTierNotActive):
		return status.Error(codes.FailedPrecondition, "tier_not_active: the tier has no published version")
	case errors.Is(err, signupaccess.ErrNotPending):
		return status.Error(codes.FailedPrecondition, "the invite is no longer pending")
	case errors.Is(err, signupaccess.ErrNotFound):
		return status.Error(codes.NotFound, "invite not found")
	default:
		slog.ErrorContext(ctx, "tenant invite admin call failed", logging.AttrComponent, "grpcserver", "op", op, logging.AttrError, err)
		return status.Error(codes.Internal, "tenant invite operation failed")
	}
}

func (s *tenantInviteAdminService) CreateTenantInvite(ctx context.Context, req *tenantinvitesv1.CreateTenantInviteRequest) (*tenantinvitesv1.CreateTenantInviteResponse, error) {
	v, replaced, err := s.svc.CreateInvite(ctx, signupaccess.CreateInviteInput{
		Email: req.GetEmail(), TierID: req.GetTierId(), GroupIDs: req.GetGroupIds(),
		ExpiresInHours: int(req.GetExpiresInHours()), Message: req.GetMessage(), Note: req.GetNote(),
		CreatedBy: req.GetCreatedBy(),
	})
	if err != nil {
		return nil, tenantInviteStatusErr(ctx, "create", err)
	}
	return &tenantinvitesv1.CreateTenantInviteResponse{Invite: s.toProto(v), Replaced: replaced}, nil
}

func (s *tenantInviteAdminService) GetTenantInvite(ctx context.Context, req *tenantinvitesv1.GetTenantInviteRequest) (*tenantinvitesv1.GetTenantInviteResponse, error) {
	id := strings.TrimSpace(req.GetId())
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	v, err := s.svc.GetInvite(ctx, id)
	if err != nil {
		return nil, tenantInviteStatusErr(ctx, "get", err)
	}
	return &tenantinvitesv1.GetTenantInviteResponse{Invite: s.toProto(v)}, nil
}

func (s *tenantInviteAdminService) ListTenantInvites(ctx context.Context, req *tenantinvitesv1.ListTenantInvitesRequest) (*tenantinvitesv1.ListTenantInvitesResponse, error) {
	afterAt, afterID, err := decodeEmailCursor(req.GetPageToken())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "page_token is malformed")
	}
	rows, more, err := s.svc.ListInvites(ctx, signupaccess.ListInvitesInput{
		Status: strings.TrimSpace(req.GetStatus()), EmailQuery: req.GetEmailQuery(), PageSize: int(req.GetPageSize()),
		AfterCreatedAt: afterAt, AfterID: afterID,
	})
	if err != nil {
		return nil, tenantInviteStatusErr(ctx, "list", err)
	}
	out := &tenantinvitesv1.ListTenantInvitesResponse{}
	for i := range rows {
		out.Invites = append(out.Invites, s.toProto(&rows[i]))
	}
	if more && len(rows) > 0 {
		last := rows[len(rows)-1]
		out.NextPageToken = encodeEmailCursor(last.CreatedAt, last.ID)
	}
	return out, nil
}

func (s *tenantInviteAdminService) ResendTenantInvite(ctx context.Context, req *tenantinvitesv1.ResendTenantInviteRequest) (*tenantinvitesv1.ResendTenantInviteResponse, error) {
	id := strings.TrimSpace(req.GetId())
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	v, err := s.svc.ResendInvite(ctx, id, int(req.GetExpiresInHours()))
	if err != nil {
		return nil, tenantInviteStatusErr(ctx, "resend", err)
	}
	return &tenantinvitesv1.ResendTenantInviteResponse{Invite: s.toProto(v)}, nil
}

func (s *tenantInviteAdminService) RevokeTenantInvite(ctx context.Context, req *tenantinvitesv1.RevokeTenantInviteRequest) (*tenantinvitesv1.RevokeTenantInviteResponse, error) {
	id := strings.TrimSpace(req.GetId())
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	v, err := s.svc.RevokeInvite(ctx, id)
	if err != nil {
		return nil, tenantInviteStatusErr(ctx, "revoke", err)
	}
	return &tenantinvitesv1.RevokeTenantInviteResponse{Invite: s.toProto(v)}, nil
}

func (s *tenantInviteAdminService) GetSignupSettings(ctx context.Context, _ *tenantinvitesv1.GetSignupSettingsRequest) (*tenantinvitesv1.GetSignupSettingsResponse, error) {
	st, err := s.svc.Settings(ctx)
	if err != nil {
		return nil, tenantInviteStatusErr(ctx, "get_settings", err)
	}
	return &tenantinvitesv1.GetSignupSettingsResponse{Settings: toSignupSettingsProto(st)}, nil
}

func (s *tenantInviteAdminService) UpdateSignupSettings(ctx context.Context, req *tenantinvitesv1.UpdateSignupSettingsRequest) (*tenantinvitesv1.UpdateSignupSettingsResponse, error) {
	st, err := s.svc.UpdateSettings(ctx, signupaccess.UpdateSettingsInput{
		Mode: req.GetMode(), MaxOwnedWorkspacesPerAccount: int(req.GetMaxOwnedWorkspacesPerAccount()), UpdatedBy: req.GetUpdatedBy(),
	})
	if err != nil {
		return nil, tenantInviteStatusErr(ctx, "update_settings", err)
	}
	return &tenantinvitesv1.UpdateSignupSettingsResponse{Settings: toSignupSettingsProto(st)}, nil
}

// strVal reads an optional string, empty when unset.
func strVal(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func toSignupSettingsProto(st models.SignupSettings) *tenantinvitesv1.SignupSettings {
	out := &tenantinvitesv1.SignupSettings{
		Mode: st.Mode, MaxOwnedWorkspacesPerAccount: int32(st.MaxOwnedWorkspacesPerAccount), UpdatedBy: st.UpdatedBy,
	}
	if !st.UpdatedAt.IsZero() {
		out.UpdatedAt = timestamppb.New(st.UpdatedAt)
	}
	return out
}

func (s *tenantInviteAdminService) toProto(v *models.TenantInviteView) *tenantinvitesv1.TenantInvite {
	out := &tenantinvitesv1.TenantInvite{
		Id: v.ID, Email: v.Email, TierId: strVal(v.TierID), TierName: v.TierName, TierPurchasable: v.TierPurchasable,
		Message: strVal(v.Message), Note: strVal(v.Note), CreatedBy: v.CreatedBy,
		Status: v.DerivedStatus(s.now()), ExpiresAt: timestamppb.New(v.ExpiresAt), SentCount: int32(v.SentCount),
		LastSentAt: timestamppb.New(v.LastSentAt), EmailStatus: v.EmailStatus, EmailLastEventAt: ts(v.EmailLastEventAt),
		RedeemedAt: ts(v.RedeemedAt), RedeemedTenantId: strVal(v.RedeemedTenantID), RedeemedTenantName: v.RedeemedTenantName,
		RedeemedTenantComplimentary: v.RedeemedTenantComplimentary, RevokedAt: ts(v.RevokedAt), CreatedAt: timestamppb.New(v.CreatedAt),
	}
	for _, g := range v.Groups {
		out.Groups = append(out.Groups, &tenantinvitesv1.TenantInviteGroup{Id: g.ID, Name: g.Name, Color: g.Color})
	}
	return out
}
