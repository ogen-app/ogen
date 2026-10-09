package server

import (
	"bytes"
	"encoding/json"
	"testing"
)

// lastMessageBlocks returns the content blocks of the body's last message.
func lastMessageBlocks(t *testing.T, body []byte) []systemBlock {
	t.Helper()
	var top struct {
		Messages []struct {
			Content []systemBlock `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &top); err != nil {
		t.Fatalf("unmarshal messages: %v", err)
	}
	if len(top.Messages) == 0 {
		t.Fatal("no messages")
	}
	return top.Messages[len(top.Messages)-1].Content
}

func TestAddAnthropicHistoryCacheControl(t *testing.T) {
	t.Run("marks the last block of the last message only", func(t *testing.T) {
		in := `{"model":"m","messages":[` +
			`{"role":"user","content":[{"type":"text","text":"q"}]},` +
			`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"x","input":{}}]},` +
			`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"a"},{"type":"text","text":"more"}]}]}`
		out, changed := addAnthropicHistoryCacheControl([]byte(in))
		if !changed {
			t.Fatal("expected changed=true")
		}
		blocks := lastMessageBlocks(t, out)
		if len(blocks) != 2 || blocks[0].CacheControl != nil {
			t.Fatalf("only the last block may be marked: %s", out)
		}
		if blocks[1].CacheControl == nil || blocks[1].CacheControl.Type != "ephemeral" {
			t.Fatalf("last block not marked: %s", out)
		}
		var top struct {
			Messages []json.RawMessage `json:"messages"`
		}
		_ = json.Unmarshal(out, &top)
		if bytes.Contains(top.Messages[0], []byte("cache_control")) || bytes.Contains(top.Messages[1], []byte("cache_control")) {
			t.Errorf("earlier messages were marked: %s", out)
		}
	})

	t.Run("string content is promoted to a marked text block", func(t *testing.T) {
		out, changed := addAnthropicHistoryCacheControl([]byte(`{"messages":[{"role":"user","content":"hello"}]}`))
		if !changed {
			t.Fatal("expected changed=true")
		}
		blocks := lastMessageBlocks(t, out)
		if len(blocks) != 1 || blocks[0].Text != "hello" || blocks[0].CacheControl == nil {
			t.Fatalf("string content not promoted: %s", out)
		}
	})

	t.Run("no-op cases pass through unchanged", func(t *testing.T) {
		cases := map[string]string{
			"no messages":    `{"model":"m"}`,
			"empty messages": `{"messages":[]}`,
			"empty string":   `{"messages":[{"role":"user","content":""}]}`,
			"already marked": `{"messages":[{"role":"user","content":[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}}]}]}`,
			"thinking last":  `{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"…"}]}]}`,
			"not valid json": `not json`,
		}
		for name, in := range cases {
			out, changed := addAnthropicHistoryCacheControl([]byte(in))
			if changed {
				t.Errorf("%s: expected changed=false", name)
			}
			if !bytes.Equal(out, []byte(in)) {
				t.Errorf("%s: body altered:\n in=%s\nout=%s", name, in, out)
			}
		}
	})
}
