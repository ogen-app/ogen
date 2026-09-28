// Package signup owns the transactional self-service signup use case:
// atomically create a tenant, its owning account + first membership user, and a
// session, and enqueue the profile-bootstrap and lifecycle-email
// jobs in the same transaction so they exist iff the tenant does.
//
// It is the application-layer counterpart to the TenantsHandler transport: the
// handler parses the request, throttles per IP, sets the session cookie and
// records the activity event; this service owns the transaction boundary and
// the entity orchestration, so no business logic or RunInTx lives in transport.
package signup

import (
	"cmp"
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

// sessionTTL mirrors handlers.sessionTTL: a fresh signup opens a
// session that lives as long as a login's.
const sessionTTL = 7 * 24 * time.Hour

// ErrEmailInUse is returned when the email already identifies an account.
// The transport maps it to 409; an existing account should log in and
// create a workspace instead.
var ErrEmailInUse = errors.New("email already in use")

// ProfileEnqueuer enqueues the Zernio profile-bootstrap job inside the
// signup transaction. nil disables it (no profile provisioning).
type ProfileEnqueuer interface {
	EnqueueBootstrapProfileTx(ctx context.Context, tx *sql.Tx, tenantID string) error
}

// EmailEnqueuer enqueues the welcome + onboarding-drip jobs inside the
// signup transaction. nil disables it (no lifecycle mail).
type EmailEnqueuer interface {
	EnqueueWelcomeEmailTx(ctx context.Context, tx *sql.Tx, userID, tenantID string) error
	EnqueueDripTx(ctx context.Context, tx *sql.Tx, userID, tenantID string) error
}

// HarborEnqueuer enqueues the "notify Harbor a new tenant registered"
// webhook job inside the signup transaction, so operators are notified iff the
// tenant commits. nil disables it (no operator notification).
type HarborEnqueuer interface {
	EnqueueNotifyHarborTenantRegisteredTx(ctx context.Context, tx *sql.Tx, tenantID string) error
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
	harbor   HarborEnqueuer

	// now is injectable so tests can pin "current time". nil → time.Now().UTC().
	now func() time.Time
}

// New wires a signup Service. profiles may be nil (no profile bootstrap).
func New(db *bun.DB, accounts repository.AccountRepository, tenants repository.TenantRepository, profiles ProfileEnqueuer) *Service {
	return &Service{db: db, accounts: accounts, tenants: tenants, profiles: profiles}
}

// SetEmailEnqueuer wires the lifecycle-email enqueuer (nil-safe: no mail sent).
func (s *Service) SetEmailEnqueuer(e EmailEnqueuer) { s.emails = e }

// SetHarborEnqueuer wires the new-tenant operator-notification enqueuer.
// nil-safe: no webhook is enqueued.
func (s *Service) SetHarborEnqueuer(e HarborEnqueuer) { s.harbor = e }

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
	// The unique constraint on accounts.email is the TOCTOU backstop.
	if _, err := s.accounts.GetByEmail(ctx, in.Email); err == nil {
		return nil, ErrEmailInUse
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("signup: check email: %w", err)
	}

	slug, err := s.uniqueSlug(ctx, in.TenantName)
	if err != nil {
		return nil, fmt.Errorf("signup: allocate slug: %w", err)
	}
	e, err := newSignupEntities(in, slug, s.clock())
	if err != nil {
		return nil, fmt.Errorf("signup: %w", err)
	}

	if err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		for _, m := range []any{e.tenant, e.account, e.user, e.session} {
			if _, err := tx.NewInsert().Model(m).Exec(ctx); err != nil {
				return err
			}
		}
		return s.enqueueSideEffectsTx(ctx, tx.Tx, e.user.ID, e.tenant.ID)
	}); err != nil {
		// A concurrent signup with the same email passes the pre-check but
		// loses the race to the accounts.email unique constraint.
		if isUniqueViolation(err) {
			return nil, ErrEmailInUse
		}
		return nil, fmt.Errorf("signup: create tenant: %w", err)
	}

	return &Result{Tenant: e.tenant, User: e.user, Session: e.session}, nil
}

// signupEntities are the rows a signup inserts.
type signupEntities struct {
	tenant  *models.Tenant
	account *models.Account
	user    *models.User
	session *models.Session
}

// newSignupEntities generates the ids, password hash and session token
// and assembles the rows. The account holds the credential; the user row
// is that account's owner membership of the new workspace. New
// workspaces start on the seeded default tier.
func newSignupEntities(in Input, slug string, now time.Time) (*signupEntities, error) {
	var tenantID, accountID, userID string
	for _, id := range []*string{&tenantID, &accountID, &userID} {
		v, err := models.NewID()
		if err != nil {
			return nil, fmt.Errorf("generate id: %w", err)
		}
		*id = v
	}
	hash, err := models.HashPassword(in.Password)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}
	token, err := models.NewSessionToken()
	if err != nil {
		return nil, fmt.Errorf("generate session token: %w", err)
	}
	return &signupEntities{
		tenant:  &models.Tenant{ID: tenantID, Name: in.TenantName, Slug: slug, TierID: models.DefaultTierID, CreatedAt: now, UpdatedAt: now},
		account: &models.Account{ID: accountID, Email: in.Email, PasswordHash: hash, Name: in.UserName, CreatedAt: now, UpdatedAt: now},
		user:    &models.User{ID: userID, AccountID: accountID, TenantID: tenantID, Name: in.UserName, Email: in.Email, Role: models.RoleOwner, CreatedAt: now, UpdatedAt: now},
		session: &models.Session{ID: token, AccountID: accountID, UserID: userID, TenantID: tenantID, ExpiresAt: now.Add(sessionTTL), CreatedAt: now},
	}, nil
}

// enqueueSideEffectsTx enqueues the Zernio profile bootstrap, the welcome
// and onboarding-drip mails and the Harbor new-tenant notification inside
// the signup transaction, so each job exists iff the tenant commits. The
// enqueues are local inserts; the external calls happen in the workers,
// so signup never blocks on Zernio or Harbor reachability.
func (s *Service) enqueueSideEffectsTx(ctx context.Context, tx *sql.Tx, userID, tenantID string) error {
	if s.profiles != nil {
		if err := s.profiles.EnqueueBootstrapProfileTx(ctx, tx, tenantID); err != nil {
			return err
		}
	}
	if s.emails != nil {
		if err := s.emails.EnqueueWelcomeEmailTx(ctx, tx, userID, tenantID); err != nil {
			return err
		}
		if err := s.emails.EnqueueDripTx(ctx, tx, userID, tenantID); err != nil {
			return err
		}
	}
	if s.harbor != nil {
		return s.harbor.EnqueueNotifyHarborTenantRegisteredTx(ctx, tx, tenantID)
	}
	return nil
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
	s = cmp.Or(s, "tenant")
	return s
}

// isUniqueViolation reports whether err is a Postgres unique-constraint
// violation (SQLSTATE 23505).
func isUniqueViolation(err error) bool {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	return ok && pgErr.Code == "23505"
}
