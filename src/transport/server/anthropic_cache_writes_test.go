package server

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/ogen-app/ogen/src/infra/vendors/llm"
)

func TestAnthropicToolOrderTransport_ReportsCacheWrites(t *testing.T) {
	streamed := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"usage":{"input_tokens":12,"cache_creation_input_tokens":104000,"cache_read_input_tokens":0,"output_tokens":1}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","usage":{"output_tokens":40,"cache_creation_input_tokens":104000}}`,
		``,
	}, "\n")
	tests := []struct {
		name string
		body string
		want int64
	}{
		{name: "streamed response", body: streamed, want: 104_000},
		{name: "plain response", body: `{"usage":{"input_tokens":5, "cache_creation_input_tokens": 2048,"cache_creation":{"ephemeral_5m_input_tokens":2048}}}`, want: 2048},
		{name: "no cache writes", body: `{"usage":{"input_tokens":5,"output_tokens":3}}`, want: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := newResp(200)
			// One byte per read, so the key and its value straddle reads.
			resp.Body = io.NopCloser(iotest.OneByteReader(strings.NewReader(tc.body)))
			tr := &anthropicToolOrderTransport{base: &stubRoundTripper{resp: resp}}

			ctx, probe := llm.WithCacheWriteProbe(t.Context())
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader([]byte(`{"max_tokens":1}`)))
			if err != nil {
				t.Fatal(err)
			}
			got, err := tr.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			read, err := io.ReadAll(got.Body)
			if err != nil {
				t.Fatal(err)
			}
			_ = got.Body.Close()

			if string(read) != tc.body {
				t.Fatalf("body altered: %q", read)
			}
			if probe.Tokens() != tc.want {
				t.Fatalf("probe = %d, want %d", probe.Tokens(), tc.want)
			}
		})
	}
}

func TestAnthropicToolOrderTransport_NoProbeLeavesBodyAlone(t *testing.T) {
	resp := newResp(200)
	body := resp.Body
	tr := &anthropicToolOrderTransport{base: &stubRoundTripper{resp: resp}}
	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader([]byte(`{"max_tokens":1}`)))
	if err != nil {
		t.Fatal(err)
	}
	got, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	if got.Body != body {
		t.Fatal("body wrapped without a probe in the request context")
	}
}
