package server

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
)

type capturingTransport struct{ body []byte }

func (c *capturingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.body, _ = io.ReadAll(req.Body)
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}")), Request: req}, nil
}

func anthropicRequest(t *testing.T, body string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestAnthropicPDFTransportRetypesPDFImages(t *testing.T) {
	base := &capturingTransport{}
	tr := &anthropicPDFTransport{base: base}
	body := `{"model":"claude-haiku-5-5","messages":[{"role":"user","content":[
		{"type":"text","text":"read it"},
		{"type":"image","source":{"type":"base64","media_type":"application/pdf","data":"JVBERg=="}},
		{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBO"}}]}]}`
	if _, err := tr.RoundTrip(anthropicRequest(t, body)); err != nil {
		t.Fatal(err)
	}
	got := string(base.body)
	if strings.Count(got, `"type":"document"`) != 1 || strings.Count(got, `"type":"image"`) != 1 || !strings.Contains(got, `"data":"JVBERg=="`) {
		t.Fatalf("sent %s", got)
	}

	plain := `{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`
	if _, err := tr.RoundTrip(anthropicRequest(t, plain)); err != nil {
		t.Fatal(err)
	}
	if string(base.body) != plain {
		t.Fatalf("a request with no PDF was rewritten: %s", base.body)
	}
}
