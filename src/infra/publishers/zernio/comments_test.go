package zernio

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPostComment(t *testing.T) {
	var gotPath, gotKey string
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.Method + " " + r.URL.Path
		gotKey = r.Header.Get("Idempotency-Key")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"success":true,"data":{"commentId":"c-9","isReply":false}}`))
	}))
	defer srv.Close()
	c := NewClient(StaticKey("k"), srv.URL, ClientOpts{Timeout: 5 * time.Second})

	id, err := c.PostComment(t.Context(), "z-1", "acc-1", "Link: https://x.y", "first-comment:p-1")
	if err != nil {
		t.Fatal(err)
	}
	if id != "c-9" {
		t.Errorf("comment id = %q, want c-9", id)
	}
	if gotPath != "POST /inbox/comments/z-1" {
		t.Errorf("request = %q", gotPath)
	}
	if gotKey != "first-comment:p-1" {
		t.Errorf("Idempotency-Key = %q", gotKey)
	}
	if gotBody["accountId"] != "acc-1" || gotBody["message"] != "Link: https://x.y" || len(gotBody) != 2 {
		t.Errorf("body = %v", gotBody)
	}
}

func TestIsTransientCommentError(t *testing.T) {
	cases := map[int]bool{
		http.StatusConflict:            true, // same idempotency key still in flight
		http.StatusTooManyRequests:     true,
		http.StatusBadGateway:          true,
		http.StatusForbidden:           false,
		http.StatusUnprocessableEntity: false,
	}
	for status, want := range cases {
		if got := IsTransientCommentError(&APIError{Status: status}); got != want {
			t.Errorf("%d: transient = %v, want %v", status, got, want)
		}
	}
	if !IsTransientCommentError(errors.New("connection reset")) {
		t.Error("a network error should be retried")
	}
}
