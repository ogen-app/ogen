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
// outgoing Anthropic /v1/messages request has its `tools` array sorted by name.
//
// The Genkit Anthropic plugin builds the tool list by ranging a map and marks
// every tool strict. Anthropic caches the compiled strict-tool grammar per
// exact, order-sensitive tool set, so a random order pays the full compile
// (~50s) on every call. A stable order keeps that cache warm.
//
// When cachePrefixModel is non-empty, requests for that model also get an
// ephemeral cache_control breakpoint on the last system block, caching the
// tool schemas and system prompt together, and one on the last message, so a
// tool loop's rounds reuse the conversation so far. Pass "" to disable.
// Prefixes under the model's caching minimum are silently not cached.
//
// Must be installed before the plugin builds its client. Idempotent;
// non-Anthropic traffic passes through untouched.
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
		if cached, ok := addAnthropicHistoryCacheControl(out); ok {
			out, changed = cached, true
			slog.Debug("anthropic history cache breakpoint added",
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

// addAnthropicHistoryCacheControl puts an ephemeral cache_control breakpoint on
// the last content block of the last message. In a tool loop every round
// resends the whole conversation; with the breakpoint, round N+1 reads rounds
// 1..N from the cache Anthropic wrote for round N instead of paying full input
// price for them again. Together with the system breakpoint this uses two of
// the four allowed. A string content is promoted to one text block. changed is
// false when there are no messages, the last one is empty, or its last block
// already has a breakpoint or cannot carry one (thinking blocks).
func addAnthropicHistoryCacheControl(body []byte) (out []byte, changed bool) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return body, false
	}
	var msgs []json.RawMessage
	if err := json.Unmarshal(top["messages"], &msgs); err != nil || len(msgs) == 0 {
		return body, false
	}
	var last map[string]json.RawMessage
	if err := json.Unmarshal(msgs[len(msgs)-1], &last); err != nil {
		return body, false
	}
	content, ok := cacheMarkedContent(last["content"])
	if !ok {
		return body, false
	}
	last["content"] = content
	nm, err := json.Marshal(last)
	if err != nil {
		return body, false
	}
	msgs[len(msgs)-1] = nm
	newMsgs, err := json.Marshal(msgs)
	if err != nil {
		return body, false
	}
	top["messages"] = newMsgs
	rewritten, err := json.Marshal(top)
	if err != nil {
		return body, false
	}
	return rewritten, true
}

// cacheMarkedContent returns a message's content with cache_control on its last
// block, or false when there is nothing to mark.
func cacheMarkedContent(raw json.RawMessage) (json.RawMessage, bool) {
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		if asString == "" {
			return nil, false
		}
		out, err := json.Marshal([]any{map[string]any{
			"type":          "text",
			"text":          asString,
			"cache_control": map[string]string{"type": "ephemeral"},
		}})
		return out, err == nil
	}
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil || len(blocks) == 0 {
		return nil, false
	}
	lastBlock := blocks[len(blocks)-1]
	if _, exists := lastBlock["cache_control"]; exists {
		return nil, false
	}
	var kind string
	_ = json.Unmarshal(lastBlock["type"], &kind)
	if kind == "thinking" || kind == "redacted_thinking" {
		return nil, false
	}
	lastBlock["cache_control"] = json.RawMessage(`{"type":"ephemeral"}`)
	out, err := json.Marshal(blocks)
	return out, err == nil
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
