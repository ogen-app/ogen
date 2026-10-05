// Package plugins connects design-tool plugins (Figma first) to a workspace.
// A plugin can't receive an OAuth redirect, so it pairs by polling: it opens a
// pairing (getting a read key it keeps and a write key it hands the browser),
// the signed-in user approves the write key in the web app, and the plugin's
// next poll with the read key collects a plugin token. The token acts as the
// approving member in the workspace the approval ran in, until revoked.
package plugins

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/crypto/envelope"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/notify"
)

// MaxClientLabelLen bounds the plugin-supplied label shown on the approval
// page and in the connections list.
const MaxClientLabelLen = 80

// Pairing errors. The handlers map them to HTTP statuses.
var (
	// ErrInvalidLabel: the client label is empty or too long (400).
	ErrInvalidLabel = errors.New("client_label must be 1-80 characters")
	// ErrPairingGone: unknown, expired or already collected (410).
	ErrPairingGone = errors.New("pairing expired or unknown")
	// ErrPairingNotPending: approve or deny on a decided pairing (409).
	ErrPairingNotPending = errors.New("pairing is no longer pending")
	// ErrPairingDenied: the user refused the pairing (403 on collect).
	ErrPairingDenied = errors.New("pairing denied")
)

// Deps are the Service's collaborators. Notifier may be nil.
type Deps struct {
	DB       *bun.DB
	Pairings repository.PluginPairingRepository
	Tokens   repository.PluginTokenRepository
	Users    repository.UserRepository
	Cipher   *envelope.Cipher
	Notifier *notify.Service
	// Now is the clock; nil uses time.Now.
	Now func() time.Time
}

// Service runs the pairing handshake and manages plugin connections.
type Service struct {
	d Deps
}

func New(d Deps) *Service {
	if d.Now == nil {
		d.Now = time.Now
	}
	return &Service{d: d}
}

func (s *Service) now() time.Time { return s.d.Now().UTC() }

// Started is a freshly opened pairing. The keys exist only here: the server
// keeps their hashes.
type Started struct {
	ReadKey   string
	WriteKey  string
	ExpiresAt time.Time
}

// Start opens a pending pairing for client, labelled for the approval page.
func (s *Service) Start(ctx context.Context, client, label, ip string) (*Started, error) {
	label = strings.TrimSpace(label)
	if label == "" || utf8.RuneCountInString(label) > MaxClientLabelLen {
		return nil, ErrInvalidLabel
	}
	keys, err := models.NewPluginPairingKeys()
	if err != nil {
		return nil, err
	}
	id, err := models.NewID()
	if err != nil {
		return nil, err
	}
	now := s.now()
	p := &models.PluginPairing{
		ID: id, Client: client, ClientLabel: label,
		ReadKeyHash: keys.ReadKeyHash, WriteKeyHash: keys.WriteKeyHash,
		Status: models.PluginPairingPending, CreatedIP: ip,
		CreatedAt: now, ExpiresAt: now.Add(models.PluginPairingTTL),
	}
	if err := s.d.Pairings.Create(ctx, p); err != nil {
		return nil, err
	}
	PairingsStarted.Add(1)
	return &Started{ReadKey: keys.ReadKey, WriteKey: keys.WriteKey, ExpiresAt: p.ExpiresAt}, nil
}

// Preview returns the live pairing behind a write key for the approval page.
func (s *Service) Preview(ctx context.Context, writeKey string) (*models.PluginPairing, error) {
	p, err := s.d.Pairings.GetLiveByWriteHash(ctx, models.HashPluginSecret(writeKey), s.now())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPairingGone
	}
	return p, err
}

// Approve mints a plugin token for the session's member in the session's
// workspace and parks it, sealed, on the pairing for the plugin to collect.
// The token insert and the guarded status flip share a transaction, so a
// concurrent second approve can't leave an orphan token.
func (s *Service) Approve(ctx context.Context, writeKey string, session *models.Session) (*models.PluginToken, error) {
	p, err := s.Preview(ctx, writeKey)
	if err != nil {
		return nil, err
	}
	if p.Status != models.PluginPairingPending {
		return nil, ErrPairingNotPending
	}
	raw, hash, err := models.NewPluginToken()
	if err != nil {
		return nil, err
	}
	id, err := models.NewID()
	if err != nil {
		return nil, err
	}
	sealed, err := s.d.Cipher.Seal([]byte(raw))
	if err != nil {
		return nil, fmt.Errorf("seal plugin token: %w", err)
	}
	now := s.now()
	tok := &models.PluginToken{
		ID: id, TenantID: session.TenantID, AccountID: session.AccountID, UserID: session.UserID,
		Client: p.Client, Label: p.ClientLabel, TokenHash: hash, CreatedAt: now,
	}
	p.TenantID, p.TokenID, p.SealedToken = &tok.TenantID, &tok.ID, sealed

	err = s.d.DB.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if err := s.d.Tokens.Create(ctx, tx, tok); err != nil {
			return err
		}
		ok, err := s.d.Pairings.Approve(ctx, tx, p, now)
		if err != nil {
			return err
		}
		if !ok {
			return ErrPairingNotPending
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	PairingsApproved.Add(1)
	s.notifyConnected(ctx, tok)
	return tok, nil
}

// Deny refuses a pending pairing; the plugin's next poll learns it.
func (s *Service) Deny(ctx context.Context, writeKey string) error {
	p, err := s.Preview(ctx, writeKey)
	if err != nil {
		return err
	}
	ok, err := s.d.Pairings.Deny(ctx, p.ID, s.now())
	if err != nil {
		return err
	}
	if !ok {
		return ErrPairingNotPending
	}
	PairingsDenied.Add(1)
	return nil
}

// Collected is a poll's answer: Pending, or the token with who it acts as.
type Collected struct {
	Pending    bool
	Token      string
	Connection *models.PluginToken
	User       *models.User
	Workspace  *models.Tenant
}

// Collect answers a plugin's poll by read key. A pending pairing reports
// Pending; an approved one returns its token exactly once and is deleted. A
// token revoked (or whose member was removed) before collection reads as
// gone, so the plugin pairs again rather than holding a dead token.
func (s *Service) Collect(ctx context.Context, readKey string) (*Collected, error) {
	now := s.now()
	hash := models.HashPluginSecret(readKey)
	p, err := s.d.Pairings.GetLiveByReadHash(ctx, hash, now)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPairingGone
	}
	if err != nil {
		return nil, err
	}
	switch p.Status {
	case models.PluginPairingPending:
		return &Collected{Pending: true}, nil
	case models.PluginPairingDenied:
		return nil, ErrPairingDenied
	}
	if p.TenantID == nil || p.TokenID == nil {
		return nil, ErrPairingGone
	}

	// Resolve everything before the destructive collect, so a failed lookup
	// leaves the pairing collectable on the next poll.
	tok, err := s.d.Tokens.GetActive(ctx, *p.TenantID, *p.TokenID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPairingGone
	}
	if err != nil {
		return nil, err
	}
	user, err := s.d.Users.GetByIDWithTenant(tenantctx.With(ctx, tok.TenantID), tok.UserID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPairingGone
	}
	if err != nil {
		return nil, err
	}
	plain, err := s.d.Cipher.Open(p.SealedToken)
	if err != nil {
		return nil, fmt.Errorf("open plugin token: %w", err)
	}
	if _, err := s.d.Pairings.Collect(ctx, hash, now); errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPairingGone // a concurrent poll collected it first
	} else if err != nil {
		return nil, err
	}
	PairingsCollected.Add(1)
	return &Collected{Token: string(plain), Connection: tok, User: user, Workspace: user.Tenant}, nil
}

// notifyConnected tells the approving member a plugin now acts for them, so
// an approval they didn't intend (a phished approve link) is visible.
func (s *Service) notifyConnected(ctx context.Context, tok *models.PluginToken) {
	err := s.d.Notifier.Emit(tenantctx.With(ctx, tok.TenantID), tok.UserID, notify.Spec{
		Level:      models.NotificationLevelInfo,
		Type:       "integration.plugin_connected",
		Title:      "Figma plugin connected",
		Body:       fmt.Sprintf("%s can now send images to this workspace. If this wasn't you, disconnect it in workspace settings.", tok.Label),
		EntityType: "plugin_connection",
		EntityID:   tok.ID,
		ActionURL:  "/workspace-settings",
		Data:       map[string]any{"client": tok.Client, "label": tok.Label},
	})
	if err != nil {
		slog.WarnContext(ctx, "plugin connected notification failed",
			logging.AttrComponent, "plugins", logging.AttrError, err)
	}
}
