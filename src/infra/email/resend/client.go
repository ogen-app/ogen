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
	"fmt"
	"io"
	"net/http"
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
