package signupaccess

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// CreateInviteInput is a new tester invite.
type CreateInviteInput struct {
	Email    string
	TierID   string
	GroupIDs []string
	// ExpiresInHours defaults to 14 days.
	ExpiresInHours int
	Message        string
	Note           string
	CreatedBy      string
}

// CreateInvite stores a tester invite and queues its email in one
// transaction. A pending invite for the same address is replaced (its link
// stops working); replaced reports that. The tier needs a published version,
// purchasable or not: hidden tiers are how testers get plans that aren't sold.
func (s *Service) CreateInvite(ctx context.Context, in CreateInviteInput) (*models.TenantInviteView, bool, error) {
	email, err := validEmail(in.Email)
	if err != nil {
		return nil, false, err
	}
	ttl, err := validTTL(in.ExpiresInHours)
	if err != nil {
		return nil, false, err
	}
	message, note, createdBy := strings.TrimSpace(in.Message), strings.TrimSpace(in.Note), strings.TrimSpace(in.CreatedBy)
	if err := validText(message, note, createdBy); err != nil {
		return nil, false, err
	}
	tierID := strings.TrimSpace(in.TierID)
	if err := s.checkTier(ctx, tierID); err != nil {
		return nil, false, err
	}
	groupIDs, err := s.checkGroups(ctx, in.GroupIDs)
	if err != nil {
		return nil, false, err
	}
	if _, err := s.d.Accounts.GetByEmailFold(ctx, email); err == nil {
		return nil, false, ErrAccountExists
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}

	id, err := models.NewID()
	if err != nil {
		return nil, false, err
	}
	token, hash, err := models.NewInvitationToken()
	if err != nil {
		return nil, false, err
	}
	now := s.now()
	inv := &models.TenantInvite{
		ID: id, Email: email, TokenHash: hash, TierID: &tierID,
		Message: optional(message), Note: optional(note), CreatedBy: createdBy,
		Status: models.TenantInvitePending, TTLHours: ttl, ExpiresAt: now.Add(time.Duration(ttl) * time.Hour),
		SentCount: 1, LastSentAt: now, CreatedAt: now, UpdatedAt: now,
	}
	var replaced bool
	err = s.d.DB.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if replaced, err = s.d.Invites.CreateReplacingPendingTx(ctx, tx, inv, groupIDs); err != nil {
			return err
		}
		return s.enqueueEmailTx(ctx, tx, inv, token)
	})
	if err != nil {
		return nil, false, err
	}
	invitesCreated.Add(1)
	view, err := s.d.Invites.GetView(ctx, id)
	return view, replaced, err
}

// ResendInvite rotates the token of a pending invite (expired included),
// extends its expiry (the original lifetime, or expiresInHours when set) and
// sends the email again. The previous link stops working: the raw token is
// never stored, so it cannot be sent twice.
func (s *Service) ResendInvite(ctx context.Context, id string, expiresInHours int) (*models.TenantInviteView, error) {
	current, err := s.d.Invites.Get(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	ttl := current.TTLHours
	if expiresInHours != 0 {
		if ttl, err = validTTL(expiresInHours); err != nil {
			return nil, err
		}
	}
	token, hash, err := models.NewInvitationToken()
	if err != nil {
		return nil, err
	}
	now := s.now()
	err = s.d.DB.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		inv, err := s.d.Invites.ResendTx(ctx, tx, id, hash, now.Add(time.Duration(ttl)*time.Hour), now)
		if err != nil {
			return err
		}
		return s.enqueueEmailTx(ctx, tx, inv, token)
	})
	if err := inviteErr(err); err != nil {
		return nil, err
	}
	invitesResent.Add(1)
	return s.d.Invites.GetView(ctx, id)
}

// RevokeInvite kills a pending invite's link. A workspace already created from
// an invite is never affected.
func (s *Service) RevokeInvite(ctx context.Context, id string) (*models.TenantInviteView, error) {
	if _, err := s.d.Invites.Revoke(ctx, id, s.now()); err != nil {
		return nil, inviteErr(err)
	}
	invitesRevoked.Add(1)
	return s.d.Invites.GetView(ctx, id)
}

// GetInvite returns one invite with its names and delivery state.
func (s *Service) GetInvite(ctx context.Context, id string) (*models.TenantInviteView, error) {
	v, err := s.d.Invites.GetView(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return v, err
}

// List page sizes.
const (
	DefaultPageSize = 50
	MaxPageSize     = 200
)

// ListInvitesInput selects one page of invites, newest first.
type ListInvitesInput struct {
	// Status is pending, expired, redeemed, revoked, or empty for all.
	Status     string
	EmailQuery string
	PageSize   int
	// AfterCreatedAt/AfterID is the last row of the previous page.
	AfterCreatedAt time.Time
	AfterID        string
}

// ListInvites returns one page and whether another follows.
func (s *Service) ListInvites(ctx context.Context, in ListInvitesInput) ([]models.TenantInviteView, bool, error) {
	switch in.Status {
	case "", models.TenantInvitePending, models.TenantInviteExpired, models.TenantInviteRedeemed, models.TenantInviteRevoked:
	default:
		return nil, false, invalid("status must be pending, expired, redeemed or revoked")
	}
	size := in.PageSize
	if size <= 0 {
		size = DefaultPageSize
	}
	size = min(size, MaxPageSize)
	rows, err := s.d.Invites.List(ctx, repository.TenantInviteFilter{
		Status: in.Status, EmailQuery: in.EmailQuery, Limit: size + 1,
		AfterCreatedAt: in.AfterCreatedAt, AfterID: in.AfterID, Now: s.now(),
	})
	if err != nil {
		return nil, false, err
	}
	more := len(rows) > size
	if more {
		rows = rows[:size]
	}
	return rows, more, nil
}

// enqueueEmailTx queues the invite email with the fresh token's link. The
// idempotency key names the send, so a resend is a new email.
func (s *Service) enqueueEmailTx(ctx context.Context, tx bun.Tx, inv *models.TenantInvite, token string) error {
	if s.d.Emails == nil {
		return nil
	}
	link := strings.TrimRight(s.d.AppBaseURL, "/") + "/signup?invite=" + token
	message := ""
	if inv.Message != nil {
		message = *inv.Message
	}
	return s.d.Emails.EnqueueTenantInviteEmailTx(ctx, tx.Tx, inv.ID, inv.SentCount, inv.Email, link,
		inv.ExpiresAt.UTC().Format("2 January 2006"), message)
}

// checkTier requires a known tier with a published version.
func (s *Service) checkTier(ctx context.Context, tierID string) error {
	if tierID == "" {
		return invalid("tier_id is required")
	}
	if _, err := s.d.Tiers.GetByID(ctx, tierID); errors.Is(err, sql.ErrNoRows) {
		return invalid("unknown tier")
	} else if err != nil {
		return err
	}
	if _, err := s.d.TierVersions.LatestActiveByTier(ctx, tierID); errors.Is(err, sql.ErrNoRows) {
		return ErrTierNotActive
	} else if err != nil {
		return err
	}
	return nil
}

// checkGroups trims and dedupes the ids and requires each to exist.
func (s *Service) checkGroups(ctx context.Context, raw []string) ([]string, error) {
	var ids []string
	for _, id := range raw {
		if id = strings.TrimSpace(id); id != "" && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	for _, id := range ids {
		if _, err := s.d.Groups.GetByID(ctx, id); errors.Is(err, sql.ErrNoRows) {
			return nil, invalid("unknown group " + id)
		} else if err != nil {
			return nil, err
		}
	}
	return ids, nil
}

func validTTL(hours int) (int, error) {
	if hours == 0 {
		return models.TenantInviteDefaultTTLHours, nil
	}
	if hours < models.TenantInviteMinTTLHours || hours > models.TenantInviteMaxTTLHours {
		return 0, invalid("expires_in_hours must be between 1 and 720")
	}
	return hours, nil
}

func validText(message, note, createdBy string) error {
	switch {
	case createdBy == "":
		return invalid("created_by is required")
	case len([]rune(message)) > models.TenantInviteTextMax:
		return invalid("message is longer than 1000 characters")
	case len([]rune(note)) > models.TenantInviteTextMax:
		return invalid("note is longer than 1000 characters")
	}
	return nil
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return new(s)
}

func inviteErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, sql.ErrNoRows):
		return ErrNotFound
	case errors.Is(err, repository.ErrTenantInviteNotPending):
		return ErrNotPending
	default:
		return err
	}
}
