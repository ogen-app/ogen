// Package resend is a thin HTTP client for the Resend email API
// (https://resend.com), implementing email.Sender (CON-154). It mirrors the
// Zernio client's conventions: the API key is resolved per request via a
// KeyResolver (so a rotated key is picked up with no restart and is never held
// on the struct), non-2xx responses become a classified *email.SendError, and
// the key never appears in log lines.
package resend

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ogen-app/ogen/src/infra/email"
)

const (
	defaultBaseURL = "https://api.resend.com"
	defaultTimeout = 15 * time.Second
	// errorBodyLimit caps how much of a non-2xx body we read into the error so
	// an unexpectedly large response can't blow up logs or memory.
	errorBodyLimit = 4096
)

// KeyResolver returns the current Resend API key. It should resolve through the
// SecretStore on each call so a rotated key takes effect without a restart.
type KeyResolver func(ctx context.Context) (string, error)

// Client is the authenticated Resend HTTP wrapper. The bearer key is fetched
// per request via keyResolver, never cached here.
type Client struct {
	httpClient  *http.Client
	baseURL     string
	keyResolver KeyResolver
}

// New constructs a Client. A nil resolver returns nil so callers can treat a
// nil *Client as "sending disabled" (no Resend key configured) — mirroring the
// Zernio client — and the send job degrades to skipped_disabled.
func New(resolver KeyResolver, baseURL string, timeout time.Duration) *Client {
	if resolver == nil {
		return nil
	}
	baseURL = cmp.Or(baseURL, defaultBaseURL)
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &Client{
		httpClient:  &http.Client{Timeout: timeout},
		baseURL:     strings.TrimRight(baseURL, "/"),
		keyResolver: resolver,
	}
}

// String redacts everything sensitive (the key isn't stored here, but keep a
// deliberate Stringer so a %v never reflects into the resolver).
func (c *Client) String() string {
	if c == nil {
		return "resend.Client(disabled)"
	}
	return fmt.Sprintf("resend.Client(baseURL=%s)", c.baseURL)
}

// sendRequest is the POST /emails body. Field names match Resend's API.
type sendRequest struct {
	From    string            `json:"from"`
	To      []string          `json:"to"`
	Subject string            `json:"subject"`
	HTML    string            `json:"html,omitempty"`
	Text    string            `json:"text,omitempty"`
	ReplyTo string            `json:"reply_to,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// Send transmits one message via POST /emails and returns Resend's message id.
// It satisfies email.Sender.
func (c *Client) Send(ctx context.Context, msg email.Message) (string, error) {
	if c == nil {
		return "", &email.SendError{Disabled: true, Message: "resend: client disabled"}
	}
	key, err := c.keyResolver(ctx)
	if err != nil {
		// A transient secret-read failure — retry rather than drop the mail.
		return "", &email.SendError{Transient: true, Message: fmt.Sprintf("resend: resolve api key: %v", err)}
	}
	if key == "" {
		return "", &email.SendError{Disabled: true, Message: "resend: api key not configured"}
	}

	buf, err := json.Marshal(sendRequest{
		From:    msg.From,
		To:      []string{msg.To},
		Subject: msg.Subject,
		HTML:    msg.HTML,
		Text:    msg.Text,
		ReplyTo: msg.ReplyTo,
		Headers: msg.Headers,
	})
	if err != nil {
		return "", &email.SendError{Transient: false, Message: fmt.Sprintf("resend: marshal request: %v", err)}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/emails", bytes.NewReader(buf))
	if err != nil {
		return "", &email.SendError{Transient: false, Message: err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if msg.IdempotencyKey != "" {
		// Resend honours Idempotency-Key so a retried job can't send twice.
		req.Header.Set("Idempotency-Key", msg.IdempotencyKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Network / timeout: retryable.
		return "", &email.SendError{Transient: true, Message: err.Error()}
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
		return "", &email.SendError{
			// 5xx and 429 are worth retrying; other 4xx are terminal (bad
			// request / rejected recipient won't succeed on a re-attempt).
			Transient: resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests,
			Status:    resp.StatusCode,
			Message:   shortErrorMessage(raw),
		}
	}

	// 2xx: the mail was accepted. Never return an error past this point — a
	// retry would double-send. If the id can't be parsed, report success with
	// an empty id.
	var out struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", nil
	}
	return out.ID, nil
}

// ErrDisabled is returned by Get when no Resend key is configured (a nil client
// or an empty key). The caller degrades to "body unavailable" — returning the
// summary + persisted timeline — rather than failing the request (CON-298).
var ErrDisabled = errors.New("resend: client disabled")

// ErrNotFound is returned by Get on an HTTP 404 — Resend has no such message
// (unknown id, or the message aged out of Resend's retention window). The caller
// distinguishes it from other fetch failures to log a precise reason for a
// missing body (resend_404 vs. resend_error, CON-306 §5.4).
var ErrNotFound = errors.New("resend: email not found")

// EmailDetail is the subset of Resend's GET /emails/{id} response the operator
// console needs (CON-298): the rendered body + envelope. Fetched live so the
// large HTML never has to be duplicated into the control-plane DB.
type EmailDetail struct {
	ID        string
	Subject   string
	HTML      string
	Text      string
	From      string
	ReplyTo   string
	To        []string
	CC        []string
	BCC       []string
	LastEvent string
}

// Get retrieves one email's rendered body + envelope from Resend
// (GET /emails/{id}). It returns ErrDisabled when the client is nil or no key is
// configured, so the caller can serve metadata with the body marked unavailable.
func (c *Client) Get(ctx context.Context, id string) (*EmailDetail, error) {
	if c == nil {
		return nil, ErrDisabled
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("resend: empty email id")
	}
	key, err := c.keyResolver(ctx)
	if err != nil {
		return nil, fmt.Errorf("resend: resolve api key: %w", err)
	}
	if key == "" {
		return nil, ErrDisabled
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/emails/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
		if resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, shortErrorMessage(raw))
		}
		return nil, fmt.Errorf("resend: get email: status %d: %s", resp.StatusCode, shortErrorMessage(raw))
	}

	var out struct {
		ID        string      `json:"id"`
		Subject   string      `json:"subject"`
		HTML      string      `json:"html"`
		Text      string      `json:"text"`
		From      string      `json:"from"`
		To        flexStrings `json:"to"`
		ReplyTo   flexStrings `json:"reply_to"`
		CC        flexStrings `json:"cc"`
		BCC       flexStrings `json:"bcc"`
		LastEvent string      `json:"last_event"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("resend: decode email: %w", err)
	}
	return &EmailDetail{
		ID:        out.ID,
		Subject:   out.Subject,
		HTML:      out.HTML,
		Text:      out.Text,
		From:      out.From,
		ReplyTo:   strings.Join(out.ReplyTo, ", "),
		To:        out.To,
		CC:        out.CC,
		BCC:       out.BCC,
		LastEvent: out.LastEvent,
	}, nil
}

// flexStrings decodes a JSON field Resend may send as a single string, an array
// of strings, or null (reply_to/cc/bcc/to vary by endpoint and whether set).
type flexStrings []string

func (f *flexStrings) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	if b[0] == '[' {
		var arr []string
		if err := json.Unmarshal(b, &arr); err != nil {
			return err
		}
		*f = arr
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if s != "" {
		*f = []string{s}
	}
	return nil
}

// shortErrorMessage extracts a brief message from a Resend error body, falling
// back to a truncated raw body when the shape is unknown.
func shortErrorMessage(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var env struct {
		Message string `json:"message"`
		Name    string `json:"name"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err == nil {
		switch {
		case env.Message != "":
			return env.Message
		case env.Error != "":
			return env.Error
		case env.Name != "":
			return env.Name
		}
	}
	if len(raw) > 200 {
		raw = raw[:200]
	}
	return string(raw)
}
