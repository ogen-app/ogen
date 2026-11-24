package handlers

import (
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
)

func TestConnectRedirectsFollowReturnTo(t *testing.T) {
	h := &ZernioHandler{appBaseURL: "https://app.example.com/"}
	cases := []struct {
		name     string
		redirect func(c *fiber.Ctx) error
		want     string
	}{
		{"settings success", func(c *fiber.Ctx) error {
			return h.redirectConnectSuccess(c, models.ZernioConnectReturnSettings, "linkedin")
		},
			"https://app.example.com/workspace-settings?connected=linkedin"},
		{"onboarding success", func(c *fiber.Ctx) error {
			return h.redirectConnectSuccess(c, models.ZernioConnectReturnOnboarding, "linkedin")
		},
			"https://app.example.com/onboarding?connected=linkedin"},
		{"onboarding error", func(c *fiber.Ctx) error {
			return h.redirectConnectError(c, models.ZernioConnectReturnOnboarding, "facebook", "permission")
		}, "https://app.example.com/onboarding?connect_error=permission&platform=facebook"},
		{"settings picker", func(c *fiber.Ctx) error { return h.redirectPicker(c, models.ZernioConnectReturnSettings, "cn1") },
			"https://app.example.com/workspace-settings/connect/cn1"},
		{"onboarding picker", func(c *fiber.Ctx) error { return h.redirectPicker(c, models.ZernioConnectReturnOnboarding, "cn1") },
			"https://app.example.com/workspace-settings/connect/cn1?returnTo=onboarding"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := fiber.New()
			app.Get("/", tc.redirect)
			resp, err := app.Test(httptest.NewRequest("GET", "/", nil))
			if err != nil {
				t.Fatal(err)
			}
			if got := resp.Header.Get("Location"); got != tc.want {
				t.Fatalf("Location = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestValidZernioConnectReturn(t *testing.T) {
	for r, want := range map[string]bool{"": true, "onboarding": true, "/evil": false, "https://x": false} {
		if got := models.ValidZernioConnectReturn(r); got != want {
			t.Errorf("ValidZernioConnectReturn(%q) = %v", r, got)
		}
	}
}
