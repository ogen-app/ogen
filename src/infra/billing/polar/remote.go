package polar

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/ogen-app/ogen/src/domain/billing"
)

// Remote calls. A billing job running in river-service has it send the call
// with its own leased token. ogen builds the request and decodes the response
// by running the ordinary Client against a transport that captures the
// request (Plan) or answers it with river-service's result (Decode).

// Upstreams river-service sends Polar calls to: one per API host, both with
// the leased polar_access_token.
const (
	UpstreamProduction = "polar"
	UpstreamSandbox    = "polar_sandbox"
)

// planBase is the base URL a planned request is built against; only the
// path below it is kept.
const planBase = "http://polar.plan"

var errCaptured = errors.New("polar: request captured")

type captureTransport struct{ call *billing.Call }

func (t *captureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.call.Method = req.Method
	t.call.Path = req.URL.Path
	if req.URL.RawQuery != "" {
		t.call.Path += "?" + req.URL.RawQuery
	}
	t.call.Headers = map[string]string{}
	for _, h := range []string{"Accept", "Content-Type", versionHeader} {
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

type replayTransport struct{ res billing.CallResult }

func (t replayTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.res.Err != "" {
		return nil, errors.New(t.res.Err)
	}
	if t.res.Status == 0 {
		return nil, errors.New("polar: no response")
	}
	return &http.Response{
		StatusCode: t.res.Status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(t.res.Body)),
		Request:    req,
	}, nil
}

// clientWith is this client's configuration over another transport, with a
// placeholder token: river-service adds the real one.
func (c *Client) clientWith(rt http.RoundTripper) *Client {
	o := c.opts
	o.AccessToken = func(context.Context) (string, error) { return "remote", nil }
	o.BaseURL = planBase
	cl := New(o)
	cl.http = &http.Client{Transport: rt}
	return cl
}

// Upstream implements billing.Remote.
func (c *Client) Upstream() string {
	if c.testMode {
		return UpstreamSandbox
	}
	return UpstreamProduction
}

// Plan implements billing.Remote.
func (c *Client) Plan(op func(p billing.Provider) error) (billing.Call, error) {
	var call billing.Call
	err := op(c.clientWith(&captureTransport{call: &call}))
	if call.Path != "" {
		return call, nil
	}
	if err == nil {
		return billing.Call{}, errors.New("polar: operation sent no request")
	}
	return billing.Call{}, fmt.Errorf("polar: plan request: %w", err)
}

// Decode implements billing.Remote: op's result and error (billing.ErrNotFound,
// an *APIError, a transport failure) are exactly what a direct call would
// have returned.
func (c *Client) Decode(res billing.CallResult, op func(p billing.Provider) error) error {
	return op(c.clientWith(replayTransport{res: res}))
}
