package server

import (
	"log/slog"
	"strings"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/kernel/config"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// applyProxyConfig makes c.IP() return the real client address when the API
// runs behind a reverse proxy. The proxy header is read only for requests
// whose socket peer is in TRUSTED_PROXIES, and only a syntactically valid IP
// from it is accepted, so a direct caller can't pick its own address. Every
// per-IP limiter and the login-alert IP depend on this.
func applyProxyConfig(fcfg *fiber.Config, cfg *config.Config) {
	proxies := splitCSV(cfg.TrustedProxies)
	if len(proxies) == 0 || strings.TrimSpace(cfg.ProxyHeader) == "" {
		if !cfg.Debug {
			slog.Warn("TRUSTED_PROXIES is not set: client IPs are the socket peer, so behind a proxy every caller shares one rate-limit bucket",
				logging.AttrComponent, "server")
		}
		return
	}
	fcfg.ProxyHeader = strings.TrimSpace(cfg.ProxyHeader)
	fcfg.EnableTrustedProxyCheck = true
	fcfg.TrustedProxies = proxies
	fcfg.EnableIPValidation = true
}

func splitCSV(s string) []string {
	var out []string
	for part := range strings.SplitSeq(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
