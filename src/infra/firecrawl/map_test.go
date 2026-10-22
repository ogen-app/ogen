package firecrawl

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMap_ReadsObjectAndStringLinks(t *testing.T) {
	var gotPath string
	var gotBody mapBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		_, _ = w.Write([]byte(`{"success":true,"links":[
			{"url":"https://ex.com/about","title":"About us","description":"d"},
			"https://ex.com/pricing",
			{"url":"","title":"dropped"}
		]}`))
	}))
	defer srv.Close()

	links, err := New(staticKey("k"), srv.URL, 0).Map(t.Context(), MapRequest{URL: "https://ex.com", Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v2/map" || gotBody.URL != "https://ex.com" || gotBody.Limit != 200 || gotBody.IncludeSubdomains {
		t.Fatalf("request = %s %+v", gotPath, gotBody)
	}
	want := []Link{{URL: "https://ex.com/about", Title: "About us", Description: "d"}, {URL: "https://ex.com/pricing"}}
	if len(links) != len(want) || links[0] != want[0] || links[1] != want[1] {
		t.Fatalf("links = %+v", links)
	}
}

func TestMap_Failures(t *testing.T) {
	for _, tt := range []struct {
		name      string
		status    int
		body      string
		transient bool
	}{
		{"unsuccessful is terminal", 200, `{"success":false,"error":"blocked"}`, false},
		{"server error is transient", 502, `{"error":"upstream"}`, true},
		{"bad request is terminal", 400, `{"error":"bad url"}`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()
			_, err := New(staticKey("k"), srv.URL, 0).Map(t.Context(), MapRequest{URL: "https://ex.com"})
			if err == nil || IsTransient(err) != tt.transient {
				t.Fatalf("err = %v, transient = %v", err, IsTransient(err))
			}
		})
	}
}
