package server

import (
	"context"
	"net"
	"testing"

	"github.com/uptrace/bun"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	plansv1 "github.com/ogen-app/ogen/gen/plans/v1"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/pgtest"
)

// TestPlanAdminRoundTrip exercises the full Harbor path for the CON-294
// PlanAdminService over a live listener + shared-token interceptor + generated
// client, backed by the real repositories on a migrated Postgres: the version
// authoring lifecycle (create draft → publish → assign → retire guard) and the
// key error codes.
func TestPlanAdminRoundTrip(t *testing.T) {
	const token = "plan-admin-token"

	db := pgtest.MustDB()
	db.DB.SetMaxOpenConns(2)
	db.DB.SetMaxIdleConns(2)
	t.Cleanup(func() { _ = db.Close() })

	srv, err := New(token, nil,
		repository.NewTenantTierRepository(db), repository.NewTenantGroupRepository(db), repository.NewTenantRepository(db),
		repository.NewPlatformRepository(db), repository.NewPlatformGlobalLimitsRepository(db),
		repository.NewTenantTierVersionRepository(db), repository.NewTenantTierAssignmentRepository(db),
		repository.NewEmailLogRepository(db), repository.NewEmailEventRepository(db), nil, nil,
		nil, nil, "",
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

	newClient := func(tok string) plansv1.PlanAdminServiceClient {
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
		return plansv1.NewPlanAdminServiceClient(conn)
	}

	ctx := t.Context()
	cli := newClient(token)

	// A bad token is rejected by the shared interceptor.
	if _, err := newClient("nope").ListFeatures(ctx, &plansv1.ListFeaturesRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("bad token: code = %v, want Unauthenticated", status.Code(err))
	}

	// ListFeatures returns the embedded catalog.
	feats, err := cli.ListFeatures(ctx, &plansv1.ListFeaturesRequest{})
	if err != nil {
		t.Fatalf("ListFeatures: %v", err)
	}
	if len(feats.GetFeatures()) == 0 {
		t.Fatal("feature catalog is empty")
	}

	// The seeded default tier has an active v1.
	vers, err := cli.ListTierVersions(ctx, &plansv1.ListTierVersionsRequest{TierId: models.DefaultTierID})
	if err != nil {
		t.Fatalf("ListTierVersions: %v", err)
	}
	if len(vers.GetVersions()) == 0 {
		t.Fatal("default tier has no versions")
	}

	// Create a draft for the trial tier by cloning trial-v1 (→ version 2).
	created, err := cli.CreateTierVersion(ctx, &plansv1.CreateTierVersionRequest{
		TierId:             "trial",
		Purchasable:        true,
		CloneFromVersionId: "ttv-trial-v1",
	})
	if err != nil {
		t.Fatalf("CreateTierVersion: %v", err)
	}
	draftID := created.GetVersion().GetId()
	if created.GetVersion().GetStatus() != "draft" || created.GetVersion().GetVersion() != 2 {
		t.Fatalf("unexpected draft: %+v", created.GetVersion())
	}
	if len(created.GetVersion().GetEntitlements().GetFields()) == 0 {
		t.Fatal("clone did not copy entitlements")
	}

	// A draft version is not assignable.
	if _, err := cli.SetTenantTierVersion(ctx, &plansv1.SetTenantTierVersionRequest{TenantId: models.DefaultTenantID, TierVersionId: draftID, Reason: models.AssignmentReasonUpgrade}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("assign to draft version: code = %v, want FailedPrecondition", status.Code(err))
	}

	// Publishing requires a change_reason.
	if _, err := cli.PublishTierVersion(ctx, &plansv1.PublishTierVersionRequest{Id: draftID}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("publish w/o reason: code = %v, want InvalidArgument", status.Code(err))
	}
	pub, err := cli.PublishTierVersion(ctx, &plansv1.PublishTierVersionRequest{Id: draftID, ChangeReason: "trial refresh"})
	if err != nil {
		t.Fatalf("PublishTierVersion: %v", err)
	}
	if pub.GetVersion().GetStatus() != "active" {
		t.Fatalf("published status = %q, want active", pub.GetVersion().GetStatus())
	}

	// A published version can no longer be edited as a draft.
	empty, _ := structpb.NewStruct(map[string]any{})
	if _, err := cli.UpdateTierVersionDraft(ctx, &plansv1.UpdateTierVersionDraftRequest{Id: draftID, Entitlements: empty}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("update published: code = %v, want FailedPrecondition", status.Code(err))
	}

	// Assign the default tenant to the new version.
	setResp, err := cli.SetTenantTierVersion(ctx, &plansv1.SetTenantTierVersionRequest{
		TenantId:      models.DefaultTenantID,
		TierVersionId: draftID,
		Reason:        models.AssignmentReasonUpgrade,
	})
	if err != nil {
		t.Fatalf("SetTenantTierVersion: %v", err)
	}
	if setResp.GetVersion().GetLiveAssignmentCount() < 1 {
		t.Fatalf("live_assignment_count = %d, want >= 1", setResp.GetVersion().GetLiveAssignmentCount())
	}

	// Retiring a version with a live assignment is refused, unless forced.
	if _, err := cli.RetireTierVersion(ctx, &plansv1.RetireTierVersionRequest{Id: draftID}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("retire w/ live assignment: code = %v, want FailedPrecondition", status.Code(err))
	}
	if _, err := cli.RetireTierVersion(ctx, &plansv1.RetireTierVersionRequest{Id: draftID, Force: true}); err != nil {
		t.Fatalf("retire force: %v", err)
	}

	// A retired version is no longer assignable.
	if _, err := cli.SetTenantTierVersion(ctx, &plansv1.SetTenantTierVersionRequest{TenantId: models.DefaultTenantID, TierVersionId: draftID, Reason: models.AssignmentReasonUpgrade}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("assign to retired version: code = %v, want FailedPrecondition", status.Code(err))
	}

	// Unknown ids → NotFound. Uses the seeded active default-v1 so the version
	// check passes and the tenant lookup is what fails.
	if _, err := cli.GetTierVersion(ctx, &plansv1.GetTierVersionRequest{Id: "nope"}); status.Code(err) != codes.NotFound {
		t.Fatalf("get unknown version: code = %v, want NotFound", status.Code(err))
	}
	if _, err := cli.SetTenantTierVersion(ctx, &plansv1.SetTenantTierVersionRequest{TenantId: "ghost", TierVersionId: "ttv-default-v1", Reason: models.AssignmentReasonUpgrade}); status.Code(err) != codes.NotFound {
		t.Fatalf("assign unknown tenant: code = %v, want NotFound", status.Code(err))
	}
}

// newPlanAdminClient stands up the PlanAdminService on a live listener behind the
// shared-token interceptor and returns a generated client authenticated with the
// given token, backed by the supplied migrated Postgres.
func newPlanAdminClient(t *testing.T, db *bun.DB, token string) plansv1.PlanAdminServiceClient {
	t.Helper()
	srv, err := New(token, nil,
		repository.NewTenantTierRepository(db), repository.NewTenantGroupRepository(db), repository.NewTenantRepository(db),
		repository.NewPlatformRepository(db), repository.NewPlatformGlobalLimitsRepository(db),
		repository.NewTenantTierVersionRepository(db), repository.NewTenantTierAssignmentRepository(db),
		repository.NewEmailLogRepository(db), repository.NewEmailEventRepository(db), nil, nil,
		nil, nil, "",
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
	conn, err := grpc.NewClient(
		lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
			return invoker(ctx, method, req, reply, cc, opts...)
		}),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return plansv1.NewPlanAdminServiceClient(conn)
}

// TestPlanAdminRetireDeleteRoundTrip exercises the CON-297 surface end to end:
// draft deletion, the guarded retire (block → reassign-or-grandfather) and its
// error codes, and ListTierVersionAssignments.
func TestPlanAdminRetireDeleteRoundTrip(t *testing.T) {
	db := pgtest.MustDB()
	db.DB.SetMaxOpenConns(2)
	db.DB.SetMaxIdleConns(2)
	t.Cleanup(func() { _ = db.Close() })

	ctx := t.Context()
	cli := newPlanAdminClient(t, db, "plan-admin-token")

	// mkActive publishes a fresh active trial version (cloned from trial-v1).
	mkActive := func(reason string) string {
		c, err := cli.CreateTierVersion(ctx, &plansv1.CreateTierVersionRequest{TierId: "trial", Purchasable: true, CloneFromVersionId: "ttv-trial-v1"})
		if err != nil {
			t.Fatalf("create %s: %v", reason, err)
		}
		id := c.GetVersion().GetId()
		if _, err := cli.PublishTierVersion(ctx, &plansv1.PublishTierVersionRequest{Id: id, ChangeReason: reason}); err != nil {
			t.Fatalf("publish %s: %v", reason, err)
		}
		return id
	}
	src := mkActive("reassign source")
	dst := mkActive("reassign target")

	// Draft deletion: a fresh draft deletes; an active version does not; missing → NotFound.
	draft, err := cli.CreateTierVersion(ctx, &plansv1.CreateTierVersionRequest{TierId: "trial", CloneFromVersionId: "ttv-trial-v1"})
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}
	if _, err := cli.DeleteTierVersion(ctx, &plansv1.DeleteTierVersionRequest{Id: draft.GetVersion().GetId()}); err != nil {
		t.Fatalf("delete draft: %v", err)
	}
	if _, err := cli.DeleteTierVersion(ctx, &plansv1.DeleteTierVersionRequest{Id: src}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("delete active: code = %v, want FailedPrecondition", status.Code(err))
	}
	if _, err := cli.DeleteTierVersion(ctx, &plansv1.DeleteTierVersionRequest{Id: "nope"}); status.Code(err) != codes.NotFound {
		t.Fatalf("delete missing: code = %v, want NotFound", status.Code(err))
	}

	// Put the default tenant on src.
	if _, err := cli.SetTenantTierVersion(ctx, &plansv1.SetTenantTierVersionRequest{TenantId: models.DefaultTenantID, TierVersionId: src, Reason: models.AssignmentReasonUpgrade}); err != nil {
		t.Fatalf("assign tenant to src: %v", err)
	}

	// ListTierVersionAssignments enumerates the tenant; empty id → InvalidArgument.
	la, err := cli.ListTierVersionAssignments(ctx, &plansv1.ListTierVersionAssignmentsRequest{TierVersionId: src})
	if err != nil {
		t.Fatalf("list assignments: %v", err)
	}
	if la.GetTotal() != 1 || len(la.GetAssignments()) != 1 || la.GetAssignments()[0].GetTenantId() != models.DefaultTenantID {
		t.Fatalf("unexpected assignments on src: %+v", la.GetAssignments())
	}
	if _, err := cli.ListTierVersionAssignments(ctx, &plansv1.ListTierVersionAssignmentsRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("list w/o id: code = %v, want InvalidArgument", status.Code(err))
	}

	// Retire guard + argument validation.
	if _, err := cli.RetireTierVersion(ctx, &plansv1.RetireTierVersionRequest{Id: src}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("retire w/ live assignment: code = %v, want FailedPrecondition", status.Code(err))
	}
	if _, err := cli.RetireTierVersion(ctx, &plansv1.RetireTierVersionRequest{Id: src, Force: true, ReassignToVersionId: dst}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("force+reassign: code = %v, want InvalidArgument", status.Code(err))
	}
	if _, err := cli.RetireTierVersion(ctx, &plansv1.RetireTierVersionRequest{Id: src, ReassignToVersionId: src}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("reassign to self: code = %v, want InvalidArgument", status.Code(err))
	}
	if _, err := cli.RetireTierVersion(ctx, &plansv1.RetireTierVersionRequest{Id: src, ReassignToVersionId: "ttv-max-v1"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("reassign to draft target: code = %v, want FailedPrecondition", status.Code(err))
	}
	if _, err := cli.RetireTierVersion(ctx, &plansv1.RetireTierVersionRequest{Id: src, ReassignToVersionId: "nope"}); status.Code(err) != codes.NotFound {
		t.Fatalf("reassign to missing target: code = %v, want NotFound", status.Code(err))
	}

	// Reassign-then-retire: migrate the tenant onto dst and retire src, atomically.
	rr, err := cli.RetireTierVersion(ctx, &plansv1.RetireTierVersionRequest{Id: src, ReassignToVersionId: dst})
	if err != nil {
		t.Fatalf("retire w/ reassign: %v", err)
	}
	if rr.GetReassignedCount() != 1 || rr.GetVersion().GetStatus() != "retired" {
		t.Fatalf("unexpected retire result: reassigned=%d status=%q", rr.GetReassignedCount(), rr.GetVersion().GetStatus())
	}
	// src is empty; dst holds the migrated tenant.
	after, err := cli.ListTierVersionAssignments(ctx, &plansv1.ListTierVersionAssignmentsRequest{TierVersionId: src})
	if err != nil {
		t.Fatalf("list src after retire: %v", err)
	}
	if after.GetTotal() != 0 {
		t.Fatalf("src still has %d live assignments after reassign-retire", after.GetTotal())
	}
	onDst, err := cli.ListTierVersionAssignments(ctx, &plansv1.ListTierVersionAssignmentsRequest{TierVersionId: dst})
	if err != nil {
		t.Fatalf("list dst after retire: %v", err)
	}
	if onDst.GetTotal() != 1 || onDst.GetAssignments()[0].GetTenantId() != models.DefaultTenantID {
		t.Fatalf("tenant not migrated onto dst: %+v", onDst.GetAssignments())
	}

	// A version with no live assignments retires directly.
	if _, err := cli.RetireTierVersion(ctx, &plansv1.RetireTierVersionRequest{Id: src}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("re-retire src (already retired): code = %v, want FailedPrecondition", status.Code(err))
	}
}
