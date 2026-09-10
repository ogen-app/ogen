package zernio

import (
	"net/http"
	"testing"
)

func TestDeleteAccountHappyPath(t *testing.T) {
	s := newStub()
	defer s.Close()

	var seenPath string
	s.handle("DELETE", "/accounts/acc-1", func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		writeJSON(w, http.StatusOK, map[string]string{"message": "Account disconnected successfully"})
	})

	if err := newClient(s).DeleteAccount(t.Context(), "acc-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if seenPath != "/accounts/acc-1" {
		t.Errorf("path: got %q want /accounts/acc-1", seenPath)
	}
}

// A 404 upstream must surface as *APIError{Status:404} so the handler can treat
// an already-gone account as an idempotent no-op rather than a hard failure.
func TestDeleteAccountNotFoundIsTypedStatus(t *testing.T) {
	s := newStub()
	defer s.Close()

	s.handle("DELETE", "/accounts/gone", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Not found"})
	})

	err := newClient(s).DeleteAccount(t.Context(), "gone")
	if err == nil {
		t.Fatal("expected error for 404")
	}
	if !IsStatus(err, http.StatusNotFound) {
		t.Errorf("want IsStatus 404, got %v", err)
	}
}

// Any non-404 upstream failure must remain a distinguishable error (not 404) so
// the handler maps it to 502 without soft-deleting locally.
func TestDeleteAccountServerErrorIsNot404(t *testing.T) {
	s := newStub()
	defer s.Close()

	s.handle("DELETE", "/accounts/acc-1", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "boom"})
	})

	err := newClient(s).DeleteAccount(t.Context(), "acc-1")
	if err == nil {
		t.Fatal("expected error for 500")
	}
	if IsStatus(err, http.StatusNotFound) {
		t.Error("500 must not read as 404")
	}
	if !IsStatus(err, http.StatusInternalServerError) {
		t.Errorf("want IsStatus 500, got %v", err)
	}
}

func TestDeleteAccountDisabledClient(t *testing.T) {
	var c *Client // nil == integration disabled
	if err := c.DeleteAccount(t.Context(), "acc-1"); err == nil {
		t.Fatal("nil client should error")
	}
}
