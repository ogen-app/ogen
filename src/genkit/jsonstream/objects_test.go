package jsonstream

import (
	"slices"
	"testing"
)

func TestObjectSplitter(t *testing.T) {
	tests := []struct {
		name   string
		chunks []string
		want   []string
	}{
		{"single object in one chunk", []string{`[{"title":"Hello"}]`}, []string{`{"title":"Hello"}`}},
		{"multiple objects in one chunk", []string{`[{"a":"1"},{"b":"2"},{"c":"3"}]`}, []string{`{"a":"1"}`, `{"b":"2"}`, `{"c":"3"}`}},
		{"object split across two chunks", []string{`[{"title":"He`, `llo"}]`}, []string{`{"title":"Hello"}`}},
		{"object split across many chunks", []string{`[`, `{`, `"k"`, `:"v"`, `}`, `]`}, []string{`{"k":"v"}`}},
		{"two objects split mid-boundary", []string{`[{"a":"1"},{`, `"b":"2"}]`}, []string{`{"a":"1"}`, `{"b":"2"}`}},
		{"braces inside string values are ignored", []string{`[{"body":"use {braces} freely"}]`}, []string{`{"body":"use {braces} freely"}`}},
		{"escaped quote inside string does not end string", []string{`[{"body":"say \"hi\""}]`}, []string{`{"body":"say \"hi\""}`}},
		{"escape split across chunks", []string{`[{"body":"a\`, `"b"}]`}, []string{`{"body":"a\"b"}`}},
		{"nested object inside field", []string{`[{"meta":{"k":"v"},"title":"T"}]`}, []string{`{"meta":{"k":"v"},"title":"T"}`}},
		{"empty array", []string{`[]`}, nil},
		{"whitespace between objects", []string{"[\n  {\"a\":\"1\"},\n  {\"b\":\"2\"}\n]"}, []string{`{"a":"1"}`, `{"b":"2"}`}},
		{"markdown fence prefix is ignored", []string{"```json\n[{\"a\":\"1\"}]\n```"}, []string{`{"a":"1"}`}},
		{
			"awkward boundaries with braces in strings",
			[]string{`[{"title":"a","content":"hello `, `{world}"},`, `{"title":"b",`, `"content":"line1\nline2"}]`},
			[]string{`{"title":"a","content":"hello {world}"}`, `{"title":"b","content":"line1\nline2"}`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewObjectSplitter()
			var got []string
			for _, chunk := range tt.chunks {
				got = append(got, s.Push(chunk)...)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStripFences(t *testing.T) {
	cases := []struct{ in, want string }{
		{"[{\"a\":1}]", `[{"a":1}]`},
		{"```json\n[{\"a\":1}]\n```", `[{"a":1}]`},
		{"```\n[1,2]\n```", `[1,2]`},
		{"  [1]  ", `[1]`},
		{"```[1]```", "```[1]"},
	}
	for _, c := range cases {
		if got := StripFences(c.in); got != c.want {
			t.Errorf("StripFences(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// uesc builds a JSON \uXXXX escape without writing one literally in source.
func uesc(hex string) string { return string([]byte{'\\', 'u'}) + hex }

func TestScanner_EscapeTable(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"simple escapes", `\"\\\/\n\t\r\b\f`, "\"\\/\n\t\r\b\f"},
		{"unknown escape kept verbatim", `a\qb`, "aqb"},
		{"bmp unicode", uesc("00e9"), "\U000000E9"},
		{"surrogate pair", uesc("d83d") + uesc("de00"), "\U0001F600"},
		{"lone low surrogate dropped", "a" + uesc("de00") + "b", "ab"},
		{"dangling high surrogate emitted before next rune", uesc("d83d") + uesc("0041"), "\U0000FFFDA"},
		{"invalid hex ignored", "a" + uesc("zzzz") + "b", "ab"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, caps := collect("k")
			s.Push(`{"k":"` + tt.raw + `"}`)
			if got := joined(*caps, "k"); got != tt.want {
				t.Errorf("streamed = %q, want %q", got, tt.want)
			}
			if got, _ := s.Values()["k"].(string); got != tt.want {
				t.Errorf("Values[k] = %q, want %q", got, tt.want)
			}
		})
	}
}
