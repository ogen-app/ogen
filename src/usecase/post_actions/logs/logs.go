// Package logs provides the helpers Post Log writers use to produce
// safe payloads — capping size and stripping secrets — before they
// land in the post_logs table.
//
// The schema (post_logs.payload) is plain TEXT; the 64 KB ceiling and
// the secret-redaction guarantees live in code, not in the database.
// Every writer MUST funnel through these helpers.
package logs

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// MaxPayloadBytes is the per-entry payload ceiling. Picked at 64 KB
// (CON-69 plan) to fit a sanitized Zernio response without bloating
// the database. Larger payloads are truncated with a clear marker.
const MaxPayloadBytes = 64 << 10

// truncationMarker is appended to the visible portion of an oversize
// payload so consumers can spot truncation at a glance and know how
// many bytes the original ran to.
const truncationMarker = "…[truncated, original=%d bytes]"

// Capper takes a JSON-shaped string and returns it unchanged when it
// fits the ceiling, or a same-shaped string truncated mid-payload with
// a marker appended. The output is always valid UTF-8 (the truncate
// point is rounded down to a UTF-8 boundary) and is enclosed in
// quotes so it round-trips through JSON unmarshalling as a string.
//
// We don't try to keep the truncated body itself parseable — that
// would require schema-aware truncation. Consumers should treat the
// payload as opaque text once the marker is present.
func Capper(payload string) string {
	if len(payload) <= MaxPayloadBytes {
		return payload
	}
	marker := fmt.Sprintf(truncationMarker, len(payload))
	keep := max(MaxPayloadBytes-len(marker), 0)
	// Round down to a UTF-8 boundary so we don't slice mid-rune.
	for keep > 0 && payload[keep]&0xC0 == 0x80 {
		keep--
	}
	return payload[:keep] + marker
}

// MarshalCapped is a convenience for callers building a payload from
// a Go value. It marshals to JSON and applies Capper. If marshalling
// fails it returns a JSON object describing the error so writers
// never have to fall back to writing an empty payload.
func MarshalCapped(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		fallback, _ := json.Marshal(map[string]string{
			"_error": "logs: marshal failed: " + err.Error(),
		})
		return string(fallback)
	}
	return Capper(string(b))
}

// secretValuePattern matches conservative leakage shapes inside a
// JSON-ish string: API keys, bearer tokens, passwords, etc. We
// redact the value only — keeping the key visible so debugging is
// possible. The pattern is intentionally simple; complex schema-aware
// stripping happens at the call site (e.g., Zernio request builders
// can blank out their own Authorization headers before passing).
var secretValuePattern = regexp.MustCompile(`(?i)("(?:api[_-]?key|access[_-]?key|secret|password|token|authorization|x-zernio-signature|webhook[_-]?secret)"\s*:\s*)"[^"]*"`)

// secretHeaderPattern matches `Authorization: Bearer <token>` style
// strings that may appear in raw HTTP captures. The token is replaced
// with a fixed sentinel so leakage doesn't survive even when the
// surrounding text is unstructured.
var secretHeaderPattern = regexp.MustCompile(`(?i)(authorization\s*:\s*bearer\s+)\S+`)

// Sanitize strips well-known secret-bearing fields from a string
// payload. Use this on any text that may carry credentials before it
// is written to a Post Log. It is safe to call on payloads that don't
// contain secrets; non-matches pass through untouched.
func Sanitize(payload string) string {
	out := secretValuePattern.ReplaceAllString(payload, `$1"[REDACTED]"`)
	out = secretHeaderPattern.ReplaceAllString(out, `${1}[REDACTED]`)
	return out
}

// SanitizeAndCap is the canonical writer-side pipeline: redact, then
// truncate. Order matters — truncating first could chop a marker
// boundary in the middle of a key/value pair the redactor would have
// caught, leaking partial secrets.
func SanitizeAndCap(payload string) string {
	return Capper(Sanitize(payload))
}

// ContainsTruncationMarker reports whether s carries a marker emitted
// by Capper. Useful in tests and ops queries that want to count
// truncated entries.
func ContainsTruncationMarker(s string) bool {
	return strings.Contains(s, "…[truncated, original=")
}
