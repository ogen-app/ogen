package server

import (
	"context"
	"database/sql"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	tenantinvitesv1 "github.com/ogen-app/ogen/gen/tenantinvites/v1"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/pgtest"
	"github.com/ogen-app/ogen/src/usecase/signupaccess"
)

type noInviteEmails struct{}

func (noInviteEmails) EnqueueTenantInviteEmailTx(context.Context, *sql.Tx, string, int, string, string, string, string) error {
	return nil
}

// TestTenantInviteAdminRoundTrip drives TenantInviteAdminService over a live
// listener, the token interceptor and the generated client on a migrated
// database: settings, create (incl. replace and the precondition codes),
// list with keyset paging, resend and revoke.
func TestTenantInviteAdminRoundTrip(t *testing.T) {
	const token = "tenant-invite-admin-token"
	db := pgtest.MustDB()
	t.Cleanup(func() { _ = db.Close() })

	invites := signupaccess.New(signupaccess.Deps{
		DB: db, Invites: repository.NewTenantInviteRepository(db), Settings: repository.NewSignupSettingsRepository(db),
		Accounts: repository.NewAccountRepository(db), Tiers: repository.NewTenantTierRepository(db),
		TierVersions: repository.NewTenantTierVersionRepository(db), Groups: repository.NewTenantGroupRepository(db),
		Emails: noInviteEmails{}, AppBaseURL: "https://app",
	})
	srv, err := New(Deps{
		Token: token, Tiers: repository.NewTenantTierRepository(db), Groups: repository.NewTenantGroupRepository(db),
		Tenants: repository.NewTenantRepository(db), TierVersions: repository.NewTenantTierVersionRepository(db),
		TierAssignments: repository.NewTenantTierAssignmentRepository(db), TenantInvites: invites,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			return invoker(metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token), method, req, reply, cc, opts...)
		}))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	c := tenantinvitesv1.NewTenantInviteAdminServiceClient(conn)
	ctx := t.Context()

	code := func(err error) codes.Code { return status.Code(err) }

	// Settings: seeded open; whole-resource update.
	got, err := c.GetSignupSettings(ctx, &tenantinvitesv1.GetSignupSettingsRequest{})
	if err != nil || got.GetSettings().GetMode() != models.SignupModeOpen || got.GetSettings().GetMaxOwnedWorkspacesPerAccount() != 2 {
		t.Fatalf("settings = %v %v", got, err)
	}
	if _, err := c.UpdateSignupSettings(ctx, &tenantinvitesv1.UpdateSignupSettingsRequest{Mode: "closed", UpdatedBy: "ops"}); code(err) != codes.InvalidArgument {
		t.Fatalf("bad mode code = %v", code(err))
	}
	upd, err := c.UpdateSignupSettings(ctx, &tenantinvitesv1.UpdateSignupSettingsRequest{Mode: models.SignupModeInviteOnly, MaxOwnedWorkspacesPerAccount: 3, UpdatedBy: "ops@getogen.com"})
	if err != nil || upd.GetSettings().GetMode() != models.SignupModeInviteOnly || upd.GetSettings().GetUpdatedBy() != "ops@getogen.com" || upd.GetSettings().GetUpdatedAt() == nil {
		t.Fatalf("update = %v %v", upd, err)
	}

	// Create: preconditions and a hidden-tier invite.
	if _, err := c.CreateTenantInvite(ctx, &tenantinvitesv1.CreateTenantInviteRequest{Email: "a@x.co", TierId: "pro", CreatedBy: "ops"}); code(err) != codes.FailedPrecondition {
		t.Fatalf("draft tier code = %v", code(err))
	}
	if _, err := c.CreateTenantInvite(ctx, &tenantinvitesv1.CreateTenantInviteRequest{Email: "nope", TierId: "default", CreatedBy: "ops"}); code(err) != codes.InvalidArgument {
		t.Fatalf("bad email code = %v", code(err))
	}
	if _, err := db.NewInsert().Model(&models.Account{ID: "acc-1", Email: "taken@x.co", Name: "t"}).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateTenantInvite(ctx, &tenantinvitesv1.CreateTenantInviteRequest{Email: "taken@x.co", TierId: "default", CreatedBy: "ops"}); code(err) != codes.FailedPrecondition {
		t.Fatalf("existing account code = %v", code(err))
	}
	first, err := c.CreateTenantInvite(ctx, &tenantinvitesv1.CreateTenantInviteRequest{Email: "jane@acme.com", TierId: "default", Message: "hi", Note: "n", CreatedBy: "ops"})
	if err != nil || first.GetReplaced() || first.GetInvite().GetStatus() != models.TenantInvitePending || first.GetInvite().GetTierPurchasable() ||
		first.GetInvite().GetTierName() != "Default" || first.GetInvite().GetMessage() != "hi" || first.GetInvite().GetNote() != "n" {
		t.Fatalf("create = %v %v", first, err)
	}
	again, err := c.CreateTenantInvite(ctx, &tenantinvitesv1.CreateTenantInviteRequest{Email: "jane@acme.com", TierId: "default", CreatedBy: "ops"})
	if err != nil || !again.GetReplaced() {
		t.Fatalf("re-invite = %v %v", again, err)
	}
	for _, e := range []string{"b@x.co", "c@x.co"} {
		if _, err := c.CreateTenantInvite(ctx, &tenantinvitesv1.CreateTenantInviteRequest{Email: e, TierId: "default", CreatedBy: "ops"}); err != nil {
			t.Fatal(err)
		}
	}

	// List: four invites (the replaced one revoked), newest first, in pages.
	page, err := c.ListTenantInvites(ctx, &tenantinvitesv1.ListTenantInvitesRequest{PageSize: 2})
	if err != nil || len(page.GetInvites()) != 2 || page.GetNextPageToken() == "" {
		t.Fatalf("page 1 = %v %v", page, err)
	}
	rest, err := c.ListTenantInvites(ctx, &tenantinvitesv1.ListTenantInvitesRequest{PageSize: 10, PageToken: page.GetNextPageToken()})
	if err != nil || len(rest.GetInvites()) != 2 || rest.GetNextPageToken() != "" {
		t.Fatalf("page 2 = %v %v", rest, err)
	}
	revoked, err := c.ListTenantInvites(ctx, &tenantinvitesv1.ListTenantInvitesRequest{Status: models.TenantInviteRevoked})
	if err != nil || len(revoked.GetInvites()) != 1 || revoked.GetInvites()[0].GetId() != first.GetInvite().GetId() {
		t.Fatalf("revoked = %v %v", revoked, err)
	}
	if _, err := c.ListTenantInvites(ctx, &tenantinvitesv1.ListTenantInvitesRequest{PageToken: "%%%"}); code(err) != codes.InvalidArgument {
		t.Fatalf("bad token code = %v", code(err))
	}

	// Resend and revoke.
	id := again.GetInvite().GetId()
	res, err := c.ResendTenantInvite(ctx, &tenantinvitesv1.ResendTenantInviteRequest{Id: id})
	if err != nil || res.GetInvite().GetSentCount() != 2 {
		t.Fatalf("resend = %v %v", res, err)
	}
	rev, err := c.RevokeTenantInvite(ctx, &tenantinvitesv1.RevokeTenantInviteRequest{Id: id})
	if err != nil || rev.GetInvite().GetStatus() != models.TenantInviteRevoked || rev.GetInvite().GetRevokedAt() == nil {
		t.Fatalf("revoke = %v %v", rev, err)
	}
	if _, err := c.ResendTenantInvite(ctx, &tenantinvitesv1.ResendTenantInviteRequest{Id: id}); code(err) != codes.FailedPrecondition {
		t.Fatalf("resend revoked code = %v", code(err))
	}
	if _, err := c.GetTenantInvite(ctx, &tenantinvitesv1.GetTenantInviteRequest{Id: "missing"}); code(err) != codes.NotFound {
		t.Fatalf("get missing code = %v", code(err))
	}
	if _, err := c.GetTenantInvite(ctx, &tenantinvitesv1.GetTenantInviteRequest{}); code(err) != codes.InvalidArgument {
		t.Fatalf("get empty code = %v", code(err))
	}
}
