package handlers

import (
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/usecase/activity/report"
)

// passThroughAuth is a no-op auth middleware for tests that only exercise
// request validation happening before any tenant-scoped read.
func passThroughAuth(c *fiber.Ctx) error { return c.Next() }

// TestActivityHandler_Validation covers the failure paths that resolve before
// the service touches the database (invalid tz/date/limit, and the 503 when the
// service is disabled), so they run without a DB. The happy paths and
// tenant/campaign scoping are covered by the report package's pure tests and the
// DB-backed suite (make test).
func TestActivityHandler_Validation(t *testing.T) {
	app := fiber.New()
	// report.New with nil repos is safe here: every case below returns before a
	// repository is consulted.
	handler := NewActivityHandler(report.New(nil, nil, nil), passThroughAuth)
	handler.Register(app)

	cases := []struct {
		name   string
		method string
		path   string
		want   int
	}{
		{"report missing tz", "GET", "/api/activity/report/2026-08-18", fiber.StatusBadRequest},
		{"report bad tz", "GET", "/api/activity/report/2026-08-18?tz=Not/AZone", fiber.StatusBadRequest},
		{"report bad date", "GET", "/api/activity/report/nope?tz=UTC", fiber.StatusBadRequest},
		{"reports missing tz", "GET", "/api/activity/reports", fiber.StatusBadRequest},
		{"reports over-max limit", "GET", "/api/activity/reports?tz=UTC&limit=999", fiber.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := app.Test(httptest.NewRequest(tc.method, tc.path, nil))
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			if resp.StatusCode != tc.want {
				t.Fatalf("%s %s = %d, want %d", tc.method, tc.path, resp.StatusCode, tc.want)
			}
		})
	}
}

// TestActivityHandler_Disabled returns 503 on both endpoints when the service is
// nil (mirrors the other read handlers' nil-disable convention).
func TestActivityHandler_Disabled(t *testing.T) {
	app := fiber.New()
	NewActivityHandler(nil, passThroughAuth).Register(app)

	for _, path := range []string{"/api/activity/report/2026-08-18?tz=UTC", "/api/activity/reports?tz=UTC"} {
		resp, err := app.Test(httptest.NewRequest("GET", path, nil))
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		if resp.StatusCode != fiber.StatusServiceUnavailable {
			t.Fatalf("GET %s = %d, want 503", path, resp.StatusCode)
		}
	}
}
