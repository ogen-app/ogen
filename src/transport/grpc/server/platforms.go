// CON-292: PlatformAdminService — the operator-facing (Harbor) gRPC surface for
// the data-driven platform catalog. Mirrors TenantAdminService and shares the
// same bearer-token gate (see server.go). Generated stubs come from
// gen/platforms/v1 (the buf.build/ogen-app/proto module; `make proto`).
package server

import (
	"context"
	"database/sql"
	"errors"
	"expvar"
	"log/slog"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	platformsv1 "github.com/ogen-app/ogen/gen/platforms/v1"
	"github.com/ogen-app/ogen/src/domain/models"
	domainplatforms "github.com/ogen-app/ogen/src/domain/platforms"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// expvar counters (CON-292 §15). Distinct "ogen_platform_admin_*" namespace.
var (
	platformAdminCreated             = expvar.NewInt("ogen_platform_admin_created")
	platformAdminUpdated             = expvar.NewInt("ogen_platform_admin_updated")
	platformAdminEnabledToggled      = expvar.NewInt("ogen_platform_admin_enabled_toggled")
	platformAdminDeleted             = expvar.NewInt("ogen_platform_admin_deleted")
	platformAdminGlobalLimitsUpdated = expvar.NewInt("ogen_platform_admin_global_limits_updated")
)

// platformAdminService adapts the platform + global-limits repositories to the
// generated PlatformAdminServiceServer. Both tables are global, so no tenantctx
// is threaded — the work is cross-tenant by design, gated only by the shared
// bearer token (see server.go). After every write it refreshes the Zernio
// resolver / limits caches so operator edits take effect without waiting for the
// periodic tick.
type platformAdminService struct {
	platformsv1.UnimplementedPlatformAdminServiceServer
	platformRepo repository.PlatformRepository
	limitsRepo   repository.PlatformGlobalLimitsRepository
}

func newPlatformAdminService(
	platformRepo repository.PlatformRepository,
	limitsRepo repository.PlatformGlobalLimitsRepository,
) *platformAdminService {
	return &platformAdminService{platformRepo: platformRepo, limitsRepo: limitsRepo}
}

// --- reads ---

func (s *platformAdminService) ListPlatforms(ctx context.Context, req *platformsv1.ListPlatformsRequest) (*platformsv1.ListPlatformsResponse, error) {
	var (
		rows []models.Platform
		err  error
	)
	if req.GetIncludeDisabled() {
		rows, err = s.platformRepo.List(ctx)
	} else {
		rows, err = s.platformRepo.ListEnabled(ctx)
	}
	if err != nil {
		return nil, s.internal(ctx, "list platforms", err)
	}
	out := make([]*platformsv1.Platform, 0, len(rows))
	for i := range rows {
		usage, uerr := s.usage(ctx, &rows[i])
		if uerr != nil {
			return nil, s.internal(ctx, "list platforms", uerr)
		}
		out = append(out, toPlatformProto(&rows[i], usage))
	}
	return &platformsv1.ListPlatformsResponse{Platforms: out}, nil
}

func (s *platformAdminService) GetPlatform(ctx context.Context, req *platformsv1.GetPlatformRequest) (*platformsv1.GetPlatformResponse, error) {
	id := strings.TrimSpace(req.GetId())
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	// id may be a Sqid or a Zernio slug.
	p, err := s.platformRepo.GetByID(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		p, err = s.platformRepo.GetByZernioID(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Error(codes.NotFound, "platform not found")
		}
	}
	if err != nil {
		return nil, s.internal(ctx, "get platform", err)
	}
	usage, err := s.usage(ctx, p)
	if err != nil {
		return nil, s.internal(ctx, "get platform", err)
	}
	return &platformsv1.GetPlatformResponse{Platform: toPlatformProto(p, usage)}, nil
}

func (s *platformAdminService) GetGlobalLimits(ctx context.Context, _ *platformsv1.GetGlobalLimitsRequest) (*platformsv1.GetGlobalLimitsResponse, error) {
	lim, err := s.currentLimits(ctx)
	if err != nil {
		return nil, s.internal(ctx, "get global limits", err)
	}
	return &platformsv1.GetGlobalLimitsResponse{Limits: toGlobalLimitsProto(lim)}, nil
}

// --- catalog writes ---

func (s *platformAdminService) CreatePlatform(ctx context.Context, req *platformsv1.CreatePlatformRequest) (*platformsv1.CreatePlatformResponse, error) {
	pb := req.GetPlatform()
	if pb == nil {
		return nil, status.Error(codes.InvalidArgument, "platform is required")
	}
	limits, err := s.currentLimits(ctx)
	if err != nil {
		return nil, s.internal(ctx, "create platform", err)
	}
	if verr := validatePlatformWrite(pb, limits); verr != nil {
		return nil, verr
	}
	id, err := models.NewID()
	if err != nil {
		return nil, s.internal(ctx, "create platform", err)
	}
	now := time.Now().UTC()
	m := platformFromProto(pb)
	m.ID = id
	m.CreatedAt = now
	m.UpdatedAt = now
	if err := s.platformRepo.Create(ctx, &m); err != nil {
		if pgCode(err) == pgUniqueViolation {
			return nil, status.Error(codes.AlreadyExists, "a platform with that name or zernio_id already exists")
		}
		return nil, s.internal(ctx, "create platform", err)
	}
	// bun coerces a zero-value enabled=false back to the column DEFAULT true on
	// INSERT, so an operator-requested disabled platform needs an explicit flip.
	if !pb.GetEnabled() {
		if _, err := s.platformRepo.SetEnabled(ctx, id, false); err != nil {
			return nil, s.internal(ctx, "create platform", err)
		}
	}
	if rerr := zernio.RefreshCatalog(ctx); rerr != nil {
		slog.WarnContext(ctx, "platform catalog refresh after create failed", logging.AttrComponent, "grpcserver", logging.AttrError, rerr)
	}
	created, err := s.platformRepo.GetByID(ctx, id)
	if err != nil {
		return nil, s.internal(ctx, "create platform", err)
	}
	platformAdminCreated.Add(1)
	slog.InfoContext(ctx, "platform created", logging.AttrComponent, "grpcserver", "platform_id", id, "zernio_id", m.ZernioID)
	usage, _ := s.usage(ctx, created)
	return &platformsv1.CreatePlatformResponse{Platform: toPlatformProto(created, usage)}, nil
}

func (s *platformAdminService) UpdatePlatform(ctx context.Context, req *platformsv1.UpdatePlatformRequest) (*platformsv1.UpdatePlatformResponse, error) {
	pb := req.GetPlatform()
	if pb == nil {
		return nil, status.Error(codes.InvalidArgument, "platform is required")
	}
	id := strings.TrimSpace(pb.GetId())
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	limits, err := s.currentLimits(ctx)
	if err != nil {
		return nil, s.internal(ctx, "update platform", err)
	}
	if verr := validatePlatformWrite(pb, limits); verr != nil {
		return nil, verr
	}
	existing, err := s.platformRepo.GetByID(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, status.Error(codes.NotFound, "platform not found")
	}
	if err != nil {
		return nil, s.internal(ctx, "update platform", err)
	}
	// Whole-resource replace: every mutable field comes from the request; id and
	// created_at are server-owned. UPDATE (unlike INSERT) writes enabled verbatim.
	m := platformFromProto(pb)
	m.ID = id
	m.CreatedAt = existing.CreatedAt
	m.UpdatedAt = time.Now().UTC()
	if err := s.platformRepo.Update(ctx, &m); err != nil {
		if pgCode(err) == pgUniqueViolation {
			return nil, status.Error(codes.AlreadyExists, "a platform with that name or zernio_id already exists")
		}
		return nil, s.internal(ctx, "update platform", err)
	}
	if rerr := zernio.RefreshCatalog(ctx); rerr != nil {
		slog.WarnContext(ctx, "platform catalog refresh after update failed", logging.AttrComponent, "grpcserver", logging.AttrError, rerr)
	}
	updated, err := s.platformRepo.GetByID(ctx, id)
	if err != nil {
		return nil, s.internal(ctx, "update platform", err)
	}
	platformAdminUpdated.Add(1)
	slog.InfoContext(ctx, "platform updated", logging.AttrComponent, "grpcserver", "platform_id", id)
	usage, _ := s.usage(ctx, updated)
	return &platformsv1.UpdatePlatformResponse{Platform: toPlatformProto(updated, usage)}, nil
}

func (s *platformAdminService) SetPlatformEnabled(ctx context.Context, req *platformsv1.SetPlatformEnabledRequest) (*platformsv1.SetPlatformEnabledResponse, error) {
	id := strings.TrimSpace(req.GetId())
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	p, err := s.platformRepo.SetEnabled(ctx, id, req.GetEnabled())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, status.Error(codes.NotFound, "platform not found")
	}
	if err != nil {
		return nil, s.internal(ctx, "set platform enabled", err)
	}
	if rerr := zernio.RefreshCatalog(ctx); rerr != nil {
		slog.WarnContext(ctx, "platform catalog refresh after enable toggle failed", logging.AttrComponent, "grpcserver", logging.AttrError, rerr)
	}
	platformAdminEnabledToggled.Add(1)
	usage, _ := s.usage(ctx, p)
	slog.InfoContext(ctx, "platform enabled toggled", logging.AttrComponent, "grpcserver", "platform_id", id, "enabled", req.GetEnabled())
	return &platformsv1.SetPlatformEnabledResponse{Platform: toPlatformProto(p, usage)}, nil
}

func (s *platformAdminService) DeletePlatform(ctx context.Context, req *platformsv1.DeletePlatformRequest) (*platformsv1.DeletePlatformResponse, error) {
	id := strings.TrimSpace(req.GetId())
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	p, err := s.platformRepo.GetByID(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, status.Error(codes.NotFound, "platform not found")
	}
	if err != nil {
		return nil, s.internal(ctx, "delete platform", err)
	}
	accounts, scheduled, err := s.platformRepo.InUseCounts(ctx, p)
	if err != nil {
		return nil, s.internal(ctx, "delete platform", err)
	}
	if !req.GetForce() && (accounts > 0 || scheduled > 0) {
		return nil, status.Errorf(codes.FailedPrecondition,
			"platform in use: %d connected account(s), %d scheduled post(s); disable it instead, or set force=true",
			accounts, scheduled)
	}
	deleted, err := s.platformRepo.Delete(ctx, id)
	if err != nil {
		return nil, s.internal(ctx, "delete platform", err)
	}
	if !deleted {
		return nil, status.Error(codes.NotFound, "platform not found")
	}
	if rerr := zernio.RefreshCatalog(ctx); rerr != nil {
		slog.WarnContext(ctx, "platform catalog refresh after delete failed", logging.AttrComponent, "grpcserver", logging.AttrError, rerr)
	}
	platformAdminDeleted.Add(1)
	slog.InfoContext(ctx, "platform deleted", logging.AttrComponent, "grpcserver", "platform_id", id, "force", req.GetForce(), "connected_accounts", accounts, "scheduled_posts", scheduled)
	return &platformsv1.DeletePlatformResponse{}, nil
}

// --- global safety caps ---

func (s *platformAdminService) UpdateGlobalLimits(ctx context.Context, req *platformsv1.UpdateGlobalLimitsRequest) (*platformsv1.UpdateGlobalLimitsResponse, error) {
	pb := req.GetLimits()
	if pb == nil {
		return nil, status.Error(codes.InvalidArgument, "limits is required")
	}
	if pb.GetMaxImageUploadBytes() < 0 || pb.GetMaxPdfUploadBytes() < 0 || pb.GetMaxVideoUploadBytes() < 0 ||
		pb.GetMaxAltTextChars() < 0 || pb.GetMaxThreadSegments() < 0 {
		return nil, status.Error(codes.InvalidArgument, "global limits must be non-negative")
	}
	m := &models.PlatformGlobalLimits{
		MaxImageUploadBytes: pb.GetMaxImageUploadBytes(),
		MaxPDFUploadBytes:   pb.GetMaxPdfUploadBytes(),
		MaxVideoUploadBytes: pb.GetMaxVideoUploadBytes(),
		MaxAltTextChars:     int(pb.GetMaxAltTextChars()),
		MaxThreadSegments:   int(pb.GetMaxThreadSegments()),
	}
	if err := s.limitsRepo.Update(ctx, m); err != nil {
		return nil, s.internal(ctx, "update global limits", err)
	}
	if rerr := domainplatforms.RefreshGlobalLimits(ctx); rerr != nil {
		slog.WarnContext(ctx, "global limits refresh after update failed", logging.AttrComponent, "grpcserver", logging.AttrError, rerr)
	}
	platformAdminGlobalLimitsUpdated.Add(1)
	slog.InfoContext(ctx, "platform global limits updated", logging.AttrComponent, "grpcserver")
	updated, err := s.currentLimits(ctx)
	if err != nil {
		return nil, s.internal(ctx, "update global limits", err)
	}
	return &platformsv1.UpdateGlobalLimitsResponse{Limits: toGlobalLimitsProto(updated)}, nil
}

// --- helpers ---

func (s *platformAdminService) currentLimits(ctx context.Context) (models.PlatformGlobalLimits, error) {
	lim, err := s.limitsRepo.Get(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return models.DefaultPlatformGlobalLimits(), nil
		}
		return models.PlatformGlobalLimits{}, err
	}
	return *lim, nil
}

func (s *platformAdminService) usage(ctx context.Context, p *models.Platform) (*platformsv1.PlatformUsage, error) {
	accounts, scheduled, err := s.platformRepo.InUseCounts(ctx, p)
	if err != nil {
		return nil, err
	}
	return &platformsv1.PlatformUsage{ConnectedAccounts: int32(accounts), ScheduledPosts: int32(scheduled)}, nil
}

func (s *platformAdminService) internal(ctx context.Context, op string, err error) error {
	slog.ErrorContext(ctx, "platform admin "+op, logging.AttrComponent, "grpcserver", logging.AttrError, err)
	return status.Error(codes.Internal, op+" failed")
}

// validatePlatformWrite enforces the §12 write rules: required name + zernio_id,
// non-negative + internally consistent numeric limits, per-platform file sizes
// within the global upload ceilings, and per-post-type char overrides keyed on a
// known post type.
func validatePlatformWrite(pb *platformsv1.Platform, limits models.PlatformGlobalLimits) error {
	if strings.TrimSpace(pb.GetName()) == "" {
		return status.Error(codes.InvalidArgument, "name is required")
	}
	if strings.TrimSpace(pb.GetZernioId()) == "" {
		return status.Error(codes.InvalidArgument, "zernio_id is required")
	}
	if img := pb.GetImageConstraints(); img != nil {
		if img.GetMaxFileSizeBytes() < 0 {
			return status.Error(codes.InvalidArgument, "image max_file_size_bytes must be >= 0")
		}
		if img.GetMaxFileSizeBytes() > limits.MaxImageUploadBytes {
			return status.Errorf(codes.InvalidArgument, "image max_file_size_bytes (%d) exceeds the global image upload ceiling (%d); raise the global cap first", img.GetMaxFileSizeBytes(), limits.MaxImageUploadBytes)
		}
	}
	if vid := pb.GetVideoConstraints(); vid != nil {
		if vid.GetMaxFileSizeBytes() < 0 || vid.GetMinDurationSeconds() < 0 || vid.GetMaxDurationSeconds() < 0 {
			return status.Error(codes.InvalidArgument, "video limits must be >= 0")
		}
		if vid.GetMaxFileSizeBytes() > limits.MaxVideoUploadBytes {
			return status.Errorf(codes.InvalidArgument, "video max_file_size_bytes (%d) exceeds the global video upload ceiling (%d); raise the global cap first", vid.GetMaxFileSizeBytes(), limits.MaxVideoUploadBytes)
		}
		if max := vid.GetMaxDurationSeconds(); max > 0 && vid.GetMinDurationSeconds() > max {
			return status.Error(codes.InvalidArgument, "video min_duration_seconds must be <= max_duration_seconds")
		}
	}
	if pdf := pb.GetPdfConstraints(); pdf != nil {
		if pdf.GetMaxFileSizeBytes() < 0 {
			return status.Error(codes.InvalidArgument, "pdf max_file_size_bytes must be >= 0")
		}
		if pdf.GetMaxFileSizeBytes() > limits.MaxPDFUploadBytes {
			return status.Errorf(codes.InvalidArgument, "pdf max_file_size_bytes (%d) exceeds the global pdf upload ceiling (%d); raise the global cap first", pdf.GetMaxFileSizeBytes(), limits.MaxPDFUploadBytes)
		}
	}
	if txt := pb.GetTextConstraints(); txt != nil {
		pts := pb.GetPostTypes()
		for slug := range txt.GetPerPostType() {
			if _, ok := pts[slug]; !ok {
				return status.Errorf(codes.InvalidArgument, "text per_post_type override %q is not one of the platform's post_types", slug)
			}
		}
	}
	return nil
}

// --- proto <-> model mapping ---

func platformFromProto(pb *platformsv1.Platform) models.Platform {
	postTypes := pb.GetPostTypes()
	if postTypes == nil {
		postTypes = map[string]string{}
	}
	supported := pb.GetSupportedPostTypes()
	if supported == nil {
		supported = []string{}
	}
	return models.Platform{
		Name:               strings.TrimSpace(pb.GetName()),
		ZernioID:           strings.TrimSpace(pb.GetZernioId()),
		Enabled:            pb.GetEnabled(),
		ConnectSupported:   pb.GetConnectSupported(),
		Cadence:            pb.GetCadence(),
		Constraints:        pb.GetConstraints(),
		PostTypes:          models.PostTypeMap(postTypes),
		SupportedPostTypes: models.StringSlice(supported),
		SortOrder:          int(pb.GetSortOrder()),
		ImageConstraints:   fromImageProto(pb.GetImageConstraints()),
		VideoConstraints:   fromVideoProto(pb.GetVideoConstraints()),
		PDFConstraints:     fromPdfProto(pb.GetPdfConstraints()),
		TextConstraints:    fromTextProto(pb.GetTextConstraints()),
	}
}

func toPlatformProto(p *models.Platform, usage *platformsv1.PlatformUsage) *platformsv1.Platform {
	if p == nil {
		return nil
	}
	return &platformsv1.Platform{
		Id:                 p.ID,
		Name:               p.Name,
		ZernioId:           p.ZernioID,
		Enabled:            p.Enabled,
		ConnectSupported:   p.ConnectSupported,
		Cadence:            p.Cadence,
		Constraints:        p.Constraints,
		PostTypes:          map[string]string(p.PostTypes),
		SupportedPostTypes: []string(p.SupportedPostTypes),
		SortOrder:          int32(p.SortOrder),
		ImageConstraints:   toImageProto(p.ImageConstraints),
		VideoConstraints:   toVideoProto(p.VideoConstraints),
		PdfConstraints:     toPdfProto(p.PDFConstraints),
		TextConstraints:    toTextProto(p.TextConstraints),
		Usage:              usage,
		CreatedAt:          timestamppb.New(p.CreatedAt),
		UpdatedAt:          timestamppb.New(p.UpdatedAt),
	}
}

func fromImageProto(c *platformsv1.ImageConstraints) models.ImageConstraints {
	if c == nil {
		return models.ImageConstraints{}
	}
	return models.ImageConstraints{
		MaxFileSizeBytes:      c.GetMaxFileSizeBytes(),
		AllowedFormats:        c.GetAllowedFormats(),
		AnimatedGIFSupported:  c.GetAnimatedGifSupported(),
		MaxAttachmentsPerPost: int(c.GetMaxAttachmentsPerPost()),
	}
}

func toImageProto(c models.ImageConstraints) *platformsv1.ImageConstraints {
	return &platformsv1.ImageConstraints{
		MaxFileSizeBytes:      c.MaxFileSizeBytes,
		AllowedFormats:        c.AllowedFormats,
		AnimatedGifSupported:  c.AnimatedGIFSupported,
		MaxAttachmentsPerPost: int32(c.MaxAttachmentsPerPost),
	}
}

func fromVideoProto(c *platformsv1.VideoConstraints) models.VideoConstraints {
	if c == nil {
		return models.VideoConstraints{}
	}
	return models.VideoConstraints{
		MaxFileSizeBytes:      c.GetMaxFileSizeBytes(),
		AllowedFormats:        c.GetAllowedFormats(),
		MaxDurationSeconds:    int(c.GetMaxDurationSeconds()),
		MinDurationSeconds:    int(c.GetMinDurationSeconds()),
		MaxWidth:              int(c.GetMaxWidth()),
		MaxHeight:             int(c.GetMaxHeight()),
		AllowedAspectRatios:   c.GetAllowedAspectRatios(),
		MaxAttachmentsPerPost: int(c.GetMaxAttachmentsPerPost()),
		RequiresVideoTitle:    c.GetRequiresVideoTitle(),
	}
}

func toVideoProto(c models.VideoConstraints) *platformsv1.VideoConstraints {
	return &platformsv1.VideoConstraints{
		MaxFileSizeBytes:      c.MaxFileSizeBytes,
		AllowedFormats:        c.AllowedFormats,
		MaxDurationSeconds:    int32(c.MaxDurationSeconds),
		MinDurationSeconds:    int32(c.MinDurationSeconds),
		MaxWidth:              int32(c.MaxWidth),
		MaxHeight:             int32(c.MaxHeight),
		AllowedAspectRatios:   c.AllowedAspectRatios,
		MaxAttachmentsPerPost: int32(c.MaxAttachmentsPerPost),
		RequiresVideoTitle:    c.RequiresVideoTitle,
	}
}

func fromPdfProto(c *platformsv1.PdfConstraints) models.PDFConstraints {
	if c == nil {
		return models.PDFConstraints{}
	}
	return models.PDFConstraints{
		MaxFileSizeBytes:      c.GetMaxFileSizeBytes(),
		AllowedFormats:        c.GetAllowedFormats(),
		MaxPages:              int(c.GetMaxPages()),
		MaxAttachmentsPerPost: int(c.GetMaxAttachmentsPerPost()),
	}
}

func toPdfProto(c models.PDFConstraints) *platformsv1.PdfConstraints {
	return &platformsv1.PdfConstraints{
		MaxFileSizeBytes:      c.MaxFileSizeBytes,
		AllowedFormats:        c.AllowedFormats,
		MaxPages:              int32(c.MaxPages),
		MaxAttachmentsPerPost: int32(c.MaxAttachmentsPerPost),
	}
}

func fromTextProto(c *platformsv1.TextConstraints) models.TextConstraints {
	if c == nil {
		return models.TextConstraints{}
	}
	var perType map[string]int
	if in := c.GetPerPostType(); len(in) > 0 {
		perType = make(map[string]int, len(in))
		for k, v := range in {
			perType[k] = int(v)
		}
	}
	return models.TextConstraints{
		MaxContentChars: int(c.GetMaxContentChars()),
		MaxTitleChars:   int(c.GetMaxTitleChars()),
		PerPostType:     perType,
	}
}

func toTextProto(c models.TextConstraints) *platformsv1.TextConstraints {
	var perType map[string]int32
	if len(c.PerPostType) > 0 {
		perType = make(map[string]int32, len(c.PerPostType))
		for k, v := range c.PerPostType {
			perType[k] = int32(v)
		}
	}
	return &platformsv1.TextConstraints{
		MaxContentChars: int32(c.MaxContentChars),
		MaxTitleChars:   int32(c.MaxTitleChars),
		PerPostType:     perType,
	}
}

func toGlobalLimitsProto(l models.PlatformGlobalLimits) *platformsv1.GlobalLimits {
	return &platformsv1.GlobalLimits{
		MaxImageUploadBytes: l.MaxImageUploadBytes,
		MaxPdfUploadBytes:   l.MaxPDFUploadBytes,
		MaxVideoUploadBytes: l.MaxVideoUploadBytes,
		MaxAltTextChars:     int32(l.MaxAltTextChars),
		MaxThreadSegments:   int32(l.MaxThreadSegments),
	}
}
