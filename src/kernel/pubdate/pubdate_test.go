package pubdate

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

func TestWeb(t *testing.T) {
	cases := []struct {
		in   []string
		want time.Time
		ok   bool
	}{
		{[]string{"2025-08-14T09:30:00+02:00"}, time.Date(2025, 8, 14, 7, 30, 0, 0, time.UTC), true},
		{[]string{"", "2024-02-29"}, time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC), true},
		{[]string{"2023-05-01T10:00:00"}, time.Date(2023, 5, 1, 10, 0, 0, 0, time.UTC), true},
		{[]string{"yesterday", "Mon, 02 Jan 2023 15:04:05 -0700"}, time.Date(2023, 1, 2, 22, 4, 5, 0, time.UTC), true},
		{[]string{"2031-01-01"}, time.Time{}, false},
		{[]string{"0001-01-01T00:00:00Z"}, time.Time{}, false},
		{nil, time.Time{}, false},
	}
	for _, tc := range cases {
		got, ok := Web(now, tc.in...)
		if ok != tc.ok || !got.Equal(tc.want) {
			t.Errorf("Web(%q) = %v, %v; want %v, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestPDF(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want time.Time
		ok   bool
	}{
		{"info with offset", "<< /Producer (x) /CreationDate (D:20240315103000+02'00') >>",
			time.Date(2024, 3, 15, 8, 30, 0, 0, time.UTC), true},
		{"info date only", "/CreationDate(D:2019)", time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC), true},
		{"info negative offset", "/CreationDate (D:20200101000000-05'00)", time.Date(2020, 1, 1, 5, 0, 0, 0, time.UTC), true},
		{"xmp element", "<xmp:CreateDate>2022-11-02T08:00:00Z</xmp:CreateDate>", time.Date(2022, 11, 2, 8, 0, 0, 0, time.UTC), true},
		{"xmp attribute", `<rdf:Description xmp:CreateDate="2021-06-30T12:00:00+00:00">`, time.Date(2021, 6, 30, 12, 0, 0, 0, time.UTC), true},
		{"bad month falls back to xmp", "/CreationDate (D:20241399) <xmp:CreateDate>2020-02-02</xmp:CreateDate>",
			time.Date(2020, 2, 2, 0, 0, 0, 0, time.UTC), true},
		{"none", "%PDF-1.7 stream compressed", time.Time{}, false},
	}
	for _, tc := range cases {
		got, ok := PDF([]byte(tc.raw), now)
		if ok != tc.ok || !got.Equal(tc.want) {
			t.Errorf("%s: got %v, %v; want %v, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

func TestSniffer(t *testing.T) {
	head := "%PDF-1.4 /CreationDate (D:20100101000000Z)"
	tail := "trailer << /Info << /CreationDate (D:20230707000000Z) >> >>"
	body := head + strings.Repeat("x", 1<<20) + tail
	s := NewSniffer(4 << 10)
	// Small writes exercise the tail's compaction.
	if _, err := io.CopyBuffer(s, strings.NewReader(body), make([]byte, 1000)); err != nil {
		t.Fatal(err)
	}
	if len(s.head) != 4<<10 || len(s.tail) > 8<<10 {
		t.Fatalf("kept %d head and %d tail bytes", len(s.head), len(s.tail))
	}
	if got, ok := s.PDF(now); !ok || !got.Equal(time.Date(2023, 7, 7, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("got %v, %v; want the trailer's date", got, ok)
	}

	headOnly := NewSniffer(4 << 10)
	_, _ = io.Copy(headOnly, bytes.NewReader([]byte(head+strings.Repeat("x", 1<<16))))
	if got, ok := headOnly.PDF(now); !ok || got.Year() != 2010 {
		t.Errorf("got %v, %v; want the head's date", got, ok)
	}
}
