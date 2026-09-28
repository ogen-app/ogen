package server

import (
	"io"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/kernel/config"
)

// clientIP runs one request through an app built with the given proxy settings
// and returns what c.IP() resolved. fiber's test connection always comes from
// 0.0.0.0, so trusting 0.0.0.0/32 stands in for "the request came via the edge".
func clientIP(t *testing.T, trusted, header string, reqHeaders map[string]string) string {
	t.Helper()
	fcfg := fiber.Config{}
	applyProxyConfig(&fcfg, &config.Config{TrustedProxies: trusted, ProxyHeader: header, Debug: true})
	app := fiber.New(fcfg)
	app.Get("/", func(c *fiber.Ctx) error { return c.SendString(c.IP()) })

	req := httptest.NewRequest(fiber.MethodGet, "/", nil)
	for k, v := range reqHeaders {
		req.Header.Set(k, v)
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

func TestProxyConfig(t *testing.T) {
	tests := []struct {
		name    string
		trusted string
		header  string
		headers map[string]string
		want    string
	}{
		{"unset ignores header", "", "X-Real-IP", map[string]string{"X-Real-IP": "203.0.113.9"}, "0.0.0.0"},
		{"trusted peer uses header", "0.0.0.0/32", "X-Real-IP", map[string]string{"X-Real-IP": "203.0.113.9"}, "203.0.113.9"},
		{"untrusted peer ignores header", "100.0.0.0/8", "X-Real-IP", map[string]string{"X-Real-IP": "203.0.113.9"}, "0.0.0.0"},
		{"list form is trimmed", " 10.0.0.0/8 , 0.0.0.0/32 ", "X-Real-IP", map[string]string{"X-Real-IP": "203.0.113.9"}, "203.0.113.9"},
		{"first valid entry of a list header", "0.0.0.0/32", "X-Forwarded-For", map[string]string{"X-Forwarded-For": "junk, 198.51.100.4, 100.1.2.3"}, "198.51.100.4"},
		{"ipv6", "0.0.0.0/32", "X-Real-IP", map[string]string{"X-Real-IP": "2001:db8::1"}, "2001:db8::1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := clientIP(t, tt.trusted, tt.header, tt.headers); got != tt.want {
				t.Fatalf("c.IP() = %q, want %q", got, tt.want)
			}
		})
	}
}
