package zernio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// Remote calls. Zernio jobs run in river-service, which sends every Zernio
// request with its own leased key. ogen still builds each request and decodes
// each response, by running the ordinary Client methods against a transport
// that captures the request (Plan) or answers it with river-service's result
// (Decode). So request building and decoding have exactly one implementation.

// planBase is the base URL a planned request is built against; only its path
// is kept.
const planBase = "http://zernio.plan"

// Call is one Zernio API request for river-service to send. Path is relative
// to the API base URL. river-service adds the Authorization header.
type Call struct {
	Method  string
	Path    string
	Query   url.Values
	Headers map[string]string
	Body    []byte
}

// Result is river-service's answer to a Call: the response's status and body,
// or why there was no response.
type Result struct {
	Status int
	Body   []byte
	Err    string
}

// errCaptured stops a planned request before it is sent.
var errCaptured = errors.New("zernio: request captured")

// standardHeaders are the ones the Client sets on every request; river-service
// sets its own, so they are not part of a Call.
var standardHeaders = map[string]bool{"Authorization": true, "Accept": true, "Content-Type": true}

type captureTransport struct{ call *Call }

func (t *captureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.call.Method = req.Method
	t.call.Path = req.URL.Path
	t.call.Query = req.URL.Query()
	for k := range req.Header {
		if !standardHeaders[k] {
			if t.call.Headers == nil {
				t.call.Headers = map[string]string{}
			}
			t.call.Headers[k] = req.Header.Get(k)
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
		return nil, errors.New("zernio: no response")
	}
	return &http.Response{
		StatusCode: t.res.Status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(t.res.Body)),
		Request:    req,
	}, nil
}

func clientWith(rt http.RoundTripper) *Client {
	c := NewClient(StaticKey("remote"), planBase, ClientOpts{})
	c.httpClient = &http.Client{Transport: rt}
	c.uploadClient = &http.Client{Transport: rt}
	return c
}

// Plan returns the request op would send, without sending it. op runs one
// Client method; an operation that sends no request is an error.
func Plan(op func(c *Client) error) (Call, error) {
	var call Call
	err := op(clientWith(&captureTransport{call: &call}))
	if errors.Is(err, errCaptured) {
		return call, nil
	}
	if err == nil {
		return Call{}, errors.New("zernio: operation sent no request")
	}
	return Call{}, fmt.Errorf("zernio: plan request: %w", err)
}

// Replay runs a read-only op that makes several requests (paging through a
// list): prior answers its first requests in order, and the first request
// past them is captured and returned instead of sent. A nil call means op
// finished on prior alone, with err its result.
func Replay(prior []Result, op func(c *Client) error) (next *Call, err error) {
	rt := &sequenceTransport{prior: prior, capture: &captureTransport{call: &Call{}}}
	err = op(clientWith(rt))
	if errors.Is(err, errCaptured) {
		return rt.capture.call, nil
	}
	return nil, err
}

type sequenceTransport struct {
	prior   []Result
	n       int
	capture *captureTransport
}

func (t *sequenceTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.n < len(t.prior) {
		res := t.prior[t.n]
		t.n++
		return replayTransport{res: res}.RoundTrip(req)
	}
	return t.capture.RoundTrip(req)
}

// Decode runs op as if Zernio had answered its request with res, so op's
// typed result and error (an *APIError for a non-2xx status) are exactly what
// a direct call would have returned.
func Decode(res Result, op func(c *Client) error) error {
	return op(clientWith(replayTransport{res: res}))
}

// Send makes a planned Call with this client's own key and returns what
// river-service would have: the response, or why there was none. It runs the
// step-run jobs ogen still works itself. keyMissing reports a disabled client
// or no key set, in which case nothing is sent.
func (c *Client) Send(ctx context.Context, call Call) (res Result, keyMissing bool) {
	if c == nil || c.keyResolver == nil {
		return Result{}, true
	}
	key, err := c.keyResolver(ctx)
	if err != nil {
		return Result{Err: "zernio: resolve api key: " + err.Error()}, false
	}
	if key == "" {
		return Result{}, true
	}
	u := c.baseURL + call.Path
	if len(call.Query) > 0 {
		u += "?" + call.Query.Encode()
	}
	var body io.Reader
	if len(call.Body) > 0 {
		body = bytes.NewReader(call.Body)
	}
	req, err := http.NewRequestWithContext(ctx, call.Method, u, body)
	if err != nil {
		return Result{Err: err.Error()}, false
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range call.Headers {
		req.Header.Set(k, v)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return Result{Err: err.Error()}, false
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, sendBodyLimit))
	if err != nil {
		return Result{Status: resp.StatusCode, Err: "zernio: read response: " + err.Error()}, false
	}
	return Result{Status: resp.StatusCode, Body: b}, false
}

// sendBodyLimit bounds a response Send reads, as river-service bounds the
// ones it hands back.
const sendBodyLimit = 32 << 20
