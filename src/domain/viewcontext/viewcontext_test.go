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
		"window": {"from": "2026-04-01", "to": "2026-04-30", "platforms": ["pl1"]},
		"visible": {"kind": "idea", "ids": ["i1", "i2", "i3"]},
		"futureField": "ignored"
	}`
	v, err := Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if v.Screen != "campaign.ideas" || v.Scope.ID != "c1" || !slices.Equal(v.Selection.IDs, []string{"i1", "i2"}) {
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

func TestParse_AllMatching(t *testing.T) {
	v, err := Parse([]byte(`{"version":1,"screen":"ideas","scope":{"kind":"workspace"},"selection":{"kind":"idea","allMatching":true,"count":412},"filter":"lens:pricing"}`))
	if err != nil || !v.Selection.AllMatching {
		t.Fatalf("v = %+v, err = %v", v, err)
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
