package flowkit

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"text/template"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/modelconfig"
)

func TestRenderTemplate(t *testing.T) {
	tmpl := template.Must(template.New("t").Parse("hi {{.}}"))
	got, err := RenderTemplate(tmpl, "there")
	if err != nil || got != "hi there" {
		t.Fatalf("RenderTemplate = %q, %v", got, err)
	}
	bad := template.Must(template.New("t").Parse("{{.Missing}}"))
	if _, err := RenderTemplate(bad, 42); err == nil {
		t.Fatal("expected an execution error")
	}
}

func TestHistory(t *testing.T) {
	type row struct{ role, content string }
	rows := []row{{RoleUser, "q"}, {"system", "skip"}, {RoleModel, "a"}}
	got := History(rows, func(r row) (string, string) { return r.role, r.content })
	if len(got) != 2 {
		t.Fatalf("got %d messages, want 2", len(got))
	}
	if got[0].Role != ai.RoleUser || got[0].Text() != "q" {
		t.Errorf("first = %s %q", got[0].Role, got[0].Text())
	}
	if got[1].Role != ai.RoleModel || got[1].Text() != "a" {
		t.Errorf("second = %s %q", got[1].Role, got[1].Text())
	}
}

func TestStreamCallback(t *testing.T) {
	var events []string
	cb := StreamCallback(StreamHandlers{
		OnChunk:      func() { events = append(events, "chunk") },
		OnText:       func(s string) { events = append(events, "text:"+s) },
		OnToolCall:   func(r *ai.ToolRequest) { events = append(events, "call:"+r.Ref) },
		OnToolResult: func(r *ai.ToolResponse) { events = append(events, "result:"+r.Ref) },
	})
	call := &ai.ToolRequest{Name: "t", Ref: "1"}
	chunks := []*ai.ModelResponseChunk{
		nil,
		{Content: []*ai.Part{ai.NewTextPart("a"), ai.NewToolRequestPart(&ai.ToolRequest{Name: "t", Ref: "1", Partial: true})}},
		{Content: []*ai.Part{ai.NewToolRequestPart(call), ai.NewToolRequestPart(call)}},
		{Content: []*ai.Part{ai.NewToolResponsePart(&ai.ToolResponse{Name: "t", Ref: "1"}), ai.NewToolResponsePart(&ai.ToolResponse{Name: "t", Ref: "1"})}},
		{Aggregated: true, Content: []*ai.Part{ai.NewTextPart("replayed")}},
	}
	for _, c := range chunks {
		if err := cb(t.Context(), c); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"chunk", "text:a", "chunk", "call:1", "chunk", "result:1"}
	if !slices.Equal(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestStreamCallback_NilHandlers(t *testing.T) {
	cb := StreamCallback(StreamHandlers{})
	chunk := &ai.ModelResponseChunk{Content: []*ai.Part{
		ai.NewTextPart("a"),
		ai.NewToolRequestPart(&ai.ToolRequest{Ref: "1"}),
		ai.NewToolResponsePart(&ai.ToolResponse{Ref: "1"}),
	}}
	if err := cb(t.Context(), chunk); err != nil {
		t.Fatal(err)
	}
}

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestUsageFinish(t *testing.T) {
	logs := captureLogs(t)
	u := Usage{Model: modelconfig.Resolved{Vendor: "v", Model: "m"}, Feature: "f", Component: "c", Attrs: []any{"post_id", "p1"}}

	u.Finish(t.Context(), nil, 10)
	if logs.Len() != 0 {
		t.Fatalf("nil response logged: %s", logs)
	}

	u.Finish(t.Context(), &ai.ModelResponse{
		FinishReason: ai.FinishReasonLength,
		Usage:        &ai.GenerationUsage{InputTokens: 3, OutputTokens: 4},
	}, 10)
	out := logs.String()
	for _, want := range []string{
		`msg="response truncated at max tokens" component=c post_id=p1 output_tokens=4 cap=10`,
		`msg=tokens component=c post_id=p1 input=3 output=4 total=7`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("logs missing %q:\n%s", want, out)
		}
	}
}

func TestUsageLogTokens_NoUsage(t *testing.T) {
	logs := captureLogs(t)
	Usage{Component: "c"}.LogTokens(t.Context(), "tokens", &ai.ModelResponse{})
	if logs.Len() != 0 {
		t.Fatalf("logged without usage: %s", logs)
	}
}

// fakeModel streams chunks, then fails with err when non-nil.
func fakeModel(t *testing.T, chunks []string, err error) *genkit.Genkit {
	t.Helper()
	g := genkit.Init(t.Context())
	genkit.DefineModel(g, "test/fake", &ai.ModelOptions{Supports: &ai.ModelSupports{Multiturn: true}},
		func(ctx context.Context, _ *ai.ModelRequest, cb ai.ModelStreamCallback) (*ai.ModelResponse, error) {
			for _, c := range chunks {
				if cb != nil {
					if cerr := cb(ctx, &ai.ModelResponseChunk{Content: []*ai.Part{ai.NewTextPart(c)}}); cerr != nil {
						return nil, cerr
					}
				}
			}
			if err != nil {
				return nil, err
			}
			return &ai.ModelResponse{Message: ai.NewModelTextMessage(strings.Join(chunks, ""))}, nil
		})
	return g
}

func TestStreamObjects(t *testing.T) {
	chunks := []string{`[{"a":1},{"b"`, `:"}"}`, `,{"c":3}]`}
	g := fakeModel(t, chunks, nil)
	var got []string
	res, err := StreamObjects(t.Context(), g, func(pos int, raw string) {
		if pos != len(got) {
			t.Errorf("pos = %d, want %d", pos, len(got))
		}
		got = append(got, raw)
	}, ai.WithModelName("test/fake"), ai.WithPrompt("go"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{`{"a":1}`, `{"b":"}"}`, `{"c":3}`}
	if !slices.Equal(got, want) {
		t.Fatalf("objects = %q, want %q", got, want)
	}
	if res.Response == nil || res.Objects != 3 || res.Chunks != 3 || res.Bytes != len(strings.Join(chunks, "")) {
		t.Fatalf("result = %+v", res)
	}
}

func TestStreamObjects_IgnoresThinking(t *testing.T) {
	g := genkit.Init(t.Context())
	genkit.DefineModel(g, "test/thinker", &ai.ModelOptions{Supports: &ai.ModelSupports{Multiturn: true}},
		func(ctx context.Context, _ *ai.ModelRequest, cb ai.ModelStreamCallback) (*ai.ModelResponse, error) {
			for _, c := range []*ai.ModelResponseChunk{
				{Content: []*ai.Part{ai.NewReasoningPart(`[{"x":0}]`, nil)}},
				{Content: []*ai.Part{ai.NewTextPart(`[{"a":1}]`)}},
			} {
				if err := cb(ctx, c); err != nil {
					return nil, err
				}
			}
			return &ai.ModelResponse{Message: ai.NewModelTextMessage(`[{"a":1}]`)}, nil
		})
	var got []string
	if _, err := StreamObjects(t.Context(), g, func(_ int, raw string) { got = append(got, raw) },
		ai.WithModelName("test/thinker"), ai.WithPrompt("go")); err != nil {
		t.Fatal(err)
	}
	if want := []string{`{"a":1}`}; !slices.Equal(got, want) {
		t.Fatalf("objects = %q, want %q", got, want)
	}
}

func TestStreamObjects_Error(t *testing.T) {
	g := fakeModel(t, []string{`[{"a":1},{"b":`}, errors.New("boom"))
	var n int
	res, err := StreamObjects(t.Context(), g, func(int, string) { n++ }, ai.WithModelName("test/fake"), ai.WithPrompt("go"))
	if err == nil {
		t.Fatal("expected the stream error")
	}
	if res.Response != nil || res.Objects != 1 || n != 1 {
		t.Fatalf("result = %+v, delivered %d", res, n)
	}
}
