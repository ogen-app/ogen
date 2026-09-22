package server

import (
	"context"
	"database/sql"
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

// EmailBodyGetter fetches one email's rendered body live from Resend.
// *resend.Client satisfies it; a nil getter (no Resend key configured) makes
// GetTenantEmail fall through to the stored body / marks it unavailable (CON-298).
type EmailBodyGetter interface {
	Get(ctx context.Context, id string) (*resend.EmailDetail, error)
}

// AdminEmailEnqueuer enqueues one admin_tenant_registered operator notification
// (CON-229). *queues.Enqueuer satisfies it; it is kept as a local interface so
// this transport package doesn't import the jobs/queues package.
type AdminEmailEnqueuer interface {
	EnqueueAdminTenantRegisteredEmail(ctx context.Context, tenantID, recipient string, vars map[string]string) error
}

// emailAdminService adapts the email_logs + email_events repositories (plus the
// persisted body store and a live Resend body fetch) to the generated
// EmailAdminServiceServer (CON-298) — the operator-facing surface Harbor's
// per-tenant Emails tab (CON-192) consumes. tenant_id is authoritative
// server-side scoping on every call; the caller's value is never trusted for a
// cross-tenant read.
type emailAdminService struct {
	emailv1.UnimplementedEmailAdminServiceServer
	logs      repository.EmailLogRepository
	events    repository.EmailEventRepository
	bodyStore repository.EmailBodyRepository // CON-306: body persisted at send (preferred)
	liveBody  EmailBodyGetter                // CON-298: live Resend fetch (fallback)
	// CON-229 admin-notification send path. tenants/users hydrate the newly-
	// registered tenant's details; enqueuer fans out one durable send per operator
	// recipient; harborBaseURL builds the "View in Harbor" deep link (empty ⇒ no
	// link). All are nil/empty-safe: an unwired path is a soft no-op.
	tenants       repository.TenantRepository
	users         repository.UserRepository
	enqueuer      AdminEmailEnqueuer
	harborBaseURL string
}

func newEmailAdminService(
	logs repository.EmailLogRepository,
	events repository.EmailEventRepository,
	bodyStore repository.EmailBodyRepository,
	liveBody EmailBodyGetter,
	tenants repository.TenantRepository,
	users repository.UserRepository,
	enqueuer AdminEmailEnqueuer,
	harborBaseURL string,
) *emailAdminService {
	return &emailAdminService{
		logs:          logs,
		events:        events,
		bodyStore:     bodyStore,
		liveBody:      liveBody,
		tenants:       tenants,
		users:         users,
		enqueuer:      enqueuer,
		harborBaseURL: strings.TrimRight(strings.TrimSpace(harborBaseURL), "/"),
	}
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

	// Prefer the body persisted at send (CON-306): it renders even after the
	// Resend message ages out of retention or the key is unset. Fall back to the
	// live Resend fetch only for rows sent before CON-306 shipped (no stored body).
	// Either way, degrade to body_available = false — summary + timeline are still
	// returned — and log WHY so a missing body is diagnosable (§5.4) rather than a
	// silent false.
	if s.serveStoredBody(ctx, detail, log.ID) {
		return &emailv1.GetTenantEmailResponse{Email: detail}, nil
	}
	s.serveLiveBody(ctx, detail, log)
	return &emailv1.GetTenantEmailResponse{Email: detail}, nil
}

// NotifyOperatorsTenantRegistered fans out the admin_tenant_registered
// operator-notification email to the given recipients (CON-229). It re-loads the
// tenant by id (authoritative) and enqueues one durable send per recipient;
// idempotency by (tenant, recipient) means a retried Harbor callback never
// double-sends. Recipients are trimmed, lower-cased, and de-duplicated; an empty
// resulting set — or an unwired notification path — is a clean no-op.
func (s *emailAdminService) NotifyOperatorsTenantRegistered(ctx context.Context, req *emailv1.NotifyOperatorsTenantRegisteredRequest) (*emailv1.NotifyOperatorsTenantRegisteredResponse, error) {
	tenantID := strings.TrimSpace(req.GetTenantId())
	if tenantID == "" {
		return nil, status.Error(codes.InvalidArgument, "tenant_id is required")
	}

	seen := make(map[string]struct{}, len(req.GetRecipientEmails()))
	recipients := make([]string, 0, len(req.GetRecipientEmails()))
	for _, r := range req.GetRecipientEmails() {
		r = strings.ToLower(strings.TrimSpace(r))
		if r == "" {
			continue
		}
		if _, dup := seen[r]; dup {
			continue
		}
		seen[r] = struct{}{}
		recipients = append(recipients, r)
	}
	// No recipients is a genuine no-op — ack so Harbor doesn't retry.
	if len(recipients) == 0 {
		return &emailv1.NotifyOperatorsTenantRegisteredResponse{Enqueued: 0}, nil
	}
	// But an UNWIRED notification path (enqueuer/tenant repo absent, e.g. the
	// insert-only enqueuer failed to build at boot) must NOT be acked as done —
	// that would permanently drop the notification. Return Unavailable so Harbor's
	// durable webhook job retries once the path is wired.
	if s.enqueuer == nil || s.tenants == nil {
		return nil, status.Error(codes.Unavailable, "tenant registration notifications are unavailable")
	}

	tenant, err := s.tenants.GetByIDWithClassification(ctx, tenantID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && tenant == nil) {
		return nil, status.Error(codes.NotFound, "tenant not found")
	} else if err != nil {
		return nil, s.internal(ctx, "notify operators: load tenant", err)
	}

	vars := s.tenantRegisteredVars(ctx, tenant)

	enqueued := 0
	for _, r := range recipients {
		if err := s.enqueuer.EnqueueAdminTenantRegisteredEmail(ctx, tenantID, r, vars); err != nil {
			return nil, s.internal(ctx, "notify operators: enqueue send", err)
		}
		enqueued++
	}
	return &emailv1.NotifyOperatorsTenantRegisteredResponse{Enqueued: int32(enqueued)}, nil
}

// tenantRegisteredVars builds the admin_tenant_registered template variables
// from the tenant (+ its owner). Owner lookup is best-effort: a failure logs and
// leaves the owner fields blank rather than aborting the notification.
func (s *emailAdminService) tenantRegisteredVars(ctx context.Context, t *models.Tenant) map[string]string {
	tier := t.TierID
	if t.Tier != nil && t.Tier.Name != "" {
		tier = t.Tier.Name
	}
	ownerName, ownerEmail := "", ""
	if s.users != nil {
		if owners, err := s.users.ListOwnersByTenant(ctx, t.ID); err != nil {
			slog.WarnContext(ctx, "notify operators: owner lookup failed", logging.AttrComponent, "grpcserver", "tenant_id", t.ID, logging.AttrError, err)
		} else if len(owners) > 0 {
			ownerName, ownerEmail = owners[0].Name, owners[0].Email
		}
	}
	tenantURL := ""
	if s.harborBaseURL != "" {
		tenantURL = s.harborBaseURL + "/tenants/" + t.ID
	}
	return map[string]string{
		"workspace_name": t.Name,
		"tenant_id":      t.ID,
		"tenant_slug":    t.Slug,
		"owner_name":     ownerName,
		"owner_email":    ownerEmail,
		"tier":           tier,
		"status":         t.Status,
		"registered_at":  t.CreatedAt.UTC().Format("2006-01-02 15:04 MST"),
		"tenant_url":     tenantURL,
	}
}

// serveStoredBody fills detail from the persisted body (CON-306) and reports
// whether it did. A lookup error is logged and treated as "no stored body" so
// the caller falls back to the live Resend fetch.
func (s *emailAdminService) serveStoredBody(ctx context.Context, detail *emailv1.EmailDetail, emailLogID string) bool {
	if s.bodyStore == nil {
		return false
	}
	b, err := s.bodyStore.GetByEmailLogID(ctx, emailLogID)
	if err != nil {
		slog.WarnContext(ctx, "stored email body lookup failed", logging.AttrComponent, "grpcserver", "email_id", emailLogID, logging.AttrError, err)
		return false
	}
	if b == nil {
		return false
	}
	detail.BodyAvailable = true
	detail.Subject = b.Subject
	detail.Html = b.HTML
	detail.Text = b.Text
	detail.From = b.From
	detail.ReplyTo = b.ReplyTo
	return true
}

// serveLiveBody fills detail from a live Resend fetch (CON-298 fallback) and, on
// failure, logs a precise reason so an unavailable body is diagnosable (CON-306
// §5.4): key_unset / no_provider_message_id / resend_404 / resend_error.
func (s *emailAdminService) serveLiveBody(ctx context.Context, detail *emailv1.EmailDetail, log *models.EmailLog) {
	if s.liveBody == nil {
		slog.InfoContext(ctx, "email body unavailable", logging.AttrComponent, "grpcserver", "email_id", log.ID, "reason", "key_unset")
		return
	}
	if log.ProviderMessageID == "" {
		slog.InfoContext(ctx, "email body unavailable", logging.AttrComponent, "grpcserver", "email_id", log.ID, "reason", "no_provider_message_id")
		return
	}
	body, ferr := s.liveBody.Get(ctx, log.ProviderMessageID)
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
		slog.InfoContext(ctx, "email body unavailable", logging.AttrComponent, "grpcserver", "email_id", log.ID, "reason", "key_unset")
	case errors.Is(ferr, resend.ErrNotFound):
		slog.InfoContext(ctx, "email body unavailable", logging.AttrComponent, "grpcserver", "email_id", log.ID, "reason", "resend_404")
	default:
		slog.WarnContext(ctx, "email body unavailable", logging.AttrComponent, "grpcserver", "email_id", log.ID, "reason", "resend_error", logging.AttrError, ferr)
	}
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
