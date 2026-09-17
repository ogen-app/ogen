package report

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/ogen-app/ogen/src/infra/repository"
)

// listRowCap bounds how many rows the day-list load pulls per stream. It is far
// above any realistic page of `limit` local days for a workspace; if a stream
// nonetheless hits it, the coverage floor drops the possibly-undercounted tail
// so every returned day is exact and the client pages on with `before`.
const listRowCap = 5000

// Service computes the Activity daily report from tenant-scoped repositories
// (CON-285). All reads inherit tenant scoping from the repositories' models, so
// it is safe to call in any tenant request context; an optional campaign_id
// narrows the same computation to one campaign.
type Service struct {
	posts     repository.PostRepository
	postLogs  repository.PostLogRepository
	campaigns repository.CampaignRepository
	now       func() time.Time
}

// New builds a Service.
func New(posts repository.PostRepository, postLogs repository.PostLogRepository, campaigns repository.CampaignRepository) *Service {
	return &Service{posts: posts, postLogs: postLogs, campaigns: campaigns, now: time.Now}
}

// Report computes the full single-day report for `date` in `tz`, optionally for
// one campaign. A future or empty day returns a zeroed report, not an error
// (CON-285 FR5). tz must be a loadable IANA zone; a foreign campaign_id is a
// 404-mapped ErrCampaignNotFound.
func (s *Service) Report(ctx context.Context, date, tz, campaignID string) (*Report, error) {
	defer track(s.now())
	Requests.Add(1)

	loc, err := loadLocation(tz)
	if err != nil {
		return nil, err
	}
	w, err := dayWindow(date, loc)
	if err != nil {
		return nil, err
	}
	if err := s.requireCampaign(ctx, campaignID); err != nil {
		return nil, err
	}

	in, err := s.loadInputs(ctx, w.Lo, w.Hi, campaignID, 0)
	if err != nil {
		return nil, err
	}
	r := computeReport(in, date, tz, w)
	return &r, nil
}

// Reports lists the non-empty local days newest-first, at most `limit`, older
// than `before` (exclusive; empty = up to now). Keyset-paginated by date via
// `before` (CON-285 FR4).
func (s *Service) Reports(ctx context.Context, tz, campaignID, before string, limit int) (*ReportList, error) {
	defer track(s.now())
	Requests.Add(1)

	loc, err := loadLocation(tz)
	if err != nil {
		return nil, err
	}
	upper := s.now()
	if before != "" {
		d, perr := time.ParseInLocation(dateLayout, before, loc)
		if perr != nil {
			return nil, ErrInvalidDate
		}
		upper = d // strictly-before-`before` day = timestamps < local midnight of `before`
	}
	if err := s.requireCampaign(ctx, campaignID); err != nil {
		return nil, err
	}

	in, floor, err := s.loadInputsBefore(ctx, upper, campaignID, listRowCap)
	if err != nil {
		return nil, err
	}
	return &ReportList{
		Reports:     bucketReports(in, loc, limit, floor),
		GeneratedAt: s.now().UTC(),
	}, nil
}

// requireCampaign 404s a campaign_id absent from this tenant. Empty = whole
// workspace, no check. The repository is tenant-scoped, so a foreign id is
// indistinguishable from a missing one — no cross-tenant enumeration.
func (s *Service) requireCampaign(ctx context.Context, campaignID string) error {
	if campaignID == "" {
		return nil
	}
	if _, err := s.campaigns.GetByID(ctx, campaignID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrCampaignNotFound
		}
		return err
	}
	return nil
}

// loadInputs pulls the four streams for the window [from, to). limit 0 = no cap
// (single-day). Used by Report.
func (s *Service) loadInputs(ctx context.Context, from, to time.Time, campaignID string, limit int) (Inputs, error) {
	in, _, err := s.load(ctx, from, to, campaignID, limit)
	return in, err
}

// loadInputsBefore pulls the four streams for [zero, upper), each capped at
// `cap`, and returns the coverage floor: the newest timestamp below which a
// capped stream may be missing rows (zero if nothing was truncated).
func (s *Service) loadInputsBefore(ctx context.Context, upper time.Time, campaignID string, rowCap int) (Inputs, time.Time, error) {
	return s.load(ctx, time.Time{}, upper, campaignID, rowCap)
}

// load fetches and maps all four streams. When limit > 0 and a stream returns
// exactly `limit` rows it may be truncated; its oldest returned timestamp raises
// the coverage floor accordingly.
func (s *Service) load(ctx context.Context, from, to time.Time, campaignID string, limit int) (Inputs, time.Time, error) {
	var (
		in    Inputs
		floor time.Time
	)
	raise := func(rowsLen int, oldest time.Time) {
		if limit > 0 && rowsLen == limit && oldest.After(floor) {
			floor = oldest
		}
	}

	pub, err := s.posts.PublishedProjectionBetween(ctx, from, to, campaignID, limit)
	if err != nil {
		return Inputs{}, time.Time{}, err
	}
	in.Published = make([]PublishedRow, 0, len(pub))
	for _, p := range pub {
		if p.PublishedAt == nil {
			continue
		}
		in.Published = append(in.Published, PublishedRow{At: *p.PublishedAt, PlatformID: p.PlatformID})
	}
	if len(pub) > 0 {
		raise(len(pub), *pub[len(pub)-1].PublishedAt)
	}

	failed, err := s.postLogs.TerminalTransitionsBetween(ctx, from, to, campaignID, limit)
	if err != nil {
		return Inputs{}, time.Time{}, err
	}
	in.Failed = make([]FailedRow, 0, len(failed))
	for _, f := range failed {
		in.Failed = append(in.Failed, FailedRow{
			At:            f.At,
			PostID:        f.PostID,
			PlatformID:    f.PlatformID,
			Status:        f.Status,
			FailureReason: f.FailureReason,
		})
	}
	if len(failed) > 0 {
		raise(len(failed), failed[len(failed)-1].At)
	}

	created, err := s.posts.CreatedProjectionBetween(ctx, from, to, campaignID, limit)
	if err != nil {
		return Inputs{}, time.Time{}, err
	}
	in.Created = make([]CreatedRow, 0, len(created))
	for _, c := range created {
		in.Created = append(in.Created, CreatedRow{
			At:        c.CreatedAt,
			CreatedBy: c.CreatedBy,
			Scheduled: c.ScheduledAt != nil,
		})
	}
	if len(created) > 0 {
		raise(len(created), created[len(created)-1].CreatedAt)
	}

	camps, err := s.campaigns.CreatedBetween(ctx, from, to, campaignID, limit)
	if err != nil {
		return Inputs{}, time.Time{}, err
	}
	in.Campaigns = make([]CampaignRow, 0, len(camps))
	for _, cm := range camps {
		in.Campaigns = append(in.Campaigns, CampaignRow{At: cm.CreatedAt, CampaignID: cm.ID})
	}
	if len(camps) > 0 {
		raise(len(camps), camps[len(camps)-1].CreatedAt)
	}

	return in, floor, nil
}

// loadLocation resolves an IANA tz name, mapping any failure (incl. empty) to
// ErrInvalidTZ so the handler answers 400. "Local" is rejected explicitly:
// time.LoadLocation("Local") succeeds but binds to the server's zone, so the
// same request would bucket differently between deployments — the report must be
// pinned to the caller's IANA zone.
func loadLocation(tz string) (*time.Location, error) {
	if tz == "" || tz == "Local" {
		return nil, ErrInvalidTZ
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, ErrInvalidTZ
	}
	return loc, nil
}

// track records elapsed compute time (coarse, CON-285 §11).
func track(start time.Time) {
	ComputeMillisTotal.Add(time.Since(start).Milliseconds())
}
