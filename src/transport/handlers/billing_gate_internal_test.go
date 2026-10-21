package handlers

import "testing"

func TestBillingAllowed(t *testing.T) {
	allowed := []string{
		"/api/sessions", "/api/auth/google/start", "/api/current_user", "/api/settings", "/api/settings/language",
		"/api/workspaces", "/api/workspaces/abc/switch", "/api/tenants/current", "/api/invitations/accept/tok",
		"/api/me/entitlements", "/api/workspace/plan", "/api/billing", "/api/billing/portal", "/api/events",
		"/api/notifications/stream", "/api/announcements", "/api/account/auth-methods",
	}
	for _, p := range allowed {
		if !billingAllowed(p) {
			t.Errorf("%s gated", p)
		}
	}
	gated := []string{"/api/campaigns", "/api/posts/1", "/api/tenants/abc", "/api/billingx", "/api/tags", "/api/integrations/zernio/connect-links"}
	for _, p := range gated {
		if billingAllowed(p) {
			t.Errorf("%s allowed", p)
		}
	}
	if !pluginBillingAllowed("GET", "/api/plugin/figma/me") || !pluginBillingAllowed("DELETE", "/api/plugin/figma/token") {
		t.Error("plugin identity/disconnect gated")
	}
	if pluginBillingAllowed("POST", "/api/plugin/figma/images") || pluginBillingAllowed("GET", "/api/plugin/figma/token") {
		t.Error("plugin content route allowed")
	}
}
