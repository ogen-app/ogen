package zernio

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPlanCapturesTheRequest(t *testing.T) {
	call, err := Plan(func(c *Client) error {
		_, err := c.CreateProfile(context.Background(), "ogen-dev-t1", "managed")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if call.Method != http.MethodPost || call.Path != "/profiles" {
		t.Fatalf("call = %s %s", call.Method, call.Path)
	}
	var body map[string]any
	if err := json.Unmarshal(call.Body, &body); err != nil || body["name"] != "ogen-dev-t1" {
		t.Errorf("body = %s, %v", call.Body, err)
	}
	if _, ok := call.Headers["Authorization"]; ok {
		t.Error("planned call carries an Authorization header; river-service adds its own")
	}
}

func TestPlanCapturesQueryAndPath(t *testing.T) {
	call, err := Plan(func(c *Client) error {
		_, err := c.GetAccountsHealth(context.Background(), "prof-1")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if call.Method != http.MethodGet || call.Path == "" {
		t.Fatalf("call = %+v", call)
	}
	if call.Path+call.Query.Encode() == "" {
		t.Error("empty request")
	}
}

func TestDecodeReturnsTheTypedResult(t *testing.T) {
	var got *Profile
	err := Decode(Result{Status: 200, Body: []byte(`{"profile":{"_id":"p1","name":"ogen-dev-t1","createdAt":"2026-10-14T10:00:00Z"}}`)}, func(c *Client) error {
		var err error
		got, err = c.GetProfile(context.Background(), "p1")
		return err
	})
	if err != nil || got == nil || got.ID != "p1" {
		t.Fatalf("Decode = %+v, %v", got, err)
	}

	err = Decode(Result{Status: 404, Body: []byte(`{"error":"not found"}`)}, func(c *Client) error {
		_, err := c.GetProfile(context.Background(), "p1")
		return err
	})
	if !IsStatus(err, http.StatusNotFound) {
		t.Errorf("404 = %v, want an APIError with status 404", err)
	}

	err = Decode(Result{Err: "dial tcp: timeout"}, func(c *Client) error {
		_, err := c.GetProfile(context.Background(), "p1")
		return err
	})
	if err == nil {
		t.Error("network error decoded as success")
	}
}

func TestSendMakesThePlannedCall(t *testing.T) {
	var got *http.Request
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":1}`))
	}))
	t.Cleanup(srv.Close)
	call, err := Plan(func(c *Client) error {
		_, err := c.PostComment(t.Context(), "z-1", "acc-1", "hi", "first-comment:p1")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	res, missing := NewClient(StaticKey("zk"), srv.URL+"/api/v1", ClientOpts{}).Send(t.Context(), call)
	if missing || res.Status != 201 || string(res.Body) != `{"ok":1}` {
		t.Fatalf("Send = %+v, missing %v", res, missing)
	}
	if got.URL.Path != "/api/v1/inbox/comments/z-1" || got.Header.Get("Authorization") != "Bearer zk" ||
		got.Header.Get("Idempotency-Key") != "first-comment:p1" || body == "" {
		t.Errorf("request %s %s %v %s", got.Method, got.URL, got.Header, body)
	}

	var disabled *Client
	if _, missing := disabled.Send(t.Context(), call); !missing {
		t.Error("a disabled client sent the call")
	}
}

func TestReplayPagesThroughAList(t *testing.T) {
	find := func(c *Client) error {
		_, err := c.FindByContent(context.Background(), "hello", time.Hour)
		return err
	}
	var found *Job
	findInto := func(c *Client) error {
		var err error
		found, err = c.FindByContent(context.Background(), "hello", time.Hour)
		return err
	}
	next, err := Replay(nil, find)
	if err != nil || next == nil || next.Query.Get("page") != "1" {
		t.Fatalf("first = %+v, %v", next, err)
	}
	page1 := Result{Status: 200, Body: []byte(`{"posts":[{"_id":"a","content":"other"}],"pagination":{"pages":2}}`)}
	next, err = Replay([]Result{page1}, find)
	if err != nil || next == nil || next.Query.Get("page") != "2" {
		t.Fatalf("second = %+v, %v", next, err)
	}
	page2 := Result{Status: 200, Body: []byte(`{"posts":[{"_id":"b","content":"hello"}],"pagination":{"pages":2}}`)}
	next, err = Replay([]Result{page1, page2}, findInto)
	if err != nil || next != nil || found == nil || found.ID != "b" {
		t.Fatalf("done = %+v, %+v, %v", next, found, err)
	}
}
