package zernio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Account mirrors a Zernio social-account object. Zernio's documented
// schema only confirms _id and platform; the remaining fields here
// (username, display name, avatar, etc.) are tolerated as zero-values
// when absent. The full payload is preserved in Raw so future Zernio
// fields survive into the local raw_json column unchanged.
type Account struct {
	ID          string          `json:"_id"`
	ProfileID   string          `json:"profileId,omitempty"`
	Platform    string          `json:"platform"`
	Username    string          `json:"username,omitempty"`
	DisplayName string          `json:"displayName,omitempty"`
	AvatarURL   string          `json:"avatarUrl,omitempty"`
	IsActive    bool            `json:"isActive,omitzero"`
	CreatedAt   time.Time       `json:"createdAt,omitzero"`
	UpdatedAt   time.Time       `json:"updatedAt,omitzero"`
	Raw         json.RawMessage `json:"-"`
}

// listAccountsEnvelope mirrors Zernio's `{"accounts": [...]}` shape.
type listAccountsEnvelope struct {
	Accounts []json.RawMessage `json:"accounts"`
}

// ListAccounts returns the accounts attached to the given profile.
// Zernio's documented endpoint takes no query parameters; the
// `profileId` filter is sent as a hint, and any unrecognized parameter
// is ignored server-side. We additionally filter client-side using
// the profileId field on each account so a future Ogen instance with
// multiple profiles isn't surprised by a wider remote view.
func (c *Client) ListAccounts(ctx context.Context, profileID string) ([]Account, error) {
	if c == nil {
		return nil, errors.New("zernio: client is disabled")
	}
	q := url.Values{"profileId": {profileID}}
	var env listAccountsEnvelope
	if err := c.do(ctx, http.MethodGet, "/accounts", q, nil, &env); err != nil {
		return nil, err
	}
	out := make([]Account, 0, len(env.Accounts))
	for _, raw := range env.Accounts {
		a, err := decodeAccount(raw)
		if err != nil {
			return nil, err
		}
		// When Zernio echoes profileId, enforce a client-side filter so
		// the reconciler's "in remote vs local" diff is trustworthy.
		// When it's absent, trust the server-side filter (or fall
		// through cleanly for tenants with a single profile).
		if a.ProfileID != "" && a.ProfileID != profileID {
			continue
		}
		out = append(out, *a)
	}
	return out, nil
}

// DeleteAccount disconnects and removes a connected social account on Zernio
// (DELETE /accounts/{id}). Zernio responds 200 on success and 404 when the id
// is unknown; the 404 surfaces as an *APIError{Status:404} so the caller can
// treat an already-gone account as an idempotent no-op (CON-133). There is no
// request body or query parameter. The id is a Zernio account id (= the local
// social_accounts primary key) and is path-escaped defensively.
func (c *Client) DeleteAccount(ctx context.Context, id string) error {
	if c == nil {
		return errors.New("zernio: client is disabled")
	}
	return c.do(ctx, http.MethodDelete, "/accounts/"+url.PathEscape(id), nil, nil, nil)
}

// UnmarshalJSON tolerates Zernio returning `profileId` as either a
// bare ObjectId string or a populated profile sub-document
// ({"_id": "...", "name": "..."}). Both shapes appear in the wild
// depending on whether the upstream query populates the reference;
// either way we project down to the string ID.
func (a *Account) UnmarshalJSON(data []byte) error {
	// Avoid recursion via a type alias that drops UnmarshalJSON.
	type accountWire struct {
		ID          string          `json:"_id"`
		ProfileID   json.RawMessage `json:"profileId"`
		Platform    string          `json:"platform"`
		Username    string          `json:"username"`
		DisplayName string          `json:"displayName"`
		AvatarURL   string          `json:"avatarUrl"`
		IsActive    bool            `json:"isActive"`
		CreatedAt   time.Time       `json:"createdAt"`
		UpdatedAt   time.Time       `json:"updatedAt"`
	}
	var w accountWire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	a.ID = w.ID
	a.Platform = w.Platform
	a.Username = w.Username
	a.DisplayName = w.DisplayName
	a.AvatarURL = w.AvatarURL
	a.IsActive = w.IsActive
	a.CreatedAt = w.CreatedAt
	a.UpdatedAt = w.UpdatedAt
	id, err := decodeRefID(w.ProfileID)
	if err != nil {
		return fmt.Errorf("zernio: account.profileId: %w", err)
	}
	a.ProfileID = id
	return nil
}

// decodeRefID unwraps a Mongo-style reference field that may arrive as
// either a string ObjectId ("abc") or a populated sub-document with an
// `_id` key. Empty / null inputs return the empty string.
func decodeRefID(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", err
		}
		return s, nil
	}
	var obj struct {
		ID string `json:"_id"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return "", fmt.Errorf("not a string or {_id} object: %w", err)
	}
	return obj.ID, nil
}

func decodeAccount(raw json.RawMessage) (*Account, error) {
	var a Account
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("zernio: decode account: %w", err)
	}
	a.Raw = append(json.RawMessage(nil), raw...)
	return &a, nil
}
