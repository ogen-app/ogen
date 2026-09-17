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

	announcementsv1 "github.com/ogen-app/ogen/gen/announcements/v1"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

const (
	defaultAnnouncementPageSize = 50
	maxAnnouncementPageSize     = 200
)

// announcementAdminService adapts the AnnouncementRepository to the generated
// AnnouncementAdminServiceServer (CON-230) — the operator-facing surface Harbor
// uses to author informational announcements (banners) and read their
// engagement. Announcements are global (not tenant-scoped); targeting reuses the
// CON-208 tenant tiers/groups the Harbor audience picker reads from
// TenantAdminService.
type announcementAdminService struct {
	announcementsv1.UnimplementedAnnouncementAdminServiceServer
	repo repository.AnnouncementRepository
}

func newAnnouncementAdminService(repo repository.AnnouncementRepository) *announcementAdminService {
	return &announcementAdminService{repo: repo}
}

func (s *announcementAdminService) ListAnnouncements(ctx context.Context, req *announcementsv1.ListAnnouncementsRequest) (*announcementsv1.ListAnnouncementsResponse, error) {
	pageSize := int(req.GetPageSize())
	if pageSize <= 0 {
		pageSize = defaultAnnouncementPageSize
	}
	pageSize = min(pageSize, maxAnnouncementPageSize)

	statusFilter := strings.TrimSpace(req.GetStatus())
	if statusFilter != "" && !models.AnnouncementStatus(statusFilter).Valid() {
		return nil, status.Error(codes.InvalidArgument, "invalid status filter")
	}
	cursorAt, cursorID, err := decodeAnnouncementCursor(req.GetPageToken())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid page_token")
	}

	rows, err := s.repo.List(ctx, repository.AnnouncementListFilter{
		Status:          models.AnnouncementStatus(statusFilter),
		Limit:           pageSize + 1, // one extra row signals a next page exists
		CursorCreatedAt: cursorAt,
		CursorID:        cursorID,
	})
	if err != nil {
		return nil, s.internal(ctx, "list announcements", err)
	}

	var next string
	if len(rows) > pageSize {
		last := rows[pageSize-1]
		next = encodeAnnouncementCursor(last.CreatedAt, last.ID)
		rows = rows[:pageSize]
	}

	items := make([]*announcementsv1.AnnouncementWithStats, 0, len(rows))
	for i := range rows {
		st, err := s.repo.StatsFor(ctx, &rows[i])
		if err != nil {
			return nil, s.internal(ctx, "announcement stats", err)
		}
		items = append(items, announcementWithStatsProto(&rows[i], st))
	}
	return &announcementsv1.ListAnnouncementsResponse{Items: items, NextPageToken: next}, nil
}

func (s *announcementAdminService) GetAnnouncement(ctx context.Context, req *announcementsv1.GetAnnouncementRequest) (*announcementsv1.GetAnnouncementResponse, error) {
	id := strings.TrimSpace(req.GetId())
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	an, err := s.repo.Get(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Error(codes.NotFound, "announcement not found")
		}
		return nil, s.internal(ctx, "get announcement", err)
	}
	st, err := s.repo.StatsFor(ctx, an)
	if err != nil {
		return nil, s.internal(ctx, "announcement stats", err)
	}
	return &announcementsv1.GetAnnouncementResponse{Item: announcementWithStatsProto(an, st)}, nil
}

func (s *announcementAdminService) CreateAnnouncement(ctx context.Context, req *announcementsv1.CreateAnnouncementRequest) (*announcementsv1.CreateAnnouncementResponse, error) {
	a, err := announcementFromInput(req.GetAnnouncement())
	if err != nil {
		return nil, err
	}
	in := req.GetAnnouncement()
	if err := s.repo.Create(ctx, a, in.GetTargetGroupIds(), in.GetTargetTierIds()); err != nil {
		return nil, s.mapWrite(ctx, "create announcement", err)
	}
	full, err := s.repo.Get(ctx, a.ID)
	if err != nil {
		return nil, s.internal(ctx, "reload announcement", err)
	}
	return &announcementsv1.CreateAnnouncementResponse{Announcement: announcementProto(full)}, nil
}

func (s *announcementAdminService) UpdateAnnouncement(ctx context.Context, req *announcementsv1.UpdateAnnouncementRequest) (*announcementsv1.UpdateAnnouncementResponse, error) {
	id := strings.TrimSpace(req.GetId())
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	a, err := announcementFromInput(req.GetAnnouncement())
	if err != nil {
		return nil, err
	}
	a.ID = id
	in := req.GetAnnouncement()
	found, err := s.repo.Update(ctx, a, in.GetTargetGroupIds(), in.GetTargetTierIds())
	if err != nil {
		return nil, s.mapWrite(ctx, "update announcement", err)
	}
	if !found {
		return nil, status.Error(codes.NotFound, "announcement not found")
	}
	full, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, s.internal(ctx, "reload announcement", err)
	}
	return &announcementsv1.UpdateAnnouncementResponse{Announcement: announcementProto(full)}, nil
}

func (s *announcementAdminService) SetAnnouncementStatus(ctx context.Context, req *announcementsv1.SetAnnouncementStatusRequest) (*announcementsv1.SetAnnouncementStatusResponse, error) {
	id := strings.TrimSpace(req.GetId())
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	st := models.AnnouncementStatus(strings.TrimSpace(req.GetStatus()))
	if !st.Valid() {
		return nil, status.Error(codes.InvalidArgument, "invalid status")
	}
	found, err := s.repo.SetStatus(ctx, id, st)
	if err != nil {
		return nil, s.internal(ctx, "set announcement status", err)
	}
	if !found {
		return nil, status.Error(codes.NotFound, "announcement not found")
	}
	full, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, s.internal(ctx, "reload announcement", err)
	}
	return &announcementsv1.SetAnnouncementStatusResponse{Announcement: announcementProto(full)}, nil
}

func (s *announcementAdminService) DeleteAnnouncement(ctx context.Context, req *announcementsv1.DeleteAnnouncementRequest) (*announcementsv1.DeleteAnnouncementResponse, error) {
	id := strings.TrimSpace(req.GetId())
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	found, deleted, err := s.repo.Delete(ctx, id)
	if err != nil {
		return nil, s.internal(ctx, "delete announcement", err)
	}
	if !found {
		return nil, status.Error(codes.NotFound, "announcement not found")
	}
	if !deleted {
		return nil, status.Error(codes.FailedPrecondition, "only draft announcements can be deleted; archive it instead")
	}
	return &announcementsv1.DeleteAnnouncementResponse{}, nil
}

// announcementFromInput validates the shared Create/Update input and maps it to a
// model (without id / status). Enforces title+body, the CTA label⊕url pairing,
// and ends_at > starts_at.
func announcementFromInput(in *announcementsv1.AnnouncementInput) (*models.Announcement, error) {
	if in == nil {
		return nil, status.Error(codes.InvalidArgument, "announcement is required")
	}
	title := strings.TrimSpace(in.GetTitle())
	body := strings.TrimSpace(in.GetBody())
	if title == "" || body == "" {
		return nil, status.Error(codes.InvalidArgument, "title and body are required")
	}
	label := strings.TrimSpace(in.GetCtaLabel())
	ctaURL := strings.TrimSpace(in.GetCtaUrl())
	if (label == "") != (ctaURL == "") {
		return nil, status.Error(codes.InvalidArgument, "cta_label and cta_url must be set together")
	}
	a := &models.Announcement{
		Title:     title,
		Body:      body,
		ImageURL:  strings.TrimSpace(in.GetImageUrl()),
		ImageAlt:  strings.TrimSpace(in.GetImageAlt()),
		CTALabel:  label,
		CTAURL:    ctaURL,
		TargetAll: in.GetTargetAll(),
	}
	if ts := in.GetStartsAt(); ts != nil {
		t := ts.AsTime()
		a.StartsAt = &t
	}
	if ts := in.GetEndsAt(); ts != nil {
		t := ts.AsTime()
		a.EndsAt = &t
	}
	if a.StartsAt != nil && a.EndsAt != nil && !a.EndsAt.After(*a.StartsAt) {
		return nil, status.Error(codes.InvalidArgument, "ends_at must be after starts_at")
	}
	return a, nil
}

func announcementProto(a *models.Announcement) *announcementsv1.Announcement {
	out := &announcementsv1.Announcement{
		Id:             a.ID,
		Title:          a.Title,
		Body:           a.Body,
		ImageUrl:       a.ImageURL,
		ImageAlt:       a.ImageAlt,
		CtaLabel:       a.CTALabel,
		CtaUrl:         a.CTAURL,
		TargetAll:      a.TargetAll,
		TargetGroupIds: a.TargetGroupIDs,
		TargetTierIds:  a.TargetTierIDs,
		Status:         string(a.Status),
		CreatedAt:      timestamppb.New(a.CreatedAt),
		UpdatedAt:      timestamppb.New(a.UpdatedAt),
	}
	if a.StartsAt != nil {
		out.StartsAt = timestamppb.New(*a.StartsAt)
	}
	if a.EndsAt != nil {
		out.EndsAt = timestamppb.New(*a.EndsAt)
	}
	if a.PublishedAt != nil {
		out.PublishedAt = timestamppb.New(*a.PublishedAt)
	}
	return out
}

func announcementStatsProto(s models.AnnouncementStats) *announcementsv1.AnnouncementStats {
	return &announcementsv1.AnnouncementStats{
		UniqueUsersClicked:     int32(s.UniqueUsersClicked),
		UniqueTenantsClicked:   int32(s.UniqueTenantsClicked),
		UniqueUsersDismissed:   int32(s.UniqueUsersDismissed),
		UniqueTenantsDismissed: int32(s.UniqueTenantsDismissed),
		EligibleTenants:        int32(s.EligibleTenants),
		EligibleUsers:          int32(s.EligibleUsers),
	}
}

func announcementWithStatsProto(a *models.Announcement, st models.AnnouncementStats) *announcementsv1.AnnouncementWithStats {
	return &announcementsv1.AnnouncementWithStats{
		Announcement: announcementProto(a),
		Stats:        announcementStatsProto(st),
	}
}

// mapWrite turns a targeting FK violation (unknown tier/group id) into
// InvalidArgument; anything else is an internal error.
func (s *announcementAdminService) mapWrite(ctx context.Context, op string, err error) error {
	if pgCode(err) == pgFKViolation {
		return status.Error(codes.InvalidArgument, "unknown target tier or group")
	}
	return s.internal(ctx, op, err)
}

// internal logs an unexpected error (op only, never sensitive values) and returns
// a generic Internal status so DB internals never cross the wire.
func (s *announcementAdminService) internal(ctx context.Context, op string, err error) error {
	slog.ErrorContext(ctx, "announcement admin "+op, logging.AttrComponent, "grpcserver", logging.AttrError, err)
	return status.Error(codes.Internal, op+" failed")
}

// encodeAnnouncementCursor / decodeAnnouncementCursor make an opaque keyset cursor
// over (created_at, id): base64("<unixNanos>|<id>"). Clients treat it as opaque.
func encodeAnnouncementCursor(createdAt time.Time, id string) string {
	raw := strconv.FormatInt(createdAt.UnixNano(), 10) + "|" + id
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeAnnouncementCursor(token string) (time.Time, string, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return time.Time{}, "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return time.Time{}, "", err
	}
	unixNanos, id, ok := strings.Cut(string(raw), "|")
	if !ok || id == "" {
		return time.Time{}, "", errors.New("malformed cursor")
	}
	nanos, err := strconv.ParseInt(unixNanos, 10, 64)
	if err != nil {
		return time.Time{}, "", err
	}
	return time.Unix(0, nanos).UTC(), id, nil
}
