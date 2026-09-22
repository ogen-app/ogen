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

	announcementsv1 "github.com/ogen-app/ogen/gen/announcements/v1"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/pgtest"
)

// TestAnnouncementAdminRoundTrip exercises the full Harbor path for the CON-230
// AnnouncementAdminService over a live listener + shared-token interceptor +
// generated client on a migrated Postgres: validation, lifecycle (create ->
// publish -> archive/delete), keyset pagination, the engagement stats rollup +
// eligible-audience denominator, and draft-only delete.
func TestAnnouncementAdminRoundTrip(t *testing.T) {
	const token = "announcement-admin-token"

	db := pgtest.MustDB()
	db.DB.SetMaxOpenConns(1)
	db.DB.SetMaxIdleConns(1)
	// FK-free seeding on a single pinned connection (mirrors the repo tests).
	if _, err := db.Exec("SET session_replication_role = replica"); err != nil {
		t.Fatalf("disable fks: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	annRepo := repository.NewAnnouncementRepository(db)

	srv, err := New(token, nil,
		repository.NewTenantTierRepository(db), repository.NewTenantGroupRepository(db), repository.NewTenantRepository(db),
		repository.NewPlatformRepository(db), repository.NewPlatformGlobalLimitsRepository(db),
		repository.NewTenantTierVersionRepository(db), repository.NewTenantTierAssignmentRepository(db),
		repository.NewEmailLogRepository(db), repository.NewEmailEventRepository(db), nil, nil,
		nil, nil, "",
		annRepo, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	newClient := func(tok string) announcementsv1.AnnouncementAdminServiceClient {
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
		return announcementsv1.NewAnnouncementAdminServiceClient(conn)
	}

	ctx := t.Context()
	cli := newClient(token)

	// One active tenant on the "pro" tier + a user, so the eligible-audience
	// denominator is deterministic (1 tenant / 1 user).
	now := time.Now().UTC()
	if _, err := db.NewInsert().Model(&models.Tenant{ID: "tn-pro", Name: "Pro", Slug: "tn-pro", TierID: "pro", Status: models.TenantStatusActive, CreatedAt: now, UpdatedAt: now}).Exec(ctx); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	if _, err := db.NewInsert().Model(&models.User{ID: "u-pro", AccountID: "u-pro", TenantID: "tn-pro", Name: "U", Email: "u@x.com", CreatedAt: now, UpdatedAt: now}).Exec(ctx); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	// The shared interceptor rejects a bad token.
	if _, err := newClient("nope").ListAnnouncements(ctx, &announcementsv1.ListAnnouncementsRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("bad token: got %v want Unauthenticated", status.Code(err))
	}

	// Validation: missing title/body, and a half-set CTA, are InvalidArgument.
	if _, err := cli.CreateAnnouncement(ctx, &announcementsv1.CreateAnnouncementRequest{Announcement: &announcementsv1.AnnouncementInput{Body: "b"}}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("missing title: got %v want InvalidArgument", status.Code(err))
	}
	if _, err := cli.CreateAnnouncement(ctx, &announcementsv1.CreateAnnouncementRequest{Announcement: &announcementsv1.AnnouncementInput{Title: "t", Body: "b", CtaLabel: "Go"}}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("half CTA: got %v want InvalidArgument", status.Code(err))
	}

	// Create a pro-tier-targeted announcement (draft).
	created, err := cli.CreateAnnouncement(ctx, &announcementsv1.CreateAnnouncementRequest{Announcement: &announcementsv1.AnnouncementInput{
		Title: "New feature", Body: "hooray", CtaLabel: "Learn more", CtaUrl: "https://x/y",
		TargetTierIds: []string{"pro"},
	}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id := created.GetAnnouncement().GetId()
	if created.GetAnnouncement().GetStatus() != string(models.AnnouncementStatusDraft) {
		t.Fatalf("created status = %q want draft", created.GetAnnouncement().GetStatus())
	}
	if got := created.GetAnnouncement().GetTargetTierIds(); len(got) != 1 || got[0] != "pro" {
		t.Fatalf("target tiers = %v want [pro]", got)
	}

	// Publish stamps published_at.
	pub, err := cli.SetAnnouncementStatus(ctx, &announcementsv1.SetAnnouncementStatusRequest{Id: id, Status: "published"})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if pub.GetAnnouncement().GetStatus() != "published" || pub.GetAnnouncement().GetPublishedAt() == nil {
		t.Fatalf("publish: status=%q published_at=%v", pub.GetAnnouncement().GetStatus(), pub.GetAnnouncement().GetPublishedAt())
	}

	// Seed engagement through the tenant path (repo), then read it back via Get.
	// The audience matches the pro-tier targeting (the gate the tenant handler applies).
	if ok, err := annRepo.RecordClick(ctx, id, repository.AnnouncementAudience{TenantID: "tn-pro", TierID: "pro", UserID: "u-pro"}); err != nil || !ok {
		t.Fatalf("seed click: ok=%v err=%v", ok, err)
	}
	got, err := cli.GetAnnouncement(ctx, &announcementsv1.GetAnnouncementRequest{Id: id})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	st := got.GetItem().GetStats()
	if st.GetUniqueUsersClicked() != 1 || st.GetUniqueTenantsClicked() != 1 {
		t.Fatalf("clicked stats: users=%d tenants=%d want 1/1", st.GetUniqueUsersClicked(), st.GetUniqueTenantsClicked())
	}
	if st.GetEligibleTenants() != 1 || st.GetEligibleUsers() != 1 {
		t.Fatalf("eligible: tenants=%d users=%d want 1/1", st.GetEligibleTenants(), st.GetEligibleUsers())
	}

	// SetStatus rejects an invalid status; Get on an unknown id is NotFound.
	if _, err := cli.SetAnnouncementStatus(ctx, &announcementsv1.SetAnnouncementStatusRequest{Id: id, Status: "live"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad status: got %v want InvalidArgument", status.Code(err))
	}
	if _, err := cli.GetAnnouncement(ctx, &announcementsv1.GetAnnouncementRequest{Id: "nope"}); status.Code(err) != codes.NotFound {
		t.Fatalf("get unknown: got %v want NotFound", status.Code(err))
	}

	// Delete: a published announcement is a FailedPrecondition; a draft deletes.
	if _, err := cli.DeleteAnnouncement(ctx, &announcementsv1.DeleteAnnouncementRequest{Id: id}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("delete published: got %v want FailedPrecondition", status.Code(err))
	}
	draft, err := cli.CreateAnnouncement(ctx, &announcementsv1.CreateAnnouncementRequest{Announcement: &announcementsv1.AnnouncementInput{Title: "d", Body: "b", TargetAll: true}})
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}
	if _, err := cli.DeleteAnnouncement(ctx, &announcementsv1.DeleteAnnouncementRequest{Id: draft.GetAnnouncement().GetId()}); err != nil {
		t.Fatalf("delete draft: %v", err)
	}

	// Publish a second announcement, then page the published history one at a
	// time: page 1 advances to a distinct page 2.
	pub2, err := cli.CreateAnnouncement(ctx, &announcementsv1.CreateAnnouncementRequest{Announcement: &announcementsv1.AnnouncementInput{Title: "second", Body: "b", TargetAll: true}})
	if err != nil {
		t.Fatalf("create second: %v", err)
	}
	if _, err := cli.SetAnnouncementStatus(ctx, &announcementsv1.SetAnnouncementStatusRequest{Id: pub2.GetAnnouncement().GetId(), Status: "published"}); err != nil {
		t.Fatalf("publish second: %v", err)
	}
	p1, err := cli.ListAnnouncements(ctx, &announcementsv1.ListAnnouncementsRequest{Status: "published", PageSize: 1})
	if err != nil {
		t.Fatalf("list p1: %v", err)
	}
	if len(p1.GetItems()) != 1 || p1.GetNextPageToken() == "" {
		t.Fatalf("p1: items=%d next=%q", len(p1.GetItems()), p1.GetNextPageToken())
	}
	p2, err := cli.ListAnnouncements(ctx, &announcementsv1.ListAnnouncementsRequest{Status: "published", PageSize: 1, PageToken: p1.GetNextPageToken()})
	if err != nil {
		t.Fatalf("list p2: %v", err)
	}
	if len(p2.GetItems()) != 1 || p2.GetItems()[0].GetAnnouncement().GetId() == p1.GetItems()[0].GetAnnouncement().GetId() {
		t.Fatalf("keyset did not advance: p1=%s p2=%v", p1.GetItems()[0].GetAnnouncement().GetId(), listIDs(p2.GetItems()))
	}
}

func listIDs(rows []*announcementsv1.AnnouncementWithStats) []string {
	out := make([]string, len(rows))
	for i := range rows {
		out[i] = rows[i].GetAnnouncement().GetId()
	}
	return out
}

// TestAnnouncementCursorRoundTrip is a pure check of the opaque keyset cursor codec.
func TestAnnouncementCursorRoundTrip(t *testing.T) {
	at := time.Unix(1_700_000_000, 123_000).UTC() // microsecond precision, like Postgres
	tok := encodeAnnouncementCursor(at, "abc123")
	gotAt, gotID, err := decodeAnnouncementCursor(tok)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !gotAt.Equal(at) || gotID != "abc123" {
		t.Fatalf("round-trip: at=%v id=%q", gotAt, gotID)
	}
	if _, _, err := decodeAnnouncementCursor(""); err != nil {
		t.Fatalf("empty token: %v", err)
	}
	if _, _, err := decodeAnnouncementCursor("!!!not-base64!!!"); err == nil {
		t.Fatal("malformed token accepted; want error")
	}
}
