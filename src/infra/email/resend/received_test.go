package resend

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func receivingServer(t *testing.T, raw string) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/emails/receiving/em-1":
			if r.Header.Get("Authorization") != "Bearer key" {
				t.Errorf("auth = %q", r.Header.Get("Authorization"))
			}
			_, _ = w.Write([]byte(`{"object":"email","id":"em-1","raw":{"download_url":"` + srv.URL + `/raw/em-1?Signature=x","expires_at":"2026-10-28T12:00:00Z"}}`))
		case "/raw/em-1":
			if r.Header.Get("Authorization") != "" {
				t.Error("the signed link was sent the API key")
			}
			_, _ = w.Write([]byte(raw))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"name":"not_found","message":"Email not found"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestGetReceivedRaw(t *testing.T) {
	srv := receivingServer(t, "From: a@b.com\r\n\r\nhi")
	c := New(staticKey("key"), srv.URL, time.Second)

	raw, err := c.GetReceivedRaw(t.Context(), "em-1", 1<<20)
	if err != nil || !strings.HasPrefix(string(raw), "From: a@b.com") {
		t.Fatalf("raw = %q, %v", raw, err)
	}
	if _, err := c.GetReceivedRaw(t.Context(), "em-1", 5); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("over the limit: %v", err)
	}
	if _, err := c.GetReceivedRaw(t.Context(), "gone", 1<<20); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := New(staticKey(""), srv.URL, time.Second).GetReceivedRaw(t.Context(), "em-1", 1<<20); !errors.Is(err, ErrDisabled) {
		t.Fatalf("no key: %v", err)
	}
	var nilClient *Client
	if _, err := nilClient.GetReceivedRaw(t.Context(), "em-1", 1<<20); !errors.Is(err, ErrDisabled) {
		t.Fatalf("nil client: %v", err)
	}
}
