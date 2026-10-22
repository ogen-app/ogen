package brandsource_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/usecase/brandsource"
)

type fakeTags struct {
	calls []string
	err   error
}

func (f *fakeTags) FindOrCreateByName(_ context.Context, name, _, createdBy string) (*models.Tag, error) {
	f.calls = append(f.calls, name+"/"+createdBy)
	if f.err != nil {
		return nil, f.err
	}
	return &models.Tag{ID: "tag-brand", Name: name}, nil
}

func TestParsePurpose(t *testing.T) {
	for _, tt := range []struct {
		in      string
		want    bool
		wantErr bool
	}{
		{"", false, false},
		{"brand_source", true, false},
		{"Brand_Source", false, true},
		{"anything", false, true},
	} {
		got, err := brandsource.ParsePurpose(tt.in)
		if got != tt.want || (err != nil) != tt.wantErr {
			t.Errorf("ParsePurpose(%q) = %v, %v", tt.in, got, err)
		}
	}
}

func TestMarkStampsOriginAndTagOnce(t *testing.T) {
	tags := &fakeTags{}
	m := &brandsource.Marker{Tags: tags}
	mark, err := m.Prepare(t.Context(), models.AssetRoleDocument, "", "user-1")
	if err != nil {
		t.Fatal(err)
	}
	a := &models.Asset{TagIDs: models.StringSlice{"tag-own", "tag-brand"}}
	mark.Apply(a)
	if !brandsource.IsSource(a) || a.OriginRef == nil || a.OriginRef.Role != models.AssetRoleDocument {
		t.Fatalf("asset = %+v / %+v", a.Origin, a.OriginRef)
	}
	if len(a.TagIDs) != 2 {
		t.Fatalf("tags = %v, want the brand tag once", a.TagIDs)
	}
	if len(tags.calls) != 1 || tags.calls[0] != brandsource.TagName+"/user-1" {
		t.Fatalf("tag lookups = %v", tags.calls)
	}
}

func TestMarkWithoutTagStore(t *testing.T) {
	var m *brandsource.Marker
	mark, err := m.Prepare(t.Context(), models.AssetRoleWebsite, "run-1", "user-1")
	if err != nil {
		t.Fatal(err)
	}
	a := &models.Asset{TagIDs: models.StringSlice{}}
	mark.Apply(a)
	if a.Origin != models.AssetOriginBrandDistill || a.OriginRef.RunID != "run-1" || len(a.TagIDs) != 0 {
		t.Fatalf("asset = %+v %+v %v", a.Origin, a.OriginRef, a.TagIDs)
	}
}

func TestMarkTagFailure(t *testing.T) {
	m := &brandsource.Marker{Tags: &fakeTags{err: errors.New("db down")}}
	if _, err := m.Prepare(t.Context(), models.AssetRoleDocument, "", "user-1"); err == nil {
		t.Fatal("want the tag error")
	}
}
