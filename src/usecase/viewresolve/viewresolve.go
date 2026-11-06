// Package viewresolve turns a turn's view context — references only — into
// what the assistant may act on: every referenced entity reloaded in the
// caller's tenant, a "select all matching" selection expanded through the
// list's own query, and a one-line summary of the screen for the prompt.
package viewresolve

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/ogen-app/ogen/src/domain/listquery"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/usecase/assets"
	"github.com/ogen-app/ogen/src/usecase/ideas"
)

// MaxSelection caps the items a selection loads. A larger "select all
// matching" keeps its full count in Resolved.SelectionTotal.
const MaxSelection = viewcontext.MaxSelectionIDs

// ErrScopeNotFound is a campaign or post scope that names nothing live in the
// caller's tenant.
var ErrScopeNotFound = errors.New("assistant scope not found")

// Titles reads entity titles; repository.EntityTitleRepository.
type Titles interface {
	Titles(ctx context.Context, kind viewcontext.EntityKind, ids []string) (map[string]string, error)
}

// IdeaIDs expands an ideas query; repository.IdeaRepository.
type IdeaIDs interface {
	QueryIDs(ctx context.Context, q models.IdeaQuery, limit int) ([]string, int, error)
}

// AssetIDs expands an assets query; repository.AssetRepository.
type AssetIDs interface {
	QueryIDs(ctx context.Context, q models.AssetQuery) ([]string, error)
}

// Item is one referenced entity. Title is empty for a kind without titles,
// whose id is passed through unverified.
type Item struct {
	Kind  viewcontext.EntityKind `json:"kind"`
	ID    string                 `json:"id"`
	Title string                 `json:"title,omitempty"`
}

// Resolved is a view context reloaded on the server.
type Resolved struct {
	View *viewcontext.ViewContext
	// Scope is the campaign or post the thread belongs to; nil for the
	// workspace.
	Scope *Item
	// Focus is nil when there was none or it no longer exists.
	Focus *Item
	// Selection keeps the client's order, without the ids that no longer
	// exist, and at most MaxSelection items.
	Selection []Item
	// SelectionTotal is the selection's full size: the whole match for "select
	// all matching", else len(Selection).
	SelectionTotal int
	// Dropped counts the selected ids that no longer exist.
	Dropped int
	Visible []Item
	// Summary is the screen in one line, for the prompt.
	Summary string
}

// Resolver reloads view contexts.
type Resolver struct {
	titles Titles
	ideas  IdeaIDs
	assets AssetIDs
	now    func() time.Time
}

// New returns a Resolver.
func New(titles Titles, ideaIDs IdeaIDs, assetIDs AssetIDs) *Resolver {
	return &Resolver{titles: titles, ideas: ideaIDs, assets: assetIDs, now: time.Now}
}

// Resolve reloads v in the caller's tenant. It returns ErrScopeNotFound for a
// scope that is gone and a *viewcontext.InvalidError for a "select all
// matching" whose filter the list rejects or whose kind has no list query.
// Focus, selection and visible ids that no longer exist are dropped, not
// refused: the screen may simply be stale.
func (r *Resolver) Resolve(ctx context.Context, v *viewcontext.ViewContext) (*Resolved, error) {
	out := &Resolved{View: v}
	var selIDs []string
	if s := v.Selection; s != nil {
		selIDs = s.IDs
		if s.AllMatching {
			ids, total, err := r.matching(ctx, v)
			if err != nil {
				return nil, err
			}
			selIDs, out.SelectionTotal = ids, total
		}
	}

	refs := newRefs()
	if v.Scope.Kind != models.AssistantScopeWorkspace {
		refs.add(scopeKind(v.Scope.Kind), v.Scope.ID)
	}
	if v.Focus != nil {
		refs.add(v.Focus.Kind, v.Focus.ID)
	}
	if v.Selection != nil {
		refs.add(v.Selection.Kind, selIDs...)
	}
	if v.Visible != nil {
		refs.add(v.Visible.Kind, v.Visible.IDs...)
	}
	if err := refs.load(ctx, r.titles); err != nil {
		return nil, err
	}

	if v.Scope.Kind != models.AssistantScopeWorkspace {
		item, ok := refs.item(scopeKind(v.Scope.Kind), v.Scope.ID)
		if !ok {
			return nil, ErrScopeNotFound
		}
		out.Scope = &item
	}
	if v.Focus != nil {
		if item, ok := refs.item(v.Focus.Kind, v.Focus.ID); ok {
			out.Focus = &item
		}
	}
	if s := v.Selection; s != nil {
		out.Selection = refs.items(s.Kind, selIDs)
		if s.AllMatching {
			// Matches deleted between the query and the title read are rare;
			// they stay in the total.
			out.Selection = out.Selection[:min(len(out.Selection), MaxSelection)]
		} else {
			out.Dropped = len(selIDs) - len(out.Selection)
			out.SelectionTotal = len(out.Selection)
		}
	}
	if v.Visible != nil {
		out.Visible = refs.items(v.Visible.Kind, v.Visible.IDs)
	}
	out.Summary = summary(out)
	return out, nil
}

// matching expands "select all matching" through the list's own query
// syntax, confined to the thread's campaign. It returns at most MaxSelection
// ids and the size of the whole match.
func (r *Resolver) matching(ctx context.Context, v *viewcontext.ViewContext) ([]string, int, error) {
	var campaign string
	if v.Scope.Kind == models.AssistantScopeCampaign {
		campaign = v.Scope.ID
	}
	q, err := r.parseFilter(v.Selection.Kind, v.Filter)
	if err != nil {
		if errors.Is(err, errNoListQuery) {
			return nil, 0, &viewcontext.InvalidError{
				Field:  "selection.allMatching",
				Reason: fmt.Sprintf("not supported for %s; send ids", v.Selection.Kind),
			}
		}
		return nil, 0, err
	}
	switch q := q.(type) {
	case models.IdeaQuery:
		q.ScopeCampaign = campaign
		ids, total, err := r.ideas.QueryIDs(ctx, q, MaxSelection)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, ErrScopeNotFound
		}
		return ids, total, err
	case models.AssetQuery:
		q.ScopeCampaign = campaign
		ids, err := r.assets.QueryIDs(ctx, q)
		if err != nil {
			return nil, 0, err
		}
		return ids[:min(len(ids), MaxSelection)], len(ids), nil
	}
	return nil, 0, errNoListQuery
}

// errNoListQuery is a kind without a list query syntax.
var errNoListQuery = errors.New("no list query for this kind")

// parseFilter reads filter in the list syntax of kind: a models.IdeaQuery or
// a models.AssetQuery. A filter the list rejects is a
// *viewcontext.InvalidError on "filter".
func (r *Resolver) parseFilter(kind viewcontext.EntityKind, filter string) (any, error) {
	params, err := url.ParseQuery(filter)
	if err != nil {
		return nil, &viewcontext.InvalidError{Field: "filter", Reason: "not a query string"}
	}
	switch kind {
	case viewcontext.KindIdea:
		q, err := ideas.ParseQuery(params, r.now())
		if err != nil {
			return nil, filterError(err)
		}
		return q, nil
	case viewcontext.KindAsset:
		q, err := assets.ParseQuery(params, r.now())
		if err != nil {
			return nil, filterError(err)
		}
		return q, nil
	}
	return nil, errNoListQuery
}

// CheckFilter reports whether the list of kind accepts filter: nil, a
// *viewcontext.InvalidError naming what it rejects, or an error for a kind
// with no filter syntax.
func (r *Resolver) CheckFilter(kind viewcontext.EntityKind, filter string) error {
	_, err := r.parseFilter(kind, filter)
	return err
}

// Items reloads ids of kind in the caller's tenant, in order, dropping those
// that no longer exist. A kind without titles passes its ids through.
func (r *Resolver) Items(ctx context.Context, kind viewcontext.EntityKind, ids []string) ([]Item, error) {
	refs := newRefs()
	refs.add(kind, ids...)
	if err := refs.load(ctx, r.titles); err != nil {
		return nil, err
	}
	return refs.items(kind, ids), nil
}

func filterError(err error) error {
	if le, ok := errors.AsType[*listquery.Error](err); ok {
		return &viewcontext.InvalidError{Field: "filter", Reason: le.Msg}
	}
	return err
}

func scopeKind(k models.AssistantScopeKind) viewcontext.EntityKind {
	if k == models.AssistantScopePost {
		return viewcontext.KindPost
	}
	return viewcontext.KindCampaign
}

// refs batches the ids of each kind so every kind costs one read.
type refs struct {
	ids    map[viewcontext.EntityKind][]string
	titles map[viewcontext.EntityKind]map[string]string
	// untitled holds the kinds without titles, whose ids pass through.
	untitled map[viewcontext.EntityKind]bool
}

func newRefs() *refs {
	return &refs{
		ids:      map[viewcontext.EntityKind][]string{},
		titles:   map[viewcontext.EntityKind]map[string]string{},
		untitled: map[viewcontext.EntityKind]bool{},
	}
}

func (r *refs) add(kind viewcontext.EntityKind, ids ...string) {
	r.ids[kind] = append(r.ids[kind], ids...)
}

func (r *refs) load(ctx context.Context, titles Titles) error {
	for kind, ids := range r.ids {
		got, err := titles.Titles(ctx, kind, ids)
		switch {
		case errors.Is(err, repository.ErrUntitledKind):
			r.untitled[kind] = true
		case err != nil:
			return fmt.Errorf("load %s titles: %w", kind, err)
		default:
			r.titles[kind] = got
		}
	}
	return nil
}

func (r *refs) item(kind viewcontext.EntityKind, id string) (Item, bool) {
	if r.untitled[kind] {
		return Item{Kind: kind, ID: id}, true
	}
	title, ok := r.titles[kind][id]
	return Item{Kind: kind, ID: id, Title: title}, ok
}

func (r *refs) items(kind viewcontext.EntityKind, ids []string) []Item {
	out := make([]Item, 0, len(ids))
	for _, id := range ids {
		if item, ok := r.item(kind, id); ok {
			out = append(out, item)
		}
	}
	return out
}

// summary renders the screen in one line, e.g.
// `On campaign.ideas in campaign "Spring launch" · 12 ideas selected`.
func summary(r *Resolved) string {
	var b strings.Builder
	fmt.Fprintf(&b, "On %s", r.View.Screen)
	if r.Scope != nil {
		fmt.Fprintf(&b, " in %s", label(*r.Scope))
	}
	parts := []string{b.String()}
	if r.Focus != nil {
		parts = append(parts, "focused on "+label(*r.Focus))
	}
	if s := r.View.Selection; s != nil {
		sel := fmt.Sprintf("%d %s selected", r.SelectionTotal, plural(s.Kind, r.SelectionTotal))
		switch {
		case s.AllMatching && r.SelectionTotal > len(r.Selection):
			sel += fmt.Sprintf(" (all matching the filter; the first %d loaded)", len(r.Selection))
		case s.AllMatching:
			sel += " (all matching the filter)"
		case r.Dropped > 0:
			sel += fmt.Sprintf(" (%d more no longer exist)", r.Dropped)
		}
		parts = append(parts, sel)
	}
	return strings.Join(parts, " · ")
}

func label(it Item) string {
	if it.Title == "" {
		return string(it.Kind) + " " + it.ID
	}
	return fmt.Sprintf("%s %q", it.Kind, it.Title)
}

func plural(kind viewcontext.EntityKind, n int) string {
	if n == 1 || kind == viewcontext.KindSeries {
		return string(kind)
	}
	return string(kind) + "s"
}
