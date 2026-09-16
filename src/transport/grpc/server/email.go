package server

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	emailv1 "github.com/ogen-app/ogen/gen/email/v1"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/email/resend"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

const (
	defaultEmailPageSize = 50
	maxEmailPageSize     = 200
)

// EmailBodyGetter fetches one email's rendered body from Resend. *resend.Client
// satisfies it; a nil getter (no Resend key configured) makes GetTenantEmail
// return the summary + timeline with the body marked unavailable (CON-298).
type EmailBodyGetter interface {
	Get(ctx context.Context, id string) (*resend.EmailDetail, error)
}

// emailAdminService adapts the email_logs + email_events repositories (plus a
// live Resend body fetch) to the generated EmailAdminServiceServer (CON-298) —
// the operator-facing surface Harbor's per-tenant Emails tab (CON-192) consumes.
// tenant_id is authoritative server-side scoping on every call; the caller's
// value is never trusted for a cross-tenant read.
type emailAdminService struct {
	emailv1.UnimplementedEmailAdminServiceServer
	logs   repository.EmailLogRepository
	events repository.EmailEventRepository
	bodies EmailBodyGetter
}

func newEmailAdminService(logs repository.EmailLogRepository, events repository.EmailEventRepository, bodies EmailBodyGetter) *emailAdminService {
	return &emailAdminService{logs: logs, events: events, bodies: bodies}
}

func (s *emailAdminService) ListTenantEmails(ctx context.Context, req *emailv1.ListTenantEmailsRequest) (*emailv1.ListTenantEmailsResponse, error) {
	tenantID := strings.TrimSpace(req.GetTenantId())
	if tenantID == "" {
		return nil, status.Error(codes.InvalidArgument, "tenant_id is required")
	}
	pageSize := int(req.GetPageSize())
	if pageSize <= 0 {
		pageSize = defaultEmailPageSize
	}
	if pageSize > maxEmailPageSize {
		pageSize = maxEmailPageSize
	}
	cursorAt, cursorID, err := decodeEmailCursor(req.GetPageToken())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid page_token")
	}

	f := repository.EmailListFilter{
		TenantID:        tenantID,
		Kind:            models.EmailKind(strings.TrimSpace(req.GetFilter().GetKind())),
		Recipient:       strings.TrimSpace(req.GetFilter().GetRecipientSubstr()),
		Limit:           pageSize + 1, // one extra row signals a next page exists
		CursorCreatedAt: cursorAt,
		CursorID:        cursorID,
	}
	for _, st := range req.GetFilter().GetStatus() {
		if v := strings.TrimSpace(st); v != "" {
			f.Statuses = append(f.Statuses, models.EmailLogStatus(v))
		}
	}

	rows, err := s.logs.ListByTenant(ctx, f)
	if err != nil {
		return nil, s.internal(ctx, "list tenant emails", err)
	}

	var next string
	if len(rows) > pageSize {
		last := rows[pageSize-1]
		next = encodeEmailCursor(last.CreatedAt, last.ID)
		rows = rows[:pageSize]
	}
	out := make([]*emailv1.EmailSummary, 0, len(rows))
	for i := range rows {
		out = append(out, emailSummaryProto(&rows[i]))
	}
	return &emailv1.ListTenantEmailsResponse{Emails: out, NextPageToken: next}, nil
}

func (s *emailAdminService) GetTenantEmail(ctx context.Context, req *emailv1.GetTenantEmailRequest) (*emailv1.GetTenantEmailResponse, error) {
	tenantID := strings.TrimSpace(req.GetTenantId())
	if tenantID == "" {
		return nil, status.Error(codes.InvalidArgument, "tenant_id is required")
	}
	emailID := strings.TrimSpace(req.GetEmailId())
	if emailID == "" {
		return nil, status.Error(codes.InvalidArgument, "email_id is required")
	}

	log, err := s.logs.GetByIDForTenant(ctx, tenantID, emailID)
	if err != nil {
		return nil, s.internal(ctx, "get tenant email", err)
	}
	if log == nil {
		return nil, status.Error(codes.NotFound, "email not found")
	}
	events, err := s.events.ListByEmailLogID(ctx, log.ID)
	if err != nil {
		return nil, s.internal(ctx, "get tenant email events", err)
	}

	detail := &emailv1.EmailDetail{
		Summary: emailSummaryProto(log),
		Events:  make([]*emailv1.EmailEvent, 0, len(events)),
	}
	for i := range events {
		detail.Events = append(detail.Events, &emailv1.EmailEvent{
			Type:       string(events[i].Type),
			OccurredAt: timestamppb.New(events[i].OccurredAt),
		})
	}

	// The rendered body is fetched live from Resend. Degrade to body_available =
	// false (summary + timeline still returned) when the key is unset, the
	// message was never accepted (no provider id), or the fetch fails.
	if s.bodies != nil && log.ProviderMessageID != "" {
		body, ferr := s.bodies.Get(ctx, log.ProviderMessageID)
		switch {
		case ferr == nil && body != nil:
			detail.BodyAvailable = true
			detail.Subject = body.Subject
			detail.Html = body.HTML
			detail.Text = body.Text
			detail.From = body.From
			detail.ReplyTo = body.ReplyTo
			detail.Cc = body.CC
			detail.Bcc = body.BCC
		case errors.Is(ferr, resend.ErrDisabled):
			// No key configured — expected in some environments, not an error.
		default:
			slog.WarnContext(ctx, "resend body fetch failed", logging.AttrComponent, "grpcserver", "email_id", log.ID, logging.AttrError, ferr)
		}
	}
	return &emailv1.GetTenantEmailResponse{Email: detail}, nil
}

// emailSummaryProto maps an email_logs row (+ rollup) to the wire summary.
func emailSummaryProto(l *models.EmailLog) *emailv1.EmailSummary {
	out := &emailv1.EmailSummary{
		Id:          l.ID,
		TenantId:    l.TenantID,
		ToEmail:     l.ToEmail,
		TemplateId:  l.TemplateID,
		Kind:        string(l.Kind),
		Status:      string(l.Status),
		LastEvent:   l.LastEvent,
		OpensCount:  int32(l.OpensCount),
		ClicksCount: int32(l.ClicksCount),
		Error:       l.Error,
		CreatedAt:   timestamppb.New(l.CreatedAt),
	}
	if !l.LastEventAt.IsZero() {
		out.LastEventAt = timestamppb.New(l.LastEventAt)
	}
	return out
}

// internal logs an unexpected error (op + ids only, never sensitive values) and
// returns a generic Internal status so DB internals never cross the wire.
func (s *emailAdminService) internal(ctx context.Context, op string, err error) error {
	slog.ErrorContext(ctx, "email admin "+op, logging.AttrComponent, "grpcserver", logging.AttrError, err)
	return status.Error(codes.Internal, op+" failed")
}

// encodeEmailCursor / decodeEmailCursor make an opaque keyset cursor over
// (created_at, id). base64("<unixNanos>|<id>"); clients treat it as opaque.
func encodeEmailCursor(createdAt time.Time, id string) string {
	raw := strconv.FormatInt(createdAt.UnixNano(), 10) + "|" + id
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeEmailCursor(token string) (time.Time, string, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return time.Time{}, "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return time.Time{}, "", err
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 || parts[1] == "" {
		return time.Time{}, "", errors.New("malformed cursor")
	}
	nanos, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return time.Time{}, "", err
	}
	return time.Unix(0, nanos).UTC(), parts[1], nil
}
