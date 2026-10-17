package savedviews

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ogen-app/ogen/src/domain/listquery"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// MaxNameLen bounds a view's name, in runes.
const MaxNameLen = 80

// Events published for shared views on the surface's topic.
const (
	EventCreated = "saved_view.created"
	EventUpdated = "saved_view.updated"
	EventDeleted = "saved_view.deleted"
)

// Topic is the bus topic of a surface's shared views.
func Topic(surface string) string { return "entity:saved_views:" + surface }

var (
	// ErrUnknownSurface names no registered surface.
	ErrUnknownSurface = errors.New("unknown surface")
	// ErrInvalidName is a blank name or one over MaxNameLen runes.
	ErrInvalidName = errors.New("name must be 1 to 80 characters")
	// ErrUnknownCount names a count the surface does not have.
	ErrUnknownCount = errors.New("unknown count")
	// ErrNotFound is a view that does not exist or the caller cannot see.
	ErrNotFound = errors.New("saved view not found")
	// ErrForbidden is a visible view the caller may not change.
	ErrForbidden = errors.New("only the view's creator or a workspace owner can change it")
	// ErrOwnerlessPrivate refuses unsharing a view whose creator has left: no
	// one could see it again.
	ErrOwnerlessPrivate = errors.New("a view whose creator has left must stay shared")
)

// QueryError is a query a surface rejects. Param names the parameter, empty
// when the string itself does not parse.
type QueryError struct {
	Param string
	Msg   string
}

func (e *QueryError) Error() string { return e.Msg }

// Service is the saved-views use case over the registered surfaces.
type Service struct {
	repo     repository.SavedViewRepository
	hub      eventhub.Hub
	surfaces map[string]Surface
	now      func() time.Time
}

// New returns the service. A nil hub publishes nothing.
func New(repo repository.SavedViewRepository, hub eventhub.Hub, surfaces ...Surface) *Service {
	s := &Service{repo: repo, hub: hub, surfaces: map[string]Surface{}, now: time.Now}
	for _, sf := range surfaces {
		s.surfaces[sf.Name()] = sf
	}
	return s
}

// Caller is who is asking: their membership id and whether they own the
// workspace.
type Caller struct {
	UserID string
	Owner  bool
}

func (c Caller) sees(v *models.SavedView) bool {
	return v.Shared || (v.CreatedBy != nil && *v.CreatedBy == c.UserID)
}

func (c Caller) canEdit(v *models.SavedView) bool {
	if v.CreatedBy != nil && *v.CreatedBy == c.UserID {
		return true
	}
	return c.Owner && (v.Shared || v.CreatedBy == nil)
}

func (s *Service) surface(name string) (Surface, error) {
	sf, ok := s.surfaces[name]
	if !ok {
		return nil, ErrUnknownSurface
	}
	return sf, nil
}

// prepare canonicalises and validates a query for sf.
func prepare(sf Surface, raw string) (string, error) {
	v, query, err := canonical(sf, raw)
	if err != nil {
		return "", err
	}
	if err := sf.Validate(v); err != nil {
		return "", asQueryError(err)
	}
	return query, nil
}

func asQueryError(err error) error {
	if qe, ok := errors.AsType[*listquery.Error](err); ok {
		return &QueryError{Param: qe.Param, Msg: qe.Msg}
	}
	return err
}

// stored parses a stored view's query in its surface's current version, false
// when it no longer parses.
func stored(sf Surface, v *models.SavedView) (url.Values, bool) {
	vals, err := url.ParseQuery(v.Query)
	if err != nil {
		return nil, false
	}
	if v.QueryVersion < sf.Version() {
		vals = sf.Upgrade(v.QueryVersion, vals)
	}
	return vals, sf.Validate(vals) == nil
}

// List returns the caller's views of surface and every shared one, newest
// first. counts names the counts each valid view carries, computed for all of
// them in one round trip; the date presets read loc.
func (s *Service) List(ctx context.Context, caller Caller, surface string, counts []string, loc *time.Location) ([]models.SavedView, error) {
	sf, err := s.surface(surface)
	if err != nil {
		return nil, err
	}
	for _, c := range counts {
		if !slices.Contains(sf.Counts(), c) {
			return nil, ErrUnknownCount
		}
	}
	views, err := s.repo.ListVisible(ctx, surface, caller.UserID)
	if err != nil {
		return nil, err
	}
	var queries []url.Values
	var counted []int
	for i := range views {
		v := &views[i]
		v.CanEdit = caller.canEdit(v)
		var vals url.Values
		if vals, v.Valid = stored(sf, v); v.Valid && len(counts) > 0 {
			queries = append(queries, vals)
			counted = append(counted, i)
		}
	}
	if len(queries) == 0 {
		return views, nil
	}
	got, err := sf.Count(ctx, queries, s.now().UTC(), loc)
	if err != nil {
		return nil, err
	}
	for j, i := range counted {
		views[i].Counts = make(map[string]int, len(counts))
		for _, c := range counts {
			views[i].Counts[c] = got[j][c]
		}
	}
	return views, nil
}

// CreateInput is a new view. Query may carry volatile parameters; they are
// dropped.
type CreateInput struct {
	Surface string
	Name    string
	Query   string
	Shared  bool
}

// Create validates and stores a view created by the caller.
func (s *Service) Create(ctx context.Context, caller Caller, in CreateInput) (*models.SavedView, error) {
	sf, err := s.surface(in.Surface)
	if err != nil {
		return nil, err
	}
	name, err := validName(in.Name)
	if err != nil {
		return nil, err
	}
	query, err := prepare(sf, in.Query)
	if err != nil {
		return nil, err
	}
	id, err := models.NewID()
	if err != nil {
		return nil, err
	}
	now := s.stamp()
	view := &models.SavedView{
		ID: id, Surface: sf.Name(), Name: name, Query: query, QueryVersion: sf.Version(),
		Shared: in.Shared, CreatedBy: new(caller.UserID), CreatedAt: now, UpdatedAt: now,
		Valid: true, CanEdit: true,
	}
	if err := s.repo.Create(ctx, view); err != nil {
		return nil, err
	}
	if view.Shared {
		s.publish(ctx, view, EventCreated)
	}
	return view, nil
}

// UpdateInput is a presence-aware edit: a nil field is left alone.
type UpdateInput struct {
	Name   *string
	Query  *string
	Shared *bool
}

// Update applies the edit to a view the caller may change.
func (s *Service) Update(ctx context.Context, caller Caller, id string, in UpdateInput) (*models.SavedView, error) {
	view, sf, err := s.editable(ctx, caller, id)
	if err != nil {
		return nil, err
	}
	if sf == nil {
		return nil, ErrUnknownSurface
	}
	wasShared := view.Shared
	var columns []string
	if in.Name != nil {
		if view.Name, err = validName(*in.Name); err != nil {
			return nil, err
		}
		columns = append(columns, "name")
	}
	if in.Query != nil {
		if view.Query, err = prepare(sf, *in.Query); err != nil {
			return nil, err
		}
		view.QueryVersion = sf.Version()
		columns = append(columns, "query", "query_version")
	}
	if in.Shared != nil {
		if !*in.Shared && view.CreatedBy == nil {
			return nil, ErrOwnerlessPrivate
		}
		view.Shared = *in.Shared
		columns = append(columns, "shared")
	}
	view.UpdatedAt = s.stamp()
	ok, err := s.repo.Update(ctx, view, columns...)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotFound
	}
	if wasShared || view.Shared {
		s.publish(ctx, view, EventUpdated)
	}
	_, view.Valid = stored(sf, view)
	view.CanEdit = caller.canEdit(view)
	return view, nil
}

// Delete removes a view the caller may change.
func (s *Service) Delete(ctx context.Context, caller Caller, id string) error {
	view, _, err := s.editable(ctx, caller, id)
	if err != nil {
		return err
	}
	ok, err := s.repo.Delete(ctx, id)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	if view.Shared {
		s.publish(ctx, view, EventDeleted)
	}
	return nil
}

// editable loads a view the caller sees and may change. A view of a surface
// no longer registered can still be deleted, never edited.
func (s *Service) editable(ctx context.Context, caller Caller, id string) (*models.SavedView, Surface, error) {
	view, err := s.repo.GetByID(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	if !caller.sees(view) {
		return nil, nil, ErrNotFound
	}
	if !caller.canEdit(view) {
		return nil, nil, ErrForbidden
	}
	return view, s.surfaces[view.Surface], nil
}

func validName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > MaxNameLen {
		return "", ErrInvalidName
	}
	return name, nil
}

// stamp is the current time at the microsecond resolution Postgres stores.
func (s *Service) stamp() time.Time {
	return s.now().UTC().Truncate(time.Microsecond)
}

// publish announces a change to a shared view to every member of the
// workspace. It is a refetch hint and fails closed without a tenant in ctx,
// because an event with neither filter set would reach every tenant.
func (s *Service) publish(ctx context.Context, view *models.SavedView, typ string) {
	if s.hub == nil {
		return
	}
	tenantID, ok := tenantctx.From(ctx)
	if !ok {
		return
	}
	evID, err := models.NewID()
	if err != nil {
		return
	}
	action := strings.TrimPrefix(typ, "saved_view.")
	_ = s.hub.Publish(ctx, eventhub.Event{
		ID:       evID,
		Topic:    Topic(view.Surface),
		Type:     typ,
		TenantID: tenantID,
		Payload:  map[string]string{"id": view.ID, "surface": view.Surface, "action": action},
	})
}
