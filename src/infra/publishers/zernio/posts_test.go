package zernio

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// stub is the minimal route-table HTTP server reused across the
// post-related client tests. Same shape as the package's existing
// integration tests use elsewhere.
type stub struct {
	*httptest.Server
	mu     sync.Mutex
	routes map[string]http.HandlerFunc
}

func newStub() *stub {
	s := &stub{routes: map[string]http.HandlerFunc{}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		h, ok := s.routes[r.Method+" "+r.URL.Path]
		s.mu.Unlock()
		if !ok {
			http.Error(w, "stub: unhandled "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		h(w, r)
	}))
	return s
}

func (s *stub) handle(method, path string, h http.HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routes[method+" "+path] = h
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func newClient(s *stub) *Client {
	return NewClient(StaticKey("key"), s.URL, ClientOpts{Timeout: 5 * time.Second})
}

func TestSubmitHappyPath(t *testing.T) {
	s := newStub()
	defer s.Close()

	s.handle("POST", "/posts", func(w http.ResponseWriter, r *http.Request) {
		var req SubmitRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Content != "hello" {
			t.Errorf("content: got %q want hello", req.Content)
		}
		writeJSON(w, http.StatusCreated, PostEnvelope{Post: Job{
			ID:     "post-1",
			Status: JobStatusScheduled,
		}})
	})

	c := newClient(s)
	job, err := c.Submit(t.Context(), SubmitRequest{Content: "hello", Platforms: []PlatformVariant{{Platform: "linkedin", AccountID: "acc1"}}})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if job.ID != "post-1" {
		t.Errorf("id: got %q want post-1", job.ID)
	}
	if job.Status != JobStatusScheduled {
		t.Errorf("status: got %q want scheduled", job.Status)
	}
}

func TestSubmit409ReturnsErrDuplicate(t *testing.T) {
	s := newStub()
	defer s.Close()
	s.handle("POST", "/posts", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "duplicate content within 24h"})
	})
	c := newClient(s)
	_, err := c.Submit(t.Context(), SubmitRequest{Content: "x"})
	if !errors.Is(err, ErrDuplicateContent) {
		t.Fatalf("expected ErrDuplicateContent, got %v", err)
	}
}

func TestStatusReturnsTerminalEnum(t *testing.T) {
	s := newStub()
	defer s.Close()
	s.handle("GET", "/posts/abc", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, PostEnvelope{Post: Job{
			ID: "abc", Status: JobStatusPublished,
			Platforms: []PlatformOutcome{{Platform: "linkedin", Status: "published", PlatformPostURL: "https://linkedin.com/posts/x"}},
		}})
	})
	c := newClient(s)
	job, err := c.Status(t.Context(), "abc")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !job.Status.IsTerminal() {
		t.Errorf("expected terminal, got %q", job.Status)
	}
	if job.Platforms[0].PlatformPostURL == "" {
		t.Errorf("missing platformPostUrl")
	}
}

func TestCancelHappyPath(t *testing.T) {
	s := newStub()
	defer s.Close()
	s.handle("DELETE", "/posts/abc", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	c := newClient(s)
	if err := c.Cancel(t.Context(), "abc"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
}

func TestCancel404TreatedAsAlreadyPublished(t *testing.T) {
	s := newStub()
	defer s.Close()
	s.handle("DELETE", "/posts/gone", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	c := newClient(s)
	err := c.Cancel(t.Context(), "gone")
	if !errors.Is(err, ErrAlreadyPublished) {
		t.Fatalf("expected ErrAlreadyPublished, got %v", err)
	}
}

func TestCancel409TreatedAsAlreadyPublished(t *testing.T) {
	s := newStub()
	defer s.Close()
	s.handle("DELETE", "/posts/raced", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "already published"})
	})
	c := newClient(s)
	err := c.Cancel(t.Context(), "raced")
	if !errors.Is(err, ErrAlreadyPublished) {
		t.Fatalf("expected ErrAlreadyPublished, got %v", err)
	}
}

func TestRetryHappyPath(t *testing.T) {
	s := newStub()
	defer s.Close()
	s.handle("POST", "/posts/abc/retry", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, PostEnvelope{Post: Job{ID: "abc", Status: JobStatusScheduled}})
	})
	c := newClient(s)
	job, err := c.Retry(t.Context(), "abc")
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if job.Status != JobStatusScheduled {
		t.Errorf("expected scheduled, got %q", job.Status)
	}
}

func TestFindByContentRecoversAfterDedupe(t *testing.T) {
	s := newStub()
	defer s.Close()
	s.handle("GET", "/posts", func(w http.ResponseWriter, r *http.Request) {
		// Verify the date filter went out (we don't pin the value).
		if r.URL.Query().Get("dateFrom") == "" {
			t.Error("dateFrom should be present")
		}
		// The status filter must be dropped so the search spans every status
		// the earlier job could be in (CON-129).
		if got := r.URL.Query().Get("status"); got != "" {
			t.Errorf("status filter should be dropped, got %q", got)
		}
		writeJSON(w, http.StatusOK, listEnvelope{Posts: []Job{
			{ID: "stale", Content: "other"},
			{ID: "match", Content: "hello", Status: JobStatusPublished},
		}})
	})
	c := newClient(s)
	job, err := c.FindByContent(t.Context(), "hello", time.Hour)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	// A match is returned regardless of status (here: published), carrying its
	// status so the caller can decide to adopt or report.
	if job == nil || job.ID != "match" || job.Status != JobStatusPublished {
		t.Errorf("expected published match, got %+v", job)
	}
}

func TestFindByContentPaginatesPastFirstPage(t *testing.T) {
	s := newStub()
	defer s.Close()
	s.handle("GET", "/posts", func(w http.ResponseWriter, r *http.Request) {
		var env listEnvelope
		switch r.URL.Query().Get("page") {
		case "1":
			env.Posts = []Job{{ID: "p1", Content: "other"}}
			env.Pagination.Pages = 2
		case "2":
			env.Posts = []Job{{ID: "match", Content: "hello"}}
			env.Pagination.Pages = 2
		default:
			t.Errorf("unexpected page %q", r.URL.Query().Get("page"))
		}
		writeJSON(w, http.StatusOK, env)
	})
	c := newClient(s)
	job, err := c.FindByContent(t.Context(), "hello", time.Hour)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if job == nil || job.ID != "match" {
		t.Errorf("expected the page-2 match, got %+v", job)
	}
}

func TestPlatformOutcomeAccountIDTolerantOfObject(t *testing.T) {
	// Zernio populates `accountId` as either a bare ObjectId string or a
	// full account sub-document depending on whether the upstream query
	// populates the reference. Both must decode down to the string ID —
	// the object shape previously crashed the submit flow at the
	// FindByContent dedupe-recovery decode.
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bare string", `{"platform":"linkedin","accountId":"acc-1","status":"published"}`, "acc-1"},
		{"populated object", `{"platform":"linkedin","accountId":{"_id":"acc-1","username":"foo"},"status":"published"}`, "acc-1"},
		{"absent", `{"platform":"linkedin","status":"failed","error":"boom"}`, ""},
		{"null", `{"platform":"linkedin","accountId":null,"status":"failed"}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var o PlatformOutcome
			if err := json.Unmarshal([]byte(tc.in), &o); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if o.AccountID != tc.want {
				t.Errorf("accountId: got %q want %q", o.AccountID, tc.want)
			}
			if o.Platform != "linkedin" {
				t.Errorf("platform: got %q want linkedin", o.Platform)
			}
		})
	}
}

func TestFindByContentDecodesPopulatedAccount(t *testing.T) {
	// Reproduces the reported failure: a listed post whose per-platform
	// outcome carries a populated `accountId` object must decode cleanly
	// through the listEnvelope path rather than aborting the submit job.
	s := newStub()
	defer s.Close()
	s.handle("GET", "/posts", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, json.RawMessage(`{"posts":[
			{"_id":"match","content":"hello","status":"published",
			 "platforms":[{"platform":"linkedin","accountId":{"_id":"acc-9","username":"foo"},"status":"published"}]}
		]}`))
	})
	c := newClient(s)
	job, err := c.FindByContent(t.Context(), "hello", time.Hour)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if job == nil || job.ID != "match" {
		t.Fatalf("expected match, got %+v", job)
	}
	if job.Platforms[0].AccountID != "acc-9" {
		t.Errorf("accountId: got %q want acc-9", job.Platforms[0].AccountID)
	}
}

func TestIsTerminalAPIError(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{&APIError{Status: 400}, true},
		{&APIError{Status: 422}, true},
		{&APIError{Status: 429}, false},
		{&APIError{Status: 500}, false},
		{nil, false},
		{errors.New("network"), false},
	}
	for _, tc := range cases {
		if got := IsTerminalAPIError(tc.err); got != tc.want {
			t.Errorf("IsTerminalAPIError(%v): got %v want %v", tc.err, got, tc.want)
		}
	}
}

func TestIsTransientAPIError(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{&APIError{Status: 500}, true},
		{&APIError{Status: 429}, true},
		{&APIError{Status: 400}, false},
		{nil, false},
		{errors.New("network"), true},
	}
	for _, tc := range cases {
		if got := IsTransientAPIError(tc.err); got != tc.want {
			t.Errorf("IsTransientAPIError(%v): got %v want %v", tc.err, got, tc.want)
		}
	}
}

func TestSubmit400IsTerminal(t *testing.T) {
	// Sanity: a 400 from Submit is NOT auto-recovered, comes through as
	// a terminal APIError so the queue layer can resolve Failed.
	s := newStub()
	defer s.Close()
	s.handle("POST", "/posts", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "platforms required"})
	})
	c := newClient(s)
	_, err := c.Submit(t.Context(), SubmitRequest{Content: "x"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !IsTerminalAPIError(err) {
		t.Fatalf("expected terminal, got %v", err)
	}
	if !strings.Contains(err.Error(), "platforms required") {
		t.Errorf("error message should include zernio's reason: %v", err)
	}
}
