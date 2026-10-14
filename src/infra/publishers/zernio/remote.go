package zernio

import (
	"bytes"
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

// Decode runs op as if Zernio had answered its request with res, so op's
// typed result and error (an *APIError for a non-2xx status) are exactly what
// a direct call would have returned.
func Decode(res Result, op func(c *Client) error) error {
	return op(clientWith(replayTransport{res: res}))
}
