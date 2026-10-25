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
	"expvar"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// sessionTTL mirrors handlers.sessionTTL: a fresh signup opens a
// session that lives as long as a login's.
const sessionTTL = 7 * 24 * time.Hour

// ErrEmailInUse is returned when the email already identifies an account.
// The transport maps it to 409; an existing account should log in and
// create a workspace instead.
var ErrEmailInUse = errors.New("email already in use")

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

// ErrIdentityLinked is returned when the provider identity a signup carries is
// already linked to an account.
var ErrIdentityLinked = errors.New("identity already linked")

// Signup access errors.
var (
	// ErrInviteRequired: self-registration is closed (invite-only mode) and
	// the signup carries no tester invite.
	ErrInviteRequired = errors.New("invite_required")
	// ErrInviteInvalid: the tester invite is unknown, expired, revoked or
	// already redeemed. One error for all of them.
	ErrInviteInvalid = errors.New("invite invalid")
	// ErrInviteEmailMismatch: the signup's address is not the invited one.
	// The invite is left unspent.
	ErrInviteEmailMismatch = errors.New("invite_email_mismatch")
)

var (
	inviteRequiredTotal = expvar.NewInt("signup_invite_required_total")
	invitesRedeemed     = expvar.NewInt("tenant_invites_redeemed_total")
)

// AccessPolicy reports the platform signup settings. *signupaccess.Service
// satisfies it.
type AccessPolicy interface {
	Settings(ctx context.Context) (models.SignupSettings, error)
}

// InviteDeps are what redeeming a tester invite writes through.
type InviteDeps struct {
	Invites      repository.TenantInviteRepository
	TierVersions repository.TenantTierVersionRepository
	Assignments  repository.TenantTierAssignmentRepository
}

// Input is the data a signup needs. A password signup sets Password; a
// provider signup sets Identity instead, and the account starts with no
// password.
type Input struct {
	TenantName string
	UserName   string
	Email      string
	Password   string
	Identity   *Identity
	// InviteToken redeems a tester invite: the workspace is created
	// complimentary, on the invite's tier and in its groups.
	InviteToken string
}

// Identity is a verified provider identity the new account signs in through.
type Identity struct {
	Provider string
	Subject  string
	Email    string
}

// Result is what a committed signup produced. Session.ID is the raw session
// token the caller sets as a cookie.
type Result struct {
	Tenant  *models.Tenant
	User    *models.User
	Session *models.Session
	// Invite is the tester invite the signup redeemed, if any.
	Invite *models.TenantInvite
}

// Service performs the signup use case. Construct with New; wire the optional
// email enqueuer with SetEmailEnqueuer.
type Service struct {
	db       *bun.DB
	accounts repository.AccountRepository
	tenants  repository.TenantRepository
	emails   EmailEnqueuer
	harbor   HarborEnqueuer
	// billingRequired gates new workspaces until they check out.
	billingRequired bool
	// access decides whether a signup without an invite is allowed; nil
	// leaves self-registration open.
	access  AccessPolicy
	invites InviteDeps

	// now is injectable so tests can pin "current time". nil → time.Now().UTC().
	now func() time.Time
}

// New wires a signup Service. The workspace's Zernio profile is not created
// here: the first connect link bootstraps it, so a signup that never connects
// an account costs nothing upstream.
func New(db *bun.DB, accounts repository.AccountRepository, tenants repository.TenantRepository) *Service {
	return &Service{db: db, accounts: accounts, tenants: tenants}
}

// SetBillingRequired makes every new workspace check out before use (on
// while billing is enabled).
func (s *Service) SetBillingRequired(required bool) { s.billingRequired = required }

// SetAccess wires the signup mode and tester-invite redemption.
func (s *Service) SetAccess(policy AccessPolicy, invites InviteDeps) {
	s.access, s.invites = policy, invites
}

// CheckAccess refuses a signup without a tester invite while self-registration
// is closed. A signup carrying a token always passes here; the token itself
// is checked when it is spent.
func (s *Service) CheckAccess(ctx context.Context, inviteToken string) error {
	if s.access == nil || strings.TrimSpace(inviteToken) != "" {
		return nil
	}
	st, err := s.access.Settings(ctx)
	if err != nil {
		return fmt.Errorf("signup: read signup settings: %w", err)
	}
	if st.InviteOnly() {
		inviteRequiredTotal.Add(1)
		return ErrInviteRequired
	}
	return nil
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
// and, under a race, from the accounts.email unique-constraint backstop. A
// provider signup returns ErrIdentityLinked when its identity is already linked.
func (s *Service) Create(ctx context.Context, in Input) (*Result, error) {
	// The gate runs first, so a closed path reveals nothing about addresses.
	if err := s.CheckAccess(ctx, in.InviteToken); err != nil {
		return nil, err
	}
	in.InviteToken = strings.TrimSpace(in.InviteToken)
	if in.InviteToken != "" && s.invites.Invites == nil {
		return nil, ErrInviteInvalid
	}
	// The unique constraint on accounts.email is the TOCTOU backstop. A provider
	// address is matched ignoring case, the way sign-in links it, so a Google
	// signup can't create a near-duplicate of a typed-in address.
	lookup := s.accounts.GetByEmail
	if in.Identity != nil {
		lookup = s.accounts.GetByEmailFold
	}
	if _, err := lookup(ctx, in.Email); err == nil {
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
	e.tenant.BillingRequired = s.billingRequired

	var invite *models.TenantInvite
	if err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var err error
		invite, err = s.createTx(ctx, tx, in, e)
		return err
	}); err != nil {
		return nil, createErr(err)
	}

	if invite != nil {
		invitesRedeemed.Add(1)
	}
	return &Result{Tenant: e.tenant, User: e.user, Session: e.session, Invite: invite}, nil
}

// createTx inserts the signup's rows and enqueues its side effects, redeeming
// the tester invite first when the signup carries one.
func (s *Service) createTx(ctx context.Context, tx bun.Tx, in Input, e *signupEntities) (*models.TenantInvite, error) {
	var invite *models.TenantInvite
	if in.InviteToken != "" {
		var err error
		if invite, err = s.spendInviteTx(ctx, tx, in, e); err != nil {
			return nil, err
		}
	}
	for _, m := range e.rows() {
		if _, err := tx.NewInsert().Model(m).Exec(ctx); err != nil {
			return nil, err
		}
	}
	if invite != nil {
		if err := s.applyInviteTx(ctx, tx, invite, e); err != nil {
			return nil, err
		}
	}
	return invite, s.enqueueSideEffectsTx(ctx, tx.Tx, e.user.ID, e.tenant.ID)
}

// createErr maps a failed signup transaction to the caller's errors.
func createErr(err error) error {
	switch {
	case errors.Is(err, ErrInviteInvalid), errors.Is(err, ErrInviteEmailMismatch):
		return err
	case isUniqueViolationOn(err, identitySubjectConstraint):
		return ErrIdentityLinked
	case isUniqueViolation(err):
		// A concurrent signup with the same email passes the pre-check but
		// loses the race to the accounts.email unique constraint.
		return ErrEmailInUse
	default:
		return fmt.Errorf("signup: create tenant: %w", err)
	}
}

// spendInviteTx consumes the tester invite inside the signup transaction and
// makes the workspace complimentary. The invite is spent if and only if the
// workspace commits: a mismatch rolls the consume back.
func (s *Service) spendInviteTx(ctx context.Context, tx bun.Tx, in Input, e *signupEntities) (*models.TenantInvite, error) {
	now := e.tenant.CreatedAt
	inv, err := s.invites.Invites.ConsumeTx(ctx, tx, models.HashInvitationToken(in.InviteToken), now)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInviteInvalid
	}
	if err != nil {
		return nil, err
	}
	if repository.NormalizeEmail(in.Email) != inv.Email {
		return nil, ErrInviteEmailMismatch
	}
	// The account carries the invited address, normalised.
	e.account.Email, e.user.Email = inv.Email, inv.Email
	if e.identity != nil {
		e.identity.Email = inv.Email
	}
	e.tenant.Complimentary, e.tenant.BillingRequired = true, false
	e.tenant.InviteID = &inv.ID
	e.tenant.TierID, e.inviteVersion = s.inviteTier(ctx, inv)
	return inv, nil
}

// inviteTier is the tier the invite starts the workspace on, with its latest
// published version. A tier deleted or unpublished since the invite was sent
// falls back to the default tier rather than failing the signup.
func (s *Service) inviteTier(ctx context.Context, inv *models.TenantInvite) (string, *string) {
	if inv.TierID != nil {
		v, err := s.invites.TierVersions.LatestActiveByTier(ctx, *inv.TierID)
		if err == nil {
			return *inv.TierID, &v.ID
		}
		slog.WarnContext(ctx, "tester invite tier has no published version; using the default tier",
			logging.AttrComponent, "signup", "invite_id", inv.ID, "tier_id", *inv.TierID, logging.AttrError, err)
	}
	if v, err := s.invites.TierVersions.LatestActiveByTier(ctx, models.DefaultTierID); err == nil {
		return models.DefaultTierID, &v.ID
	}
	return models.DefaultTierID, nil
}

// applyInviteTx records the complimentary tier assignment, copies the invite's
// groups onto the workspace and stores what the invite became.
func (s *Service) applyInviteTx(ctx context.Context, tx bun.Tx, inv *models.TenantInvite, e *signupEntities) error {
	now := e.tenant.CreatedAt
	if _, err := s.invites.Assignments.ReassignTx(ctx, tx, e.tenant.ID, e.tenant.TierID, e.inviteVersion, models.AssignmentReasonComp, now); err != nil {
		return err
	}
	groupIDs, err := s.invites.Invites.GroupIDs(ctx, tx, inv.ID)
	if err != nil {
		return err
	}
	for _, g := range groupIDs {
		if _, err := tx.NewInsert().Model(&models.TenantGroupAssignment{TenantID: e.tenant.ID, GroupID: g, CreatedAt: now}).
			On("CONFLICT (tenant_id, group_id) DO NOTHING").Exec(ctx); err != nil {
			return err
		}
	}
	return s.invites.Invites.RecordRedemptionTx(ctx, tx, inv.ID, e.tenant.ID, e.account.ID, now)
}

// signupEntities are the rows a signup inserts.
type signupEntities struct {
	tenant   *models.Tenant
	account  *models.Account
	identity *models.AccountIdentity // nil for a password signup
	user     *models.User
	session  *models.Session
	// inviteVersion is the tier version a redeemed invite starts on.
	inviteVersion *string
}

// rows lists the entities in insert (foreign-key) order.
func (e *signupEntities) rows() []any {
	rows := []any{e.tenant, e.account}
	if e.identity != nil {
		rows = append(rows, e.identity)
	}
	return append(rows, e.user, e.session)
}

// newSignupEntities generates the ids, password hash and session token
// and assembles the rows. The account holds the credential (a password, or the
// linked identity); the user row is that account's owner membership of the new
// workspace. New workspaces start on the seeded default tier.
func newSignupEntities(in Input, slug string, now time.Time) (*signupEntities, error) {
	var tenantID, accountID, userID, identityID string
	for _, id := range []*string{&tenantID, &accountID, &userID, &identityID} {
		v, err := models.NewID()
		if err != nil {
			return nil, fmt.Errorf("generate id: %w", err)
		}
		*id = v
	}
	var hash string
	if in.Identity == nil {
		var err error
		if hash, err = models.HashPassword(in.Password); err != nil {
			return nil, fmt.Errorf("hash password: %w", err)
		}
	}
	token, err := models.NewSessionToken()
	if err != nil {
		return nil, fmt.Errorf("generate session token: %w", err)
	}
	e := &signupEntities{
		tenant:  &models.Tenant{ID: tenantID, Name: in.TenantName, Slug: slug, TierID: models.DefaultTierID, CreatedAt: now, UpdatedAt: now},
		account: &models.Account{ID: accountID, Email: in.Email, PasswordHash: hash, Name: in.UserName, CreatedAt: now, UpdatedAt: now},
		user:    &models.User{ID: userID, AccountID: accountID, TenantID: tenantID, Name: in.UserName, Email: in.Email, Role: models.RoleOwner, CreatedAt: now, UpdatedAt: now},
		session: &models.Session{ID: token, AccountID: accountID, UserID: userID, TenantID: tenantID, ExpiresAt: now.Add(sessionTTL), CreatedAt: now},
	}
	if in.Identity != nil {
		e.identity = &models.AccountIdentity{
			ID: identityID, AccountID: accountID, Provider: in.Identity.Provider, Subject: in.Identity.Subject,
			Email: in.Identity.Email, LinkedAt: now, LastUsedAt: &now,
		}
	}
	return e, nil
}

// enqueueSideEffectsTx enqueues the welcome and onboarding-drip mails and the
// Harbor new-tenant notification inside the signup transaction, so each job
// exists iff the tenant commits. The enqueues are local inserts; the external
// calls happen in the workers, so signup never blocks on Harbor reachability.
func (s *Service) enqueueSideEffectsTx(ctx context.Context, tx *sql.Tx, userID, tenantID string) error {
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

// identitySubjectConstraint is the unique (provider, subject) constraint on
// account_identities.
const identitySubjectConstraint = "account_identities_provider_subject_key"

func isUniqueViolationOn(err error, constraint string) bool {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	return ok && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}
