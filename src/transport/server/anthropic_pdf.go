package server

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/ogen-app/ogen/src/kernel/logging"
)

var installAnthropicPDFOnce sync.Once

// InstallAnthropicPDFDocuments wraps http.DefaultTransport so a PDF a flow
// attaches reaches Anthropic as a document block. The genkit plugin sends
// every media part as an image block, and the API rejects an image whose
// media type is application/pdf; a document block is the same source with
// another type. Idempotent; every other request passes through untouched.
func InstallAnthropicPDFDocuments() {
	installAnthropicPDFOnce.Do(func() {
		base := http.DefaultTransport
		if base == nil {
			base = &http.Transport{}
		}
		http.DefaultTransport = &anthropicPDFTransport{base: base}
		slog.Info("anthropic pdf document rewrite installed", logging.AttrComponent, "anthropic.http")
	})
}

type anthropicPDFTransport struct {
	base http.RoundTripper
}

var pdfMediaType = []byte(`"application/pdf"`)

func (t *anthropicPDFTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body == nil || !strings.Contains(req.URL.Host, "anthropic") || !strings.HasSuffix(req.URL.Path, "/messages") {
		return t.base.RoundTrip(req)
	}
	body := readAnthropicReqBody(req)
	if !bytes.Contains(body, pdfMediaType) {
		return t.base.RoundTrip(req)
	}
	out, changed := rewriteTop(body, pdfImagesToDocuments)
	if !changed {
		return t.base.RoundTrip(req)
	}
	clone := req.Clone(req.Context())
	setAnthropicReqBody(clone, out)
	return t.base.RoundTrip(clone)
}

// pdfImagesToDocuments retypes every image block whose source is a PDF as a
// document block, in place, and reports whether it changed any.
func pdfImagesToDocuments(top map[string]json.RawMessage) bool {
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(top["messages"], &messages); err != nil {
		return false
	}
	changed := false
	for _, m := range messages {
		var blocks []map[string]json.RawMessage
		if err := json.Unmarshal(m["content"], &blocks); err != nil {
			continue
		}
		edited := false
		for _, b := range blocks {
			var typ string
			var src struct {
				MediaType string `json:"media_type"`
			}
			if json.Unmarshal(b["type"], &typ) != nil || typ != "image" || json.Unmarshal(b["source"], &src) != nil || src.MediaType != "application/pdf" {
				continue
			}
			b["type"] = json.RawMessage(`"document"`)
			edited = true
		}
		if !edited {
			continue
		}
		raw, err := json.Marshal(blocks)
		if err != nil {
			return false
		}
		m["content"], changed = raw, true
	}
	if !changed {
		return false
	}
	raw, err := json.Marshal(messages)
	if err != nil {
		return false
	}
	top["messages"] = raw
	return true
}
