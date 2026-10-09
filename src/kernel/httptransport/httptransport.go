// Package httptransport tunes the process-wide default HTTP transport.
//
// The Genkit Anthropic and Gemini plugins and the Zernio, Firecrawl and
// Resend clients all send through http.DefaultTransport, which keeps only two
// idle connections per host. With more concurrent calls to one host than that
// (parallel post-quality checks, batched drafts, publish polling), every
// extra connection is closed after use and the next call pays a fresh TLS
// handshake.
package httptransport

import "net/http"

const (
	maxIdleConns        = 128
	maxIdleConnsPerHost = 32
)

// TuneDefault replaces http.DefaultTransport with a clone that keeps more
// idle connections per host. Call it at boot, before anything wraps the
// default transport (telemetry, the Anthropic request rewriter) or builds a
// client from it. It is a no-op if the default transport was already replaced.
func TuneDefault() {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return
	}
	t = t.Clone()
	t.MaxIdleConns = maxIdleConns
	t.MaxIdleConnsPerHost = maxIdleConnsPerHost
	http.DefaultTransport = t
}
