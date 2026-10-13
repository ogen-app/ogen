// Package pubdate reads when a source was published from its metadata: a web
// page's published-time meta tag, or a PDF's creation date.
package pubdate

import (
	"regexp"
	"slices"
	"strings"
	"time"
)

// earliest is the oldest date taken as a publication date; anything before it
// is a zeroed or broken metadata field.
var earliest = time.Date(1950, 1, 1, 0, 0, 0, 0, time.UTC)

// plausible reports whether t can be a publication date seen at now.
func plausible(t, now time.Time) bool {
	return !t.Before(earliest) && !t.After(now.Add(24*time.Hour))
}

var webLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05Z0700",
	"2006-01-02T15:04:05",
	"2006-01-02T15:04",
	"2006-01-02 15:04:05",
	"2006-01-02",
	time.RFC1123Z,
	time.RFC1123,
}

// Web parses a page's published time (article:published_time and the like),
// the first of values that reads as a plausible date at now.
func Web(now time.Time, values ...string) (time.Time, bool) {
	for _, v := range values {
		v = strings.TrimSpace(v)
		for _, layout := range webLayouts {
			if t, err := time.Parse(layout, v); err == nil && plausible(t, now) {
				return t.UTC(), true
			}
		}
	}
	return time.Time{}, false
}

var (
	// infoCreation is the Info dictionary's /CreationDate (D:YYYYMMDDHHmmSS
	// then an optional offset), its seconds and offset optional.
	infoCreation = regexp.MustCompile(`/CreationDate\s*\(D:(\d{4})(\d{2})?(\d{2})?(\d{2})?(\d{2})?(\d{2})?([Zz+\-])?(\d{2})?'?(\d{2})?`)
	// xmpCreate is XMP's xmp:CreateDate as an element or an attribute.
	xmpCreate = regexp.MustCompile(`xmp:CreateDate(?:>|\s*=\s*["'])\s*([0-9T:.+\-Z]+)`)
)

// PDF finds a PDF's creation date in its raw bytes: the Info dictionary's
// /CreationDate, else the XMP packet's xmp:CreateDate. Either can sit in a
// compressed object stream, where it is not found.
func PDF(raw []byte, now time.Time) (time.Time, bool) {
	// An incremental update appends a newer Info dictionary, so the last
	// plausible date wins.
	matches := infoCreation.FindAllSubmatch(raw, -1)
	for _, m := range slices.Backward(matches) {
		if t, ok := pdfDate(m); ok && plausible(t, now) {
			return t, true
		}
	}
	if m := xmpCreate.FindSubmatch(raw); m != nil {
		return Web(now, string(m[1]))
	}
	return time.Time{}, false
}

// pdfDate builds the time of an infoCreation match. Missing parts default to
// their lowest value and a missing offset to UTC.
func pdfDate(m [][]byte) (time.Time, bool) {
	num := func(i, def int) int {
		if len(m[i]) == 0 {
			return def
		}
		n := 0
		for _, c := range m[i] {
			n = n*10 + int(c-'0')
		}
		return n
	}
	month, day := num(2, 1), num(3, 1)
	if month < 1 || month > 12 || day < 1 || day > 31 {
		return time.Time{}, false
	}
	loc := time.UTC
	if sign := string(m[7]); sign == "+" || sign == "-" {
		offset := num(8, 0)*3600 + num(9, 0)*60
		if sign == "-" {
			offset = -offset
		}
		loc = time.FixedZone("", offset)
	}
	year, hour, minute, sec := num(1, 0), num(4, 0), num(5, 0), num(6, 0)
	t := time.Date(year, time.Month(month), day, hour, minute, sec, 0, loc)
	// time.Date normalises out-of-range parts (Feb 31 becomes March); such a
	// date is not the one the file holds.
	if t.Year() != year || int(t.Month()) != month || t.Day() != day ||
		t.Hour() != hour || t.Minute() != minute || t.Second() != sec {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// Sniffer keeps the head and the tail of a stream written through it, where a
// PDF keeps its metadata, without holding the whole file.
type Sniffer struct {
	head, tail []byte
	limit      int
}

// NewSniffer keeps up to limit bytes of each end.
func NewSniffer(limit int) *Sniffer { return &Sniffer{limit: limit} }

func (s *Sniffer) Write(p []byte) (int, error) {
	n := len(p)
	if room := s.limit - len(s.head); room > 0 {
		k := min(room, len(p))
		s.head = append(s.head, p[:k]...)
		p = p[k:]
	}
	if len(p) > 0 {
		// The tail grows to twice the limit before it drops its oldest bytes,
		// so each byte is copied a bounded number of times.
		s.tail = append(s.tail, p...)
		if over := len(s.tail) - s.limit; over > s.limit {
			s.tail = append(s.tail[:0], s.tail[over:]...)
		}
	}
	return n, nil
}

// PDF reads the creation date from the kept bytes, the tail first: an
// incremental update appends a newer Info dictionary there.
func (s *Sniffer) PDF(now time.Time) (time.Time, bool) {
	tail := s.tail
	if over := len(tail) - s.limit; over > 0 {
		tail = tail[over:]
	}
	if t, ok := PDF(tail, now); ok {
		return t, true
	}
	return PDF(s.head, now)
}
