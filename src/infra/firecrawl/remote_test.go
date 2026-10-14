package firecrawl

import (
	"context"
	"encoding/json"
	"testing"
)

func TestPlanAndDecodeAScrape(t *testing.T) {
	maxAge := 0
	scrape := func(out **ScrapeResult) func(*Client) error {
		return func(c *Client) error {
			res, err := c.Scrape(context.Background(), ScrapeRequest{URL: "https://example.com", MaxAge: &maxAge})
			if out != nil {
				*out = res
			}
			return err
		}
	}
	call, err := Plan(scrape(nil))
	if err != nil {
		t.Fatal(err)
	}
	var body scrapeBody
	if call.Method != "POST" || call.Path != "/v2/scrape" || json.Unmarshal(call.Body, &body) != nil || body.URL != "https://example.com" || *body.MaxAge != 0 {
		t.Fatalf("call = %+v", call)
	}

	var got *ScrapeResult
	ok := Result{Status: 200, Body: []byte(`{"success":true,"data":{"markdown":"# Hi","metadata":{"title":"Hi","statusCode":200}}}`)}
	if err := Decode(ok, scrape(&got)); err != nil || got.Markdown != "# Hi" || got.Title != "Hi" {
		t.Fatalf("decode = %+v, %v", got, err)
	}
	if err := Decode(Result{Status: 429, Body: []byte(`{"error":"slow down"}`)}, scrape(nil)); !IsTransient(err) {
		t.Errorf("429 = %v, want transient", err)
	}
	if err := Decode(Result{Status: 403, Body: []byte(`{"error":"blocked"}`)}, scrape(nil)); err == nil || IsTransient(err) {
		t.Errorf("403 = %v, want terminal", err)
	}
	if err := Decode(Result{Err: "dial tcp: timeout"}, scrape(nil)); !IsTransient(err) {
		t.Errorf("no response = %v, want transient", err)
	}
}
