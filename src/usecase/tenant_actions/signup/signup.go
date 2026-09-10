// Package signup owns the transactional self-service signup use case (CON-97):
// atomically create a tenant, its owning account + first membership user, and a
// session, and enqueue the profile-bootstrap (CON-102) and lifecycle-email
// (CON-154) jobs in the same transaction so they exist iff the tenant does.
//
// It is the application-layer counterpart to the TenantsHandler transport: the
// handler parses the request, throttles per IP, sets the session cookie and
// records the activity event; this service owns the transaction boundary and
// the entity orchestration, so no business logic or RunInTx lives in transport.
package signup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// sessionTTL mirrors handlers.sessionTTL (CON-147): a fresh signup opens a
// session that lives as long as a login's.
const sessionTTL = 7 * 24 * time.Hour

// ErrEmailInUse is returned when the email already identifies an account
// (CON-147). The transport maps it to 409; an existing account should log in and
// create a workspace instead.
var ErrEmailInUse = errors.New("email already in use")

// ProfileEnqueuer enqueues the CON-102 Zernio profile-bootstrap job inside the
// signup transaction. nil disables it (no profile provisioning).
type ProfileEnqueuer interface {
	EnqueueBootstrapProfileTx(ctx context.Context, tx *sql.Tx, tenantID string) error
}

// EmailEnqueuer enqueues the CON-154 welcome + onboarding-drip jobs inside the
// signup transaction. nil disables it (no lifecycle mail).
type EmailEnqueuer interface {
	EnqueueWelcomeEmailTx(ctx context.Context, tx *sql.Tx, userID, tenantID string) error
	EnqueueDripTx(ctx context.Context, tx *sql.Tx, userID, tenantID string) error
}

// Input is the data a signup needs.
type Input struct {
	TenantName string
	UserName   string
	Email      string
	Password   string
}

// Result is what a committed signup produced. Session.ID is the raw session
// token the caller sets as a cookie.
type Result struct {
	Tenant  *models.Tenant
	User    *models.User
	Session *models.Session
}

// Service performs the signup use case. Construct with New; wire the optional
// email enqueuer with SetEmailEnqueuer.
type Service struct {
	db       *bun.DB
	accounts repository.AccountRepository
	tenants  repository.TenantRepository
	profiles ProfileEnqueuer
	emails   EmailEnqueuer

	// now is injectable so tests can pin "current time". nil → time.Now().UTC().
	now func() time.Time
}

// New wires a signup Service. profiles may be nil (no profile bootstrap).
func New(db *bun.DB, accounts repository.AccountRepository, tenants repository.TenantRepository, profiles ProfileEnqueuer) *Service {
	return &Service{db: db, accounts: accounts, tenants: tenants, profiles: profiles}
}

// SetEmailEnqueuer wires the lifecycle-email enqueuer (nil-safe: no mail sent).
func (s *Service) SetEmailEnqueuer(e EmailEnqueuer) { s.emails = e }

func (s *Service) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now().UTC()
}

// Create runs the signup transactionally. It returns ErrEmailInUse (map to 409)
// when the email already identifies an account — both from the up-front check
// and, under a race, from the accounts.email unique-constraint backstop.
func (s *Service) Create(ctx context.Context, in Input) (*Result, error) {
	// Email uniquely identifies an ACCOUNT (CON-147). Reject a duplicate up front;
	// the unique constraint below is the TOCTOU backstop.
	if _, err := s.accounts.GetByEmail(ctx, in.Email); err == nil {
		return nil, ErrEmailInUse
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	slug, err := s.uniqueSlug(ctx, in.TenantName)
	if err != nil {
		return nil, err
	}

	tenantID, err := models.NewID()
	if err != nil {
		return nil, err
	}
	accountID, err := models.NewID()
	if err != nil {
		return nil, err
	}
	userID, err := models.NewID()
	if err != nil {
		return nil, err
	}
	hash, err := models.HashPassword(in.Password)
	if err != nil {
		return nil, err
	}
	token, err := models.NewSessionToken()
	if err != nil {
		return nil, err
	}

	now := s.clock()
	// CON-208: tenant.tier_id is required (NOT NULL). New workspaces start on the
	// seeded default tier; Harbor reassigns them later over the gRPC admin surface.
	tenant := &models.Tenant{ID: tenantID, Name: in.TenantName, Slug: slug, TierID: models.DefaultTierID, CreatedAt: now, UpdatedAt: now}
	// Identity (the credential) lives on the account; the users row is this
	// account's membership of the new workspace (CON-147). The signup user creates
	// the workspace, so they are its first owner (CON-26).
	account := &models.Account{ID: accountID, Email: in.Email, PasswordHash: hash, Name: in.UserName, CreatedAt: now, UpdatedAt: now}
	user := &models.User{ID: userID, AccountID: accountID, TenantID: tenantID, Name: in.UserName, Email: in.Email, Role: models.RoleOwner, CreatedAt: now, UpdatedAt: now}
	session := &models.Session{ID: token, AccountID: accountID, UserID: userID, TenantID: tenantID, ExpiresAt: now.Add(sessionTTL), CreatedAt: now}

	if err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewInsert().Model(tenant).Exec(ctx); err != nil {
			return err
		}
		if _, err := tx.NewInsert().Model(account).Exec(ctx); err != nil {
			return err
		}
		if _, err := tx.NewInsert().Model(user).Exec(ctx); err != nil {
			return err
		}
		if _, err := tx.NewInsert().Model(session).Exec(ctx); err != nil {
			return err
		}
		// CON-102: eagerly provision this tenant's Zernio profile in the
		// background, enqueued inside THIS tx so the job exists iff the tenant
		// does. The enqueue is a local DB insert; the Zernio call happens later in
		// the worker, so signup never blocks on Zernio reachability.
		if s.profiles != nil {
			if err := s.profiles.EnqueueBootstrapProfileTx(ctx, tx.Tx, tenantID); err != nil {
				return err
			}
		}
		// CON-154: welcome (immediate) + onboarding drip (day 2/5/7) enqueued in
		// this same tx, so a rolled-back signup queues no mail and a committed one
		// durably queues exactly one welcome + drip.
		if s.emails != nil {
			if err := s.emails.EnqueueWelcomeEmailTx(ctx, tx.Tx, userID, tenantID); err != nil {
				return err
			}
			if err := s.emails.EnqueueDripTx(ctx, tx.Tx, userID, tenantID); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		// A concurrent signup with the same email passes the pre-check but loses
		// the race to the accounts.email unique constraint — surface that as
		// ErrEmailInUse (409) rather than a raw 500.
		if isUniqueViolation(err) {
			return nil, ErrEmailInUse
		}
		return nil, err
	}

	return &Result{Tenant: tenant, User: user, Session: session}, nil
}

// uniqueSlug returns slugify(name), suffixed with -2, -3, … until free.
func (s *Service) uniqueSlug(ctx context.Context, name string) (string, error) {
	base := slugify(name)
	slug := base
	for i := 2; i <= 1000; i++ {
		_, err := s.tenants.GetBySlug(ctx, slug)
		if errors.Is(err, sql.ErrNoRows) {
			return slug, nil
		}
		if err != nil {
			return "", err
		}
		slug = fmt.Sprintf("%s-%d", base, i)
	}
	return "", errors.New("could not allocate a unique slug")
}

var slugNonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// slugify produces a lowercase, URL-safe label from a tenant name. Kept
// byte-identical to handlers.slugify so a signup slugs exactly as a workspace
// created while logged in.
func slugify(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = slugNonAlnum.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "tenant"
	}
	return s
}

// isUniqueViolation reports whether err is a Postgres unique-constraint
// violation (SQLSTATE 23505).
func isUniqueViolation(err error) bool {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	return ok && pgErr.Code == "23505"
}
