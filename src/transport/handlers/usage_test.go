package handlers

import (
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
)

// TestRequireOperator verifies the PUT /api/usage/limits operator gate:
// it fails closed when no token is configured, rejects a wrong or
// missing X-Admin-Token, and only lets a matching token through — so tenant
// users can never raise their own caps or disable enforcement.
func TestRequireOperator(t *testing.T) {
	newApp := func(token string) *fiber.App {
		app := fiber.New()
		h := &UsageHandler{adminToken: token}
		app.Put("/limits", h.requireOperator, func(c *fiber.Ctx) error {
			return c.SendStatus(fiber.StatusOK)
		})
		return app
	}

	do := func(t *testing.T, app *fiber.App, header string) int {
		t.Helper()
		req := httptest.NewRequest("PUT", "/limits", nil)
		if header != "" {
			req.Header.Set("X-Admin-Token", header)
		}
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		return resp.StatusCode
	}

	cases := []struct {
		name   string
		token  string
		header string
		want   int
	}{
		{"no token configured fails closed", "", "anything", fiber.StatusForbidden},
		{"missing header rejected", "secret", "", fiber.StatusForbidden},
		{"wrong token rejected", "secret", "wrong", fiber.StatusForbidden},
		{"correct token passes", "secret", "secret", fiber.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := do(t, newApp(tc.token), tc.header); got != tc.want {
				t.Fatalf("status = %d, want %d", got, tc.want)
			}
		})
	}
}
