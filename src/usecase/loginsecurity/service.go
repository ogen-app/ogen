// Package loginsecurity recognises the browsers an account signs in from and
// emails the account when a sign-in comes from one it hasn't used before. The
// email's "This wasn't me" link lets the owner sign out every session and set a
// new password without signing in.
//
// A browser is identified by a random token in a long-lived cookie; the
// transport reads and writes the cookie, this package owns everything else.
// Nothing here may fail a login: every error on the observe path is logged,
// recorded as a failed activity, and swallowed.
package loginsecurity

import (
	"context"
	"database/sql"
	"expvar"
	"log/slog"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/jobs/queues"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

const (
	component = "loginsecurity"

	// alertsPerHour caps new-device emails per account. Someone who already has
	// the password could otherwise script logins with cleared cookies and flood
	// the owner's inbox. Above the cap the device is still enrolled.
	alertsPerHour = 10
	alertWindow   = time.Hour

	loginTimeLayout = "2 Jan 2006, 15:04 UTC"
)

var (
	metricAlertsSent       = expvar.NewInt("ogen_login_alerts_sent")
	metricAlertsSuppressed = expvar.NewInt("ogen_login_alerts_suppressed")
	metricAccountsSecured  = expvar.NewInt("ogen_accounts_secured")
)

// Locator resolves an IP to an approximate "City, Country" label, or "".
type Locator interface {
	Lookup(ip string) string
}

// AlertEnqueuer queues the new-device email inside the transaction that stores
// its token. Implemented by *queues.Enqueuer.
type AlertEnqueuer interface {
	EnqueueNewDeviceLoginEmailTx(ctx context.Context, tx *sql.Tx, userID, tenantID, tokenID string, v queues.NewDeviceLoginVars) error
}

// Deps are the Service's collaborators. Geo, Emails and Activity may be nil:
// no location, no email, no activity events respectively.
type Deps struct {
	DB         *bun.DB
	Devices    repository.KnownDeviceRepository
	Alerts     repository.LoginAlertTokenRepository
	Sessions   repository.SessionRepository
	Users      repository.UserRepository
	Accounts   repository.AccountRepository
	Geo        Locator
	Emails     AlertEnqueuer
	Activity   *activity.Recorder
	AppBaseURL string
}

// Service runs new-device detection and the secure-account action.
type Service struct {
	d Deps
	// now is injectable so tests can pin the clock. nil → time.Now().UTC().
	now func() time.Time
}

// New builds a Service.
func New(d Deps) *Service {
	return &Service{d: d}
}

func (s *Service) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now().UTC()
}

// Login describes a session that was just opened.
type Login struct {
	AccountID string
	UserID    string
	TenantID  string
	// Email is the account address; the alert shows it masked.
	Email     string
	IP        string
	UserAgent string
	// DeviceToken is the device cookie the request carried, "" when none.
	DeviceToken string
}

// Observe runs after a successful password login. A known device is refreshed
// silently; an unknown one is enrolled and, unless it is the account's first
// device or the hourly cap is reached, triggers the alert email. It returns
// the device token the caller must (re)issue as the cookie, or "" if none
// could be minted. It never fails the login.
func (s *Service) Observe(ctx context.Context, in Login) string {
	token, hash, fresh, err := deviceToken(in.DeviceToken)
	if err != nil {
		s.fail(ctx, in, err)
		return ""
	}
	label := DeviceLabel(in.UserAgent)
	now := s.clock()

	if !fresh {
		known, err := s.d.Devices.Touch(ctx, in.AccountID, hash, in.IP, in.UserAgent, label, now)
		if err != nil {
			s.fail(ctx, in, err)
			return token
		}
		if known {
			return token
		}
	}

	out, err := s.enrolNewDevice(ctx, in, hash, label, now)
	if err != nil {
		s.fail(ctx, in, err)
		return token
	}
	s.recordNewDevice(ctx, in, out)
	return token
}

// Enroll records the browser that opened a brand-new account's first session
// (signup, accepting an invite as a new account) as known, so the owner's
// first real login from it isn't reported. No email is sent. It returns the
// device token to set as the cookie, or "" on failure.
func (s *Service) Enroll(ctx context.Context, in Login) string {
	token, hash, _, err := deviceToken(in.DeviceToken)
	if err != nil {
		s.logFailure(ctx, in, err)
		return ""
	}
	now := s.clock()
	d := &models.KnownDevice{
		AccountID: in.AccountID, DeviceHash: hash, DeviceLabel: DeviceLabel(in.UserAgent),
		UserAgent: in.UserAgent, LastIP: in.IP, FirstSeenAt: now, LastSeenAt: now,
	}
	if d.ID, err = models.NewID(); err == nil {
		err = s.d.Devices.Upsert(ctx, nil, d)
	}
	if err != nil {
		s.logFailure(ctx, in, err)
		return ""
	}
	return token
}

// deviceToken reuses a well-formed cookie value or mints a new one. fresh is
// true when the token was just minted, so it can't be known yet.
func deviceToken(cookie string) (token, hash string, fresh bool, err error) {
	if models.ValidDeviceToken(cookie) {
		return cookie, models.HashDeviceToken(cookie), false, nil
	}
	token, hash, err = models.NewDeviceToken()
	return token, hash, true, err
}

// enrolOutcome is what enrolling an unknown device decided.
type enrolOutcome struct {
	location   string
	first      bool
	alertSent  bool
	suppressed bool
}

// enrolNewDevice stores the device and, when warranted, the alert token plus
// its email, in one transaction. The account row is locked so concurrent
// logins agree on "first device" and on the hourly cap.
func (s *Service) enrolNewDevice(ctx context.Context, in Login, hash, label string, now time.Time) (enrolOutcome, error) {
	var out enrolOutcome
	// Resolved before the transaction: it is an in-memory lookup, but there is
	// no reason to hold the account lock across it.
	if s.d.Geo != nil {
		out.location = s.d.Geo.Lookup(in.IP)
	}
	deviceID, err := models.NewID()
	if err != nil {
		return out, err
	}

	err = s.d.DB.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var locked string
		if err := tx.NewSelect().Table("accounts").Column("id").
			Where("id = ?", in.AccountID).For("UPDATE").Scan(ctx, &locked); err != nil {
			return err
		}
		known, err := s.d.Devices.CountForAccount(ctx, tx, in.AccountID)
		if err != nil {
			return err
		}
		d := &models.KnownDevice{
			ID: deviceID, AccountID: in.AccountID, DeviceHash: hash, DeviceLabel: label,
			UserAgent: in.UserAgent, LastIP: in.IP, FirstSeenAt: now, LastSeenAt: now,
		}
		if err := s.d.Devices.Upsert(ctx, tx, d); err != nil {
			return err
		}
		if known == 0 {
			out.first = true
			return nil
		}
		sent, err := s.d.Alerts.CountSince(ctx, tx, in.AccountID, now.Add(-alertWindow))
		if err != nil {
			return err
		}
		if sent >= alertsPerHour {
			out.suppressed = true
			return nil
		}
		out.alertSent = true
		return s.createAlert(ctx, tx, in, d, out.location, now)
	})
	return out, err
}

// createAlert stores the alert token and queues its email in tx.
func (s *Service) createAlert(ctx context.Context, tx bun.Tx, in Login, d *models.KnownDevice, location string, now time.Time) error {
	raw, hash, err := models.NewLoginAlertToken()
	if err != nil {
		return err
	}
	id, err := models.NewID()
	if err != nil {
		return err
	}
	tok := &models.LoginAlertToken{
		ID: id, AccountID: in.AccountID, DeviceID: &d.ID, UserID: in.UserID, TenantID: in.TenantID,
		TokenHash: hash, IP: in.IP, DeviceLabel: d.DeviceLabel, Location: location,
		LoginAt: now, ExpiresAt: now.Add(models.LoginAlertTokenTTL), CreatedAt: now,
	}
	if err := s.d.Alerts.Create(ctx, tx, tok); err != nil {
		return err
	}
	if s.d.Emails == nil {
		return nil
	}
	return s.d.Emails.EnqueueNewDeviceLoginEmailTx(ctx, tx.Tx, in.UserID, in.TenantID, id, queues.NewDeviceLoginVars{
		LoginTime:   now.UTC().Format(loginTimeLayout),
		DeviceLabel: d.DeviceLabel,
		IPAddress:   in.IP,
		Location:    location,
		SecureURL:   s.secureURL(raw),
		MaskedEmail: MaskEmail(in.Email),
	})
}

func (s *Service) secureURL(token string) string {
	return strings.TrimRight(s.d.AppBaseURL, "/") + "/auth/secure-account?token=" + token
}

func (s *Service) recordNewDevice(ctx context.Context, in Login, out enrolOutcome) {
	var tags []string
	switch {
	case out.first:
		tags = append(tags, "first_device")
	case out.suppressed:
		tags = append(tags, "alert_suppressed")
		metricAlertsSuppressed.Add(1)
	case out.alertSent:
		metricAlertsSent.Add(1)
	}
	s.d.Activity.Record(actorCtx(ctx, in.TenantID, in.UserID), activity.CategoryAuthentication, "new_device_login",
		activity.WithEntity("user", in.UserID),
		activity.WithSource(activity.SourceAPI),
		activity.WithStatus("success"),
		activity.WithTags(tags...),
		activity.WithPayload(map[string]any{
			"device_label": DeviceLabel(in.UserAgent),
			"ip":           in.IP,
			"location":     out.location,
			"alert_sent":   out.alertSent,
		}),
	)
}

// fail logs an observe-path error and records it as a failed activity.
func (s *Service) fail(ctx context.Context, in Login, err error) {
	s.logFailure(ctx, in, err)
	s.d.Activity.Record(actorCtx(ctx, in.TenantID, in.UserID), activity.CategoryAuthentication, "new_device_login",
		activity.WithEntity("user", in.UserID),
		activity.WithSource(activity.SourceAPI),
		activity.WithStatus("failed"),
	)
}

func (s *Service) logFailure(ctx context.Context, in Login, err error) {
	slog.ErrorContext(ctx, "known-device tracking failed; login continues",
		logging.AttrComponent, component, "account", in.AccountID, logging.AttrError, err)
}

// actorCtx attributes an event raised outside tenant scope to the membership
// the login opened.
func actorCtx(ctx context.Context, tenantID, userID string) context.Context {
	return logging.WithUserID(tenantctx.With(ctx, tenantID), userID)
}

// MaskEmail hides all but the first character of the local part:
// "jane@acme.com" → "j***@acme.com".
func MaskEmail(email string) string {
	at := strings.LastIndexByte(email, '@')
	if at <= 0 {
		return "***"
	}
	first := []rune(email[:at])[0]
	return string(first) + "***" + email[at:]
}
