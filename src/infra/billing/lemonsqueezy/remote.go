package lemonsqueezy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// Remote calls. A billing job running in river-service has it send the call
// with its own leased key. ogen builds the request and decodes the response
// by running the ordinary Client against a transport that captures the
// request (Plan) or answers it with river-service's result (Decode).

// planBase is the base URL a planned request is built against; only the
// path below it is kept.
const planBase = "http://lemonsqueezy.plan"

// Call is one planned request, its path relative to the API base URL.
// river-service adds the Authorization header.
type Call struct {
	Method  string
	Path    string
	Headers map[string]string
	Body    []byte
}

// Result is river-service's answer to a Call: the response, or why there was
// none.
type Result struct {
	Status int
	Body   []byte
	Err    string
}

var errCaptured = errors.New("lemonsqueezy: request captured")

type captureTransport struct{ call *Call }

func (t *captureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.call.Method = req.Method
	t.call.Path = req.URL.Path
	if req.URL.RawQuery != "" {
		t.call.Path += "?" + req.URL.RawQuery
	}
	t.call.Headers = map[string]string{}
	for _, h := range []string{"Accept", "Content-Type"} {
		if v := req.Header.Get(h); v != "" {
			t.call.Headers[h] = v
		}
	}
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
		return nil, errors.New("lemonsqueezy: no response")
	}
	return &http.Response{
		StatusCode: t.res.Status,
		Header:     http.Header{"Content-Type": []string{mediaType}},
		Body:       io.NopCloser(bytes.NewReader(t.res.Body)),
		Request:    req,
	}, nil
}

func clientWith(rt http.RoundTripper) *Client {
	remote := func(context.Context) (string, error) { return "remote", nil }
	c := New(Options{APIKey: remote, BaseURL: planBase})
	c.http = &http.Client{Transport: rt}
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
		return Call{}, errors.New("lemonsqueezy: operation sent no request")
	}
	return Call{}, fmt.Errorf("lemonsqueezy: plan request: %w", err)
}

// Decode runs op as if Lemon Squeezy had answered its request with res, so
// op's result and error (billing.ErrNotFound, an *APIError, a transport
// failure) are exactly what a direct call would have returned.
func Decode(res Result, op func(c *Client) error) error {
	return op(clientWith(replayTransport{res: res}))
}
