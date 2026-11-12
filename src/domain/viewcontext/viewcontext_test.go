package viewcontext

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestParse_Valid(t *testing.T) {
	raw := `{
		"version": 1,
		"screen": "campaign.ideas",
		"scope": {"kind": "campaign", "id": "c1"},
		"selection": {"kind": "idea", "ids": ["i1", "i2", "i1"], "count": 2},
		"filter": "verdict:undecided fit>=70",
		"window": {"from": "2026-04-01", "to": "2026-04-30", "platforms": ["pl1"], "preset": "30d"},
		"visible": {"kind": "idea", "ids": ["i1", "i2", "i3"]},
		"futureField": "ignored"
	}`
	v, err := Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if v.Screen != "campaign.ideas" || v.Scope.ID != "c1" || !slices.Equal(v.Selection.IDs, []string{"i1", "i2"}) || v.Window.Preset != "30d" {
		t.Fatalf("parsed = %+v", v)
	}
	m, err := v.Stored()
	if err != nil {
		t.Fatal(err)
	}
	if m["screen"] != "campaign.ideas" || m["version"] != float64(1) {
		t.Fatalf("stored = %v", m)
	}
}

// A window's changes name the period, the platforms as a set and the
// campaign; from no window, whatever is set changed.
func TestWindowChanges(t *testing.T) {
	before := &Window{From: "2026-03-01", To: "2026-03-31", Platforms: []string{"li", "ig"}}
	if got := (Window{From: "2026-03-01", To: "2026-03-31", Platforms: []string{"ig", "li"}}).Changes(before); got != nil {
		t.Fatalf("same = %v", got)
	}
	if got := (Window{From: "2026-03-01", To: "2026-03-31", Preset: "30d", CampaignID: "c1"}).Changes(before); !slices.Equal(got, []string{WindowPeriod, WindowPlatforms, WindowCampaign}) {
		t.Fatalf("all = %v", got)
	}
	if got := (Window{Platforms: []string{"li"}}).Changes(nil); !slices.Equal(got, []string{WindowPlatforms}) {
		t.Fatalf("from none = %v", got)
	}
	c := before.Clone()
	c.Platforms[0] = "x"
	if before.Platforms[0] != "li" {
		t.Fatal("the clone shares platforms")
	}
}

// A preset keeps one form: no leading zeros, and only what ValidPreset
// accepts.
func TestCanonicalPreset(t *testing.T) {
	for in, want := range map[string]string{"30d": "30d", "0007d": "7d", "012w": "12w", "6mo": "6mo", "0d": "", "1000d": "", "30 days": "", "": ""} {
		if got := CanonicalPreset(in); got != want {
			t.Errorf("%q = %q, want %q", in, got, want)
		}
	}
}

func TestParse_AllMatching(t *testing.T) {
	v, err := Parse([]byte(`{"version":1,"screen":"ideas","scope":{"kind":"workspace"},"selection":{"kind":"idea","allMatching":true,"count":412},"filter":"lens:pricing"}`))
	if err != nil || !v.Selection.AllMatching {
		t.Fatalf("v = %+v, err = %v", v, err)
	}
}

func TestScreenKinds(t *testing.T) {
	for _, s := range Screens() {
		if !s.Valid() {
			t.Fatalf("%s is listed but not valid", s)
		}
		if k, ok := s.DetailKind(); ok && !slices.Contains(kinds, k) {
			t.Fatalf("%s shows unknown kind %s", s, k)
		}
		if k, ok := s.ListKind(); ok && k != KindIdea && k != KindAsset && k != KindPost {
			t.Fatalf("%s lists %s, which has no filter syntax", s, k)
		}
	}
	for s := range detailKinds {
		if !s.Valid() {
			t.Fatalf("detail screen %s is not a screen", s)
		}
	}
	for s, k := range listKinds {
		if !s.Valid() {
			t.Fatalf("list screen %s is not a screen", s)
		}
		// Every filterable list can be ticked too.
		if sk, ok := s.SelectKind(); !ok || sk != k {
			t.Fatalf("list screen %s can't select its %s", s, k)
		}
	}
	for s := range selectKinds {
		if !s.Valid() {
			t.Fatalf("select screen %s is not a screen", s)
		}
	}
	if !Screen("campaign.ideas").InCampaign() || Screen("ideas").InCampaign() || Screen("billing").Valid() {
		t.Fatal("screen predicates")
	}
	if k, ok := Screen("analytics.post").DetailKind(); !ok || k != KindPost {
		t.Fatal("analytics.post shows a post")
	}
}

func TestParse_Invalid(t *testing.T) {
	ids := func(n int) string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("%q", fmt.Sprint("i", i))
		}
		return strings.Join(out, ",")
	}
	long := strings.Repeat("x", MaxIDLen+1)
	const base = `{"version":1,"screen":"ideas","scope":{"kind":"workspace"},`
	cases := map[string]struct {
		raw   string
		field string
	}{
		"not json":            {`nope`, ""},
		"old version":         {`{"version":0,"screen":"ideas","scope":{"kind":"workspace"}}`, "version"},
		"unknown screen":      {`{"version":1,"screen":"billing","scope":{"kind":"workspace"}}`, "screen"},
		"workspace with id":   {`{"version":1,"screen":"ideas","scope":{"kind":"workspace","id":"x"}}`, "scope.id"},
		"campaign without id": {`{"version":1,"screen":"campaign.list","scope":{"kind":"campaign"}}`, "scope.id"},
		"unknown scope":       {`{"version":1,"screen":"ideas","scope":{"kind":"team"}}`, "scope.kind"},
		"focus without id":    {`{"version":1,"screen":"asset","scope":{"kind":"workspace"},"focus":{"kind":"asset"}}`, "focus.id"},
		"empty selection":     {`{"version":1,"screen":"ideas","scope":{"kind":"workspace"},"selection":{"kind":"idea"}}`, "selection"},
		"ids and all":         {`{"version":1,"screen":"ideas","scope":{"kind":"workspace"},"selection":{"kind":"idea","ids":["a"],"allMatching":true}}`, "selection"},
		"too many ids": {`{"version":1,"screen":"ideas","scope":{"kind":"workspace"},"selection":{"kind":"idea","ids":[` + ids(MaxSelectionIDs+1) + `]}}`,
			"selection.ids"},
		"blank id":       {`{"version":1,"screen":"ideas","scope":{"kind":"workspace"},"selection":{"kind":"idea","ids":[""]}}`, "selection.ids[0]"},
		"bad date":       {`{"version":1,"screen":"analytics","scope":{"kind":"workspace"},"window":{"from":"April"}}`, "window.from"},
		"window reverse": {`{"version":1,"screen":"analytics","scope":{"kind":"workspace"},"window":{"from":"2026-05-01","to":"2026-04-01"}}`, "window"},
		"long filter":    {`{"version":1,"screen":"ideas","scope":{"kind":"workspace"},"filter":"` + strings.Repeat("x", MaxFilterLen+1) + `"}`, "filter"},

		"long scope id":          {`{"version":1,"screen":"campaign.list","scope":{"kind":"campaign","id":"` + long + `"}}`, "scope.id"},
		"unknown focus kind":     {base + `"focus":{"kind":"task","id":"t1"}}`, "focus.kind"},
		"long focus id":          {base + `"focus":{"kind":"idea","id":"` + long + `"}}`, "focus.id"},
		"unknown selection kind": {base + `"selection":{"kind":"task","ids":["t1"]}}`, "selection.kind"},
		"long selection id":      {base + `"selection":{"kind":"idea","ids":["a","` + long + `"]}}`, "selection.ids[1]"},
		"long saved view":        {base + `"savedViewId":"` + long + `"}`, "savedViewId"},
		"long sort":              {base + `"sort":"` + long + `"}`, "sort"},
		"bad to date":            {base + `"window":{"to":"2026-13-01"}}`, "window.to"},
		"too many platforms":     {base + `"window":{"platforms":[` + ids(MaxPlatforms+1) + `]}}`, "window.platforms"},
		"blank platform":         {base + `"window":{"platforms":["pl1",""]}}`, "window.platforms[1]"},
		"bad preset":             {base + `"window":{"preset":"30 days"}}`, "window.preset"},
		"long preset":            {base + `"window":{"preset":"3000d"}}`, "window.preset"},
		"unknown visible kind":   {base + `"visible":{"kind":"task","ids":["t1"]}}`, "visible.kind"},
		"too many visible ids":   {base + `"visible":{"kind":"idea","ids":[` + ids(MaxVisibleIDs+1) + `]}}`, "visible.ids"},
		"blank visible id":       {base + `"visible":{"kind":"idea","ids":[""]}}`, "visible.ids[0]"},
		"long visible id":        {base + `"visible":{"kind":"idea","ids":["` + long + `"]}}`, "visible.ids[0]"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(tc.raw))
			ie, ok := errors.AsType[*InvalidError](err)
			if !ok || ie.Field != tc.field {
				t.Fatalf("err = %v, want field %q", err, tc.field)
			}
		})
	}
}
