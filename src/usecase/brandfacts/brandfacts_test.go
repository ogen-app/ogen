package brandfacts

import (
	"errors"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
)

func day(s string) *models.CalendarDate {
	d, err := models.ParseCalendarDate(s)
	if err != nil {
		panic(err)
	}
	return &d
}

// A renewal is checked today and keeps the span the fact had, counted from
// when it was checked, else added; a given date wins; no span is a rule error.
func TestRenewal(t *testing.T) {
	s := &Service{now: func() time.Time { return time.Date(2026, 11, 12, 15, 0, 0, 0, time.UTC) }}
	cases := []struct {
		name      string
		fact      models.BrandFact
		expiresAt *models.CalendarDate
		want      string
	}{
		{"from checked", models.BrandFact{AddedAt: day("2025-01-01"), CheckedAt: day("2026-03-01"), ExpiresAt: day("2026-05-31")}, nil, "2027-02-11"},
		{"from added", models.BrandFact{AddedAt: day("2026-01-01"), ExpiresAt: day("2026-01-31")}, nil, "2026-12-12"},
		{"date given", models.BrandFact{}, day("2027-01-31"), "2027-01-31"},
	}
	for _, c := range cases {
		in, err := s.Renewal(&c.fact, c.expiresAt)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if in.CheckedAt.String() != "2026-11-12" || in.ExpiresAt.String() != c.want {
			t.Fatalf("%s: checked %v, expires %v; want 2026-11-12, %s", c.name, in.CheckedAt, in.ExpiresAt, c.want)
		}
	}
	if _, err := s.Renewal(&models.BrandFact{}, day("2026-11-11")); !errors.Is(err, ErrRenewalInPast) {
		t.Fatalf("renewal until yesterday: err = %v, want ErrRenewalInPast", err)
	}
	if _, err := s.Renewal(&models.BrandFact{}, day("2026-11-12")); err != nil {
		t.Fatalf("renewal until today: %v", err)
	}
	for _, f := range []models.BrandFact{{}, {ExpiresAt: day("2026-05-31")}, {CheckedAt: day("2026-05-31"), ExpiresAt: day("2026-05-31")}} {
		if _, err := s.Renewal(&f, nil); !IsRule(err) || !errors.Is(err, ErrNoRenewalSpan) {
			t.Fatalf("renewal of %+v: err = %v, want ErrNoRenewalSpan", f, err)
		}
	}
}
