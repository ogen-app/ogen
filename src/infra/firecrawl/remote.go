package firecrawl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// Remote scrapes. URL ingestion running in river-service has it send the
// scrape with its own leased key. ogen builds the request and decodes the
// response by running the ordinary Client against a transport that captures
// the request (Plan) or answers it with river-service's result (Decode).

// planBase is the base URL a planned request is built against; only its path
// is kept.
const planBase = "http://firecrawl.plan"

// Call is one planned Firecrawl request, its path relative to the API base
// URL. river-service adds the Authorization header.
type Call struct {
	Method string
	Path   string
	Body   []byte
}

// Result is river-service's answer to a Call: the response, or why there was
// none.
type Result struct {
	Status int
	Body   []byte
	Err    string
}

var errCaptured = errors.New("firecrawl: request captured")

type captureTransport struct{ call *Call }

func (t *captureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.call.Method, t.call.Path = req.Method, req.URL.Path
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		t.call.Body = b
	}
	return nil, errCaptured
}

type replayTransport struct{ res Result }

func (t replayTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.res.Err != "" {
		return nil, errors.New(t.res.Err)
	}
	if t.res.Status == 0 {
		return nil, errors.New("firecrawl: no response")
	}
	return &http.Response{
		StatusCode: t.res.Status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(t.res.Body)),
		Request:    req,
	}, nil
}

func clientWith(rt http.RoundTripper) *Client {
	c := New(func(context.Context) (string, error) { return "remote", nil }, planBase, 0)
	c.httpClient = &http.Client{Transport: rt}
	return c
}

// Plan returns the request op would send, without sending it.
func Plan(op func(c *Client) error) (Call, error) {
	var call Call
	err := op(clientWith(&captureTransport{call: &call}))
	if call.Path != "" {
		return call, nil
	}
	if err == nil {
		return Call{}, errors.New("firecrawl: operation sent no request")
	}
	return Call{}, fmt.Errorf("firecrawl: plan request: %w", err)
}

// Decode runs op as if Firecrawl had answered its request with res, so op's
// result and error (an *APIError, Transient for a 429/5xx or no response) are
// exactly what a direct call would have returned.
func Decode(res Result, op func(c *Client) error) error {
	return op(clientWith(replayTransport{res: res}))
}
