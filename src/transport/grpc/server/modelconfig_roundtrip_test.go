package server

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	modelconfigv1 "github.com/ogen-app/ogen/gen/modelconfig/v1"
	"github.com/ogen-app/ogen/src/domain/modelconfig"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/vendors"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/pgtest"
)

// TestModelConfigAdminRoundTrip drives the vision and transcribe slots through
// the Harbor path on a migrated Postgres: boot reconcile seeds them from the env
// defaults, an assignment takes effect in the resolver immediately, a tier
// override wins for its tier only, clearing it falls back to the global, and the
// vendor guard rejects a model from the wrong vendor in either direction.
func TestModelConfigAdminRoundTrip(t *testing.T) {
	const token = "modelconfig-admin-token"

	db := pgtest.MustDB()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	// FK-free on a single pinned connection, so a tier override needs no tier row.
	if _, err := db.Exec("SET session_replication_role = replica"); err != nil {
		t.Fatalf("disable fks: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	repo := repository.NewFlowModelConfigRepository(db)
	modelconfig.Init(t.Context(), repo, modelconfig.Defaults{
		Generation:     "claude-sonnet-4-5-20250929",
		Quality:        "claude-sonnet-4-5-20250929",
		Planning:       "claude-haiku-4-5-20251001",
		Embed:          "gemini-embedding-2",
		VisionClassify: "gemini-2.5-flash",
		VisionExtract:  "gemini-2.5-pro",
		VisionEscalate: "gemini-2.5-pro",
		Transcribe:     "gemini-2.5-flash",
	}, vendors.VendorOf, tenantctx.TierFrom)

	srv, err := New(Deps{
		Token:            token,
		Tiers:            repository.NewTenantTierRepository(db),
		Groups:           repository.NewTenantGroupRepository(db),
		Tenants:          repository.NewTenantRepository(db),
		Platforms:        repository.NewPlatformRepository(db),
		PlatformLimits:   repository.NewPlatformGlobalLimitsRepository(db),
		FlowModelConfigs: repo,
		TierVersions:     repository.NewTenantTierVersionRepository(db),
		TierAssignments:  repository.NewTenantTierAssignmentRepository(db),
		EmailLogs:        repository.NewEmailLogRepository(db),
		EmailEvents:      repository.NewEmailEventRepository(db),
		Announcements:    repository.NewAnnouncementRepository(db),
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
	client := modelconfigv1.NewModelConfigAdminServiceClient(conn)
	ctx := t.Context()
	pro := tenantctx.WithTier(ctx, "pro")

	// Reconcile seeded every vision/transcribe slot from the env defaults.
	eff, err := client.GetEffectiveConfig(ctx, &modelconfigv1.GetEffectiveConfigRequest{TierId: "pro"})
	if err != nil {
		t.Fatalf("GetEffectiveConfig: %v", err)
	}
	seeded := map[string]string{}
	for _, s := range eff.GetSlots() {
		seeded[s.GetFlowKey()+"/"+s.GetSlotKey()] = s.GetModelId()
	}
	for slot, want := range map[string]string{
		"vision/classify":   "gemini-2.5-flash",
		"vision/extract":    "gemini-2.5-pro",
		"vision/escalate":   "gemini-2.5-pro",
		"vision/alt_text":   "gemini-2.5-flash",
		"transcribe/main":   "gemini-2.5-flash",
		"content_plan/main": "claude-sonnet-4-5-20250929",
	} {
		if seeded[slot] != want {
			t.Errorf("seeded %s = %q, want %q", slot, seeded[slot], want)
		}
	}

	// A global assignment takes effect in the resolver with no restart.
	if _, err := client.SetSlotModel(ctx, &modelconfigv1.SetSlotModelRequest{FlowKey: "vision", SlotKey: "extract", ModelId: "gemini-2.5-flash"}); err != nil {
		t.Fatalf("SetSlotModel(vision/extract): %v", err)
	}
	if got := modelconfig.Model(ctx, modelconfig.FlowVision, modelconfig.SlotExtract); got != "gemini-2.5-flash" {
		t.Fatalf("resolver vision/extract = %q, want gemini-2.5-flash", got)
	}

	// A tier override wins for that tier only; clearing it restores the global.
	if _, err := client.SetSlotModel(ctx, &modelconfigv1.SetSlotModelRequest{TierId: "pro", FlowKey: "transcribe", SlotKey: "main", ModelId: "gemini-2.5-pro"}); err != nil {
		t.Fatalf("SetSlotModel(pro transcribe/main): %v", err)
	}
	if got := modelconfig.Model(pro, modelconfig.FlowTranscribe, modelconfig.SlotMain); got != "gemini-2.5-pro" {
		t.Fatalf("pro transcribe = %q, want the tier override gemini-2.5-pro", got)
	}
	if got := modelconfig.Model(ctx, modelconfig.FlowTranscribe, modelconfig.SlotMain); got != "gemini-2.5-flash" {
		t.Fatalf("tierless transcribe = %q, want the global gemini-2.5-flash", got)
	}
	if _, err := client.ClearSlotModel(ctx, &modelconfigv1.ClearSlotModelRequest{TierId: "pro", FlowKey: "transcribe", SlotKey: "main"}); err != nil {
		t.Fatalf("ClearSlotModel: %v", err)
	}
	if got := modelconfig.Model(pro, modelconfig.FlowTranscribe, modelconfig.SlotMain); got != "gemini-2.5-flash" {
		t.Fatalf("pro transcribe after clear = %q, want the global gemini-2.5-flash", got)
	}

	// The vendor guard holds both ways.
	for _, bad := range []*modelconfigv1.SetSlotModelRequest{
		{FlowKey: "vision", SlotKey: "alt_text", ModelId: "claude-sonnet-4-5-20250929"},
		{FlowKey: "transcribe", SlotKey: "main", ModelId: "gemini-embedding-2"},
		{FlowKey: "content_plan", SlotKey: "main", ModelId: "gemini-2.5-pro"},
	} {
		_, err := client.SetSlotModel(ctx, bad)
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("SetSlotModel(%s/%s = %s): code %v, want InvalidArgument", bad.GetFlowKey(), bad.GetSlotKey(), bad.GetModelId(), status.Code(err))
		}
	}
	if got := modelconfig.Model(ctx, modelconfig.FlowVision, modelconfig.SlotAltText); got != "gemini-2.5-flash" {
		t.Fatalf("rejected write leaked into the resolver: alt_text = %q", got)
	}
}
