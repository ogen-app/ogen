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
	"slices"
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

// Window is an analytics range and platform filter.
type Window struct {
	From      string   `json:"from,omitempty"` // YYYY-MM-DD
	To        string   `json:"to,omitempty"`   // YYYY-MM-DD
	Platforms []string `json:"platforms,omitempty"`
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

func (e *InvalidError) Error() string { return fmt.Sprintf("viewContext.%s: %s", e.Field, e.Reason) }

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
	if !slices.Contains(screens, v.Screen) {
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
	return checkIDs("window.platforms", w.Platforms)
}

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

func checkKind(field string, k EntityKind) error {
	if !slices.Contains(kinds, k) {
		return invalid(field, "unknown kind %q", k)
	}
	return nil
}

func checkID(field, id string) error {
	if id == "" {
		return invalid(field, "is required")
	}
	if len(id) > MaxIDLen {
		return invalid(field, "longer than %d characters", MaxIDLen)
	}
	return nil
}

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
