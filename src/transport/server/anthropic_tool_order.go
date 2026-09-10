package server

import (
	"bytes"
	"cmp"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/ogen-app/ogen/src/kernel/logging"
)

var installAnthropicToolOrderOnce sync.Once

// InstallAnthropicToolOrderStabilizer wraps http.DefaultTransport so every
// outgoing Anthropic /v1/messages request has its `tools` array sorted by name
// before it is sent. This is the CON-112 fix.
//
// Root cause: the Genkit Anthropic plugin builds the outgoing tool list by
// ranging a Go map (firebase/genkit go/ai/generate.go), which yields a *random*
// order on every request, and it forces `strict: true` on every tool
// (plugins/internal/anthropic). Anthropic compiles strict tool schemas into a
// constrained-decoding grammar that is cached per exact tool-set, and the cache
// key is order-sensitive. A fresh random order therefore misses the cache on
// every call and pays the full server-side compile (~50s) each time — the
// chronic latency on the campaign_assistant and other tool-using flows.
//
// Imposing a deterministic order at the wire makes the cache warm after the
// first request (per ~24h TTL) and every subsequent request fast. The plugin
// builds its client from http.DefaultClient (→ http.DefaultTransport), so this
// intercepts every model call without forking the plugin — same hook point as
// InstallAnthropicHTTPLogging. Must be installed before the plugin builds its
// client. Idempotent; non-Anthropic traffic passes through untouched.
//
// cachePrefixModel, when non-empty, additionally gets an ephemeral
// cache_control breakpoint on `system` (token-level prompt caching, distinct
// from the strict-tool grammar cache above). Anthropic assembles the prefix as
// tools→system, so one breakpoint on the last system block caches the tool
// schemas AND the system prompt together. Scoped to a single model — the cheap
// planning model behind campaign_assistant (CON-112) — because a flow whose
// system prompt changes every request with no reads would only pay the ~1.25×
// cache-write premium. Pass "" to disable. Prompt caching only fires when the
// cached prefix clears the model's minimum (~4096 tokens on Haiku 4.5);
// shorter prefixes silently do nothing — verify via cache_read_input_tokens.
func InstallAnthropicToolOrderStabilizer(cachePrefixModel string) {
	installAnthropicToolOrderOnce.Do(func() {
		base := http.DefaultTransport
		if base == nil {
			base = &http.Transport{}
		}
		http.DefaultTransport = &anthropicToolOrderTransport{base: base, cachePrefixModel: cachePrefixModel}
		slog.Info("anthropic tool-order stabilizer installed",
			logging.AttrComponent, "anthropic.http", "cache_prefix_model", cachePrefixModel)
	})
}

type anthropicToolOrderTransport struct {
	base http.RoundTripper
	// cachePrefixModel is the model whose requests get a system cache_control
	// breakpoint. Empty disables prompt caching (tool-order sorting is
	// unconditional). See InstallAnthropicToolOrderStabilizer.
	cachePrefixModel string
}

func (t *anthropicToolOrderTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body == nil ||
		!strings.Contains(req.URL.Host, "anthropic") ||
		!strings.HasSuffix(req.URL.Path, "/messages") {
		return t.base.RoundTrip(req)
	}

	body := readAnthropicReqBody(req)
	if len(body) == 0 {
		return t.base.RoundTrip(req)
	}

	out, changed := body, false
	if sorted, n, ok := sortAnthropicToolsByName(out); ok {
		out, changed = sorted, true
		slog.Debug("anthropic tools reordered for cache stability",
			logging.AttrComponent, "anthropic.http", "tools", n)
	}
	// Prompt caching is scoped to the one model configured for it, so other
	// flows never pay the cache-write premium on a prefix they won't reuse.
	if t.cachePrefixModel != "" && anthropicRequestModel(out) == t.cachePrefixModel {
		if cached, ok := addAnthropicSystemCacheControl(out); ok {
			out, changed = cached, true
			slog.Debug("anthropic system cache breakpoint added",
				logging.AttrComponent, "anthropic.http")
		}
	}
	if !changed {
		return t.base.RoundTrip(req)
	}

	// RoundTrip must not modify the caller's request (net/http contract), so
	// rewrite the transformed body on a clone and leave the original untouched.
	clone := req.Clone(req.Context())
	setAnthropicReqBody(clone, out)
	return t.base.RoundTrip(clone)
}

// anthropicRequestModel returns the top-level `model` of a /v1/messages body,
// or "" when it can't be read.
func anthropicRequestModel(body []byte) string {
	var t struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &t)
	return t.Model
}

// addAnthropicSystemCacheControl puts one ephemeral cache_control breakpoint on
// the request's `system` field. Because Anthropic assembles the prompt prefix
// as tools→system, this caches the tool schemas AND the system prompt together.
// The `system` field may arrive as a JSON string or as an array of content
// blocks (both are valid Anthropic input); handle both. Every other top-level
// field is preserved as raw bytes, so the transform is lossless. changed is
// false when there is no system, it is empty, or a breakpoint already exists.
func addAnthropicSystemCacheControl(body []byte) (out []byte, changed bool) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return body, false
	}
	raw, ok := top["system"]
	if !ok {
		return body, false
	}

	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		// String form: promote to a single cached text block.
		if asString == "" {
			return body, false
		}
		block := map[string]any{
			"type":          "text",
			"text":          asString,
			"cache_control": map[string]string{"type": "ephemeral"},
		}
		newSys, err := json.Marshal([]any{block})
		if err != nil {
			return body, false
		}
		top["system"] = newSys
	} else {
		// Array form: attach cache_control to the last block.
		var blocks []json.RawMessage
		if err := json.Unmarshal(raw, &blocks); err != nil || len(blocks) == 0 {
			return body, false
		}
		var last map[string]json.RawMessage
		if err := json.Unmarshal(blocks[len(blocks)-1], &last); err != nil {
			return body, false
		}
		if _, exists := last["cache_control"]; exists {
			return body, false // already marked; nothing to do
		}
		last["cache_control"] = json.RawMessage(`{"type":"ephemeral"}`)
		nb, err := json.Marshal(last)
		if err != nil {
			return body, false
		}
		blocks[len(blocks)-1] = nb
		newSys, err := json.Marshal(blocks)
		if err != nil {
			return body, false
		}
		top["system"] = newSys
	}

	rewritten, err := json.Marshal(top)
	if err != nil {
		return body, false
	}
	return rewritten, true
}

// sortAnthropicToolsByName returns the body with its top-level `tools` array
// sorted by tool name. Only the tools array is reordered; every other top-level
// field is preserved as raw bytes, so the transform is lossless and
// deterministic across runs. changed is false when there is nothing to reorder
// (no tools, <2 tools, or a parse failure) so the original body is sent as-is.
func sortAnthropicToolsByName(body []byte) (out []byte, n int, changed bool) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return body, 0, false
	}
	raw, ok := top["tools"]
	if !ok {
		return body, 0, false
	}
	var tools []json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil || len(tools) < 2 {
		return body, len(tools), false
	}
	slices.SortStableFunc(tools, func(a, b json.RawMessage) int {
		return cmp.Compare(anthropicToolName(a), anthropicToolName(b))
	})
	newTools, err := json.Marshal(tools)
	if err != nil {
		return body, len(tools), false
	}
	top["tools"] = newTools
	rewritten, err := json.Marshal(top)
	if err != nil {
		return body, len(tools), false
	}
	return rewritten, len(tools), true
}

func anthropicToolName(raw json.RawMessage) string {
	var t struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(raw, &t)
	return t.Name
}

// readAnthropicReqBody returns a copy of the request body without mutating req.
// It reads through GetBody, which net/http populates for in-memory bodies such
// as the SDK's /v1/messages payload, so the caller's one-shot Body is never
// consumed. Returns nil when GetBody is unavailable — the caller then forwards
// the request untouched rather than draining the original body.
func readAnthropicReqBody(req *http.Request) []byte {
	if req.GetBody == nil {
		return nil
	}
	rc, err := req.GetBody()
	if err != nil {
		return nil
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return nil
	}
	return b
}

func setAnthropicReqBody(req *http.Request, body []byte) {
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
}
