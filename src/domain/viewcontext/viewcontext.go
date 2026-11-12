// Package viewcontext is the contract for what the user was looking at when
// they sent an assistant turn: the screen, the thread's scope, the entity in
// focus, the selection, and the filters and visible items around it. The
// client sends references only — ids and filters, never content — and the
// server reloads everything it uses, so nothing on screen is trusted as data.
package viewcontext

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
)

// Version is the schema version this server reads.
const Version = 1

// Limits keep a turn's context small. A larger selection travels as a filter
// ("select all matching") that the server resolves itself.
const (
	MaxSelectionIDs = 200
	MaxVisibleIDs   = 50
	MaxFilterLen    = 2000
	MaxIDLen        = 64
	MaxPlatforms    = 20
)

// Screen names a screen of the app. Campaign-level screens are prefixed
// "campaign.", detail screens name what they show.
type Screen string

var screens = []Screen{
	"home", "ideas", "assets", "asset", "analytics", "analytics.post", "calendar", "campaigns", "tasks",
	"foundation", "foundation.voices", "foundation.voice", "foundation.audiences", "foundation.audience",
	"foundation.facts", "foundation.guardrails", "foundation.look", "foundation.questions",
	"foundation.series", "foundation.series_item", "foundation.templates",
	"activity", "settings", "profile",
	"campaign.overview", "campaign.strategy", "campaign.list", "campaign.ideas", "campaign.assets",
	"campaign.asset", "campaign.analytics", "campaign.calendar", "campaign.activity", "campaign.settings",
	"post",
}

// Screens lists every screen, in a stable order.
func Screens() []Screen { return slices.Clone(screens) }

// Valid reports whether s names a screen of the app.
func (s Screen) Valid() bool { return slices.Contains(screens, s) }

// InCampaign reports whether s is a campaign-level screen, which needs a
// campaign to show.
func (s Screen) InCampaign() bool { return strings.HasPrefix(string(s), "campaign.") }

// detailKinds names the entity each detail screen shows.
var detailKinds = map[Screen]EntityKind{
	"asset": KindAsset, "campaign.asset": KindAsset,
	"post": KindPost, "analytics.post": KindPost,
	"foundation.voice": KindVoice, "foundation.audience": KindAudience,
	"foundation.series_item": KindSeries,
}

// DetailKind is the kind of entity a detail screen shows; false for a screen
// that shows no single entity.
func (s Screen) DetailKind() (EntityKind, bool) {
	k, ok := detailKinds[s]
	return k, ok
}

// listKinds names what each filterable list screen lists. A campaign's posts
// list and the calendars read the posts list's filter; on a calendar the
// window narrows it.
var listKinds = map[Screen]EntityKind{
	"ideas": KindIdea, "campaign.ideas": KindIdea,
	"assets": KindAsset, "campaign.assets": KindAsset,
	"campaign.list": KindPost, "calendar": KindPost, "campaign.calendar": KindPost,
}

// selectKinds names what each list screen whose items can be ticked lists:
// the filterable lists, and the foundation lists, which are short enough to
// select by id.
var selectKinds = func() map[Screen]EntityKind {
	kinds := map[Screen]EntityKind{
		"foundation.voices": KindVoice, "foundation.audiences": KindAudience,
		"foundation.facts": KindFact, "foundation.series": KindSeries,
	}
	maps.Copy(kinds, listKinds)
	return kinds
}()

// SelectKind is the kind a list screen lets the assistant tick; false for a
// screen without a selectable list.
func (s Screen) SelectKind() (EntityKind, bool) {
	k, ok := selectKinds[s]
	return k, ok
}

// ListKind is the kind a filterable list screen lists; false for any other
// screen.
func (s Screen) ListKind() (EntityKind, bool) {
	k, ok := listKinds[s]
	return k, ok
}

// EntityKind names what a reference points at.
type EntityKind string

const (
	KindIdea     EntityKind = "idea"
	KindAsset    EntityKind = "asset"
	KindPost     EntityKind = "post"
	KindCampaign EntityKind = "campaign"
	KindVoice    EntityKind = "voice"
	KindAudience EntityKind = "audience"
	KindFact     EntityKind = "fact"
	KindSeries   EntityKind = "series"
)

var kinds = []EntityKind{KindIdea, KindAsset, KindPost, KindCampaign, KindVoice, KindAudience, KindFact, KindSeries}

// Kinds lists every entity kind, in a stable order.
func Kinds() []EntityKind { return slices.Clone(kinds) }

// Scope is the thread a turn belongs to.
type Scope struct {
	Kind models.AssistantScopeKind `json:"kind"`
	// ID is the campaign or post id; empty for the workspace.
	ID string `json:"id,omitempty"`
}

// Ref points at one entity.
type Ref struct {
	Kind EntityKind `json:"kind"`
	ID   string     `json:"id"`
}

// Selection is what the user ticked: either explicit ids, or every item
// matching the screen's filter ("select all matching").
type Selection struct {
	Kind        EntityKind `json:"kind"`
	IDs         []string   `json:"ids,omitempty"`
	AllMatching bool       `json:"allMatching,omitempty"`
	// Count is how many items the user saw selected; the server's own count
	// after resolving wins.
	Count int `json:"count,omitempty"`
}

// Window is a date range and its platform and campaign filters: an analytics
// dashboard's, or the days a calendar shows. On a calendar it narrows the
// filter that "select all matching" resolves, its days read in the filter's
// tz.
type Window struct {
	From      string   `json:"from,omitempty"` // YYYY-MM-DD
	To        string   `json:"to,omitempty"`   // YYYY-MM-DD
	Platforms []string `json:"platforms,omitempty"`
	// CampaignID is the campaign the dashboard is narrowed to; empty for
	// the whole workspace.
	CampaignID string `json:"campaignId,omitempty"`
	// Preset is the relative span the dates resolve, e.g. 30d, 12w or 6mo,
	// when the period picker shows one; empty for dates picked by hand.
	Preset string `json:"preset,omitempty"`
}

// What a window change can change.
const (
	WindowPeriod    = "period"
	WindowPlatforms = "platforms"
	WindowCampaign  = "campaign"
)

// Clone is w with its own platforms.
func (w Window) Clone() Window {
	w.Platforms = slices.Clone(w.Platforms)
	return w
}

// Changes names what differs from before, in WindowPeriod, WindowPlatforms,
// WindowCampaign order; a nil before is no window, every field empty.
// Platforms compare as a set.
func (w Window) Changes(before *Window) []string {
	var b Window
	if before != nil {
		b = *before
	}
	var out []string
	if w.From != b.From || w.To != b.To || w.Preset != b.Preset {
		out = append(out, WindowPeriod)
	}
	if !slices.Equal(slices.Sorted(slices.Values(w.Platforms)), slices.Sorted(slices.Values(b.Platforms))) {
		out = append(out, WindowPlatforms)
	}
	if w.CampaignID != b.CampaignID {
		out = append(out, WindowCampaign)
	}
	return out
}

// CanonicalPreset is the relative span s as a window keeps it, its count
// without leading zeros ("0007d" is "7d"); "" when s isn't one that
// ValidPreset accepts that way.
func CanonicalPreset(s string) string {
	unit := strings.TrimLeft(s, "0123456789")
	digits := strings.TrimLeft(s[:len(s)-len(unit)], "0")
	if p := digits + unit; ValidPreset(p) {
		return p
	}
	return ""
}

// ValidPreset reports whether s is a relative span: 1 to 3 digits, then d,
// w or mo.
func ValidPreset(s string) bool {
	unit := strings.TrimLeft(s, "0123456789")
	digits := len(s) - len(unit)
	return digits >= 1 && digits <= 3 && (unit == "d" || unit == "w" || unit == "mo")
}

// Visible lists the first items on screen, so "the third one" resolves.
type Visible struct {
	Kind EntityKind `json:"kind"`
	IDs  []string   `json:"ids"`
}

// ViewContext is one turn's screen. Only Version, Screen and Scope are
// required.
type ViewContext struct {
	Version   int        `json:"version"`
	Screen    Screen     `json:"screen"`
	Scope     Scope      `json:"scope"`
	Focus     *Ref       `json:"focus,omitempty"`
	Selection *Selection `json:"selection,omitempty"`
	// Filter is the screen's filter query, in the list endpoint's own syntax;
	// with Selection.AllMatching it defines the selection.
	Filter      string   `json:"filter,omitempty"`
	SavedViewID string   `json:"savedViewId,omitempty"`
	Sort        string   `json:"sort,omitempty"`
	Window      *Window  `json:"window,omitempty"`
	Visible     *Visible `json:"visible,omitempty"`
}

// InvalidError is a view context the server won't use (HTTP 400).
type InvalidError struct {
	Field  string
	Reason string
}

// Error names the field and why it was rejected.
func (e *InvalidError) Error() string { return fmt.Sprintf("viewContext.%s: %s", e.Field, e.Reason) }

// invalid returns an InvalidError for field with a formatted reason.
func invalid(field, format string, args ...any) error {
	return &InvalidError{Field: field, Reason: fmt.Sprintf(format, args...)}
}

// Parse decodes and validates a view context. Unknown fields are ignored, so
// a newer client stays readable as long as it sends a version this server
// knows.
func Parse(raw []byte) (*ViewContext, error) {
	var v ViewContext
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&v); err != nil {
		return nil, invalid("", "not a JSON object: %v", err)
	}
	if err := v.Validate(); err != nil {
		return nil, err
	}
	v.normalize()
	return &v, nil
}

// Validate checks the shape and the limits.
func (v *ViewContext) Validate() error {
	if v.Version != Version {
		return invalid("version", "unsupported version %d, want %d", v.Version, Version)
	}
	if !v.Screen.Valid() {
		return invalid("screen", "unknown screen %q", v.Screen)
	}
	if err := v.Scope.validate(); err != nil {
		return err
	}
	if v.Focus != nil {
		if err := checkKind("focus.kind", v.Focus.Kind); err != nil {
			return err
		}
		if err := checkID("focus.id", v.Focus.ID); err != nil {
			return err
		}
	}
	if err := v.Selection.validate(); err != nil {
		return err
	}
	if len(v.Filter) > MaxFilterLen {
		return invalid("filter", "longer than %d characters", MaxFilterLen)
	}
	for field, s := range map[string]string{"savedViewId": v.SavedViewID, "sort": v.Sort} {
		if len(s) > MaxIDLen {
			return invalid(field, "longer than %d characters", MaxIDLen)
		}
	}
	if err := v.Window.validate(); err != nil {
		return err
	}
	return v.Visible.validate()
}

// validate checks the scope kind, and that the id is set exactly when the
// scope needs one.
func (s Scope) validate() error {
	switch s.Kind {
	case models.AssistantScopeWorkspace:
		if s.ID != "" {
			return invalid("scope.id", "must be empty for the workspace")
		}
		return nil
	case models.AssistantScopeCampaign, models.AssistantScopePost:
		return checkID("scope.id", s.ID)
	}
	return invalid("scope.kind", "unknown scope %q", s.Kind)
}

// validate checks a selection, when there is one: a known kind and either
// ids within the limit or allMatching.
func (s *Selection) validate() error {
	if s == nil {
		return nil
	}
	if err := checkKind("selection.kind", s.Kind); err != nil {
		return err
	}
	if s.AllMatching && len(s.IDs) > 0 {
		return invalid("selection", "either ids or allMatching, not both")
	}
	if !s.AllMatching && len(s.IDs) == 0 {
		return invalid("selection", "needs ids or allMatching")
	}
	if len(s.IDs) > MaxSelectionIDs {
		return invalid("selection.ids", "more than %d ids; send allMatching with the filter instead", MaxSelectionIDs)
	}
	return checkIDs("selection.ids", s.IDs)
}

// validate checks a window, when there is one: ISO dates in order, a
// bounded platform list and a campaign id.
func (w *Window) validate() error {
	if w == nil {
		return nil
	}
	var from, to time.Time
	var err error
	if w.From != "" {
		if from, err = time.Parse(time.DateOnly, w.From); err != nil {
			return invalid("window.from", "not a YYYY-MM-DD date")
		}
	}
	if w.To != "" {
		if to, err = time.Parse(time.DateOnly, w.To); err != nil {
			return invalid("window.to", "not a YYYY-MM-DD date")
		}
	}
	if w.From != "" && w.To != "" && to.Before(from) {
		return invalid("window", "to is before from")
	}
	if len(w.Platforms) > MaxPlatforms {
		return invalid("window.platforms", "more than %d platforms", MaxPlatforms)
	}
	if err := checkIDs("window.platforms", w.Platforms); err != nil {
		return err
	}
	if w.Preset != "" && !ValidPreset(w.Preset) {
		return invalid("window.preset", "not a relative span like 30d, 12w or 6mo")
	}
	if w.CampaignID != "" {
		return checkID("window.campaignId", w.CampaignID)
	}
	return nil
}

// validate checks the visible items, when there are any: a known kind and
// ids within the limit.
func (v *Visible) validate() error {
	if v == nil {
		return nil
	}
	if err := checkKind("visible.kind", v.Kind); err != nil {
		return err
	}
	if len(v.IDs) > MaxVisibleIDs {
		return invalid("visible.ids", "more than %d ids", MaxVisibleIDs)
	}
	return checkIDs("visible.ids", v.IDs)
}

// checkKind rejects an entity kind this server does not know.
func checkKind(field string, k EntityKind) error {
	if !slices.Contains(kinds, k) {
		return invalid(field, "unknown kind %q", k)
	}
	return nil
}

// checkID rejects a blank or oversized id.
func checkID(field, id string) error {
	if id == "" {
		return invalid(field, "is required")
	}
	if len(id) > MaxIDLen {
		return invalid(field, "longer than %d characters", MaxIDLen)
	}
	return nil
}

// checkIDs runs checkID on each id, naming the bad one by its index.
func checkIDs(field string, ids []string) error {
	for i, id := range ids {
		if err := checkID(fmt.Sprintf("%s[%d]", field, i), id); err != nil {
			return err
		}
	}
	return nil
}

// normalize drops duplicate ids, keeping their first order.
func (v *ViewContext) normalize() {
	if v.Selection != nil {
		v.Selection.IDs = dedupe(v.Selection.IDs)
	}
	if v.Visible != nil {
		v.Visible.IDs = dedupe(v.Visible.IDs)
	}
}

// dedupe drops repeated ids in place, keeping the first of each.
func dedupe(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := ids[:0]
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// Stored returns the view context as stored with a user message.
func (v *ViewContext) Stored() (models.JSONMap, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m models.JSONMap
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, errors.Join(errors.New("encode view context"), err)
	}
	return m, nil
}
