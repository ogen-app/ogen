package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/kernel/config"
)

const testUIOrigin = "https://app.example.com"

// preflight sends a CORS preflight through the real middleware stack.
func preflight(t *testing.T, app *fiber.App, path, origin, method, headers string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(fiber.MethodOptions, path, nil)
	req.Header.Set(fiber.HeaderOrigin, origin)
	req.Header.Set(fiber.HeaderAccessControlRequestMethod, method)
	if headers != "" {
		req.Header.Set(fiber.HeaderAccessControlRequestHeaders, headers)
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestPluginRoutesAnswerNullOriginPreflight(t *testing.T) {
	for _, uiOrigins := range []string{testUIOrigin, ""} {
		t.Run("ui origins="+uiOrigins, func(t *testing.T) {
			app := newFiberApp(&config.Config{CORSAllowedOrigins: uiOrigins})
			resp := preflight(t, app, "/api/plugins/figma/images", "null", fiber.MethodPost, "authorization,content-type")

			if resp.StatusCode != fiber.StatusNoContent {
				t.Fatalf("status = %d, want 204", resp.StatusCode)
			}
			if got := resp.Header.Get(fiber.HeaderAccessControlAllowOrigin); got != "*" {
				t.Fatalf("allow-origin = %q, want *", got)
			}
			if got := resp.Header.Get(fiber.HeaderAccessControlAllowCredentials); got != "" {
				t.Fatalf("plugin routes must not allow credentials, got %q", got)
			}
			if got := resp.Header.Get(fiber.HeaderAccessControlAllowHeaders); got != "Authorization,Content-Type" {
				t.Fatalf("allow-headers = %q", got)
			}
		})
	}
}

func TestUICORSUnchangedOutsidePluginRoutes(t *testing.T) {
	app := newFiberApp(&config.Config{CORSAllowedOrigins: testUIOrigin})

	resp := preflight(t, app, "/api/posts", testUIOrigin, fiber.MethodPut, "content-type")
	if got := resp.Header.Get(fiber.HeaderAccessControlAllowOrigin); got != testUIOrigin {
		t.Fatalf("allow-origin = %q, want the UI origin", got)
	}
	if got := resp.Header.Get(fiber.HeaderAccessControlAllowCredentials); got != "true" {
		t.Fatalf("allow-credentials = %q, want true", got)
	}

	// The wildcard is confined to the plugin prefix: a lookalike path and a
	// null origin elsewhere get nothing.
	for _, path := range []string{"/api/posts", "/api/pluginsx/figma"} {
		resp = preflight(t, app, path, "null", fiber.MethodGet, "")
		if got := resp.Header.Get(fiber.HeaderAccessControlAllowOrigin); got != "" {
			t.Fatalf("%s: null origin got allow-origin %q", path, got)
		}
	}
}
