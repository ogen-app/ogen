// Package netguard provides SSRF defenses for outbound HTTP to untrusted URLs
// (the URL-asset scraper mirrors images referenced by arbitrary scraped
// pages). The authoritative control is SafeClient, whose dialer rejects any
// connection whose *resolved* address is private/loopback/link-local — this
// closes the TOCTOU window between a name lookup and the actual dial (DNS
// rebinding), which a pre-flight LookupIP alone cannot. ResolveAllowed is a
// cheaper best-effort pre-check for request handlers.
package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"
)

// ErrBlocked is returned when a host/address resolves to a non-public range.
var ErrBlocked = errors.New("netguard: address not allowed")

// gcpMetadataV6 is Google Cloud's IMDS IPv6 (fd00:ec2::254). It sits in fc00::/7
// (unique-local), which Blocked already rejects via IsPrivate, but we name it
// explicitly for clarity and defense in depth. The AWS/GCP IPv4 metadata IP
// 169.254.169.254 is link-local, also covered below.
var gcpMetadataV6 = net.ParseIP("fd00:ec2::254")

// Blocked reports whether ip is one we must never connect to: loopback,
// RFC1918/ULA private, link-local (incl. 169.254.169.254 cloud metadata),
// multicast, or the unspecified address.
func Blocked(ip net.IP) bool {
	if ip == nil {
		return true
	}
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() ||
		ip.IsInterfaceLocalMulticast() ||
		ip.Equal(gcpMetadataV6)
}

// NormalizeHost lower-cases and strips a trailing dot (FQDN root) so denylist
// comparisons and lookups don't miss "Example.com." style hosts.
func NormalizeHost(host string) string {
	return strings.ToLower(strings.TrimRight(host, "."))
}

// ResolveAllowed is a best-effort pre-flight check for a request handler: it
// rejects a literal blocked IP, and rejects a hostname that resolves to any
// blocked address. A resolution *failure* returns nil (unknown — the dial-time
// guard in SafeClient is authoritative), so a transient DNS hiccup or an
// offline test environment never rejects a legitimate submission.
func ResolveAllowed(ctx context.Context, host string) error {
	host = NormalizeHost(host)
	if host == "" {
		return fmt.Errorf("%w: empty host", ErrBlocked)
	}
	if ip := net.ParseIP(host); ip != nil {
		if Blocked(ip) {
			return fmt.Errorf("%w: %s", ErrBlocked, host)
		}
		return nil
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return nil // can't determine; SafeClient blocks at connect time
	}
	for _, ip := range ips {
		if Blocked(ip) {
			return fmt.Errorf("%w: %s -> %s", ErrBlocked, host, ip)
		}
	}
	return nil
}

// SafeClient returns an *http.Client that refuses to connect to a blocked
// address at dial time and re-validates every redirect hop. Proxies from the
// environment are disabled so they can't be used to bypass the dialer guard.
func SafeClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: dialGuard}
	return &http.Client{
		Timeout: timeout,
		// The hosts are arbitrary pages and image CDNs, rarely called twice, so
		// idle connections are capped and reaped instead of kept (with their
		// read/write goroutines) until each remote end closes them.
		Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         dialer.DialContext,
			ForceAttemptHTTP2:   true,
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     30 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("netguard: too many redirects")
			}
			return ResolveAllowed(req.Context(), req.URL.Hostname())
		},
	}
}

// dialGuard is a net.Dialer.Control hook: address is the concrete, already
// resolved "ip:port" about to be dialed, so checking it here is immune to DNS
// rebinding between LookupIP and Dial.
func dialGuard(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || Blocked(ip) {
		return fmt.Errorf("%w: %s", ErrBlocked, address)
	}
	return nil
}
