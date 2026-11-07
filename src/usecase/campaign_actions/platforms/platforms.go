// Package platforms plans additions to the platforms a campaign publishes to
// and the post types it uses on each, checked against the enabled platform
// catalog. It is pure: the caller loads the catalog and saves the result.
package platforms

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/ogen-app/ogen/src/domain/models"
)

// Request asks for a platform, optionally narrowed to some of its post types.
// Platform is a catalog id or name.
type Request struct {
	Platform  string   `json:"platform"            jsonschema:"description=Platform name or id, e.g. \"Threads\"."`
	PostTypes []string `json:"postTypes,omitempty" jsonschema:"description=Post-type slugs to allow on it, e.g. [\"text-post\"]. Omit to allow all of the platform's types."`
}

// InvalidError is a request the catalog can't satisfy. Reason is safe to show
// the user; Alternatives are values that would work.
type InvalidError struct {
	Reason       string
	Alternatives []string
	// PostType is the type the platform doesn't have; empty when the
	// platform itself is unknown.
	PostType string
}

// Error returns the reason.
func (e *InvalidError) Error() string { return e.Reason }

// Added describes one change a plan makes, for the user-facing summary.
type Added struct {
	Name string
	// PostTypes are the types newly allowed; empty when a whole platform was
	// added with all its types, or a restricted one opened to all of them.
	PostTypes []string
	// NewPlatform is true when the platform was not targeted before.
	NewPlatform bool
}

// Plan returns the campaign's target platforms with reqs applied, and what
// changed. A request already satisfied changes nothing; when no request
// changes anything, added is empty.
func Plan(current models.CampaignPlatforms, catalog []models.Platform, reqs []Request) (models.CampaignPlatforms, []Added, error) {
	next := slices.Clone(current)
	var added []Added
	for _, r := range reqs {
		p, err := find(catalog, r.Platform)
		if err != nil {
			return nil, nil, err
		}
		types, err := checkTypes(p, r.PostTypes)
		if err != nil {
			return nil, nil, err
		}
		i := slices.IndexFunc(next, func(tp models.CampaignPlatform) bool { return tp.ID == p.ID })
		if i < 0 {
			next = append(next, models.CampaignPlatform{ID: p.ID, PostTypes: types})
			added = append(added, Added{Name: p.Name, PostTypes: types, NewPlatform: true})
			continue
		}
		// An empty selection already allows every type.
		if len(next[i].PostTypes) == 0 {
			continue
		}
		// Omitted types ask for all of them: lift the restriction.
		if len(types) == 0 {
			next[i].PostTypes = nil
			added = append(added, Added{Name: p.Name})
			continue
		}
		var fresh []string
		for _, t := range types {
			if !slices.Contains(next[i].PostTypes, t) {
				fresh = append(fresh, t)
			}
		}
		if len(fresh) > 0 {
			next[i].PostTypes = append(slices.Clone(next[i].PostTypes), fresh...)
			added = append(added, Added{Name: p.Name, PostTypes: fresh})
		}
	}
	return next, added, nil
}

// Summary describes added changes in one line, e.g. "Threads, plus the
// carousel post type on LinkedIn".
func Summary(added []Added) string {
	parts := make([]string, len(added))
	for i, a := range added {
		switch {
		case a.NewPlatform && len(a.PostTypes) == 0:
			parts[i] = a.Name
		case len(a.PostTypes) == 0:
			parts[i] = "every post type on " + a.Name
		case a.NewPlatform:
			parts[i] = fmt.Sprintf("%s (%s)", a.Name, strings.Join(a.PostTypes, ", "))
		default:
			parts[i] = fmt.Sprintf("the %s post type on %s", strings.Join(a.PostTypes, ", "), a.Name)
		}
	}
	return strings.Join(parts, "; ")
}

// find resolves a catalog id or name.
func find(catalog []models.Platform, ref string) (models.Platform, error) {
	ref = strings.TrimSpace(ref)
	for _, p := range catalog {
		if p.ID == ref || strings.EqualFold(p.Name, ref) {
			return p, nil
		}
	}
	names := make([]string, len(catalog))
	for i, p := range catalog {
		names[i] = p.Name
	}
	return models.Platform{}, &InvalidError{Reason: fmt.Sprintf("%q isn't a platform Ogen publishes to.", ref), Alternatives: names}
}

// checkTypes trims and dedupes types, refusing one the platform lacks.
func checkTypes(p models.Platform, types []string) ([]string, error) {
	var out []string
	for _, t := range types {
		t = strings.TrimSpace(t)
		if _, ok := p.PostTypes[t]; !ok {
			return nil, &InvalidError{
				Reason:       fmt.Sprintf("%s has no %q post type.", p.Name, t),
				Alternatives: slices.Sorted(maps.Keys(p.PostTypes)),
				PostType:     t,
			}
		}
		if !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	return out, nil
}
