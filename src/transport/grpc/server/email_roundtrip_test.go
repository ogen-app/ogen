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

	emailv1 "github.com/ogen-app/ogen/gen/email/v1"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/email/resend"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/pgtest"
)

// fakeBodyGetter stands in for the live Resend fetch: it returns a canned body
// for one message id and ErrDisabled otherwise (so the degrade path is covered).
type fakeBodyGetter struct{}

func (fakeBodyGetter) Get(_ context.Context, id string) (*resend.EmailDetail, error) {
	if id == "msg_x" {
		return &resend.EmailDetail{Subject: "Hello", HTML: "<p>hi</p>", From: "n@o.com", CC: []string{"c@o.com"}}, nil
	}
	return nil, resend.ErrDisabled
}

// TestEmailAdminRoundTrip exercises the full Harbor path for the CON-298
// EmailAdminService over a live listener + shared-token interceptor + generated
// client on a migrated Postgres: keyset pagination, tenant scoping, filters, the
// rollup surfaced on the summary, and the live-body degrade path.
func TestEmailAdminRoundTrip(t *testing.T) {
	const token = "email-admin-token"

	db := pgtest.MustDB()
	db.DB.SetMaxOpenConns(1)
	db.DB.SetMaxIdleConns(1)
	// FK-free seeding on a single pinned connection (mirrors the repo tests).
	if _, err := db.Exec("SET session_replication_role = replica"); err != nil {
		t.Fatalf("disable fks: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	logs := repository.NewEmailLogRepository(db)
	events := repository.NewEmailEventRepository(db)

	srv, err := New(token, nil,
		repository.NewTenantTierRepository(db), repository.NewTenantGroupRepository(db), repository.NewTenantRepository(db),
		repository.NewPlatformRepository(db), repository.NewPlatformGlobalLimitsRepository(db),
		repository.NewTenantTierVersionRepository(db), repository.NewTenantTierAssignmentRepository(db),
		logs, events, repository.NewEmailBodyRepository(db), fakeBodyGetter{},
		repository.NewAnnouncementRepository(db), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	newClient := func(tok string) emailv1.EmailAdminServiceClient {
		conn, err := grpc.NewClient(
			lis.Addr().String(),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
				ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+tok)
				return invoker(ctx, method, req, reply, cc, opts...)
			}),
		)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		return emailv1.NewEmailAdminServiceClient(conn)
	}

	ctx := t.Context()
	cli := newClient(token)

	// The shared interceptor rejects a bad token.
	if _, err := newClient("nope").ListTenantEmails(ctx, &emailv1.ListTenantEmailsRequest{TenantId: "tn-a"}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("bad token: got %v want Unauthenticated", status.Code(err))
	}
	// tenant_id is required.
	if _, err := cli.ListTenantEmails(ctx, &emailv1.ListTenantEmailsRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("missing tenant_id: got %v want InvalidArgument", status.Code(err))
	}

	base := time.Now().UTC().Truncate(time.Second)
	seed := func(id, tenant, msgID string, at time.Time) {
		t.Helper()
		if err := logs.Insert(ctx, &models.EmailLog{
			ID: id, TenantID: tenant, TemplateID: "welcome", Kind: models.EmailKindTransactional,
			ToEmail: "u@x.com", Status: models.EmailLogSent, Provider: models.ProviderResend,
			ProviderMessageID: msgID, CreatedAt: at,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	seed("a1", "tn-a", "msg_x", base.Add(time.Minute))
	seed("a2", "tn-a", "", base.Add(2*time.Minute)) // no provider id → body unavailable
	seed("b1", "tn-b", "msg_y", base.Add(3*time.Minute))
	if err := events.Record(ctx, &models.EmailEvent{ID: "ev1", EmailLogID: "a1", ProviderMessageID: "msg_x", Type: models.EmailEventOpened, OccurredAt: base.Add(90 * time.Second), SvixID: "sv1"}); err != nil {
		t.Fatalf("record event: %v", err)
	}

	// Keyset pagination, newest-first: page 1 = a2, page 2 = a1.
	p1, err := cli.ListTenantEmails(ctx, &emailv1.ListTenantEmailsRequest{TenantId: "tn-a", PageSize: 1})
	if err != nil {
		t.Fatalf("list p1: %v", err)
	}
	if len(p1.Emails) != 1 || p1.Emails[0].Id != "a2" || p1.NextPageToken == "" {
		t.Fatalf("p1: emails=%v next=%q", summaryIDs(p1.Emails), p1.NextPageToken)
	}
	p2, err := cli.ListTenantEmails(ctx, &emailv1.ListTenantEmailsRequest{TenantId: "tn-a", PageSize: 1, PageToken: p1.NextPageToken})
	if err != nil {
		t.Fatalf("list p2: %v", err)
	}
	if len(p2.Emails) != 1 || p2.Emails[0].Id != "a1" {
		t.Fatalf("p2: %v", summaryIDs(p2.Emails))
	}
	if p2.Emails[0].OpensCount != 1 || p2.Emails[0].Status != string(models.EmailLogOpened) {
		t.Fatalf("a1 rollup on summary: opens=%d status=%s", p2.Emails[0].OpensCount, p2.Emails[0].Status)
	}

	// Tenant scoping: b1 never appears under tn-a.
	all, err := cli.ListTenantEmails(ctx, &emailv1.ListTenantEmailsRequest{TenantId: "tn-a", PageSize: 50})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	for _, e := range all.Emails {
		if e.Id == "b1" {
			t.Fatal("cross-tenant leak: b1 in tn-a list")
		}
	}

	// Detail with a live body + event timeline.
	det, err := cli.GetTenantEmail(ctx, &emailv1.GetTenantEmailRequest{TenantId: "tn-a", EmailId: "a1"})
	if err != nil {
		t.Fatalf("detail a1: %v", err)
	}
	if !det.Email.BodyAvailable || det.Email.Subject != "Hello" || len(det.Email.Cc) != 1 {
		t.Fatalf("a1 body: %+v", det.Email)
	}
	if len(det.Email.Events) != 1 || det.Email.Events[0].Type != string(models.EmailEventOpened) {
		t.Fatalf("a1 timeline: %+v", det.Email.Events)
	}

	// Detail with no provider id degrades to body_available=false.
	det2, err := cli.GetTenantEmail(ctx, &emailv1.GetTenantEmailRequest{TenantId: "tn-a", EmailId: "a2"})
	if err != nil {
		t.Fatalf("detail a2: %v", err)
	}
	if det2.Email.BodyAvailable {
		t.Fatal("a2 has no provider id; body must be unavailable")
	}

	// Stored-body preference (CON-306): a row with BOTH a persisted body and a
	// live-fetchable provider id must serve the stored body, so it still renders
	// after the Resend message ages out. Seeded here (after the pagination
	// assertions) so it doesn't perturb them.
	seed("a3", "tn-a", "msg_x", base.Add(4*time.Minute))
	if err := repository.NewEmailBodyRepository(db).Insert(ctx, &models.EmailBody{
		EmailLogID: "a3", Subject: "StoredSubject", HTML: "<p>stored</p>", Text: "stored",
		From: "stored@o.com", ReplyTo: "reply@o.com",
	}); err != nil {
		t.Fatalf("seed a3 body: %v", err)
	}
	det3, err := cli.GetTenantEmail(ctx, &emailv1.GetTenantEmailRequest{TenantId: "tn-a", EmailId: "a3"})
	if err != nil {
		t.Fatalf("detail a3: %v", err)
	}
	if !det3.Email.BodyAvailable || det3.Email.Subject != "StoredSubject" || det3.Email.Text != "stored" {
		t.Fatalf("a3 must serve the persisted body, got %+v", det3.Email)
	}
	if det3.Email.From != "stored@o.com" || det3.Email.ReplyTo != "reply@o.com" {
		t.Fatalf("a3 persisted envelope: %+v", det3.Email)
	}
	// The live getter returns a CC for msg_x; its absence proves the stored path
	// short-circuited the live fetch.
	if len(det3.Email.Cc) != 0 {
		t.Fatalf("stored-body path must not consult the live fetch (got cc=%v)", det3.Email.Cc)
	}

	// Cross-tenant detail is NotFound (never leaks another tenant's row).
	if _, err := cli.GetTenantEmail(ctx, &emailv1.GetTenantEmailRequest{TenantId: "tn-a", EmailId: "b1"}); status.Code(err) != codes.NotFound {
		t.Fatalf("cross-tenant detail: got %v want NotFound", status.Code(err))
	}
}

func summaryIDs(rows []*emailv1.EmailSummary) []string {
	out := make([]string, len(rows))
	for i := range rows {
		out[i] = rows[i].GetId()
	}
	return out
}

// TestEmailCursorRoundTrip is a pure check of the opaque keyset cursor codec.
func TestEmailCursorRoundTrip(t *testing.T) {
	at := time.Unix(1_700_000_000, 123_000).UTC() // microsecond precision, like Postgres
	tok := encodeEmailCursor(at, "abc123")
	gotAt, gotID, err := decodeEmailCursor(tok)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !gotAt.Equal(at) || gotID != "abc123" {
		t.Fatalf("round-trip: at=%v id=%q", gotAt, gotID)
	}
	// Empty token = first page, no error.
	if _, _, err := decodeEmailCursor(""); err != nil {
		t.Fatalf("empty token: %v", err)
	}
	// Malformed token is rejected.
	if _, _, err := decodeEmailCursor("!!!not-base64!!!"); err == nil {
		t.Fatal("malformed token accepted; want error")
	}
}
