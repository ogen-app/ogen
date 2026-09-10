package firecrawl

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func staticKey(k string) KeyResolver {
	return func(_ context.Context) (string, error) { return k, nil }
}

func TestScrape_Success(t *testing.T) {
	var gotAuth, gotPath string
	var gotBody scrapeBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		_, _ = w.Write([]byte(`{"success":true,"data":{"markdown":"# Hello","metadata":{"title":"Hi","description":"d","sourceURL":"https://ex.com/final","statusCode":200,"contentType":"text/html"}}}`))
	}))
	defer srv.Close()

	c := New(staticKey("secret-key"), srv.URL, 0)
	res, err := c.Scrape(t.Context(), ScrapeRequest{URL: "https://ex.com/x"})
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	if gotAuth != "Bearer secret-key" {
		t.Fatalf("auth header = %q, want Bearer secret-key", gotAuth)
	}
	if gotPath != "/v2/scrape" {
		t.Fatalf("path = %q, want /v2/scrape", gotPath)
	}
	if len(gotBody.Formats) != 1 || gotBody.Formats[0] != "markdown" || !gotBody.OnlyMainContent {
		t.Fatalf("request body not as expected: %+v", gotBody)
	}
	if res.Markdown != "# Hello" || res.Title != "Hi" || res.SourceURL != "https://ex.com/final" {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestScrape_FlexStringTitleArray(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"success":true,"data":{"markdown":"x","metadata":{"title":["A","B"]}}}`))
	}))
	defer srv.Close()

	res, err := New(staticKey("k"), srv.URL, 0).Scrape(t.Context(), ScrapeRequest{URL: "https://ex.com"})
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	if !strings.Contains(res.Title, "A") || !strings.Contains(res.Title, "B") {
		t.Fatalf("array title should be joined, got %q", res.Title)
	}
}

func TestScrape_TerminalAndTransientClassification(t *testing.T) {
	cases := []struct {
		status    int
		transient bool
	}{
		{http.StatusNotFound, false},
		{http.StatusBadRequest, false},
		{http.StatusUnauthorized, false},
		{http.StatusTooManyRequests, true},
		{http.StatusInternalServerError, true},
		{http.StatusBadGateway, true},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(`{"error":"boom"}`))
		}))
		_, err := New(staticKey("k"), srv.URL, 0).Scrape(t.Context(), ScrapeRequest{URL: "https://ex.com"})
		srv.Close()
		if err == nil {
			t.Fatalf("status %d: expected an error", tc.status)
		}
		if IsTransient(err) != tc.transient {
			t.Fatalf("status %d: IsTransient=%v, want %v", tc.status, IsTransient(err), tc.transient)
		}
	}
}

func TestScrape_SuccessFalseIsTerminal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"success":false,"error":"could not scrape"}`))
	}))
	defer srv.Close()

	_, err := New(staticKey("k"), srv.URL, 0).Scrape(t.Context(), ScrapeRequest{URL: "https://ex.com"})
	if err == nil {
		t.Fatal("success:false should be an error")
	}
	if IsTransient(err) {
		t.Fatal("success:false should be terminal, not transient")
	}
}

func TestHasKey(t *testing.T) {
	if New(staticKey(""), "", 0).HasKey(t.Context()) {
		t.Fatal("empty key should report HasKey=false")
	}
	if !New(staticKey("k"), "", 0).HasKey(t.Context()) {
		t.Fatal("non-empty key should report HasKey=true")
	}
}

func TestNew_NilResolverDisabled(t *testing.T) {
	if New(nil, "", 0) != nil {
		t.Fatal("nil resolver should return a nil (disabled) client")
	}
}
