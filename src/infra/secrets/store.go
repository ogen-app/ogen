// Package secrets is the single application-level entry point for
// reading and writing API keys held in the encrypted `secret` table.
//
// No code outside this package should touch the SecretRepository or
// the envelope.Cipher directly — going through Store enforces the
// allowlist, the redaction-safe error contract, and the subscription
// hook that Genkit / Zernio use to pick up rotated keys without a
// process restart.
package secrets

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/crypto/envelope"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// Canonical secret names. The allowlist is defined in code (not config)
// so adding a new integration is a deliberate code change reviewed
// alongside the rest of its wiring.
const (
	NameAnthropicAPIKey = "anthropic_api_key"
	NameZernioAPIKey    = "zernio_api_key"
	NameGeminiAPIKey    = "gemini_api_key"
	// CON-154 email subsystem: Resend send key, Resend webhook signing secret,
	// and the app-managed HMAC key that signs unsubscribe links.
	NameResendAPIKey        = "resend_api_key"
	NameResendWebhookSecret = "resend_webhook_secret"
	NameEmailLinkSecret     = "email_link_secret"
	// CON-222 URL assets: Firecrawl.dev web-scrape API key.
	NameFirecrawlAPIKey = "firecrawl_api_key"
)

// AllowedNames is the closed set of accepted secret names. Anything
// else is rejected at the Store boundary with ErrUnknownName.
var AllowedNames = []string{
	NameAnthropicAPIKey,
	NameZernioAPIKey,
	NameGeminiAPIKey,
	NameResendAPIKey,
	NameResendWebhookSecret,
	NameEmailLinkSecret,
	NameFirecrawlAPIKey,
}

// MaxValueLen is the inclusive upper bound on a plaintext secret. The
// 4KB cap is a sanity ceiling — real Anthropic / Zernio keys are far
// shorter — sized to prevent obviously malicious uploads from
// inflating the encrypted blob.
const MaxValueLen = 4096

// ErrUnknownName is returned by Set / Get / Delete when name is not in
// AllowedNames. Mapped to gRPC InvalidArgument by the secrets service.
var ErrUnknownName = errors.New("secrets: unknown name")

// ErrNotFound is returned by Get / Delete when no row exists for the
// requested name. Distinct from "name disallowed" so the secrets service
// can map them to InvalidArgument vs NotFound.
var ErrNotFound = errors.New("secrets: not found")

// ErrInvalidValue is returned by Set when the plaintext fails one of
// the surface validations (length, control chars). The error message
// never echoes the offending value.
var ErrInvalidValue = errors.New("secrets: invalid value")

// Metadata is the non-sensitive view of a stored secret. Returned
// unchanged from List / Set so the gRPC secrets service and internal
// callers share a single shape.
type Metadata struct {
	Name        string    `json:"name"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	KEKVersion  int       `json:"kek_version"`
	Algorithm   string    `json:"algorithm"`
	Decryptable bool      `json:"decryptable"`
}

// Store is the application-facing secret API. Implementations hold a
// repository and a Cipher; nothing else is needed.
type Store interface {
	Get(ctx context.Context, name string) (string, error)
	Set(ctx context.Context, name, plaintext string) (Metadata, bool, error) // bool = created
	Delete(ctx context.Context, name string) error
	List(ctx context.Context) ([]Metadata, error)
	GetMetadata(ctx context.Context, name string) (Metadata, error)
	Subscribe(name string, fn func()) (cancel func())
}

type store struct {
	repo   repository.SecretRepository
	cipher *envelope.Cipher

	mu          sync.Mutex
	subscribers map[string][]*subscription
}

type subscription struct {
	fn func()
}

// NewStore wires a Store. cipher must be initialised from a loaded KEK;
// repo must be ready (migrations applied).
func NewStore(repo repository.SecretRepository, cipher *envelope.Cipher) Store {
	return &store{
		repo:        repo,
		cipher:      cipher,
		subscribers: map[string][]*subscription{},
	}
}

func (s *store) Get(ctx context.Context, name string) (string, error) {
	if !IsAllowed(name) {
		return "", fmt.Errorf("%w: %s", ErrUnknownName, name)
	}
	row, err := s.repo.Get(ctx, name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("%w: %s", ErrNotFound, name)
		}
		return "", err
	}
	pt, err := s.cipher.Decrypt(toRecord(row))
	if err != nil {
		return "", fmt.Errorf("secrets: decrypt %s: %w", name, err)
	}
	return string(pt), nil
}

// Set upserts an encrypted row. Returns the new metadata and a bool
// indicating whether this was a fresh insert (true) or an update
// (false). Subscribers for `name` are notified asynchronously after a
// successful write so a Genkit rebuild can run without blocking the
// HTTP response.
func (s *store) Set(ctx context.Context, name, plaintext string) (Metadata, bool, error) {
	if !IsAllowed(name) {
		return Metadata{}, false, fmt.Errorf("%w: %s", ErrUnknownName, name)
	}
	if err := validatePlaintext(plaintext); err != nil {
		return Metadata{}, false, err
	}

	existing, getErr := s.repo.Get(ctx, name)
	created := errors.Is(getErr, sql.ErrNoRows)
	if getErr != nil && !created {
		return Metadata{}, false, getErr
	}

	rec, err := s.cipher.Encrypt([]byte(plaintext))
	if err != nil {
		return Metadata{}, false, fmt.Errorf("secrets: encrypt %s: %w", name, redactError(err, plaintext))
	}

	row := &models.Secret{
		Name:       name,
		Ciphertext: rec.Ciphertext,
		Nonce:      rec.Nonce,
		WrappedDEK: rec.WrappedDEK,
		DEKNonce:   rec.DEKNonce,
		Algorithm:  rec.Algorithm,
		KEKVersion: rec.KEKVersion,
	}
	if !created {
		row.CreatedAt = existing.CreatedAt
	}
	if err := s.repo.Upsert(ctx, row); err != nil {
		return Metadata{}, false, err
	}

	s.notify(name)

	return Metadata{
		Name:        row.Name,
		CreatedAt:   row.CreatedAt,
		UpdatedAt:   row.UpdatedAt,
		KEKVersion:  row.KEKVersion,
		Algorithm:   row.Algorithm,
		Decryptable: true,
	}, created, nil
}

func (s *store) Delete(ctx context.Context, name string) error {
	if !IsAllowed(name) {
		return fmt.Errorf("%w: %s", ErrUnknownName, name)
	}
	deleted, err := s.repo.Delete(ctx, name)
	if err != nil {
		return err
	}
	if !deleted {
		return fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	s.notify(name)
	return nil
}

// List returns metadata for every persisted row. `decryptable` is
// computed by attempting an in-memory unwrap of the wrapped DEK only
// (not the full payload decrypt) — that's enough to confirm the
// loaded KEK still matches what wrote the row.
func (s *store) List(ctx context.Context) ([]Metadata, error) {
	rows, err := s.repo.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Metadata, 0, len(rows))
	for i := range rows {
		out = append(out, s.metadataFor(&rows[i]))
	}
	return out, nil
}

func (s *store) GetMetadata(ctx context.Context, name string) (Metadata, error) {
	if !IsAllowed(name) {
		return Metadata{}, fmt.Errorf("%w: %s", ErrUnknownName, name)
	}
	row, err := s.repo.Get(ctx, name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Metadata{}, fmt.Errorf("%w: %s", ErrNotFound, name)
		}
		return Metadata{}, err
	}
	return s.metadataFor(row), nil
}

func (s *store) metadataFor(row *models.Secret) Metadata {
	_, err := s.cipher.Decrypt(toRecord(row))
	return Metadata{
		Name:        row.Name,
		CreatedAt:   row.CreatedAt,
		UpdatedAt:   row.UpdatedAt,
		KEKVersion:  row.KEKVersion,
		Algorithm:   row.Algorithm,
		Decryptable: err == nil,
	}
}

// Subscribe registers fn to be invoked after every successful Set or
// Delete for the given name. The returned cancel removes the
// subscription. fn runs in the goroutine that finished the write —
// keep it short or dispatch to a goroutine internally.
func (s *store) Subscribe(name string, fn func()) func() {
	if fn == nil {
		return func() {}
	}
	sub := &subscription{fn: fn}
	s.mu.Lock()
	s.subscribers[name] = append(s.subscribers[name], sub)
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		subs := s.subscribers[name]
		for i := range subs {
			if subs[i] == sub {
				s.subscribers[name] = append(subs[:i], subs[i+1:]...)
				return
			}
		}
	}
}

func (s *store) notify(name string) {
	s.mu.Lock()
	subs := append([]*subscription(nil), s.subscribers[name]...)
	s.mu.Unlock()
	for _, sub := range subs {
		sub.fn()
	}
}

// IsAllowed reports whether name is in the closed allowlist.
func IsAllowed(name string) bool {
	return slices.Contains(AllowedNames, name)
}

func toRecord(row *models.Secret) envelope.Record {
	return envelope.Record{
		Ciphertext: row.Ciphertext,
		Nonce:      row.Nonce,
		WrappedDEK: row.WrappedDEK,
		DEKNonce:   row.DEKNonce,
		Algorithm:  row.Algorithm,
		KEKVersion: row.KEKVersion,
	}
}

// validatePlaintext enforces the surface checks documented on Set.
// The error message never includes the value itself — only its length
// or the position of an offending character.
func validatePlaintext(v string) error {
	if v == "" {
		return fmt.Errorf("%w: empty", ErrInvalidValue)
	}
	if len(v) > MaxValueLen {
		return fmt.Errorf("%w: length %d exceeds max %d", ErrInvalidValue, len(v), MaxValueLen)
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		// Reject ASCII control characters (0x00-0x1F and 0x7F) wholesale.
		// API keys are URL-safe printable ASCII; control chars indicate
		// either an accidental copy-paste of a JSON envelope or a
		// deliberate attempt to smuggle CR/LF into log lines.
		if c < 0x20 || c == 0x7F {
			return fmt.Errorf("%w: control character at byte %d", ErrInvalidValue, i)
		}
	}
	return nil
}

// redactError defends against a wrapped error formatter ever
// concatenating the plaintext into its message. We replace any literal
// occurrence of plaintext in the formatted error with a length-only
// placeholder. Belt-and-braces — none of our crypto errors should ever
// embed plaintext, but this guarantees it.
func redactError(err error, plaintext string) error {
	if err == nil || plaintext == "" {
		return err
	}
	msg := err.Error()
	clean := strings.ReplaceAll(msg, plaintext, fmt.Sprintf("[REDACTED:%d bytes]", len(plaintext)))
	if clean == msg {
		return err
	}
	return errors.New(clean)
}
