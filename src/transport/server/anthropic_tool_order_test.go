package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/ogen-app/ogen/src/infra/vendors/llm"
)

func TestSortAnthropicToolsByName(t *testing.T) {
	body := []byte(`{
		"max_tokens": 2048,
		"model": "claude-haiku-4-5-20251001",
		"system": [{"type":"text","text":"sys"}],
		"messages": [{"role":"user","content":"hi"}],
		"tools": [
			{"name":"charlie","description":"c","strict":true},
			{"name":"alpha","description":"a","strict":true},
			{"name":"bravo","description":"b","strict":true}
		]
	}`)

	out, n, changed := sortAnthropicToolsByName(body)
	if !changed || n != 3 {
		t.Fatalf("expected changed=true n=3, got changed=%v n=%d", changed, n)
	}

	// Tools are now in name order.
	var top struct {
		MaxTokens int               `json:"max_tokens"`
		Model     string            `json:"model"`
		Messages  []json.RawMessage `json:"messages"`
		Tools     []struct {
			Name   string `json:"name"`
			Strict bool   `json:"strict"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(out, &top); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	want := []string{"alpha", "bravo", "charlie"}
	for i, w := range want {
		if top.Tools[i].Name != w {
			t.Errorf("tool[%d] = %q, want %q", i, top.Tools[i].Name, w)
		}
		if !top.Tools[i].Strict {
			t.Errorf("tool[%d] lost strict:true", i)
		}
	}

	// Non-tool fields preserved.
	if top.MaxTokens != 2048 || top.Model != "claude-haiku-4-5-20251001" || len(top.Messages) != 1 {
		t.Errorf("non-tool fields not preserved: %+v", top)
	}

	// Deterministic: re-running on already-sorted input yields identical bytes —
	// this is the property that keeps Anthropic's compilation cache warm.
	out2, _, _ := sortAnthropicToolsByName(out)
	if !bytes.Equal(out, out2) {
		t.Errorf("not idempotent/deterministic:\n first=%s\nsecond=%s", out, out2)
	}
}

func TestSortAnthropicToolsByName_NoOp(t *testing.T) {
	cases := map[string]string{
		"no tools field": `{"max_tokens":10,"messages":[]}`,
		"single tool":    `{"tools":[{"name":"only"}]}`,
		"empty tools":    `{"tools":[]}`,
		"not valid json": `not json at all`,
	}
	for name, in := range cases {
		out, _, changed := sortAnthropicToolsByName([]byte(in))
		if changed {
			t.Errorf("%s: expected changed=false", name)
		}
		if !bytes.Equal(out, []byte(in)) {
			t.Errorf("%s: body altered when it should pass through:\n in=%s\nout=%s", name, in, out)
		}
	}
}

// The transport must sort the tools on a clone and leave the caller's request
// untouched (net/http RoundTripper contract).
func TestAnthropicToolOrderTransport_ClonesWithoutMutatingOriginal(t *testing.T) {
	body := []byte(`{"max_tokens":1,"tools":[{"name":"charlie"},{"name":"alpha"},{"name":"bravo"}]}`)

	base := &stubRoundTripper{resp: newResp(200)}
	tr := &anthropicToolOrderTransport{base: base}

	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	_ = resp.Body.Close()

	// Base receives a clone, not the caller's request, carrying sorted tools.
	if base.last == req {
		t.Fatal("base received the original request; want a clone")
	}
	sent, _ := io.ReadAll(base.last.Body)
	if got := toolNameOrder(t, sent); got != "alpha,bravo,charlie" {
		t.Errorf("sent tool order = %q, want sorted", got)
	}

	// Original request is untouched — its GetBody still yields the original order.
	rc, err := req.GetBody()
	if err != nil {
		t.Fatalf("original GetBody: %v", err)
	}
	orig, _ := io.ReadAll(rc)
	rc.Close()
	if got := toolNameOrder(t, orig); got != "charlie,alpha,bravo" {
		t.Errorf("original tool order = %q, want unchanged", got)
	}
}

func TestAddAnthropicSystemCacheControl(t *testing.T) {
	t.Run("string system is promoted to a cached text block", func(t *testing.T) {
		out, changed := addAnthropicSystemCacheControl([]byte(`{"model":"m","system":"you are a bot"}`))
		if !changed {
			t.Fatal("expected changed=true")
		}
		blocks := systemBlocks(t, out)
		if len(blocks) != 1 {
			t.Fatalf("want 1 system block, got %d", len(blocks))
		}
		if blocks[0].Type != "text" || blocks[0].Text != "you are a bot" {
			t.Errorf("block content not preserved: %+v", blocks[0])
		}
		if blocks[0].CacheControl == nil || blocks[0].CacheControl.Type != "ephemeral" {
			t.Errorf("missing ephemeral cache_control: %+v", blocks[0])
		}
	})

	t.Run("array system marks the last block only", func(t *testing.T) {
		in := `{"system":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}`
		out, changed := addAnthropicSystemCacheControl([]byte(in))
		if !changed {
			t.Fatal("expected changed=true")
		}
		blocks := systemBlocks(t, out)
		if len(blocks) != 2 {
			t.Fatalf("want 2 system blocks, got %d", len(blocks))
		}
		if blocks[0].CacheControl != nil {
			t.Errorf("first block should be unmarked, got %+v", blocks[0].CacheControl)
		}
		if blocks[1].CacheControl == nil || blocks[1].CacheControl.Type != "ephemeral" {
			t.Errorf("last block should carry ephemeral cache_control, got %+v", blocks[1].CacheControl)
		}
	})

	t.Run("no-op cases pass through unchanged", func(t *testing.T) {
		cases := map[string]string{
			"no system field": `{"model":"m","messages":[]}`,
			"empty string":    `{"system":""}`,
			"empty array":     `{"system":[]}`,
			"already marked":  `{"system":[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}}]}`,
			"not valid json":  `not json`,
		}
		for name, in := range cases {
			out, changed := addAnthropicSystemCacheControl([]byte(in))
			if changed {
				t.Errorf("%s: expected changed=false", name)
			}
			if !bytes.Equal(out, []byte(in)) {
				t.Errorf("%s: body altered:\n in=%s\nout=%s", name, in, out)
			}
		}
	})
}

// The system cache breakpoint is added only for a call that asked for prompt
// caching, whatever its model; the tool sort applies to every Anthropic request.
func TestAnthropicToolOrderTransport_SystemCacheScopedToCall(t *testing.T) {
	const body = `{"model":"any-model","system":"sys","tools":[{"name":"charlie"},{"name":"alpha"}]}`

	roundTrip := func(t *testing.T, cache bool) []byte {
		t.Helper()
		base := &stubRoundTripper{resp: newResp(200)}
		tr := &anthropicToolOrderTransport{base: base}
		ctx := t.Context()
		if cache {
			ctx = llm.WithPromptCache(ctx)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader([]byte(body)))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		resp, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatalf("round trip: %v", err)
		}
		_ = resp.Body.Close()
		sent, _ := io.ReadAll(base.last.Body)
		return sent
	}

	t.Run("caching call gets a system cache breakpoint", func(t *testing.T) {
		sent := roundTrip(t, true)
		if got := toolNameOrder(t, sent); got != "alpha,charlie" {
			t.Errorf("tools not sorted: %q", got)
		}
		blocks := systemBlocks(t, sent)
		if len(blocks) != 1 || blocks[0].CacheControl == nil {
			t.Errorf("expected a cached system block, got %s", sent)
		}
	})

	t.Run("other call gets tool sort but no cache breakpoint", func(t *testing.T) {
		sent := roundTrip(t, false)
		if got := toolNameOrder(t, sent); got != "alpha,charlie" {
			t.Errorf("tools not sorted: %q", got)
		}
		// system stays a bare string — no breakpoint added.
		var top struct {
			System json.RawMessage `json:"system"`
		}
		if err := json.Unmarshal(sent, &top); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(top.System) == 0 || top.System[0] != '"' {
			t.Errorf("system should stay an unmodified string, got %s", top.System)
		}
	})
}

type systemBlock struct {
	Type         string `json:"type"`
	Text         string `json:"text"`
	CacheControl *struct {
		Type string `json:"type"`
	} `json:"cache_control"`
}

func systemBlocks(t *testing.T, body []byte) []systemBlock {
	t.Helper()
	var top struct {
		System []systemBlock `json:"system"`
	}
	if err := json.Unmarshal(body, &top); err != nil {
		t.Fatalf("unmarshal system blocks: %v", err)
	}
	return top.System
}

func toolNameOrder(t *testing.T, body []byte) string {
	t.Helper()
	var top struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &top); err != nil {
		t.Fatalf("unmarshal tools: %v", err)
	}
	names := make([]string, len(top.Tools))
	for i, tl := range top.Tools {
		names[i] = tl.Name
	}
	return strings.Join(names, ",")
}
