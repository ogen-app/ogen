package zernio

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
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
