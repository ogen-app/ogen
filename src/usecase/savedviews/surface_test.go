package savedviews

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/listquery"
	"github.com/ogen-app/ogen/src/domain/models"
)

// fakeSurface takes kind (a list) and text (a single value), drops page.
type fakeSurface struct{}

func (fakeSurface) Name() string                              { return "fake" }
func (fakeSurface) Version() int                              { return 2 }
func (fakeSurface) Params() map[string]bool                   { return map[string]bool{"kind": true, "text": false} }
func (fakeSurface) Volatile() []string                        { return []string{"page"} }
func (fakeSurface) Counts() []string                          { return []string{"total"} }
func (fakeSurface) Upgrade(from int, v url.Values) url.Values { return renameKinds(from, v) }
func (fakeSurface) Validate(v url.Values) error {
	for _, k := range v["kind"] {
		if k == "bad" {
			return listquery.Invalid("kind", "unknown value %q", k)
		}
	}
	return nil
}

func (fakeSurface) Count(context.Context, []url.Values, time.Time, *time.Location) ([]map[string]int, error) {
	return nil, nil
}

// renameKinds is version 1's upgrade: the parameter was called type.
func renameKinds(from int, v url.Values) url.Values {
	if from < 2 && v.Has("type") {
		v["kind"] = v["type"]
		v.Del("type")
	}
	return v
}

func TestCanonicalSortsSplitsAndDrops(t *testing.T) {
	for raw, want := range map[string]string{
		"kind=web,pdf&kind=pdf":         "kind=pdf,web",
		"text=+a,+b+&page=3&kind=":      "text=a%2C+b",
		"kind=+audio+,,&text=":          "kind=audio",
		"":                              "",
		"text=x&text=last&kind=z%26y,a": "kind=a,z%26y&text=last",
	} {
		_, got, err := canonical(fakeSurface{}, raw)
		if err != nil || got != want {
			t.Errorf("canonical(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
}

func TestCanonicalRejectsUnknownParameters(t *testing.T) {
	_, _, err := canonical(fakeSurface{}, "kind=pdf&colour=red")
	qe, ok := errors.AsType[*QueryError](err)
	if !ok || qe.Param != "colour" {
		t.Errorf("err = %v, want a QueryError naming colour", err)
	}
	if _, err := prepare(fakeSurface{}, "kind=bad"); err == nil {
		t.Error("prepare accepted a value the surface rejects")
	} else if qe, ok := errors.AsType[*QueryError](err); !ok || qe.Param != "kind" {
		t.Errorf("err = %v, want a QueryError naming kind", err)
	}
}

func TestStoredUpgradesOlderQueries(t *testing.T) {
	vals, ok := stored(fakeSurface{}, &models.SavedView{Query: "type=pdf", QueryVersion: 1})
	if !ok || vals.Get("kind") != "pdf" || vals.Has("type") {
		t.Errorf("stored = %v, %v; want type renamed to kind", vals, ok)
	}
	if _, ok := stored(fakeSurface{}, &models.SavedView{Query: "kind=bad", QueryVersion: 2}); ok {
		t.Error("a query the surface rejects reads back valid")
	}
}
