package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"
	"unicode/utf8"

	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// newTestLogger returns a JSON logger writing to buf, wrapped in the
// ContextHandler under test, at debug level so nothing is filtered.
func newTestLogger(buf *bytes.Buffer) *slog.Logger {
	base := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(ContextHandler{Handler: base})
}

func decode(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("log line is not valid JSON: %v\nline: %s", err, buf.String())
	}
	return m
}

func TestContextHandlerEnrichesFromContext(t *testing.T) {
	var buf bytes.Buffer
	logger := newTestLogger(&buf)

	ctx := t.Context()
	ctx = WithRequestID(ctx, "req-123")
	ctx = tenantctx.With(ctx, "tenant-abc")
	ctx = WithUserID(ctx, "user-xyz")

	logger.InfoContext(ctx, "hello", "k", "v")

	m := decode(t, &buf)
	if m[AttrRequestID] != "req-123" {
		t.Errorf("request_id = %v, want req-123", m[AttrRequestID])
	}
	if m[AttrTenantID] != "tenant-abc" {
		t.Errorf("tenant_id = %v, want tenant-abc", m[AttrTenantID])
	}
	if m[AttrUserID] != "user-xyz" {
		t.Errorf("user_id = %v, want user-xyz", m[AttrUserID])
	}
	if m["msg"] != "hello" || m["k"] != "v" {
		t.Errorf("message/attrs not preserved: %v", m)
	}
}

func TestContextHandlerOmitsAbsentIDs(t *testing.T) {
	var buf bytes.Buffer
	logger := newTestLogger(&buf)

	logger.InfoContext(t.Context(), "plain")

	m := decode(t, &buf)
	for _, k := range []string{AttrRequestID, AttrTenantID, AttrUserID} {
		if _, ok := m[k]; ok {
			t.Errorf("expected %q to be absent, got %v", k, m[k])
		}
	}
}

func TestContextHandlerSurvivesWithAttrs(t *testing.T) {
	var buf bytes.Buffer
	logger := newTestLogger(&buf).With(AttrComponent, "test")

	ctx := WithRequestID(t.Context(), "req-9")
	logger.InfoContext(ctx, "msg")

	m := decode(t, &buf)
	if m[AttrComponent] != "test" {
		t.Errorf("component attr lost through With: %v", m)
	}
	if m[AttrRequestID] != "req-9" {
		t.Errorf("enrichment lost through With: %v", m)
	}
}

func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"DEBUG":   slog.LevelDebug,
		"info":    slog.LevelInfo,
		"":        slog.LevelInfo,
		"bogus":   slog.LevelInfo,
		"warn":    slog.LevelWarn,
		"warning": slog.LevelWarn,
		"error":   slog.LevelError,
	}
	for in, want := range cases {
		if got := ParseLevel(in); got != want {
			t.Errorf("ParseLevel(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestPreview(t *testing.T) {
	if got := Preview("short", 100); got != "short" {
		t.Errorf("under cap: got %q, want %q", got, "short")
	}
	if got := Preview("hello world", 5); got != "hello…" {
		t.Errorf("over cap: got %q, want %q", got, "hello…")
	}
	// Truncation must not split a multi-byte rune. "aé" is 3 bytes (a + é);
	// a cap of 2 lands inside é and must trim back to the valid boundary.
	if got := Preview("aé", 2); got != "a…" {
		t.Errorf("utf8 boundary: got %q, want %q", got, "a…")
	}
	if !utf8.ValidString(Preview("日本語テスト", 4)) {
		t.Errorf("Preview produced invalid UTF-8")
	}
}

func TestUseJSON(t *testing.T) {
	cases := []struct {
		format string
		debug  bool
		want   bool
	}{
		{"json", false, true},
		{"json", true, true},   // explicit wins over debug
		{"text", false, false}, // explicit wins
		{"text", true, false},
		{"", false, true}, // unset, prod → JSON
		{"", true, false}, // unset, debug → text
		{"JSON", false, true},
	}
	for _, c := range cases {
		if got := UseJSON(c.format, c.debug); got != c.want {
			t.Errorf("UseJSON(%q, %v) = %v, want %v", c.format, c.debug, got, c.want)
		}
	}
}
