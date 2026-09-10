package resend

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/infra/email"
)

func staticKey(k string) KeyResolver {
	return func(context.Context) (string, error) { return k, nil }
}

func TestSendSuccess(t *testing.T) {
	var gotAuth, gotIdem string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotIdem = r.Header.Get("Idempotency-Key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_123"}`))
	}))
	defer srv.Close()

	c := New(staticKey("key"), srv.URL, time.Second)
	id, err := c.Send(t.Context(), email.Message{To: "a@b.com", Subject: "hi", HTML: "<p>hi</p>", IdempotencyKey: "welcome:u1"})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if id != "msg_123" {
		t.Fatalf("id: got %q, want msg_123", id)
	}
	if gotAuth != "Bearer key" {
		t.Fatalf("auth header: got %q", gotAuth)
	}
	if gotIdem != "welcome:u1" {
		t.Fatalf("idempotency header: got %q", gotIdem)
	}
}

func TestSendTerminal4xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"invalid to","name":"validation_error"}`))
	}))
	defer srv.Close()

	c := New(staticKey("key"), srv.URL, time.Second)
	_, err := c.Send(t.Context(), email.Message{To: "bad", Subject: "s"})
	if err == nil {
		t.Fatal("want error")
	}
	if email.IsTransient(err) {
		t.Fatal("422 should be terminal, not transient")
	}
	if email.IsDisabled(err) {
		t.Fatal("422 should not be disabled")
	}
}

func TestSendTransient5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New(staticKey("key"), srv.URL, time.Second)
	_, err := c.Send(t.Context(), email.Message{To: "a@b.com", Subject: "s"})
	if !email.IsTransient(err) {
		t.Fatalf("500 should be transient; got %v", err)
	}
}

func TestSendDisabledNoKey(t *testing.T) {
	c := New(staticKey(""), "http://unused", time.Second)
	_, err := c.Send(t.Context(), email.Message{To: "a@b.com", Subject: "s"})
	if !email.IsDisabled(err) {
		t.Fatalf("empty key should be disabled; got %v", err)
	}
}
