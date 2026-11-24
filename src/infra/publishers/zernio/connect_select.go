package zernio

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/url"
	"strings"
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

// The kinds LinkedIn's select step tells apart: the member's own profile, or a
// company page they administer.
const (
	KindPersonal     = "personal"
	KindOrganization = "organization"
)

// linkedInPersonalTargetID is the target id of the member's own profile.
// Organization ids are LinkedIn's numeric ids, so it never collides with one.
const linkedInPersonalTargetID = "personal"

const linkedInOrgURNPrefix = "urn:li:organization:"

// connectSelectSpec describes, per platform, the Zernio headless select
// endpoints and payload field names. Facebook/Instagram list with a GET on
// `/connect/{platform}/select-page` (list key `pages`) and select with
// `pageId`. LinkedIn has no list call — its organizations come with the
// redirect or the pending-data read — and selects with accountType +
// selectedOrganization (linkedInSelectBody).
type connectSelectSpec struct {
	// segment is appended after "/connect/{platform}/" for the POST (finalize)
	// call and, where the platform has one, the GET (list) call.
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
	"linkedin":  {segment: "select-organization", kind: KindOrganization},
}

// SupportsConnectSelect reports whether a platform has a headless secondary
// selection step. Platforms without one finalize during OAuth and never reach
// the list/select path (the callback sees no `step`).
func SupportsConnectSelect(platform string) bool {
	_, ok := connectSelectSpecs[platform]
	return ok
}

// usesPendingData reports whether a platform's headless redirect should carry
// only a pendingDataToken (`pendingDataOnly=true` on redirect_url), which keeps
// the OAuth token and the organization list out of URLs. Zernio honours it for
// LinkedIn only.
func usesPendingData(platform string) bool { return platform == "linkedin" }

// ConnectCallback is what Zernio's headless redirect carried for the select
// step. UserProfile and Organizations are already-decoded JSON, or nil.
type ConnectCallback struct {
	TempToken        string
	ConnectToken     string
	UserProfile      json.RawMessage
	PendingDataToken string
	Organizations    json.RawMessage
}

// ConnectSelection is what the select step needs: the targets to choose from,
// and the OAuth token and user profile that finalize the pick. For LinkedIn the
// token and profile come from the pending-data read, not the redirect.
type ConnectSelection struct {
	TempToken   string
	UserProfile json.RawMessage
	Targets     []ConnectTarget
}

// ListConnectTargets lists the selectable targets after a headless OAuth.
//
// Facebook/Instagram list with a GET on the select endpoint, authenticated
// with the bearer key AND the short-lived X-Connect-Token from the callback.
// Decode is tolerant: Zernio wraps the array under a platform-specific key
// (`pages`, …), so we take the first array-valued field and read each
// element's id/name/username/avatar from any of several known field names.
//
// LinkedIn answers that GET with 405; see listLinkedInTargets.
func (c *Client) ListConnectTargets(ctx context.Context, platform, profileID string, cb ConnectCallback) (ConnectSelection, error) {
	if c == nil {
		return ConnectSelection{}, errors.New("zernio: client is disabled")
	}
	spec, ok := connectSelectSpecs[platform]
	if !ok {
		return ConnectSelection{}, errors.New("zernio: platform has no connect-select step: " + platform)
	}
	if platform == "linkedin" {
		return c.listLinkedInTargets(ctx, cb)
	}
	q := url.Values{"profileId": {profileID}, "tempToken": {cb.TempToken}}
	headers := map[string]string{"X-Connect-Token": cb.ConnectToken}
	path := "/connect/" + url.PathEscape(platform) + "/" + spec.segment
	var raw map[string]json.RawMessage
	if err := c.doHeaders(ctx, http.MethodGet, path, q, headers, nil, &raw); err != nil {
		return ConnectSelection{}, err
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
	return ConnectSelection{TempToken: cb.TempToken, UserProfile: cb.UserProfile, Targets: out}, nil
}

// pendingConnectData is the part of GET /connect/pending-data that LinkedIn's
// select step uses.
type pendingConnectData struct {
	TempToken     string          `json:"tempToken"`
	UserProfile   json.RawMessage `json:"userProfile"`
	Organizations json.RawMessage `json:"organizations"`
}

// listLinkedInTargets builds LinkedIn's targets: the member's own profile
// first, then each organization they administer. With a pendingDataToken the
// OAuth token, profile and organizations are read from Zernio (the read does
// not consume the token); otherwise they are the ones the redirect carried.
func (c *Client) listLinkedInTargets(ctx context.Context, cb ConnectCallback) (ConnectSelection, error) {
	sel := ConnectSelection{TempToken: cb.TempToken, UserProfile: cb.UserProfile}
	orgs := cb.Organizations
	if cb.PendingDataToken != "" {
		var pd pendingConnectData
		q := url.Values{"token": {cb.PendingDataToken}}
		if err := c.doHeaders(ctx, http.MethodGet, "/connect/pending-data", q, nil, nil, &pd); err != nil {
			return ConnectSelection{}, err
		}
		sel.TempToken = cmp.Or(pd.TempToken, sel.TempToken)
		if isJSONObject(pd.UserProfile) {
			sel.UserProfile = pd.UserProfile
		}
		if len(pd.Organizations) > 0 {
			orgs = pd.Organizations
		}
	}

	if isJSONObject(sel.UserProfile) {
		me := decodeConnectTarget(sel.UserProfile)
		sel.Targets = append(sel.Targets, ConnectTarget{
			ID:        linkedInPersonalTargetID,
			Name:      cmp.Or(me.Name, me.Username, "Personal profile"),
			Username:  me.Username,
			Kind:      KindPersonal,
			AvatarURL: me.AvatarURL,
		})
	}
	items, _ := asJSONArray(orgs)
	for _, it := range items {
		t := decodeConnectTarget(it)
		if t.ID == "" {
			continue
		}
		t.ID = strings.TrimPrefix(t.ID, linkedInOrgURNPrefix)
		t.Kind = KindOrganization
		t.Name = cmp.Or(t.Name, t.Username, t.ID)
		sel.Targets = append(sel.Targets, t)
	}
	return sel, nil
}

// SelectConnectTarget finalizes the connection by choosing one target.
// userProfile is the opaque JSON Zernio handed back on the callback (or the
// pending-data read); it is forwarded verbatim. On success the account is
// created upstream and surfaces locally via the existing account sync.
func (c *Client) SelectConnectTarget(ctx context.Context, platform, profileID, tempToken, connectToken string, target ConnectTarget, userProfile json.RawMessage) error {
	if c == nil {
		return errors.New("zernio: client is disabled")
	}
	spec, ok := connectSelectSpecs[platform]
	if !ok {
		return errors.New("zernio: platform has no connect-select step: " + platform)
	}
	body := map[string]any{
		"profileId": profileID,
		"tempToken": tempToken,
	}
	if platform == "linkedin" {
		maps.Copy(body, linkedInSelectBody(target))
	} else {
		body[spec.idField] = target.ID
	}
	if len(userProfile) > 0 {
		body["userProfile"] = userProfile
	}
	headers := map[string]string{"X-Connect-Token": connectToken}
	path := "/connect/" + url.PathEscape(platform) + "/" + spec.segment
	return c.doHeaders(ctx, http.MethodPost, path, nil, headers, body, nil)
}

// linkedInSelectBody is the account part of POST
// /connect/linkedin/select-organization: accountType "personal", or
// "organization" with selectedOrganization {id, urn, name[, vanityName,
// logoUrl]}.
func linkedInSelectBody(t ConnectTarget) map[string]any {
	if t.Kind == KindPersonal {
		return map[string]any{"accountType": KindPersonal}
	}
	org := map[string]any{
		"id":   t.ID,
		"urn":  linkedInOrgURNPrefix + t.ID,
		"name": cmp.Or(t.Name, t.ID),
	}
	if t.Username != "" {
		org["vanityName"] = t.Username
	}
	if t.AvatarURL != "" {
		org["logoUrl"] = t.AvatarURL
	}
	return map[string]any{"accountType": KindOrganization, "selectedOrganization": org}
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

func isJSONObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '{'
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
		AvatarURL: firstString(m, "avatarUrl", "avatar_url", "picture", "profilePicture", "logoUrl", "imageUrl"),
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
