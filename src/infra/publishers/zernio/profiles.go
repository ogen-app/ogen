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

// Profile mirrors a Zernio profile object. Known fields are typed for
// ergonomic access; the full payload is preserved in Raw so we don't
// silently drop fields when Zernio adds new ones. Zernio's documented
// schema only confirms _id, name, and description; the timestamp and
// color fields are tolerated as zero-values when absent.
type Profile struct {
	ID          string          `json:"_id"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Color       string          `json:"color,omitempty"`
	CreatedAt   time.Time       `json:"createdAt,omitzero"`
	UpdatedAt   time.Time       `json:"updatedAt,omitzero"`
	Raw         json.RawMessage `json:"-"`
}

// listProfilesEnvelope mirrors Zernio's list-response shape:
// `{"profiles": [...]}`. The single-key envelope is consistent across
// every list endpoint Zernio exposes.
type listProfilesEnvelope struct {
	Profiles []json.RawMessage `json:"profiles"`
}

// getProfileEnvelope mirrors Zernio's single-resource shape:
// `{"profile": {...}}`. Both GET /profiles/{id} and POST /profiles
// return this shape.
type getProfileEnvelope struct {
	Profile json.RawMessage `json:"profile"`
}

// ListProfiles returns the full set of profiles for the authenticated
// account. The Zernio docs do not expose pagination on this endpoint,
// so a single GET is sufficient — for a Phase 1 single-tenant Ogen
// the list is guaranteed to be small.
func (c *Client) ListProfiles(ctx context.Context) ([]Profile, error) {
	if c == nil {
		return nil, errors.New("zernio: client is disabled")
	}
	var env listProfilesEnvelope
	if err := c.do(ctx, http.MethodGet, "/profiles", nil, nil, &env); err != nil {
		return nil, err
	}
	out := make([]Profile, 0, len(env.Profiles))
	for _, raw := range env.Profiles {
		p, err := decodeProfile(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, nil
}

// GetProfile fetches one profile by ID. Returns an *APIError with
// Status=404 when the profile does not exist on Zernio.
func (c *Client) GetProfile(ctx context.Context, id string) (*Profile, error) {
	if c == nil {
		return nil, errors.New("zernio: client is disabled")
	}
	var env getProfileEnvelope
	if err := c.do(ctx, http.MethodGet, "/profiles/"+url.PathEscape(id), nil, nil, &env); err != nil {
		return nil, err
	}
	return decodeProfile(env.Profile)
}

// CreateProfile creates a new profile. Zernio returns the created
// object wrapped in `{"profile": {...}}`; we unwrap and adopt its
// generated _id directly.
func (c *Client) CreateProfile(ctx context.Context, name, description string) (*Profile, error) {
	if c == nil {
		return nil, errors.New("zernio: client is disabled")
	}
	body := map[string]string{"name": name, "description": description}
	var env getProfileEnvelope
	if err := c.do(ctx, http.MethodPost, "/profiles", nil, body, &env); err != nil {
		return nil, err
	}
	return decodeProfile(env.Profile)
}

// CreateConnectLink fetches the OAuth authorization URL for a platform.
// The Zernio endpoint is a GET (despite returning a derived value)
// with the platform as a path segment and `profileId` as a query
// parameter. The response shape is `{"authUrl": "..."}`.
//
// CON-217: the request is always headless (`headless=true`) so Zernio renders
// none of its own hosted selection screens. After OAuth it redirects the
// browser back to redirectURL with tempToken / connect_token / step so Ogen
// drives any secondary target selection (LinkedIn org, Facebook page) itself.
// redirectURL must be an absolute URL Ogen controls (the backend callback
// carrying the connect-session id); when empty it falls back to the Client's
// configured RedirectURL.
//
// Caller is responsible for log redaction — the returned URL contains
// a short-lived token that must not appear in log lines verbatim.
func (c *Client) CreateConnectLink(ctx context.Context, profileID, platform, redirectURL string) (string, error) {
	if c == nil {
		return "", errors.New("zernio: client is disabled")
	}
	q := url.Values{"profileId": {profileID}, "headless": {"true"}}
	if redirectURL == "" {
		redirectURL = c.redirectURL
	}
	if redirectURL != "" {
		q.Set("redirect_url", redirectURL)
	}
	var out struct {
		AuthURL string `json:"authUrl"`
	}
	if err := c.do(ctx, http.MethodGet, "/connect/"+url.PathEscape(platform), q, nil, &out); err != nil {
		return "", err
	}
	return out.AuthURL, nil
}

func decodeProfile(raw json.RawMessage) (*Profile, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("zernio: decode profile: empty body")
	}
	var p Profile
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("zernio: decode profile: %w", err)
	}
	// Defensive copy — the raw slice may share backing storage with the
	// HTTP response buffer, which is reused once the body is closed.
	p.Raw = append(json.RawMessage(nil), raw...)
	return &p, nil
}
