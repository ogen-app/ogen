package linkpreview

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func allowAll(context.Context, string) error { return nil }

// newTestService points the service at srv. httptest serves on 127.0.0.1,
// which ValidateLink rejects for its missing dot, so requests go to a dotted
// host that the transport rewrites to the test server.
func newTestService(t *testing.T, srv *httptest.Server) *Service {
	t.Helper()
	client := srv.Client()
	base := client.Transport
	client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		out := r.Clone(r.Context())
		out.URL.Scheme = "http"
		out.URL.Host = strings.TrimPrefix(srv.URL, "http://")
		resp, err := base.RoundTrip(out)
		if resp != nil {
			resp.Request = r
		}
		return resp, err
	})
	return New(client, allowAll)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGetReadsOpenGraph(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><html><head>
			<title>Fallback title</title>
			<meta property="og:title" content="  The   launch ">
			<meta name="twitter:description" content="Twitter description">
			<meta property="og:image" content="/img/card.png">
			<meta property="og:site_name" content="Example">
			</head><body><meta property="og:description" content="ignored after body"></body></html>`))
	}))
	defer srv.Close()

	p, err := newTestService(t, srv).Get(t.Context(), "https://www.example.com/post#section")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if p.URL != "https://www.example.com/post" {
		t.Errorf("url: got %q, want the fragment dropped", p.URL)
	}
	if p.Domain != "example.com" || p.Title != "The launch" || p.SiteName != "Example" {
		t.Errorf("unexpected preview %+v", p)
	}
	if p.Description != "Twitter description" {
		t.Errorf("description: got %q, want the twitter fallback", p.Description)
	}
	if p.ImageURL != "https://www.example.com/img/card.png" {
		t.Errorf("image: got %q, want it resolved against the page", p.ImageURL)
	}
}

func TestGetFallsBackToTitleTag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><head><title>Plain page</title><meta name="description" content="Desc"></head></html>`))
	}))
	defer srv.Close()

	p, err := newTestService(t, srv).Get(t.Context(), "https://example.com")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if p.Title != "Plain page" || p.Description != "Desc" || p.ImageURL != "" {
		t.Errorf("unexpected preview %+v", p)
	}
}

func TestGetDegradesToDomainAndCaches(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()

	svc := newTestService(t, srv)
	now := time.Now()
	svc.now = func() time.Time { return now }
	for range 2 {
		p, err := svc.Get(t.Context(), "https://example.com/broken")
		if err != nil {
			t.Fatalf("a failed fetch must not error: %v", err)
		}
		if p.Domain != "example.com" || p.Title != "" {
			t.Errorf("want a domain-only preview, got %+v", p)
		}
	}
	if hits.Load() != 1 {
		t.Errorf("want the failure cached, got %d fetches", hits.Load())
	}
	now = now.Add(failureTTL + time.Second)
	if _, err := svc.Get(t.Context(), "https://example.com/broken"); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 2 {
		t.Errorf("want a refetch after the failure TTL, got %d fetches", hits.Load())
	}
}

func TestGetIgnoresNonHTML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write([]byte(`<meta property="og:title" content="not html">`))
	}))
	defer srv.Close()

	p, err := newTestService(t, srv).Get(t.Context(), "https://example.com/file.pdf")
	if err != nil || p.Title != "" {
		t.Fatalf("want a domain-only preview, got %+v, %v", p, err)
	}
}

func TestGetRejectsInvalidAndBlockedURLs(t *testing.T) {
	blocked := New(http.DefaultClient, func(context.Context, string) error { return errors.New("private") })
	if _, err := blocked.Get(t.Context(), "https://internal.example.com"); !errors.Is(err, ErrInvalidURL) {
		t.Errorf("blocked host: want ErrInvalidURL, got %v", err)
	}
	svc := New(http.DefaultClient, allowAll)
	for _, raw := range []string{"", "example.com", "file:///etc/passwd", "http://localhost:8080"} {
		if _, err := svc.Get(t.Context(), raw); !errors.Is(err, ErrInvalidURL) {
			t.Errorf("%q: want ErrInvalidURL, got %v", raw, err)
		}
	}
}
