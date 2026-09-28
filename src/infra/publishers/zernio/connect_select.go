package zernio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
)

// ConnectTarget is one selectable posting destination surfaced by the headless
// connect list step: a Facebook Page, a LinkedIn organization (or the
// personal profile), an Instagram account, etc. Only display fields are kept —
// the in-Ogen picker never needs Zernio's per-target access tokens.
type ConnectTarget struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Username  string `json:"username,omitempty"`
	Kind      string `json:"kind,omitempty"`
	AvatarURL string `json:"avatarUrl,omitempty"`
}

// connectSelectSpec describes, per platform, the Zernio headless select
// endpoints and payload field names. Facebook/Instagram are documented
// (`/connect/{platform}/select-page`, list key `pages`, select id `pageId`).
// LinkedIn selects an organization; its exact path/fields are TODO-flagged to
// confirm against Zernio's API reference at integration time — the mechanism is
// identical, only names differ. The list decode is tolerant (§ListConnectTargets)
// so an unexpected array key still parses.
type connectSelectSpec struct {
	// segment is appended after "/connect/{platform}/" for both the GET (list)
	// and POST (finalize) calls.
	segment string
	// idField is the request-body field carrying the chosen target id.
	idField string
	// kind is stamped on ConnectTarget when the payload doesn't carry its own.
	kind string
}

// connectSelectSpecs maps a Zernio platform id to its secondary-selection
// contract. Only the platforms Zernio flags as needing a pick appear here; a
// platform absent from the map finalizes during OAuth and never lists targets.
var connectSelectSpecs = map[string]connectSelectSpec{
	"facebook":  {segment: "select-page", idField: "pageId", kind: "page"},
	"instagram": {segment: "select-page", idField: "pageId", kind: "page"},
	// TODO: confirm LinkedIn's headless org list/select segment + id
	// field against Zernio's API reference / OpenAPI. Best-known shape below;
	// the tolerant decode below already copes with the response variance.
	"linkedin": {segment: "select-organization", idField: "organizationId", kind: "organization"},
}

// SupportsConnectSelect reports whether a platform has a headless secondary
// selection step. Platforms without one finalize during OAuth and never reach
// the list/select path (the callback sees no `step`).
func SupportsConnectSelect(platform string) bool {
	_, ok := connectSelectSpecs[platform]
	return ok
}

// ListConnectTargets lists the selectable targets after a headless OAuth.
// It authenticates with the bearer key AND the short-lived
// X-Connect-Token from the callback. Decode is tolerant: Zernio wraps the array
// under a platform-specific key (`pages`, `organizations`, …), so we take the
// first array-valued field and read each element's id/name/username/avatar from
// any of several known field names.
func (c *Client) ListConnectTargets(ctx context.Context, platform, profileID, tempToken, connectToken string) ([]ConnectTarget, error) {
	if c == nil {
		return nil, errors.New("zernio: client is disabled")
	}
	spec, ok := connectSelectSpecs[platform]
	if !ok {
		return nil, errors.New("zernio: platform has no connect-select step: " + platform)
	}
	q := url.Values{"profileId": {profileID}, "tempToken": {tempToken}}
	headers := map[string]string{"X-Connect-Token": connectToken}
	path := "/connect/" + url.PathEscape(platform) + "/" + spec.segment
	var raw map[string]json.RawMessage
	if err := c.doHeaders(ctx, http.MethodGet, path, q, headers, nil, &raw); err != nil {
		return nil, err
	}
	items := firstJSONArray(raw)
	out := make([]ConnectTarget, 0, len(items))
	for _, it := range items {
		t := decodeConnectTarget(it)
		if t.Kind == "" {
			t.Kind = spec.kind
		}
		if t.ID != "" {
			out = append(out, t)
		}
	}
	return out, nil
}

// SelectConnectTarget finalizes the connection by choosing one target.
// userProfile is the opaque JSON Zernio handed back on the callback;
// it is forwarded verbatim. On success the account is created upstream and
// surfaces locally via the existing account sync.
func (c *Client) SelectConnectTarget(ctx context.Context, platform, profileID, tempToken, connectToken, targetID string, userProfile json.RawMessage) error {
	if c == nil {
		return errors.New("zernio: client is disabled")
	}
	spec, ok := connectSelectSpecs[platform]
	if !ok {
		return errors.New("zernio: platform has no connect-select step: " + platform)
	}
	body := map[string]any{
		"profileId":  profileID,
		spec.idField: targetID,
		"tempToken":  tempToken,
	}
	if len(userProfile) > 0 {
		body["userProfile"] = userProfile
	}
	headers := map[string]string{"X-Connect-Token": connectToken}
	path := "/connect/" + url.PathEscape(platform) + "/" + spec.segment
	return c.doHeaders(ctx, http.MethodPost, path, nil, headers, body, nil)
}

// firstJSONArray returns the first array-valued field in m, preferring the
// known Zernio list keys for determinism before falling back to any array.
func firstJSONArray(m map[string]json.RawMessage) []json.RawMessage {
	for _, key := range []string{"pages", "organizations", "accounts", "boards", "locations", "profiles"} {
		if raw, ok := m[key]; ok {
			if arr, ok := asJSONArray(raw); ok {
				return arr
			}
		}
	}
	for _, raw := range m {
		if arr, ok := asJSONArray(raw); ok {
			return arr
		}
	}
	return nil
}

func asJSONArray(raw json.RawMessage) ([]json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, false
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(trimmed, &arr); err != nil {
		return nil, false
	}
	return arr, true
}

func decodeConnectTarget(raw json.RawMessage) ConnectTarget {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return ConnectTarget{}
	}
	return ConnectTarget{
		ID:        firstString(m, "id", "_id", "pageId", "organizationId", "organizationUrn", "urn"),
		Name:      firstString(m, "name", "displayName", "localizedName", "title"),
		Username:  firstString(m, "username", "handle", "vanityName", "slug"),
		AvatarURL: firstString(m, "avatarUrl", "avatar_url", "picture", "logoUrl", "imageUrl"),
	}
}

func firstString(m map[string]json.RawMessage, keys ...string) string {
	for _, k := range keys {
		raw, ok := m[k]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil && s != "" {
			return s
		}
	}
	return ""
}
