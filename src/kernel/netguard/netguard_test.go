package netguard

import (
	"errors"
	"net"
	"testing"
)

func TestBlocked(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "::1", // loopback
		"10.0.0.5", "192.168.1.1", "172.16.0.1", // RFC1918
		"169.254.169.254", // AWS/GCP IMDS (link-local)
		"fd00:ec2::254",   // GCP IMDS IPv6 (ULA)
		"0.0.0.0",         // unspecified
		"224.0.0.1",       // multicast
		"fc00::1",         // ULA
	}
	for _, s := range blocked {
		if !Blocked(net.ParseIP(s)) {
			t.Errorf("Blocked(%s) = false, want true", s)
		}
	}
	allowed := []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:2800:220:1::"}
	for _, s := range allowed {
		if Blocked(net.ParseIP(s)) {
			t.Errorf("Blocked(%s) = true, want false", s)
		}
	}
	if !Blocked(nil) {
		t.Error("Blocked(nil) should be true")
	}
}

func TestNormalizeHost(t *testing.T) {
	if got := NormalizeHost("Example.COM."); got != "example.com" {
		t.Errorf("NormalizeHost = %q, want example.com", got)
	}
}

func TestResolveAllowed_LiteralIPs(t *testing.T) {
	// A literal private IP must be rejected without any DNS lookup.
	if err := ResolveAllowed(t.Context(), "169.254.169.254"); !errors.Is(err, ErrBlocked) {
		t.Errorf("ResolveAllowed(metadata IP) = %v, want ErrBlocked", err)
	}
	if err := ResolveAllowed(t.Context(), "10.0.0.1"); !errors.Is(err, ErrBlocked) {
		t.Errorf("ResolveAllowed(private IP) = %v, want ErrBlocked", err)
	}
	// A literal public IP is allowed (no lookup needed).
	if err := ResolveAllowed(t.Context(), "8.8.8.8"); err != nil {
		t.Errorf("ResolveAllowed(public IP) = %v, want nil", err)
	}
}
